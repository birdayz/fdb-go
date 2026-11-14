package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"

	"fdb.dev/gen"
	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	otelDomain    = "OTEL"
	tenantSchema  = "T0"
	splitSize     = 100_000
	setupBoundary = "Go-owned setup through the pure-Go client for any arm: atomic fresh /OTEL/R_<RUN> reservation, production otel.NewExporter span-only initialization then Shutdown, and an exact baseline/catalog seal; outside every measured window"
)

var spanIndexes = []string{"SPANS_TIME", "SPANS_SPAN_ID", "SPANS_STR_ATTR", "SPANS_LONG_STR_ATTR", "SPANS_INT_ATTR", "SPANS_DOUBLE_ATTR", "SPANS_BOOL_ATTR", "SPANS_REF_CHUNKS"}

type expectedValue struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

// layoutManifest is exactly the Rust writer's Layout (writer.rs, deny_unknown_fields).
type layoutManifest struct {
	Version          int               `json:"version"`
	DescriptorSHA256 string            `json:"descriptor_sha256"`
	RecordPrefix     string            `json:"record_prefix"`
	IndexPrefixes    map[string]string `json:"index_prefixes"`
	Checks           []expectedValue   `json:"checks"`
	EmptyRanges      [][2]string       `json:"empty_ranges"`
	Messages         string            `json:"messages"`
}

type baselineSummary struct {
	RecordIdentities   int64            `json:"record_identities"`
	IndexEntries       map[string]int64 `json:"index_entries"`
	PhysicalRecordKVs  int64            `json:"physical_record_kvs"`
	HeaderKVs          int64            `json:"header_kvs"`
	NormalizedKVSHA256 string           `json:"normalized_kv_sha256"`
}

type setupReport struct {
	reportCommon
	DatabasePath string          `json:"database_path"`
	Tenant       int             `json:"tenant"`
	Baseline     baselineSummary `json:"baseline"`
}

// parseManifest decodes a layout manifest with exactly the Rust Layout's fields.
func parseManifest(data []byte) (layoutManifest, error) {
	bad := refuse("invalid or unsupported OTEL_V1 layout manifest")
	if checkStructure(data) != nil {
		return layoutManifest{}, bad
	}
	top, err := exactObject("layout", data, []string{"version", "descriptor_sha256", "record_prefix", "index_prefixes", "checks", "empty_ranges", "messages"})
	if err != nil {
		return layoutManifest{}, bad
	}
	var checks []json.RawMessage
	if json.Unmarshal(top["checks"], &checks) != nil {
		return layoutManifest{}, bad
	}
	for _, c := range checks {
		if _, err := exactObject("layout.checks", c, []string{"key", "value"}, "value"); err != nil {
			return layoutManifest{}, bad
		}
	}
	var m layoutManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil {
		return layoutManifest{}, bad
	}
	return m, nil
}

func tenantPaths(run string) (string, string) {
	return "/" + otelDomain + "/R_" + strings.ToUpper(run), tenantSchema
}

// exporterConfig is the production Exporter configuration of a job: every storage and
// write field explicit; only reader-side limits come from the library defaults.
func exporterConfig(j *job) otel.ExporterConfig {
	cfg := otel.DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant = j.ClusterFile, j.Run, 0
	cfg.RecordCompression = j.RecordCompression
	cfg.BulkIndexConflicts, cfg.EnableMessageDeltas, cfg.EnableMessageFullText = false, false, false
	cfg.BatchSize, cfg.Workers = j.WriterBatchSize, j.WriterWorkers
	cfg.MaxInFlightTransactions = j.globalWriteBound()
	cfg.TransactionTimeout = txnTimeout
	return cfg
}

func descriptorDigest() (string, error) {
	files := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var walk func(protoreflect.FileDescriptor)
	walk = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		for i := range fd.Imports().Len() {
			walk(fd.Imports().Get(i).FileDescriptor)
		}
		files.File = append(files.File, protodesc.ToFileDescriptorProto(fd))
	}
	walk(tracepb.File_opentelemetry_proto_trace_v1_trace_proto)
	walk(otelv1pb.File_OTEL_V1)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(files)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// expectedTemplate is the OTEL_V1 template exactly as its DDL builds it, unbound: the
// form whose proto serializes, so the exact comparison is made on it.
func expectedTemplate() (*recordlayer.RecordMetaData, error) {
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(otelsql.TemplateDDL())
	if err != nil {
		return nil, err
	}
	return tmpl.Underlying(), nil
}

// expectedMetadata is the OTEL_V1 template bound to the compiled types, as stores open it.
func expectedMetadata() (*recordlayer.RecordMetaData, error) {
	tmpl, err := expectedTemplate()
	if err != nil {
		return nil, err
	}
	p, err := tmpl.ToProto()
	if err != nil {
		return nil, err
	}
	return recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
}

// exactTemplate requires stored, a tenant's template metadata as the catalog holds it
// before any compiled binding, to serialize exactly as the OTEL_V1 template: records
// descriptor, record types, indexes, version and settings. Bound metadata cannot be
// serialized (Record Layer refuses a local descriptor), so it is refused here.
func exactTemplate(stored *recordlayer.RecordMetaData) error {
	want, err := expectedTemplate()
	if err != nil {
		return err
	}
	wantProto, err := want.ToProto()
	if err != nil {
		return err
	}
	if stored == nil {
		return fault("layout", "no stored template metadata to compare")
	}
	gotProto, err := stored.ToProto()
	if err != nil {
		return fault("layout", "the stored template metadata does not serialize")
	}
	if !proto.Equal(gotProto, wantProto) {
		return fault("layout", "the stored metadata is not exactly the OTEL_V1 template")
	}
	return nil
}

// storedTemplate is the tenant schema's template metadata as the catalog holds it, read
// in rc's transaction through the catalog API otelsql itself uses.
func storedTemplate(rc *recordlayer.FDBRecordContext, path, schema string) (*recordlayer.RecordMetaData, error) {
	cat, err := catalog.NewRecordLayerStoreCatalog(keyspace.New(subspace.Sub()).CatalogSubspace())
	if err != nil {
		return nil, err
	}
	s, err := cat.LoadSchema(catalog.NewFDBTransaction(rc), path, schema)
	if err != nil {
		return nil, err
	}
	up, ok := s.SchemaTemplate().(interface {
		Underlying() *recordlayer.RecordMetaData
	})
	if !ok {
		return nil, fault("layout", "the tenant's template is not Record Layer metadata")
	}
	return up.Underlying(), nil
}

// openTenant opens the tenant's compiled store in one record context. In that context,
// before otelsql resolves or binds anything, it rereads the stored template and refuses
// one that is not exactly OTEL_V1. It returns the store with that template, which
// checkTenantMetadata checks again with the binding.
func openTenant(newContext func() *recordlayer.FDBRecordContext, path, schema string, serializer *recordlayer.TransformedRecordSerializer) (*otelsql.TenantStore, *recordlayer.RecordMetaData, error) {
	var stored *recordlayer.RecordMetaData
	ts, err := otelsql.OpenTenantStoreCompiledIn(func(fn func(*recordlayer.FDBRecordContext) error) error {
		rc := newContext()
		var err error
		if stored, err = storedTemplate(rc, path, schema); err != nil {
			return err
		}
		if err := exactTemplate(stored); err != nil {
			return err
		}
		return fn(rc)
	}, path, schema, serializer)
	if err != nil {
		return nil, nil, err
	}
	return ts, stored, nil
}

// checkTenantMetadata requires the stored template to be exactly OTEL_V1 and the store's
// compiled binding of it to be that version and keep every invariant writers rely on.
func checkTenantMetadata(ts *otelsql.TenantStore, stored *recordlayer.RecordMetaData) error {
	if err := exactTemplate(stored); err != nil {
		return err
	}
	return validateMetadata(ts.MetaData, stored.Version())
}

// validateMetadata requires md, the compiled binding, to be at the stored template's
// version: unversioned, count-free and splitting, its SPANS with the eight VALUE indexes
// and its STORE_META with only STORE_META_NAME.
func validateMetadata(md *recordlayer.RecordMetaData, version int) error {
	bad := func(reason string) error { return fault("layout", reason) }
	if md.Version() != version || md.IsStoreRecordVersions() || md.GetRecordCountKey() != nil || !md.IsSplitLongRecords() {
		return bad("metadata is not the exact non-versioned, count-free OTEL_V1 template")
	}
	rt := md.GetRecordType("SPANS")
	if rt == nil || md.GetUnionFieldForRecordType(rt).Number() != 1 || len(rt.GetAllIndexes()) != len(spanIndexes) {
		return bad("unsupported SPANS union field or index population")
	}
	for _, name := range spanIndexes {
		idx := md.GetIndex(name)
		if idx == nil || idx.Type != recordlayer.IndexTypeValue || idx.IsUnique() || idx.HasPredicate() {
			return bad("all eight SPANS indexes must be ordinary VALUE indexes")
		}
	}
	meta := md.GetRecordType("STORE_META")
	metaIndexes := md.GetIndexesForRecordType("STORE_META")
	if meta == nil || len(metaIndexes) != 1 || metaIndexes[0].Name != "STORE_META_NAME" ||
		metaIndexes[0].Type != recordlayer.IndexTypeValue || metaIndexes[0].IsUnique() || metaIndexes[0].HasPredicate() {
		return bad("STORE_META must have exactly the ordinary VALUE index STORE_META_NAME")
	}
	return nil
}

func validateHeader(wire []byte, md *recordlayer.RecordMetaData) error {
	if len(wire) == 0 {
		return fault("layout", "store header is absent")
	}
	var h gen.DataStoreInfo
	if err := proto.Unmarshal(wire, &h); err != nil {
		return fault("layout", "store header does not decode")
	}
	if h.GetFormatVersion() != 14 || h.GetMetaDataversion() != int32(md.Version()) || h.GetOmitUnsplitRecordSuffix() ||
		h.RecordCountKey != nil || h.StoreLockState != nil || len(h.ProtoReflect().GetUnknown()) != 0 {
		return fault("layout", "unsupported header: format 14, exact metadata, suffixes and no count, lock or unknown fields required")
	}
	return nil
}

func asTuple(in []any) tuple.Tuple {
	out := make(tuple.Tuple, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func prefixRange(ss subspace.Subspace) fdb.KeyRange {
	b, e := ss.FDBRangeKeys()
	return fdb.KeyRange{Begin: b.FDBKey(), End: e.FDBKey()}
}

func recordSubspace(ts *otelsql.TenantStore) subspace.Subspace {
	return ts.Subspace.Sub(int64(recordlayer.RecordKey))
}

func indexSubspace(ts *otelsql.TenantStore, idx *recordlayer.Index) subspace.Subspace {
	return ts.Subspace.Sub(int64(recordlayer.IndexKey), idx.SubspaceTupleKey())
}

func headerKey(ts *otelsql.TenantStore) []byte {
	return ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.StoreInfoKey)})
}

func compressionLevel(mode string) (int, bool) {
	switch mode {
	case "zlib-1":
		return 1, true
	case "zlib-default":
		return zlib.DefaultCompression, true
	}
	return 0, false
}

// goStored is the transformed record value the Go serializer stores for union under
// mode: the bare union, a clear-prefixed union, or a compressed one (prefix 4, version
// 1, BE32 union length, zlib) when compression saves more than its 5-byte header.
func goStored(union []byte, mode string) ([]byte, bool, error) {
	level, compress := compressionLevel(mode)
	if !compress {
		return union, false, nil
	}
	if len(union) > 5 {
		var buf bytes.Buffer
		w, err := zlib.NewWriterLevel(&buf, level)
		if err != nil {
			return nil, false, err
		}
		if _, err := w.Write(union); err != nil {
			return nil, false, err
		}
		if err := w.Close(); err != nil {
			return nil, false, err
		}
		if buf.Len() < len(union)-5 {
			out := make([]byte, 6, 6+buf.Len())
			out[0], out[1] = 4, 1
			binary.BigEndian.PutUint32(out[2:6], uint32(len(union)))
			return append(out, buf.Bytes()...), true, nil
		}
	}
	return append([]byte{2}, union...), false, nil
}

// chunkCount is how many physical KVs a stored value of n bytes splits into.
func chunkCount(n int) int {
	if n <= splitSize {
		return 1
	}
	return (n + splitSize - 1) / splitSize
}

// splitKeys are the record keys of a stored value of n bytes: suffix 0 unsplit, 1..k split.
func splitKeys(records subspace.Subspace, pk tuple.Tuple, n int) [][]byte {
	if chunkCount(n) == 1 {
		return [][]byte{records.Pack(append(slices.Clone(pk), int64(0)))}
	}
	var out [][]byte
	for i := range chunkCount(n) {
		out = append(out, records.Pack(append(slices.Clone(pk), int64(i+1))))
	}
	return out
}

func splitValue(stored []byte) [][]byte {
	if len(stored) <= splitSize {
		return [][]byte{stored}
	}
	var out [][]byte
	for off := 0; off < len(stored); off += splitSize {
		out = append(out, stored[off:min(off+splitSize, len(stored))])
	}
	return out
}

// baseline is the exact Go-owned content of an initialized tenant: its header, the
// STORE_META("layout") row and that row's one STORE_META_NAME entry. Only the header
// bytes come from the store; the row, its stored chunks and the entry are derived.
type baseline struct {
	header       []byte
	kvs          map[string][]byte
	layoutPK     tuple.Tuple
	layout       *otelv1pb.STORE_META
	layoutStored []byte
	records      int
	indexKey     string
}

func deriveBaseline(ts *otelsql.TenantStore, header []byte, mode string) (*baseline, error) {
	md := ts.MetaData
	if err := validateHeader(header, md); err != nil {
		return nil, err
	}
	meta := md.GetRecordType("STORE_META")
	nameIndex := md.GetIndex("STORE_META_NAME")
	if meta == nil || nameIndex == nil {
		return nil, fault("layout", "STORE_META or STORE_META_NAME is absent")
	}
	value := otel.StoreLayoutNone
	row, err := otelsql.BuildStoreMetaCompiled(otelsql.StoreMeta{Name: otelsql.StoreMetaLayout, Str: &value})
	if err != nil {
		return nil, err
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(row)
	if err != nil {
		return nil, err
	}
	rec := &recordlayer.FDBStoredRecord[proto.Message]{Record: row, RecordType: meta}
	pks, err := meta.PrimaryKey.Evaluate(rec, row)
	if err != nil || len(pks) != 1 {
		return nil, fault("layout", "STORE_META primary key is not exactly one tuple")
	}
	pk := asTuple(pks[0])
	entries, err := nameIndex.RootExpression.Evaluate(rec, row)
	if err != nil || len(entries) != 1 {
		return nil, fault("layout", "STORE_META_NAME does not have exactly one entry")
	}
	rest, err := nameIndex.TrimPrimaryKey(pk)
	if err != nil {
		return nil, err
	}
	union := protowire.AppendBytes(protowire.AppendTag(nil, protowire.Number(md.GetUnionFieldForRecordType(meta).Number()), protowire.BytesType), wire)
	stored, _, err := goStored(union, mode)
	if err != nil {
		return nil, err
	}
	b := &baseline{header: header, kvs: map[string][]byte{string(headerKey(ts)): header}, layoutPK: pk, layout: row, layoutStored: stored}
	keys, values := splitKeys(recordSubspace(ts), pk, len(stored)), splitValue(stored)
	for i, k := range keys {
		b.kvs[string(k)] = values[i]
	}
	b.records = len(keys)
	b.indexKey = string(indexSubspace(ts, nameIndex).Pack(append(asTuple(entries[0]), rest...)))
	b.kvs[b.indexKey] = []byte{}
	if len(b.kvs) != 2+b.records {
		return nil, fault("layout", "baseline header, record and index keys collide")
	}
	return b, nil
}

func (b *baseline) summary() baselineSummary {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(b.kvs)) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(k)))
		h.Write(n[:])
		h.Write([]byte(k))
		binary.BigEndian.PutUint64(n[:], uint64(len(b.kvs[k])))
		h.Write(n[:])
		h.Write(b.kvs[k])
	}
	return baselineSummary{
		RecordIdentities: 1, IndexEntries: map[string]int64{"STORE_META_NAME": 1},
		PhysicalRecordKVs: int64(b.records), HeaderKVs: 1, NormalizedKVSHA256: hex.EncodeToString(h.Sum(nil)),
	}
}

// checkStandardBaseline reads the baseline back through the standard serializer: the
// store holds exactly one record, the derived STORE_META("layout") row.
func checkStandardBaseline(ctx context.Context, store *recordlayer.FDBRecordStore, b *baseline) error {
	rec, err := store.LoadRecord(b.layoutPK)
	if err != nil {
		return err
	}
	if rec == nil || rec.RecordType.Name != "STORE_META" {
		return fault("layout", "the STORE_META(layout) row is absent")
	}
	got, ok := rec.Record.(*otelv1pb.STORE_META)
	if !ok {
		return fault("layout", "the layout row is not a STORE_META")
	}
	gotWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(got)
	if err != nil {
		return err
	}
	wantWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(b.layout)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotWire, wantWire) {
		return fault("layout", "the STORE_META(layout) row differs from messages=none")
	}
	cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
	defer func() { _ = cursor.Close() }()
	n := 0
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return err
		}
		if !result.HasNext() {
			break
		}
		n++
	}
	if n != 1 {
		return fault("layout", "the baseline store must hold exactly the layout record")
	}
	return nil
}

// reserve claims path in the same serializable catalog transaction that checks it is
// new; a failed later setup leaves it claimed, so a rerun can never adopt it.
func reserve(ctx context.Context, db *recordlayer.FDBDatabase, path string) error {
	cat, err := catalog.OpenRecordLayerStoreCatalog()
	if err != nil {
		return err
	}
	_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := rc.Transaction().Options().SetTimeout(txnTimeout.Milliseconds()); err != nil {
			return nil, err
		}
		tx := catalog.NewFDBTransaction(rc)
		if err := cat.Initialize(tx); err != nil {
			return nil, err
		}
		exists, err := cat.DoesDatabaseExist(tx, path)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fault("setup", "the run database already exists; a fresh namespace is required")
		}
		return nil, cat.CreateDatabase(tx, path)
	})
	return err
}

func runSetup(ctx context.Context, rt runtime, j *job, r *setupReport) error {
	path, schema := tenantPaths(j.Run)
	r.DatabasePath, r.Tenant = path, 0
	out, err := newOutput(j.LayoutFile)
	if err != nil {
		return fault("setup", "layout_file must be a new writable file")
	}
	defer out.Close()
	backend, err := rt.openPure(ctx, j.ClusterFile)
	if err != nil {
		return err
	}
	defer backend.Close()
	keyspace.RegisterDomainIfNotExists(otelDomain)
	if err := reserve(ctx, recordlayer.NewFDBDatabaseWithBackend(backend), path); err != nil {
		return err
	}
	// The production Exporter writes the real header and STORE_META("layout") baseline.
	e, err := otel.NewExporter(ctx, exporterConfig(j))
	if err != nil {
		return err
	}
	if err := e.Shutdown(ctx); err != nil {
		return err
	}
	m, b, err := captureLayout(ctx, backend, j, path, schema)
	if err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	a, err := fileArtifact("layout", j.LayoutFile)
	if err != nil {
		return err
	}
	r.LayoutSHA256 = &a.SHA256
	r.Artifacts = append(r.Artifacts, a)
	r.Baseline = b.summary()
	return nil
}

// pointReads records the value of every key a fresh resolver reads, so directory,
// interning and catalog lookups are sealed without reimplementing their key layout.
type pointReads struct {
	mu     sync.Mutex
	values map[string]fdb.FutureByteSlice
}

func (p *pointReads) get(tx fdb.ReadTransaction, key fdb.KeyConvertible) fdb.FutureByteSlice {
	future := tx.Get(key)
	k := string(key.FDBKey())
	p.mu.Lock()
	if _, exists := p.values[k]; !exists && len(k) > 0 && k[0] != 0xff {
		p.values[k] = future
	}
	p.mu.Unlock()
	return future
}

type sealingTransaction struct {
	fdb.WritableTransaction
	points *pointReads
}

func (t *sealingTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return t.points.get(t.WritableTransaction, key)
}

func (t *sealingTransaction) Snapshot() fdb.ReadTransaction {
	return sealingRead{ReadTransaction: t.WritableTransaction.Snapshot(), points: t.points}
}

type sealingRead struct {
	fdb.ReadTransaction
	points *pointReads
}

func (t sealingRead) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return t.points.get(t.ReadTransaction, key)
}

// inspection is a transaction that is never committed: whatever opening a store would
// write stays tentative and is discarded.
func inspection(ctx context.Context, backend fdb.BackendDatabase) (fdb.WritableTransaction, func(), error) {
	tx, err := backend.CreateWritableTransaction()
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, tx.Cancel)
	done := func() {
		stop()
		tx.Cancel()
	}
	for _, set := range []func() error{
		func() error { return tx.Options().SetTimeout(txnTimeout.Milliseconds()) },
		func() error { return tx.Options().SetRetryLimit(0) },
		func() error { return tx.Options().SetReadSystemKeys() },
	} {
		if err := set(); err != nil {
			done()
			return nil, nil, err
		}
	}
	return tx, done, nil
}

// captureLayout derives the expected baseline from Go metadata and the serializer, then
// requires the tenant to hold exactly it, and seals the baseline, the resolver's point
// reads, the tenant's catalog records with their gaps and the index-state subspaces.
func captureLayout(ctx context.Context, backend fdb.BackendDatabase, j *job, path, schema string) (layoutManifest, *baseline, error) {
	m := layoutManifest{Version: 1, Messages: "none", IndexPrefixes: map[string]string{}, Checks: []expectedValue{}, EmptyRanges: [][2]string{}}
	tx, done, err := inspection(ctx, backend)
	if err != nil {
		return m, nil, err
	}
	defer done()
	serializer, err := otel.RecordSerializer(j.RecordCompression)
	if err != nil {
		return m, nil, err
	}
	points := &pointReads{values: map[string]fdb.FutureByteSlice{}}
	wrapped := &sealingTransaction{WritableTransaction: tx, points: points}
	fresh := recordlayer.NewFDBDatabaseWithBackend(backend)
	ts, stored, err := openTenant(func() *recordlayer.FDBRecordContext {
		return fresh.NewRecordContextIn(ctx, wrapped)
	}, path, schema, serializer)
	if err != nil {
		return m, nil, err
	}
	if err := checkTenantMetadata(ts, stored); err != nil {
		return m, nil, err
	}
	if m.DescriptorSHA256, err = descriptorDigest(); err != nil {
		return m, nil, err
	}
	m.RecordPrefix = hex.EncodeToString(recordSubspace(ts).Bytes())
	for _, name := range spanIndexes {
		m.IndexPrefixes[name] = hex.EncodeToString(indexSubspace(ts, ts.MetaData.GetIndex(name)).Bytes())
	}
	header, err := wrapped.Get(fdb.Key(headerKey(ts))).Get()
	if err != nil {
		return m, nil, err
	}
	b, err := deriveBaseline(ts, header, j.RecordCompression)
	if err != nil {
		return m, nil, err
	}
	kvs, err := tx.GetRange(prefixRange(ts.Subspace), fdb.RangeOptions{Limit: len(b.kvs) + 1}).GetSliceWithError()
	if err != nil {
		return m, nil, err
	}
	if len(kvs) != len(b.kvs) {
		return m, nil, fault("setup", "the initialized tenant is not exactly header + STORE_META(layout) + STORE_META_NAME")
	}
	for _, kv := range kvs {
		if want, ok := b.kvs[string(kv.Key)]; !ok || !bytes.Equal(want, kv.Value) {
			return m, nil, fault("setup", "an initialized tenant key differs from the derived baseline")
		}
	}
	if err := standardBaseline(ctx, backend, ts, b); err != nil {
		return m, nil, err
	}
	checks := maps.Clone(b.kvs)
	points.mu.Lock()
	for key, future := range points.values {
		value, err := future.Get()
		if err != nil {
			points.mu.Unlock()
			return m, nil, err
		}
		checks[key] = bytes.Clone(value)
	}
	points.mu.Unlock()
	for k := recordlayer.IndexSecondarySpaceKey; k <= recordlayer.IndexSlidingWindowSpaceKey; k++ {
		r := prefixRange(ts.Subspace.Sub(int64(k)))
		m.EmptyRanges = append(m.EmptyRanges, [2]string{hex.EncodeToString(r.Begin.FDBKey()), hex.EncodeToString(r.End.FDBKey())})
	}
	records := catalog.DefaultCatalogSubspace().Sub(int64(recordlayer.RecordKey))
	for _, prefix := range []subspace.Subspace{
		records.Sub(catalog.SchemaRecordTypeKey, path, schema),
		records.Sub(catalog.DatabaseInfoRecordTypeKey, path),
		records.Sub(catalog.SchemaTemplateRecordTypeKey, otelsql.TemplateName),
	} {
		r := prefixRange(prefix)
		kvs, err := tx.GetRange(r, fdb.RangeOptions{Limit: 257}).GetSliceWithError()
		if err != nil {
			return m, nil, err
		}
		if len(kvs) == 0 || len(kvs) > 256 {
			return m, nil, fault("setup", "a catalog seal is absent or exceeds its 256-KV bound")
		}
		if err := sealRange(&m, checks, r, kvs); err != nil {
			return m, nil, err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(checks)) {
		var value *string
		if checks[k] != nil {
			v := hex.EncodeToString(checks[k])
			value = &v
		}
		m.Checks = append(m.Checks, expectedValue{Key: hex.EncodeToString([]byte(k)), Value: value})
	}
	if err := checkSeal(tx, m); err != nil {
		return m, nil, err
	}
	return m, b, ctx.Err()
}

// standardBaseline loads the baseline through the standard serializer in its own
// uncommitted transaction, so a store open's tentative writes never reach the seals.
func standardBaseline(ctx context.Context, backend fdb.BackendDatabase, ts *otelsql.TenantStore, b *baseline) error {
	return inStore(ctx, backend, ts, func(store *recordlayer.FDBRecordStore) error {
		return checkStandardBaseline(ctx, store, b)
	})
}

// inStore runs fn on the standard store in its own uncommitted transaction.
func inStore(ctx context.Context, backend fdb.BackendDatabase, ts *otelsql.TenantStore, fn func(*recordlayer.FDBRecordStore) error) error {
	tx, done, err := inspection(ctx, backend)
	if err != nil {
		return err
	}
	defer done()
	store, err := ts.Builder(recordlayer.NewFDBDatabaseWithBackend(backend).NewRecordContextIn(ctx, tx)).Open()
	if err != nil {
		return err
	}
	return fn(store)
}

// openSealed checks every manifest seal before anything resolves or opens the tenant,
// then resolves its metadata and derives its baseline, all in one transaction that is
// never committed: a metadata open's tentative writes are discarded, so nothing is
// repaired. With baselineOnly the tenant must also hold exactly that baseline.
func openSealed(ctx context.Context, backend fdb.BackendDatabase, j *job, m *layoutManifest, baselineOnly bool) (*otelsql.TenantStore, *baseline, error) {
	serializer, err := otel.RecordSerializer(j.RecordCompression)
	if err != nil {
		return nil, nil, err
	}
	tx, done, err := inspection(ctx, backend)
	if err != nil {
		return nil, nil, err
	}
	defer done()
	if err := checkSeal(tx, *m); err != nil {
		return nil, nil, err
	}
	path, schema := tenantPaths(j.Run)
	db := recordlayer.NewFDBDatabaseWithBackend(backend)
	ts, stored, err := openTenant(func() *recordlayer.FDBRecordContext {
		return db.NewRecordContextIn(ctx, tx)
	}, path, schema, serializer)
	if err != nil {
		return nil, nil, err
	}
	b, err := validateManifest(ts, stored, *m, j.RecordCompression)
	if err != nil {
		return nil, nil, err
	}
	if baselineOnly {
		kvs, err := tx.GetRange(prefixRange(ts.Subspace), fdb.RangeOptions{Limit: len(b.kvs) + 1}).GetSliceWithError()
		if err != nil {
			return nil, nil, err
		}
		if len(kvs) != len(b.kvs) {
			return nil, nil, fault("ingest", "the tenant must hold exactly its sealed baseline before ingest")
		}
		for _, kv := range kvs {
			if want, ok := b.kvs[string(kv.Key)]; !ok || !bytes.Equal(want, kv.Value) {
				return nil, nil, fault("ingest", "the tenant must hold exactly its sealed baseline before ingest")
			}
		}
	}
	return ts, b, nil
}

func sealRange(m *layoutManifest, checks map[string][]byte, r fdb.KeyRange, kvs []fdb.KeyValue) error {
	lo := bytes.Clone(r.Begin.FDBKey())
	end := r.End.FDBKey()
	for _, kv := range kvs {
		if bytes.Compare(lo, kv.Key) > 0 || bytes.Compare(kv.Key, end) >= 0 {
			return fault("setup", "a catalog range result is unordered or outside its bounds")
		}
		if bytes.Compare(lo, kv.Key) < 0 {
			m.EmptyRanges = append(m.EmptyRanges, [2]string{hex.EncodeToString(lo), hex.EncodeToString(kv.Key)})
		}
		checks[string(kv.Key)] = bytes.Clone(kv.Value)
		lo = append(bytes.Clone(kv.Key), 0)
	}
	if bytes.Compare(lo, end) < 0 {
		m.EmptyRanges = append(m.EmptyRanges, [2]string{hex.EncodeToString(lo), hex.EncodeToString(end)})
	}
	return nil
}

// checkSeal requires every sealed key to hold its value (or be absent) and every sealed
// range to be empty.
func checkSeal(tx fdb.ReadTransaction, m layoutManifest) error {
	for _, c := range m.Checks {
		key, err := hex.DecodeString(c.Key)
		if err != nil || len(key) == 0 || key[0] == 0xff {
			return fault("layout", "invalid seal key")
		}
		var want []byte
		if c.Value != nil {
			if want, err = hex.DecodeString(*c.Value); err != nil {
				return fault("layout", "invalid seal value")
			}
		}
		got, err := tx.Get(fdb.Key(key)).Get()
		if err != nil {
			return err
		}
		if (got == nil) != (c.Value == nil) || !bytes.Equal(got, want) {
			return fault("layout", "a sealed key changed")
		}
	}
	for _, r := range m.EmptyRanges {
		begin, e1 := hex.DecodeString(r[0])
		end, e2 := hex.DecodeString(r[1])
		if e1 != nil || e2 != nil || len(begin) == 0 || bytes.Compare(begin, end) >= 0 || end[0] == 0xff {
			return fault("layout", "invalid empty-range seal")
		}
		kvs, err := tx.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(kvs) != 0 {
			return fault("layout", "a sealed empty range changed")
		}
	}
	return nil
}

// validateManifest requires ts's stored template to be exactly OTEL_V1 and m to name ts
// exactly: its descriptors, prefixes, index-state guards and, among the checks, exactly
// the tenant's derived baseline. It returns that baseline.
func validateManifest(ts *otelsql.TenantStore, stored *recordlayer.RecordMetaData, m layoutManifest, mode string) (*baseline, error) {
	if err := checkTenantMetadata(ts, stored); err != nil {
		return nil, err
	}
	digest, err := descriptorDigest()
	if err != nil {
		return nil, err
	}
	bad := func(reason string) error { return fault("layout", reason) }
	if m.Version != 1 || m.Messages != "none" || m.DescriptorSHA256 != digest || m.RecordPrefix != hex.EncodeToString(recordSubspace(ts).Bytes()) || len(m.IndexPrefixes) != len(spanIndexes) {
		return nil, bad("the manifest does not name the resolved OTEL_V1 store")
	}
	for _, name := range spanIndexes {
		if m.IndexPrefixes[name] != hex.EncodeToString(indexSubspace(ts, ts.MetaData.GetIndex(name)).Bytes()) {
			return nil, bad("a manifest index prefix differs from the metadata")
		}
	}
	tenant := map[string][]byte{}
	prefix := ts.Subspace.Bytes()
	for _, c := range m.Checks {
		key, err := hex.DecodeString(c.Key)
		if err != nil || c.Value == nil || !bytes.HasPrefix(key, prefix) {
			continue
		}
		value, err := hex.DecodeString(*c.Value)
		if err != nil {
			return nil, bad("invalid seal value")
		}
		tenant[string(key)] = value
	}
	b, err := deriveBaseline(ts, tenant[string(headerKey(ts))], mode)
	if err != nil {
		return nil, err
	}
	if len(tenant) != len(b.kvs) {
		return nil, bad("the manifest's tenant seals are not exactly the derived baseline")
	}
	for k, v := range b.kvs {
		if got, ok := tenant[k]; !ok || !bytes.Equal(got, v) {
			return nil, bad("the manifest's tenant seals are not exactly the derived baseline")
		}
	}
	for k := recordlayer.IndexSecondarySpaceKey; k <= recordlayer.IndexSlidingWindowSpaceKey; k++ {
		r := prefixRange(ts.Subspace.Sub(int64(k)))
		guard := [2]string{hex.EncodeToString(r.Begin.FDBKey()), hex.EncodeToString(r.End.FDBKey())}
		if !slices.Contains(m.EmptyRanges, guard) {
			return nil, bad("the manifest omits an index-state subspace guard")
		}
	}
	return b, nil
}
