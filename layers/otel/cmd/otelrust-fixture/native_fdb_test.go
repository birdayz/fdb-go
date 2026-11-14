package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var (
	otelRustRunfile    = flag.String("otel-rust", "", "runfile of //layers/otel/rust:otel-rust")
	nativeProofRunfile = flag.String("native-proof", "", "runfile of //layers/otel/rust:native_proof")
	libfdbCRunfile     = flag.String("libfdb-c", "", "runfile of @libfdb_c_linux_x86_64//file")
)

const (
	envRefused     = "Config: inherited FDB_NETWORK_OPTION_* environment variables are not allowed"
	proofScenario  = "applied_commit_unknown_result"
	proofBoundary  = "applied commit reported as commit_unknown_result at the client boundary; fault-injected by a test-only C-boundary interceptor, not server- or transport-generated"
	childTimeout   = 2 * time.Minute
	proofTimeout   = 4 * time.Minute
	storeMetaValue = otel.StoreLayoutNone
)

// TestNativeRustAgainstOwnedFDB is the live native proof: the production otel-rust
// executable and the test-only applied-1021 driver write real Go-created OTEL tenants
// through the pinned libfdb_c 7.3.77, and Go audits every row and physical key.
func TestNativeRustAgainstOwnedFDB(t *testing.T) {
	t.Parallel()
	h := newNativeHarness(t)
	vectors, wf, descriptors, err := fixture()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(wire)
	h.digest = hex.EncodeToString(digest[:])
	var executed []string
	t.Run("production_cli", func(t *testing.T) {
		var population []vector
		for _, v := range vectors {
			if strings.HasPrefix(v.Name, "v2_") {
				population = append(population, v)
			}
		}
		for _, c := range wf.Cases {
			population = append(population, c.Outputs...)
		}
		if len(population) != 20 {
			t.Fatalf("CLI population is %d frames, want 8 v2 + 12 wire outputs", len(population))
		}
		h.productionCLI(t, population)
		executed = append(executed, "production_cli")
	})
	t.Run(proofScenario, func(t *testing.T) {
		c := wf.Cases[0]
		if c.Name != "object_unknowns_at_every_node" || len(c.Outputs) != 4 {
			t.Fatalf("applied-1021 input is %s with %d spans", c.Name, len(c.Outputs))
		}
		h.appliedCommitUnknown(t, c)
		executed = append(executed, proofScenario)
	})
	if want := []string{"production_cli", proofScenario}; !slices.Equal(executed, want) {
		t.Fatalf("executed native scenarios %v, want %v", executed, want)
	}
	t.Logf("executed native scenarios: %v", executed)
}

type nativeHarness struct {
	ctx                context.Context
	cluster            string
	client, cli, proof string
	digest             string
	raw                fdb.Database
	db                 *recordlayer.FDBDatabase
}

func newNativeHarness(t *testing.T) *nativeHarness {
	setup, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	container, err := foundationdb.Run(setup, "", foundationdb.WithVersion("7.3.77"), foundationdb.WithAPIVersion(730), foundationdb.WithDirectIP())
	if err != nil {
		if foundationdb.SkipWithoutDocker(err) {
			t.Skip("FDB not available (no Docker)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { foundationdb.TeardownT(t, container) })
	text, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), proofTimeout)
	t.Cleanup(stop)
	h := &nativeHarness{ctx: ctx, cluster: filepath.Join(t.TempDir(), "fdb.cluster")}
	if err := os.WriteFile(h.cluster, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	rf, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		dst  *string
		path string
	}{{&h.client, *libfdbCRunfile}, {&h.cli, *otelRustRunfile}, {&h.proof, *nativeProofRunfile}} {
		if p.path == "" {
			t.Fatal("native proof runfile flags are required")
		}
		if *p.dst, err = rf.Rlocation(p.path); err != nil {
			t.Fatal(err)
		}
		if *p.dst, err = filepath.Abs(*p.dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := fdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}
	cf, err := client.ParseClusterFile(h.cluster)
	if err != nil {
		t.Fatal(err)
	}
	if h.raw, err = fdb.OpenDatabaseFromConfig(h.ctx, cf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.raw.Close)
	h.db = recordlayer.NewFDBDatabase(h.raw)
	return h
}

// tenant creates (run, tenant 0) through the production Exporter, so its header,
// STORE_META("layout") row and STORE_META index entry are the real baseline.
func (h *nativeHarness) tenant(t *testing.T, run string) *otelsql.TenantStore {
	t.Helper()
	cfg := otel.DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant, cfg.RecordCompression = h.cluster, run, 0, "none"
	exp, err := otel.NewExporter(h.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.Shutdown(h.ctx); err != nil {
		t.Fatal(err)
	}
	// Mirrors relTenantPaths in layers/otel/tenant.go: /OTEL/R_<RUN>, schema T<tenant>.
	ts, err := otelsql.OpenTenantStoreCompiled(h.ctx, h.db, "/OTEL/R_"+strings.ToUpper(run), "T0", nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func keyRange(ss subspace.Subspace) fdb.KeyRange {
	b, e := ss.FDBRangeKeys()
	return fdb.KeyRange{Begin: b.FDBKey(), End: e.FDBKey()}
}

func (h *nativeHarness) read(t *testing.T, r fdb.KeyRange) []fdb.KeyValue {
	t.Helper()
	// Another process wrote last: never reuse a cached read version.
	h.raw.InvalidateGRVCache()
	out, err := h.db.RunRead(h.ctx, func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatal(err)
	}
	var kvs []fdb.KeyValue
	for _, kv := range out.([]fdb.KeyValue) {
		kvs = append(kvs, fdb.KeyValue{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value)})
	}
	return kvs
}

// physical is the tenant's complete key/value map keyed relative to its subspace.
func (h *nativeHarness) physical(t *testing.T, ts *otelsql.TenantStore) map[string][]byte {
	t.Helper()
	prefix := ts.Subspace.Bytes()
	out := map[string][]byte{}
	for _, kv := range h.read(t, keyRange(ts.Subspace)) {
		out[string(kv.Key[len(prefix):])] = kv.Value
	}
	return out
}

// outside is every user key except the target tenant's: catalog, other tenants, sentinels.
func (h *nativeHarness) outside(t *testing.T, ts *otelsql.TenantStore) []fdb.KeyValue {
	t.Helper()
	r := keyRange(ts.Subspace)
	var out []fdb.KeyValue
	for _, kv := range h.read(t, fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}) {
		if bytes.Compare(kv.Key, r.Begin.FDBKey()) < 0 || bytes.Compare(kv.Key, r.End.FDBKey()) >= 0 {
			out = append(out, kv)
		}
	}
	return out
}

func sameKVs(a, b []fdb.KeyValue) bool {
	return slices.EqualFunc(a, b, func(x, y fdb.KeyValue) bool {
		return bytes.Equal(x.Key, y.Key) && bytes.Equal(x.Value, y.Value)
	})
}

func mapDiff(got, want map[string][]byte) string {
	var missing, extra, changed []string
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			missing = append(missing, hex.EncodeToString([]byte(k)))
		case !bytes.Equal(g, v):
			changed = append(changed, hex.EncodeToString([]byte(k)))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, hex.EncodeToString([]byte(k)))
		}
	}
	if len(missing)+len(extra)+len(changed) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(changed)
	return fmt.Sprintf("%d keys, want %d: missing %v extra %v changed %v", len(got), len(want), missing, extra, changed)
}

// baseline requires the fresh production tenant to be exactly its header plus the
// STORE_META("layout") row and STORE_META_NAME entry, built here from Go metadata.
func (h *nativeHarness) baseline(t *testing.T, ts *otelsql.TenantStore) map[string][]byte {
	t.Helper()
	md := ts.MetaData
	rt := md.GetRecordType("SPANS")
	if rt == nil || md.GetUnionFieldForRecordType(rt).Number() != 1 {
		t.Fatal("SPANS must be RecordTypeUnion field 1")
	}
	for _, name := range spanIndexes {
		if idx := md.GetIndex(name); idx == nil || idx.Type != recordlayer.IndexTypeValue || idx.IsUnique() {
			t.Fatalf("%s is not an ordinary VALUE index", name)
		}
	}
	metaType := md.GetRecordType("STORE_META")
	metaIndexes := md.GetIndexesForRecordType("STORE_META")
	if metaType == nil || len(metaIndexes) != 1 || metaIndexes[0].Name != "STORE_META_NAME" ||
		metaIndexes[0].Type != recordlayer.IndexTypeValue || metaIndexes[0].IsUnique() || metaIndexes[0].HasPredicate() {
		t.Fatal("STORE_META must have exactly one index, the ordinary VALUE index STORE_META_NAME")
	}
	nameIndex := metaIndexes[0]
	layout := storeMetaValue
	want, err := otelsql.BuildStoreMetaCompiled(otelsql.StoreMeta{Name: otelsql.StoreMetaLayout, Str: &layout})
	if err != nil {
		t.Fatal(err)
	}
	row, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordlayer.FDBStoredRecord[proto.Message]{Record: want, RecordType: metaType}
	pks, err := metaType.PrimaryKey.Evaluate(rec, want)
	if err != nil || len(pks) != 1 {
		t.Fatalf("STORE_META primary key: %v, %v", pks, err)
	}
	pk := asTuple(pks[0])
	entries, err := nameIndex.RootExpression.Evaluate(rec, want)
	if err != nil || len(entries) != 1 {
		t.Fatalf("STORE_META_NAME entries: %v, %v", entries, err)
	}
	rest, err := nameIndex.TrimPrimaryKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	prefix := len(ts.Subspace.Bytes())
	rel := func(k []byte) string { return string(k[prefix:]) }
	headerKey := rel(ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.StoreInfoKey)}))
	base := h.physical(t, ts)
	header := base[headerKey]
	if len(header) == 0 {
		t.Fatal("store header is absent")
	}
	// Only the header bytes are taken as observed; the row and its entry are built here.
	union := protowire.Number(md.GetUnionFieldForRecordType(metaType).Number())
	recordKey := rel(ts.Subspace.Sub(recordlayer.RecordKey).Pack(append(slices.Clone(pk), int64(0))))
	indexKey := rel(ts.Subspace.Sub(recordlayer.IndexKey, nameIndex.SubspaceTupleKey()).Pack(append(asTuple(entries[0]), rest...)))
	exact := map[string][]byte{
		headerKey: header,
		recordKey: protowire.AppendBytes(protowire.AppendTag(nil, union, protowire.BytesType), row),
		indexKey:  {},
	}
	if len(exact) != 3 {
		t.Fatal("baseline header, record and index keys collide")
	}
	if diff := mapDiff(base, exact); diff != "" {
		t.Fatalf("baseline is not exactly header + STORE_META(layout) + STORE_META_NAME: %s", diff)
	}
	var loaded []*recordlayer.FDBStoredRecord[proto.Message]
	_, err = h.db.Run(h.ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		loaded = nil
		cursor := store.ScanRecords(nil, recordlayer.ForwardScan())
		defer func() { _ = cursor.Close() }()
		for {
			result, err := cursor.OnNext(h.ctx)
			if err != nil {
				return nil, err
			}
			if !result.HasNext() {
				return nil, nil
			}
			loaded = append(loaded, result.GetValue())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].RecordType.Name != "STORE_META" || !bytes.Equal(loaded[0].PrimaryKey.Pack(), pk.Pack()) {
		t.Fatalf("baseline must hold exactly one STORE_META(layout) record, got %d", len(loaded))
	}
	got, ok := loaded[0].Record.(*otelv1pb.STORE_META)
	if !ok || !proto.Equal(got, want) {
		t.Fatalf("baseline STORE_META row %v, want %v", loaded[0].Record, want)
	}
	if wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(got); err != nil || !bytes.Equal(wire, row) {
		t.Fatalf("baseline STORE_META bytes differ: %v", err)
	}
	if diff := mapDiff(h.physical(t, ts), base); diff != "" {
		t.Fatalf("baseline audit changed the store: %s", diff)
	}
	return base
}

type expectedValue struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

// layoutManifest is the Rust writer's Layout (writer.rs), derived from the real store.
type layoutManifest struct {
	Version          int               `json:"version"`
	DescriptorSHA256 string            `json:"descriptor_sha256"`
	RecordPrefix     string            `json:"record_prefix"`
	IndexPrefixes    map[string]string `json:"index_prefixes"`
	Checks           []expectedValue   `json:"checks"`
	EmptyRanges      [][2]string       `json:"empty_ranges"`
	Messages         string            `json:"messages"`
}

// layout seals every baseline key and the index-state subspaces, which must be empty.
func (h *nativeHarness) layout(t *testing.T, ts *otelsql.TenantStore, base map[string][]byte) layoutManifest {
	t.Helper()
	m := layoutManifest{Version: 1, DescriptorSHA256: h.digest, Messages: "none", IndexPrefixes: map[string]string{}}
	m.RecordPrefix = hex.EncodeToString(ts.Subspace.Sub(recordlayer.RecordKey).Bytes())
	for _, name := range spanIndexes {
		m.IndexPrefixes[name] = hex.EncodeToString(ts.Subspace.Sub(recordlayer.IndexKey, ts.MetaData.GetIndex(name).SubspaceTupleKey()).Bytes())
	}
	for _, k := range slices.Sorted(maps.Keys(base)) {
		value := hex.EncodeToString(base[k])
		m.Checks = append(m.Checks, expectedValue{Key: hex.EncodeToString(append(bytes.Clone(ts.Subspace.Bytes()), k...)), Value: &value})
	}
	for k := recordlayer.IndexSecondarySpaceKey; k <= recordlayer.IndexSlidingWindowSpaceKey; k++ {
		r := keyRange(ts.Subspace.Sub(k))
		if kvs := h.read(t, r); len(kvs) != 0 {
			t.Fatalf("index-state subspace %d is not empty", k)
		}
		m.EmptyRanges = append(m.EmptyRanges, [2]string{hex.EncodeToString(r.Begin.FDBKey()), hex.EncodeToString(r.End.FDBKey())})
	}
	return m
}

// expected is base plus each vector's unsplit record (union field 1) and VALUE index keys,
// all from the Go fixture's metadata evaluation, not from the Rust writer.
func expected(t *testing.T, ts *otelsql.TenantStore, base map[string][]byte, want []vector) map[string][]byte {
	t.Helper()
	prefix := len(ts.Subspace.Bytes())
	out := map[string][]byte{}
	for k, v := range base {
		out[k] = v
	}
	records := string(ts.Subspace.Sub(recordlayer.RecordKey).Bytes()[prefix:])
	for _, v := range want {
		pk, err := hex.DecodeString(v.PK)
		if err != nil {
			t.Fatal(err)
		}
		value := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), v.Row)
		if len(value) > 100_000 {
			t.Fatalf("%s would split; this census expects unsplit records", v.Name)
		}
		key := records + string(pk) + string(tuple.Tuple{int64(0)}.Pack())
		if _, dup := out[key]; dup {
			t.Fatalf("%s repeats a source identity", v.Name)
		}
		out[key] = value
		for name, entries := range v.Keys {
			index := string(ts.Subspace.Sub(recordlayer.IndexKey, ts.MetaData.GetIndex(name).SubspaceTupleKey()).Bytes()[prefix:])
			for _, entry := range entries {
				suffix, err := hex.DecodeString(entry)
				if err != nil {
					t.Fatal(err)
				}
				out[index+string(suffix)] = []byte{}
			}
		}
	}
	return out
}

func stripBase(m, base map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range m {
		if b, ok := base[k]; !ok || !bytes.Equal(b, v) {
			out[k] = v
		}
	}
	return out
}

// reference saves want through the Go Record Layer serializer into a second fresh tenant.
func (h *nativeHarness) reference(t *testing.T, run string, want []vector) map[string][]byte {
	t.Helper()
	ref := h.tenant(t, run)
	base := h.baseline(t, ref)
	_, err := h.db.Run(h.ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ref.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		for _, v := range want {
			row := &otelv1pb.SPANS{}
			if err := proto.Unmarshal(v.Row, row); err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(row); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := h.physical(t, ref)
	if diff := mapDiff(got, expected(t, ref, base, want)); diff != "" {
		t.Fatalf("Go SaveRecord reference differs from the fixture expectation: %s", diff)
	}
	return stripBase(got, base)
}

// audit requires the exact physical map, the same non-baseline keys as the Go reference,
// and every row read back through the normal Go serializer, which must write nothing.
func (h *nativeHarness) audit(t *testing.T, ts *otelsql.TenantStore, base, ref map[string][]byte, want []vector) map[string][]byte {
	t.Helper()
	got := h.physical(t, ts)
	if diff := mapDiff(got, expected(t, ts, base, want)); diff != "" {
		t.Fatalf("native physical map: %s", diff)
	}
	if diff := mapDiff(stripBase(got, base), ref); diff != "" {
		t.Fatalf("native writes differ from the Go SaveRecord reference: %s", diff)
	}
	_, err := h.db.Run(h.ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := ts.Builder(rc).Open()
		if err != nil {
			return nil, err
		}
		for _, v := range want {
			pkBytes, err := hex.DecodeString(v.PK)
			if err != nil {
				return nil, err
			}
			pk, err := tuple.Unpack(pkBytes)
			if err != nil {
				return nil, err
			}
			rec, err := store.LoadRecord(pk)
			if err != nil {
				return nil, err
			}
			if rec == nil {
				return nil, fmt.Errorf("%s is missing", v.Name)
			}
			row, ok := rec.Record.(*otelv1pb.SPANS)
			wantRow := &otelv1pb.SPANS{}
			if err := proto.Unmarshal(v.Row, wantRow); err != nil {
				return nil, err
			}
			if !ok || rec.RecordType.Name != "SPANS" || !proto.Equal(row, wantRow) {
				return nil, fmt.Errorf("%s compiled row differs", v.Name)
			}
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(row)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(wire, v.Row) || !bytes.Equal(row.GetOTLP(), v.OTLP) {
				return nil, fmt.Errorf("%s row or OTLP bytes differ", v.Name)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := mapDiff(h.physical(t, ts), got); diff != "" {
		t.Fatalf("Go read-back changed the store: %s", diff)
	}
	return got
}

// sentinels writes unrelated keys immediately outside both ends of the tenant range.
func (h *nativeHarness) sentinels(t *testing.T, ts *otelsql.TenantStore) {
	t.Helper()
	r := keyRange(ts.Subspace)
	_, err := h.db.Run(h.ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		rc.Transaction().Set(fdb.Key(ts.Subspace.Bytes()), []byte("sentinel-before-tenant"))
		rc.Transaction().Set(r.End, []byte("sentinel-after-tenant"))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type child struct {
	stdout, stderr []byte
	err            error
}

// run executes a child with exactly env (never the test's environment) under a deadline.
func (h *nativeHarness) run(binary string, args, env []string) child {
	ctx, cancel := context.WithTimeout(h.ctx, childTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append([]string{}, env...)
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return child{stdout: stdout.Bytes(), stderr: stderr.Bytes(), err: err}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cliJob is the sealed flat job main.rs reads (deny_unknown_fields).
type cliJob struct {
	ClientLibrary     string `json:"client_library"`
	ClusterFile       string `json:"cluster_file"`
	LayoutFile        string `json:"layout_file"`
	CorpusFile        string `json:"corpus_file"`
	CorpusSHA256      string `json:"corpus_sha256"`
	BinarySHA256      string `json:"binary_sha256"`
	SourceRevision    string `json:"source_revision"`
	ExpectedFrames    int    `json:"expected_frames"`
	RowBudget         int    `json:"row_budget"`
	BatchSize         int    `json:"batch_size"`
	Workers           int    `json:"workers"`
	RecordCompression string `json:"record_compression"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	Output            string `json:"output"`
}

type cliReceipt struct {
	Frames           int     `json:"frames"`
	CommittedSpans   int     `json:"committed_spans"`
	CommittedBatches int     `json:"committed_batches"`
	Retries          int     `json:"retries"`
	Error            *string `json:"error"`
	Samples          []struct {
		ReceivedSpans  int     `json:"received_spans"`
		CommittedSpans int     `json:"committed_spans"`
		Error          *string `json:"error"`
	} `json:"samples"`
}

// productionCLI runs the real otel-rust executable: a refused inherited network option
// first, then 2 workers x 4-frame calls, then an identical replay.
func (h *nativeHarness) productionCLI(t *testing.T, want []vector) {
	target := h.tenant(t, "rustcli_target")
	base := h.baseline(t, target)
	ref := h.reference(t, "rustcli_reference", want)
	h.sentinels(t, target)
	dir := t.TempDir()
	layoutPath, corpusPath := filepath.Join(dir, "layout.json"), filepath.Join(dir, "corpus")
	writeJSON(t, layoutPath, h.layout(t, target, base))
	var corpus []byte
	for _, v := range want {
		corpus = binary.BigEndian.AppendUint32(corpus, uint32(len(v.OTLP)))
		corpus = append(corpus, v.OTLP...)
	}
	if err := os.WriteFile(corpusPath, corpus, 0o600); err != nil {
		t.Fatal(err)
	}
	job := func(name string) (string, string) {
		output := filepath.Join(dir, name+".receipt.json")
		path := filepath.Join(dir, name+".job.json")
		writeJSON(t, path, cliJob{
			ClientLibrary: h.client, ClusterFile: h.cluster, LayoutFile: layoutPath, CorpusFile: corpusPath,
			CorpusSHA256: fileSHA256(t, corpusPath), BinarySHA256: fileSHA256(t, h.cli), SourceRevision: strings.Repeat("0", 40),
			ExpectedFrames: len(want), RowBudget: len(want), BatchSize: 4, Workers: 2, RecordCompression: "none",
			TimeoutSeconds: 90, Output: output,
		})
		return path, output
	}
	outside := h.outside(t, target)

	// The inherited option is refused before libfdb_c loads: no store or receipt bytes.
	path, output := job("refused")
	refused := h.run(h.cli, []string{path}, []string{"FDB_NETWORK_OPTION_TRACE_ENABLE=" + t.TempDir()})
	if refused.err == nil || strings.TrimSpace(string(refused.stderr)) != envRefused {
		t.Fatalf("inherited network option: err=%v stderr=%q", refused.err, refused.stderr)
	}
	if info, err := os.Stat(output); err != nil || info.Size() != 0 {
		t.Fatalf("refused run must leave its reserved receipt empty: %v", err)
	}
	if diff := mapDiff(h.physical(t, target), base); diff != "" {
		t.Fatalf("refused run changed the tenant: %s", diff)
	}
	if !sameKVs(h.outside(t, target), outside) {
		t.Fatal("refused run changed keys outside the tenant")
	}

	var written map[string][]byte
	for _, name := range []string{"first", "replay"} {
		path, output := job(name)
		res := h.run(h.cli, []string{path}, []string{})
		if res.err != nil {
			t.Fatalf("%s run: %v stderr=%q", name, res.err, res.stderr)
		}
		var r cliReceipt
		b, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if r.Error != nil || r.Frames != 20 || r.CommittedSpans != 20 || r.CommittedBatches != 5 || len(r.Samples) != 5 {
			t.Fatalf("%s receipt: %s", name, b)
		}
		for _, s := range r.Samples {
			if s.ReceivedSpans != 4 || s.CommittedSpans != 4 || s.Error != nil {
				t.Fatalf("%s receipt sample: %s", name, b)
			}
		}
		t.Logf("%s run: 20 frames, 20 spans, 5 batches, %d retries", name, r.Retries)
		got := h.audit(t, target, base, ref, want)
		if written != nil {
			if diff := mapDiff(got, written); diff != "" {
				t.Fatalf("replay changed the map: %s", diff)
			}
		}
		written = got
		if !sameKVs(h.outside(t, target), outside) {
			t.Fatalf("%s run changed keys outside the tenant", name)
		}
	}
}

type proofReport struct {
	Scenario            string     `json:"scenario"`
	Boundary            string     `json:"boundary"`
	Error               *string    `json:"error"`
	TransactionsCreated int        `json:"transactions_created"`
	SameHandle          bool       `json:"same_transaction_handle"`
	OptionCalls         [][2]int64 `json:"option_calls"`
	CommitCalls         int        `json:"commit_calls"`
	RealCommitSuccesses int        `json:"real_commit_successes"`
	Exposed             int        `json:"exposed_commit_unknown_result"`
	OnErrorCodes        []int      `json:"on_error_codes"`
	CommittedSpans      int        `json:"committed_spans"`
	CommittedBatches    int        `json:"committed_batches"`
	Retries             int        `json:"retries"`
	LogicalOTLPBytes    int        `json:"logical_otlp_bytes"`
}

// appliedCommitUnknown runs the driver: one Writer call whose single batch really
// commits, is reported as 1021 once, goes through real OnError(1021) and replays.
func (h *nativeHarness) appliedCommitUnknown(t *testing.T, c wireCase) {
	target := h.tenant(t, "rust1021_target")
	base := h.baseline(t, target)
	ref := h.reference(t, "rust1021_reference", c.Outputs)
	h.sentinels(t, target)
	dir := t.TempDir()
	paths := map[string]string{
		"LAYOUT": filepath.Join(dir, "layout.json"),
		"INPUT":  filepath.Join(dir, "input.pb"),
		"REPORT": filepath.Join(dir, "report.json"),
	}
	writeJSON(t, paths["LAYOUT"], h.layout(t, target, base))
	if err := os.WriteFile(paths["INPUT"], c.Input, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := h.outside(t, target)
	env := []string{"OTEL_NATIVE_PROOF_CLIENT=" + h.client, "OTEL_NATIVE_PROOF_CLUSTER=" + h.cluster}
	for name, path := range paths {
		env = append(env, "OTEL_NATIVE_PROOF_"+name+"="+path)
	}
	sort.Strings(env)
	res := h.run(h.proof, []string{"--exact", proofScenario, "--nocapture", "--test-threads=1"}, env)
	if res.err != nil || !strings.Contains(string(res.stdout), "test result: ok. 1 passed") {
		t.Fatalf("driver: %v stdout=%q stderr=%q", res.err, res.stdout, res.stderr)
	}
	b, err := os.ReadFile(paths["REPORT"])
	if err != nil {
		t.Fatal(err)
	}
	var r proofReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	otlpBytes := 0
	for _, v := range c.Outputs {
		otlpBytes += len(v.OTLP)
	}
	if r.Scenario != proofScenario || r.Boundary != proofBoundary || r.Error != nil ||
		r.TransactionsCreated != 1 || !r.SameHandle || !slices.Equal(r.OptionCalls, [][2]int64{{500, 10000}, {501, -1}}) ||
		r.CommitCalls != 2 || r.RealCommitSuccesses != 2 || r.Exposed != 1 || !slices.Equal(r.OnErrorCodes, []int{1021}) ||
		r.CommittedSpans != len(c.Outputs) || r.CommittedBatches != 1 || r.Retries != 1 || r.LogicalOTLPBytes != otlpBytes {
		t.Fatalf("applied-1021 report: %s", b)
	}
	t.Logf("%s: %s", proofScenario, b)
	h.audit(t, target, base, ref, c.Outputs)
	if !sameKVs(h.outside(t, target), outside) {
		t.Fatal("driver changed keys outside the tenant")
	}
}

func TestNativeCensusMapDiff(t *testing.T) {
	t.Parallel()
	want := map[string][]byte{"a": {1}, "b": {}}
	for _, c := range []struct {
		name string
		got  map[string][]byte
		diff string
	}{
		{"equal", map[string][]byte{"a": {1}, "b": {}}, ""},
		{"missing", map[string][]byte{"a": {1}}, "1 keys, want 2: missing [62] extra [] changed []"},
		{"extra", map[string][]byte{"a": {1}, "b": {}, "c": {}}, "3 keys, want 2: missing [] extra [63] changed []"},
		{"changed", map[string][]byte{"a": {2}, "b": {}}, "2 keys, want 2: missing [] extra [] changed [61]"},
	} {
		if got := mapDiff(c.got, want); got != c.diff {
			t.Errorf("%s: mapDiff = %q, want %q", c.name, got, c.diff)
		}
	}
}

func TestNativeCensusStripBase(t *testing.T) {
	t.Parallel()
	base := map[string][]byte{"header": {1}, "meta": {2}}
	got := stripBase(map[string][]byte{"header": {1}, "meta": {3}, "row": {4}}, base)
	// An unchanged baseline key goes; a changed one and a new one stay visible.
	if want := map[string][]byte{"meta": {3}, "row": {4}}; !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("stripBase = %v, want %v", got, want)
	}
	if got := stripBase(map[string][]byte{"header": {1}, "meta": {2}}, base); len(got) != 0 {
		t.Errorf("an unchanged baseline must strip to empty, got %v", got)
	}
	whole := map[string][]byte{"header": {1}, "row": {4}}
	if got := stripBase(whole, map[string][]byte{}); !maps.EqualFunc(got, whole, bytes.Equal) {
		t.Errorf("an empty baseline must strip nothing, got %v", got)
	}
}
