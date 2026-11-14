package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// maxUnionBytes is the paired Rust writer's decoded-union bound (storage.rs): a source
// row it cannot store is refused here rather than verified for one arm only.
const maxUnionBytes = 16 << 20

// physicalBasis states what the census's physical counts are conditioned on.
const physicalBasis = "span identities and every index entry are derived from the source rows and metadata; each identity's stored value must be a legal representation of its exact source union (the bare union under none; a clear fallback or one complete, checksummed, byte-saving zlib stream under the zlib modes, identically for every arm), and the expected physical span KVs are the canonical chunk counts of those validated stored lengths; a legal clear fallback or zlib stream cannot prove compression effort or the exact zlib level"

// counts is one side of the census: expected from the source, or actual in the store.
type counts struct {
	HeaderKVs                int64            `json:"header_kvs"`
	SpanRecordIdentities     int64            `json:"span_record_identities"`
	PhysicalSpanRecordKVs    int64            `json:"physical_span_record_kvs"`
	BaselineRecordIdentities int64            `json:"baseline_record_identities"`
	BaselineRecordKVs        int64            `json:"baseline_record_kvs"`
	IndexEntries             map[string]int64 `json:"index_entries"`
	BaselineIndexEntries     map[string]int64 `json:"baseline_index_entries"`
	UnexpectedKVs            int64            `json:"unexpected_kvs"`
}

// newCounts names all eight SPANS indexes and STORE_META_NAME, so a zero is reported.
func newCounts() counts {
	c := counts{IndexEntries: map[string]int64{}, BaselineIndexEntries: map[string]int64{"STORE_META_NAME": 0}}
	for _, name := range spanIndexes {
		c.IndexEntries[name] = 0
	}
	return c
}

// transformCounts are the source identities whose stored value fully validated, by
// representation; unresolved ones did not.
type transformCounts struct {
	BareUnion     int64 `json:"bare_union"`
	ClearFallback int64 `json:"clear_fallback"`
	ZlibV1        int64 `json:"zlib_v1"`
	Unresolved    int64 `json:"unresolved"`
}

type transform int

const (
	bareUnion transform = iota
	clearFallback
	zlibV1
)

var errRepresentation = errors.New("not a legal representation of the source union")

// validateStored checks a record's complete stored value against its source union for
// mode, the same way for every arm. Under none only the bare union is legal. Under
// either zlib mode the clear fallback (prefix 2, union) and the compressed form (prefix
// 4, version 1, BE32 union length, one zlib stream strictly shorter than the union
// less 5) are both legal, whichever a compressor would choose. The stream is decoded
// to at most one byte past the union, must end with a verified checksum and must be
// the whole remaining value; no length is taken from the stored header before it
// matches the source.
func validateStored(stored []byte, mode string, unionLen int, unionHash [32]byte) (transform, error) {
	if _, zlibMode := compressionLevel(mode); !zlibMode {
		if len(stored) == unionLen && sha256.Sum256(stored) == unionHash {
			return bareUnion, nil
		}
		return 0, errRepresentation
	}
	if len(stored) == 0 {
		return 0, errRepresentation
	}
	switch stored[0] {
	case 2:
		if len(stored) == unionLen+1 && sha256.Sum256(stored[1:]) == unionHash {
			return clearFallback, nil
		}
	case 4:
		if len(stored) < 6 || stored[1] != 1 || uint64(binary.BigEndian.Uint32(stored[2:6])) != uint64(unionLen) {
			return 0, errRepresentation
		}
		if streamLen := len(stored) - 6; unionLen <= 5 || streamLen >= unionLen-5 {
			return 0, errRepresentation
		}
		in := bytes.NewReader(stored[6:])
		// bytes.Reader is an io.ByteReader, so zlib reads no byte past its stream; a
		// preset dictionary fails here (ErrDictionary).
		zr, err := zlib.NewReader(in)
		if err != nil {
			return 0, errRepresentation
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(zr, int64(unionLen)+1))
		// With one byte of limit left, a nil error means zlib reached its end and
		// verified the checksum; a truncated or unfinished stream is an error.
		if err != nil || n != int64(unionLen) || zr.Close() != nil || in.Len() != 0 {
			return 0, errRepresentation
		}
		if [32]byte(h.Sum(nil)) == unionHash {
			return zlibV1, nil
		}
	}
	return 0, errRepresentation
}

// maxStored is the most stored bytes a legal representation of a union can have.
func maxStored(unionLen int, mode string) int {
	if _, zlibMode := compressionLevel(mode); zlibMode {
		return unionLen + 1
	}
	return unionLen
}

// sourceRow is one corpus frame's independently built SPANS row and its keys.
type sourceRow struct {
	pk        tuple.Tuple
	row       *otelv1pb.SPANS
	rowWire   []byte
	canonical []byte
	indexKeys [][]string
}

// spanExpect is a source identity's expected union and, once its stored value has
// been seen and validated, that representation.
type spanExpect struct {
	unionHash [32]byte
	unionLen  int
	observed  bool
	validated bool
	storedLen int
	transform transform
}

// expectation is the complete expected tenant: the sealed baseline, every source
// identity's union and every SPANS index entry, all derived from Go metadata.
type expectation struct {
	ts        *otelsql.TenantStore
	base      *baseline
	mode      string
	spans     map[string]*spanExpect
	indexKeys map[string]int
	counts    counts
}

func newExpectation(ts *otelsql.TenantStore, base *baseline, mode string) *expectation {
	e := &expectation{ts: ts, base: base, mode: mode, spans: map[string]*spanExpect{}, indexKeys: map[string]int{}, counts: newCounts()}
	e.counts.HeaderKVs, e.counts.BaselineRecordIdentities, e.counts.BaselineRecordKVs = 1, 1, int64(base.records)
	e.counts.BaselineIndexEntries["STORE_META_NAME"] = 1
	return e
}

// newJobExpectation is a job's expectation: its compression mode, never its arm, decides
// which representations are legal.
func newJobExpectation(j *job, ts *otelsql.TenantStore, base *baseline) *expectation {
	return newExpectation(ts, base, j.RecordCompression)
}

// buildRow builds a frame's expected row through the Go projection, its primary key
// through the metadata, and each SPANS index's distinct entries.
func buildRow(ts *otelsql.TenantStore, wire []byte) (*sourceRow, error) {
	rs, err := decodeFrame(wire)
	if err != nil {
		return nil, err
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(rs)
	if err != nil {
		return nil, err
	}
	ss := rs.ScopeSpans[0]
	row, err := otelsql.BuildCompiled(otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: ss.Spans[0], OTLP: canonical})
	if err != nil {
		return nil, err
	}
	rowWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(row)
	if err != nil {
		return nil, err
	}
	rt := ts.MetaData.GetRecordType("SPANS")
	rec := &recordlayer.FDBStoredRecord[proto.Message]{Record: row, RecordType: rt}
	pks, err := rt.PrimaryKey.Evaluate(rec, row)
	if err != nil || len(pks) != 1 {
		return nil, fault("verify", "the SPANS primary key is not exactly one tuple")
	}
	out := &sourceRow{pk: asTuple(pks[0]), row: row, rowWire: rowWire, canonical: canonical, indexKeys: make([][]string, len(spanIndexes))}
	for i, name := range spanIndexes {
		idx := ts.MetaData.GetIndex(name)
		entries, err := idx.RootExpression.Evaluate(rec, row)
		if err != nil {
			return nil, err
		}
		rest, err := idx.TrimPrimaryKey(out.pk)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			key := string(indexSubspace(ts, idx).Pack(append(asTuple(entry), rest...)))
			if !seen[key] {
				seen[key] = true
				out.indexKeys[i] = append(out.indexKeys[i], key)
			}
		}
	}
	return out, nil
}

// spanUnion is the record-type union message holding a SPANS row, as stores serialize it.
func spanUnion(ts *otelsql.TenantStore, rowWire []byte) []byte {
	field := protowire.Number(ts.MetaData.GetUnionFieldForRecordType(ts.MetaData.GetRecordType("SPANS")).Number())
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), rowWire)
}

// add records a source row's expected union and index entries; a repeated source
// identity is a corpus error.
func (e *expectation) add(r *sourceRow) error {
	id := string(r.pk.Pack())
	if e.spans[id] != nil || id == string(e.base.layoutPK.Pack()) {
		return fault("verify", "the corpus repeats a source identity")
	}
	union := spanUnion(e.ts, r.rowWire)
	if len(union) > maxUnionBytes {
		return fault("verify", "a source row exceeds the paired Rust writer's 16 MiB union bound")
	}
	e.spans[id] = &spanExpect{unionHash: sha256.Sum256(union), unionLen: len(union)}
	e.counts.SpanRecordIdentities++
	for i, keys := range r.indexKeys {
		for _, k := range keys {
			if _, dup := e.indexKeys[k]; dup {
				return fault("verify", "two source rows share an index entry")
			}
			e.indexKeys[k] = i
		}
		e.counts.IndexEntries[spanIndexes[i]] += int64(len(keys))
	}
	return nil
}

type chunk struct {
	key    []byte
	suffix int64
	value  []byte
}

// recordGroup is the contiguous record KVs of one primary key. Chunks are retained
// only for a known identity and only within its legal stored-size bound.
type recordGroup struct {
	pk      []byte
	base    bool
	exp     *spanExpect
	chunks  []chunk
	n       int64
	size    int
	invalid bool
}

// census classifies every KV of the tenant: the sealed baseline first, then source
// records and SPANS index entries; anything else is unexpected.
type census struct {
	exp          *expectation
	actual       counts
	problems     []string
	nproblems    int
	header       []byte
	recordPrefix []byte
	basePK       []byte
	indexNames   map[string]string
	group        *recordGroup
}

func newCensus(exp *expectation) *census {
	c := &census{
		exp: exp, actual: newCounts(), problems: []string{}, header: headerKey(exp.ts), recordPrefix: recordSubspace(exp.ts).Bytes(),
		basePK: exp.base.layoutPK.Pack(), indexNames: map[string]string{},
	}
	for _, name := range slices.Concat(spanIndexes, []string{"STORE_META_NAME"}) {
		c.indexNames[string(tuple.Tuple{exp.ts.MetaData.GetIndex(name).SubspaceTupleKey()}.Pack())] = name
	}
	return c
}

func (c *census) problem(format string, args ...any) {
	c.nproblems++
	if len(c.problems) < 16 {
		c.problems = append(c.problems, fmt.Sprintf(format, args...))
	}
}

func (c *census) unexpected(n int64, reason string) {
	c.actual.UnexpectedKVs += n
	c.problem("%s", reason)
}

func (c *census) add(kv fdb.KeyValue) {
	t, err := c.exp.ts.Subspace.Unpack(kv.Key)
	if err != nil || len(t) == 0 {
		c.flush()
		c.unexpected(1, "a tenant key is not a tuple")
		return
	}
	kind, _ := t[0].(int64)
	if kind != recordlayer.RecordKey {
		c.flush()
	}
	switch kind {
	case recordlayer.StoreInfoKey:
		if len(t) != 1 || !bytes.Equal(kv.Key, c.header) {
			c.unexpected(1, "an unexpected store-info key")
			return
		}
		c.actual.HeaderKVs++
		if !bytes.Equal(kv.Value, c.exp.base.header) {
			c.problem("the store header changed")
		}
	case recordlayer.RecordKey:
		c.addRecord(kv, t)
	case recordlayer.IndexKey:
		c.addIndex(kv, t)
	default:
		c.unexpected(1, "a key in a count, state, version, build or secondary subspace")
	}
}

func (c *census) addRecord(kv fdb.KeyValue, t tuple.Tuple) {
	if len(t) < 2 || !bytes.HasPrefix(kv.Key, c.recordPrefix) {
		c.flush()
		c.unexpected(1, "a malformed record key")
		return
	}
	// The identity is everything before the last element, so a malformed suffix under a
	// known identity is still that identity's KV: counted, and failing it. The key is
	// canonical only if it is exactly that identity and a nonnegative integer suffix.
	pk := tuple.Tuple(t[1 : len(t)-1]).Pack()
	suffix, ok := t[len(t)-1].(int64)
	canonical := ok && suffix >= 0 && len(t) >= 3 &&
		bytes.Equal(kv.Key, slices.Concat(c.recordPrefix, pk, tuple.Tuple{suffix}.Pack()))
	if c.group == nil || !bytes.Equal(pk, c.group.pk) {
		c.flush()
		c.group = &recordGroup{pk: bytes.Clone(pk), base: bytes.Equal(pk, c.basePK), exp: c.exp.spans[string(pk)]}
	}
	g := c.group
	g.n++
	limit := 0
	switch {
	case g.base:
		limit = len(c.exp.base.layoutStored)
	case g.exp != nil:
		limit = maxStored(g.exp.unionLen, c.exp.mode)
	default:
		// No identity outside the corpus and baseline is retained.
		c.actual.UnexpectedKVs++
		if g.n == 1 {
			c.problem("a record outside the corpus and the baseline")
		}
		return
	}
	// The group is bounded by its legal size before anything is retained.
	if !canonical || g.invalid || len(kv.Value) == 0 || len(kv.Value) > splitSize || len(kv.Value) > limit-g.size {
		g.invalid, g.chunks = true, nil
		return
	}
	g.size += len(kv.Value)
	g.chunks = append(g.chunks, chunk{key: bytes.Clone(kv.Key), suffix: suffix, value: bytes.Clone(kv.Value)})
}

// joinChunks returns a record's stored value if its chunks are canonical: one chunk with
// suffix 0 of at most a split, or suffixes 1..n of full chunks and a nonempty last one,
// more than a split in total.
func joinChunks(chunks []chunk) ([]byte, bool) {
	if len(chunks) == 1 {
		v := chunks[0].value
		return v, chunks[0].suffix == 0 && len(v) > 0 && len(v) <= splitSize
	}
	var out []byte
	for i, ch := range chunks {
		last := i == len(chunks)-1
		if ch.suffix != int64(i+1) || (!last && len(ch.value) != splitSize) || (last && (len(ch.value) == 0 || len(ch.value) > splitSize)) {
			return nil, false
		}
		out = append(out, ch.value...)
	}
	return out, len(chunks) > 1
}

// canonicalKeys are the exact record keys a stored value of n bytes splits into.
func canonicalKeys(recordPrefix, pk []byte, n int) [][]byte {
	key := func(suffix int64) []byte {
		return slices.Concat(recordPrefix, pk, tuple.Tuple{suffix}.Pack())
	}
	if chunkCount(n) == 1 {
		return [][]byte{key(0)}
	}
	out := make([][]byte, 0, chunkCount(n))
	for i := range chunkCount(n) {
		out = append(out, key(int64(i+1)))
	}
	return out
}

// validGroup requires a group's chunks to be canonical and keyed exactly as its stored
// length splits, returning the stored value.
func validGroup(g *recordGroup, recordPrefix []byte) ([]byte, bool) {
	if g.invalid {
		return nil, false
	}
	value, ok := joinChunks(g.chunks)
	if !ok {
		return nil, false
	}
	keys := canonicalKeys(recordPrefix, g.pk, len(value))
	if len(keys) != len(g.chunks) {
		return nil, false
	}
	for i, ch := range g.chunks {
		if !bytes.Equal(ch.key, keys[i]) {
			return nil, false
		}
	}
	return value, true
}

func (c *census) flush() {
	g := c.group
	if g == nil {
		return
	}
	c.group = nil
	switch {
	case g.base:
		c.actual.BaselineRecordIdentities++
		c.actual.BaselineRecordKVs += g.n
		value, ok := validGroup(g, c.recordPrefix)
		if !ok || !bytes.Equal(value, c.exp.base.layoutStored) {
			c.problem("the STORE_META(layout) record differs from the sealed baseline")
		}
	case g.exp != nil:
		e := g.exp
		c.actual.SpanRecordIdentities++
		c.actual.PhysicalSpanRecordKVs += g.n
		if e.observed {
			c.problem("a source identity is stored twice")
			return
		}
		e.observed = true
		value, ok := validGroup(g, c.recordPrefix)
		if !ok {
			c.problem("a span record's chunks are missing, extra, noncanonical or over its legal size")
			return
		}
		tr, err := validateStored(value, c.exp.mode, e.unionLen, e.unionHash)
		if err != nil {
			c.problem("a span record is not a legal %s representation of its source union", c.exp.mode)
			return
		}
		e.validated, e.storedLen, e.transform = true, len(value), tr
	}
}

func (c *census) addIndex(kv fdb.KeyValue, t tuple.Tuple) {
	if len(t) < 2 {
		c.unexpected(1, "a malformed index key")
		return
	}
	name := c.indexNames[string(tuple.Tuple{t[1]}.Pack())]
	switch {
	case name == "STORE_META_NAME":
		if string(kv.Key) != c.exp.base.indexKey || len(kv.Value) != 0 {
			c.unexpected(1, "a STORE_META_NAME entry other than the baseline's")
			return
		}
		c.actual.BaselineIndexEntries[name]++
	case name != "":
		i, ok := c.exp.indexKeys[string(kv.Key)]
		if !ok || spanIndexes[i] != name || len(kv.Value) != 0 {
			c.unexpected(1, "a SPANS index entry outside the expected set, or with a value")
			return
		}
		c.actual.IndexEntries[name]++
	default:
		c.unexpected(1, "an entry of an index other than the SPANS and STORE_META_NAME indexes")
	}
}

// finish closes the census. The expected physical span KVs are the canonical chunk
// counts of the validated representations; it passes only if every source identity
// validated, nothing was extra, substituted or malformed, and the counts agree.
func (c *census) finish() bool {
	c.flush()
	var unresolved, physical int64
	for _, e := range c.exp.spans {
		if !e.validated {
			unresolved++
			continue
		}
		physical += int64(chunkCount(e.storedLen))
	}
	c.exp.counts.PhysicalSpanRecordKVs = physical
	if unresolved > 0 {
		c.problem("%d source identities are missing or did not validate", unresolved)
	}
	if !reflect.DeepEqual(c.actual, c.exp.counts) {
		c.problem("the actual census differs from the expected")
	}
	return c.nproblems == 0
}

func (c *census) transforms() transformCounts {
	var t transformCounts
	for _, e := range c.exp.spans {
		switch {
		case !e.validated:
			t.Unresolved++
		case e.transform == bareUnion:
			t.BareUnion++
		case e.transform == clearFallback:
			t.ClearFallback++
		default:
			t.ZlibV1++
		}
	}
	return t
}

// pageThrough visits r in key order, limit KVs per fetch, until a short page; a page out
// of order, outside r or larger than limit fails rather than silently skipping.
func pageThrough(ctx context.Context, fetch func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error), r fdb.KeyRange, limit int, visit func(fdb.KeyValue)) error {
	begin, end := bytes.Clone(r.Begin.FDBKey()), r.End.FDBKey()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := fetch(ctx, fdb.KeyRange{Begin: fdb.Key(begin), End: end}, limit)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			return fault("census", "a page exceeded its limit")
		}
		var last []byte
		for _, kv := range rows {
			if bytes.Compare(kv.Key, begin) < 0 || bytes.Compare(kv.Key, end) >= 0 || (last != nil && bytes.Compare(kv.Key, last) <= 0) {
				return fault("census", "a page is out of order or outside the tenant")
			}
			visit(kv)
			last = kv.Key
		}
		if len(rows) < limit {
			return nil
		}
		begin = append(bytes.Clone(last), 0)
	}
}

// fetchRange is one page in its own read transaction, copied out of it; it opens no store.
func fetchRange(db *recordlayer.FDBDatabase) func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error) {
	return func(ctx context.Context, r fdb.KeyRange, limit int) ([]fdb.KeyValue, error) {
		out, err := db.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) {
			kvs, err := tx.GetRange(r, fdb.RangeOptions{Limit: limit}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			copied := make([]fdb.KeyValue, len(kvs))
			for i, kv := range kvs {
				copied[i] = fdb.KeyValue{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value)}
			}
			return copied, nil
		})
		if err != nil {
			return nil, err
		}
		return out.([]fdb.KeyValue), nil
	}
}

func (c *census) result() censusResult {
	return censusResult{
		Expected: c.exp.counts, Actual: c.actual, Matched: c.nproblems == 0 && reflect.DeepEqual(c.actual, c.exp.counts),
		Problems: c.problems, ProblemCount: c.nproblems, ValidatedSpanTransforms: c.transforms(), PhysicalBasis: physicalBasis,
	}
}

type censusResult struct {
	Expected                counts          `json:"expected"`
	Actual                  counts          `json:"actual"`
	Matched                 bool            `json:"matched"`
	Problems                []string        `json:"problems"`
	ProblemCount            int             `json:"problem_count"`
	ValidatedSpanTransforms transformCounts `json:"validated_span_transforms"`
	PhysicalBasis           string          `json:"physical_basis"`
}
