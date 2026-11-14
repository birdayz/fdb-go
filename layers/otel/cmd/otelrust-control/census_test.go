package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// tenantFixture is a tenant of the real OTEL_V1 metadata under a test subspace: its
// baseline, its source rows and each row's stored value, Go's by default.
type tenantFixture struct {
	ts *otelsql.TenantStore
	// template is the unbound stored template the store's metadata is bound from.
	template *recordlayer.RecordMetaData
	base     *baseline
	mode     string
	rows     []*sourceRow
	stored   [][]byte
}

func newTenantFixture(t *testing.T, mode string, frames [][]byte) *tenantFixture {
	t.Helper()
	md, err := expectedMetadata()
	if err != nil {
		t.Fatal(err)
	}
	template, err := expectedTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if err := exactTemplate(template); err != nil {
		t.Fatalf("the raw OTEL_V1 template: %v", err)
	}
	ts := &otelsql.TenantStore{DatabasePath: "/OTEL/R_CENSUS_TEST", Schema: tenantSchema, MetaData: md, Subspace: subspace.Sub("census", int64(7))}
	header, err := proto.Marshal(&gen.DataStoreInfo{FormatVersion: proto.Int32(14), MetaDataversion: proto.Int32(int32(md.Version()))})
	if err != nil {
		t.Fatal(err)
	}
	base, err := deriveBaseline(ts, header, mode)
	if err != nil {
		t.Fatal(err)
	}
	tf := &tenantFixture{ts: ts, template: template, base: base, mode: mode}
	for _, wire := range frames {
		row, err := buildRow(ts, wire)
		if err != nil {
			t.Fatal(err)
		}
		stored, _, err := goStored(tf.union(row), mode)
		if err != nil {
			t.Fatal(err)
		}
		tf.rows, tf.stored = append(tf.rows, row), append(tf.stored, stored)
	}
	return tf
}

func (tf *tenantFixture) union(row *sourceRow) []byte { return spanUnion(tf.ts, row.rowWire) }

// recordKeys are row i's physical record keys for its current stored value.
func (tf *tenantFixture) recordKeys(i int) []string {
	var out []string
	for _, k := range splitKeys(recordSubspace(tf.ts), tf.rows[i].pk, len(tf.stored[i])) {
		out = append(out, string(k))
	}
	return out
}

// kvs is the whole tenant a correct writer leaves for the fixture's stored values.
func (tf *tenantFixture) kvs() map[string][]byte {
	out := maps.Clone(tf.base.kvs)
	for i, row := range tf.rows {
		for j, v := range splitValue(tf.stored[i]) {
			out[tf.recordKeys(i)[j]] = v
		}
		for _, keys := range row.indexKeys {
			for _, k := range keys {
				out[k] = []byte{}
			}
		}
	}
	return out
}

func (tf *tenantFixture) census(t *testing.T, kvs map[string][]byte) *census {
	t.Helper()
	exp := newExpectation(tf.ts, tf.base, tf.mode)
	for _, row := range tf.rows {
		if err := exp.add(row); err != nil {
			t.Fatal(err)
		}
	}
	c := newCensus(exp)
	for _, k := range slices.Sorted(maps.Keys(kvs)) {
		c.add(fdb.KeyValue{Key: fdb.Key(k), Value: kvs[k]})
	}
	c.finish()
	return c
}

// frameWith is fixture frame 0 under a new span ID with extra attributes.
func frameWith(t *testing.T, spanID byte, attrs ...*commonpb.KeyValue) []byte {
	t.Helper()
	_, frames := testCorpus(t, 1)
	rs, err := decodeFrame(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	sp := rs.ScopeSpans[0].Spans[0]
	sp.SpanId = []byte{spanID, 9, 9, 9, 9, 9, 9, 9}
	sp.Attributes = append(sp.Attributes, attrs...)
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func bytesAttr(key string, value []byte) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: value}}}
}

// noise is deterministic incompressible bytes.
func noise(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.Uint32())
	}
	return out
}

// deflate is one complete zlib stream of data; each flush adds an empty sync block,
// a valid stream that is longer than the compressor alone would write.
func deflate(t *testing.T, data []byte, level, flushes int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	for range flushes {
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zlibForm is the compressed representation of a union with the given stream.
func zlibForm(unionLen int, stream []byte) []byte {
	out := []byte{4, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[2:6], uint32(unionLen))
	return append(out, stream...)
}

func TestValidateStoredAcceptsOnlyCompleteSourceRepresentations(t *testing.T) {
	t.Parallel()
	union := spanUnionOf(t)
	u, h := len(union), sha256.Sum256(union)
	good := deflate(t, union, zlib.BestSpeed, 0)
	other := deflate(t, union, zlib.BestCompression, 3)
	if bytes.Equal(good, other) {
		t.Fatal("the two valid streams must differ")
	}
	changed := slices.Clone(union)
	changed[len(changed)/2] ^= 0x55
	// The same row bytes under a non-minimal length varint: another envelope.
	envelope := append([]byte{union[0]}, 0x80|union[1], 0x80|union[2], 0x00)
	envelope = append(envelope, union[3:]...)
	var dict bytes.Buffer
	dw, err := zlib.NewWriterLevelDict(&dict, zlib.BestSpeed, []byte("preset dictionary"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dw.Write(union); err != nil {
		t.Fatal(err)
	}
	if err := dw.Close(); err != nil {
		t.Fatal(err)
	}
	// A sync-flushed stream without its final block and checksum: the legacy form the
	// production serializer still reads.
	var legacy bytes.Buffer
	lw, _ := zlib.NewWriterLevel(&legacy, zlib.BestSpeed)
	if _, err := lw.Write(union); err != nil {
		t.Fatal(err)
	}
	if err := lw.Flush(); err != nil {
		t.Fatal(err)
	}
	bomb := slices.Concat(union, bytes.Repeat([]byte{'x'}, 4096))
	header := func(n uint32) []byte {
		v := zlibForm(u, good)
		binary.BigEndian.PutUint32(v[2:6], n)
		return v
	}
	for _, c := range []struct {
		name   string
		mode   string
		stored []byte
		want   transform
		ok     bool
	}{
		{"bare union under none", "none", union, bareUnion, true},
		{"clear fallback under none", "none", append([]byte{2}, union...), 0, false},
		{"compressed under none", "none", zlibForm(u, good), 0, false},
		{"clear fallback", "zlib-1", append([]byte{2}, union...), clearFallback, true},
		{"Go's stream", "zlib-1", zlibForm(u, good), zlibV1, true},
		{"another valid stream", "zlib-default", zlibForm(u, other), zlibV1, true},
		{"bare union under zlib", "zlib-1", union, 0, false},
		{"non-minimal prefix", "zlib-1", append([]byte{0x82, 0}, union...), 0, false},
		{"encrypted prefix", "zlib-1", append([]byte{8}, union...), 0, false},
		{"empty", "zlib-1", nil, 0, false},
		{"header only", "zlib-1", zlibForm(u, nil), 0, false},
		{"garbage body", "zlib-1", zlibForm(u, []byte("not a zlib stream at all")), 0, false},
		{"unknown version", "zlib-1", append([]byte{4, 2}, zlibForm(u, good)[2:]...), 0, false},
		{"truncated header", "zlib-1", []byte{4, 1, 0, 0}, 0, false},
		{"declared U-1", "zlib-1", header(uint32(u - 1)), 0, false},
		{"declared U+1", "zlib-1", header(uint32(u + 1)), 0, false},
		{"declared huge", "zlib-1", header(0xffffffff), 0, false},
		{"declared negative as int32", "zlib-1", header(0x80000000), 0, false},
		{"truncated body", "zlib-1", zlibForm(u, good[:len(good)/2]), 0, false},
		{"no checksum", "zlib-1", zlibForm(u, good[:len(good)-4]), 0, false},
		{"one checksum byte", "zlib-1", zlibForm(u, good[:len(good)-3]), 0, false},
		{"three checksum bytes", "zlib-1", zlibForm(u, good[:len(good)-1]), 0, false},
		{"bad checksum", "zlib-1", zlibForm(u, append(slices.Clone(good[:len(good)-1]), good[len(good)-1]^1)), 0, false},
		{"legacy unfinished stream", "zlib-1", zlibForm(u, legacy.Bytes()), 0, false},
		{"trailing byte", "zlib-1", zlibForm(u, append(slices.Clone(good), 0)), 0, false},
		{"second stream", "zlib-1", zlibForm(u, slices.Concat(good, deflate(t, nil, zlib.BestSpeed, 0))), 0, false},
		{"preset dictionary", "zlib-1", zlibForm(u, dict.Bytes()), 0, false},
		{"different same-length union", "zlib-1", zlibForm(u, deflate(t, changed, zlib.BestSpeed, 0)), 0, false},
		{"different clear union", "zlib-1", append([]byte{2}, changed...), 0, false},
		{"other union envelope", "zlib-1", zlibForm(u, deflate(t, envelope, zlib.BestSpeed, 0)), 0, false},
		{"output past the union", "zlib-1", zlibForm(u, deflate(t, bomb, zlib.BestSpeed, 0)), 0, false},
		{"output short of the union", "zlib-1", zlibForm(u, deflate(t, union[:u-1], zlib.BestSpeed, 0)), 0, false},
	} {
		got, err := validateStored(c.stored, c.mode, u, h)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("%s: transform %v, error %v", c.name, got, err)
		}
	}
	if len(envelope) == len(union) || bytes.Equal(envelope, union) {
		t.Fatal("the alternative envelope must be other bytes")
	}
}

// spanUnionOf is a real Go-projected SPANS union of a fixture frame.
func spanUnionOf(t *testing.T) []byte {
	t.Helper()
	tf := newTenantFixture(t, "zlib-1", [][]byte{frameWith(t, 1, strAttr("padding", strings.Repeat("compressible ", 200)))})
	return tf.union(tf.rows[0])
}

// The compressed form must save more than its header: a stream of exactly U-5 bytes
// does not qualify, one of U-6 does, and a union of at most 5 bytes is never compressed.
func TestValidateStoredSavingThreshold(t *testing.T) {
	t.Parallel()
	head := noise(200, 7)
	found := map[int]bool{}
	for n := 0; n < 400 && len(found) < 2; n++ {
		data := slices.Concat(head, bytes.Repeat([]byte{'x'}, n))
		stream := deflate(t, data, zlib.BestCompression, 0)
		diff := len(data) - len(stream)
		if diff != 5 && diff != 6 {
			continue
		}
		_, err := validateStored(zlibForm(len(data), stream), "zlib-1", len(data), sha256.Sum256(data))
		if (err == nil) != (diff == 6) {
			t.Fatalf("stream %d bytes for a %d-byte union: %v", len(stream), len(data), err)
		}
		found[diff] = true
	}
	if !found[5] || !found[6] {
		t.Fatalf("the threshold search found %v within its budget", found)
	}
	tiny := []byte{0x0a, 1, 2}
	if tr, err := validateStored(append([]byte{2}, tiny...), "zlib-1", 3, sha256.Sum256(tiny)); err != nil || tr != clearFallback {
		t.Fatal("a tiny union's clear fallback was refused")
	}
	if _, err := validateStored(zlibForm(3, deflate(t, tiny, zlib.BestSpeed, 0)), "zlib-1", 3, sha256.Sum256(tiny)); err == nil {
		t.Fatal("a tiny union was accepted compressed")
	}
}

// Literal physical boundaries: unsplit at exactly a split, two chunks one byte past it,
// two full chunks at twice it.
func TestCanonicalSplitBoundaries(t *testing.T) {
	t.Parallel()
	prefix, pk := []byte("records/"), tuple.Tuple{"trace", "span"}.Pack()
	group := func(value []byte, suffixes []int64, sizes []int) *recordGroup {
		g := &recordGroup{pk: pk}
		off := 0
		for i, s := range suffixes {
			g.chunks = append(g.chunks, chunk{key: slices.Concat(prefix, pk, tuple.Tuple{s}.Pack()), suffix: s, value: value[off : off+sizes[i]]})
			off += sizes[i]
		}
		return g
	}
	for _, c := range []struct {
		n        int
		suffixes []int64
		sizes    []int
	}{
		{100_000, []int64{0}, []int{100_000}},
		{100_001, []int64{1, 2}, []int{100_000, 1}},
		{200_000, []int64{1, 2}, []int{100_000, 100_000}},
	} {
		value := noise(c.n, uint64(c.n))
		keys := canonicalKeys(prefix, pk, c.n)
		if len(keys) != len(c.suffixes) || chunkCount(c.n) != len(c.suffixes) {
			t.Fatalf("%d bytes: %d keys", c.n, len(keys))
		}
		for i, s := range c.suffixes {
			if !bytes.Equal(keys[i], slices.Concat(prefix, pk, tuple.Tuple{s}.Pack())) {
				t.Fatalf("%d bytes: key %d", c.n, i)
			}
		}
		if got, ok := validGroup(group(value, c.suffixes, c.sizes), prefix); !ok || !bytes.Equal(got, value) {
			t.Fatalf("%d bytes: the canonical layout was refused", c.n)
		}
	}
	for name, g := range map[string]*recordGroup{
		"100001 unsplit":          group(noise(100_001, 1), []int64{0}, []int{100_001}),
		"100000 split":            group(noise(100_000, 2), []int64{1}, []int{100_000}),
		"200000 as three":         group(noise(200_000, 3), []int64{1, 2, 3}, []int{100_000, 99_999, 1}),
		"short nonfinal chunk":    group(noise(100_001, 4), []int64{1, 2}, []int{99_999, 2}),
		"gap":                     group(noise(100_001, 5), []int64{1, 3}, []int{100_000, 1}),
		"suffix 0 with a split":   group(noise(100_001, 6), []int64{0, 1, 2}, []int{1, 100_000, 0}),
		"empty final chunk":       group(noise(100_000, 7), []int64{1, 2}, []int{100_000, 0}),
		"single suffix 1 unsplit": group(noise(10, 8), []int64{1}, []int{10}),
	} {
		if _, ok := validGroup(g, prefix); ok {
			t.Errorf("%s was accepted", name)
		}
	}
	// A key spelled other than the canonical tuple fails even with canonical values.
	g := group(noise(10, 9), []int64{0}, []int{10})
	g.chunks[0].key = append(slices.Clone(g.chunks[0].key), 0)
	if _, ok := validGroup(g, prefix); ok {
		t.Error("a noncanonical key was accepted")
	}
}

func otherIndex(t *testing.T, md *recordlayer.RecordMetaData) *recordlayer.Index {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(md.GetAllIndexes())) {
		if !slices.Contains(spanIndexes, name) && name != "STORE_META_NAME" {
			return md.GetIndex(name)
		}
	}
	t.Fatal("OTEL_V1 has no index besides SPANS and STORE_META_NAME")
	return nil
}

func TestCensusClassifiesEveryKey(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 4)
	tf := newTenantFixture(t, "none", append(frames, frameWith(t, 1, strAttr("big", strings.Repeat("x", 150_000)))))
	kvs := tf.kvs()
	c := tf.census(t, kvs)
	if !c.result().Matched {
		t.Fatalf("a correct tenant failed the census: %v", c.problems)
	}
	smallKeys, bigKeys := tf.recordKeys(0), tf.recordKeys(4)
	if len(smallKeys) != 1 || len(bigKeys) < 2 {
		t.Fatalf("record chunks %d and %d, want 1 and a split", len(smallKeys), len(bigKeys))
	}
	got := c.result()
	if got.Actual.SpanRecordIdentities != 5 || got.Expected.PhysicalSpanRecordKVs != int64(4+len(bigKeys)) || got.Actual.PhysicalSpanRecordKVs != int64(4+len(bigKeys)) ||
		got.Actual.BaselineRecordIdentities != 1 || got.Actual.HeaderKVs != 1 || got.Actual.BaselineIndexEntries["STORE_META_NAME"] != 1 ||
		got.ValidatedSpanTransforms != (transformCounts{BareUnion: 5}) {
		t.Fatalf("census %+v transforms %+v", got.Actual, got.ValidatedSpanTransforms)
	}
	zero := false
	for _, name := range spanIndexes {
		n, ok := got.Expected.IndexEntries[name]
		if !ok {
			t.Fatalf("%s is not reported", name)
		}
		zero = zero || n == 0
	}
	if !zero || len(got.Actual.IndexEntries) != len(spanIndexes) {
		t.Fatalf("expected-zero indexes must be reported as zero: %v", got.Expected.IndexEntries)
	}
	// A split record straddling census pages validates as one group.
	paged := newCensus(newExpectation(tf.ts, tf.base, tf.mode))
	for _, row := range tf.rows {
		if err := paged.exp.add(row); err != nil {
			t.Fatal(err)
		}
	}
	sorted := slices.Sorted(maps.Keys(kvs))
	all := make([]fdb.KeyValue, len(sorted))
	for i, k := range sorted {
		all[i] = fdb.KeyValue{Key: fdb.Key(k), Value: kvs[k]}
	}
	if err := pageThrough(t.Context(), servePages(all), tenantRange(tf), 1, paged.add); err != nil || !paged.finish() {
		t.Fatalf("a one-KV-per-page census failed: %v %v", err, paged.problems)
	}

	md := tf.ts.MetaData
	small, big := tf.rows[0], tf.rows[4]
	anIndexKey := small.indexKeys[0][0]
	spanID := md.GetIndex("SPANS_SPAN_ID")
	fabricated := string(indexSubspace(tf.ts, spanID).Pack(tuple.Tuple{"0000000000000000", "fabricated"}))
	// Same index as anIndexKey: a substitution keeps every per-index count.
	substitute := string(indexSubspace(tf.ts, md.GetIndex(spanIndexes[0])).Pack(tuple.Tuple{int64(1), "fabricated"}))
	layoutRecord := string(recordSubspace(tf.ts).Pack(append(slices.Clone(tf.base.layoutPK), int64(0))))
	otherMeta := slices.Clone(tf.base.layoutPK)
	otherMeta[len(otherMeta)-1] = "other"
	_, outsideFrames := testCorpus(t, 6)
	outsideRow, err := buildRow(tf.ts, outsideFrames[5])
	if err != nil {
		t.Fatal(err)
	}
	outsideKey := string(recordSubspace(tf.ts).Pack(append(slices.Clone(outsideRow.pk), int64(0))))
	for name, mutate := range map[string]func(map[string][]byte){
		"missing span record":    func(m map[string][]byte) { delete(m, smallKeys[0]) },
		"missing index entry":    func(m map[string][]byte) { delete(m, anIndexKey) },
		"extra index entry":      func(m map[string][]byte) { m[fabricated] = []byte{} },
		"substituted index":      func(m map[string][]byte) { delete(m, anIndexKey); m[substitute] = []byte{} },
		"index entry with value": func(m map[string][]byte) { m[anIndexKey] = []byte("x") },
		"header changed":         func(m map[string][]byte) { m[string(headerKey(tf.ts))] = append(slices.Clone(tf.base.header), 0) },
		"baseline row missing":   func(m map[string][]byte) { delete(m, layoutRecord) },
		"baseline row changed":   func(m map[string][]byte) { m[layoutRecord] = append(slices.Clone(m[layoutRecord]), 0) },
		"baseline entry missing": func(m map[string][]byte) { delete(m, tf.base.indexKey) },
		"another STORE_META row": func(m map[string][]byte) {
			m[string(recordSubspace(tf.ts).Pack(append(otherMeta, int64(0))))] = m[layoutRecord]
		},
		"another STORE_META_NAME entry": func(m map[string][]byte) {
			m[string(indexSubspace(tf.ts, md.GetIndex("STORE_META_NAME")).Pack(tuple.Tuple{"other", "x"}))] = []byte{}
		},
		"a LOGS-like row": func(m map[string][]byte) {
			m[string(recordSubspace(tf.ts).Pack(tuple.Tuple{int64(5), "log", int64(0)}))] = []byte{0x0a, 0}
		},
		"another table's index": func(m map[string][]byte) {
			m[string(indexSubspace(tf.ts, otherIndex(t, md)).Pack(tuple.Tuple{"x"}))] = []byte{}
		},
		"record count key": func(m map[string][]byte) {
			m[string(tf.ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.RecordCountKey)}))] = []byte{1}
		},
		"index state": func(m map[string][]byte) {
			m[string(tf.ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.IndexStateSpaceKey), "SPANS_TIME"}))] = []byte{1}
		},
		"record version": func(m map[string][]byte) {
			m[string(tf.ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.RecordVersionKey), "x"}))] = []byte{1}
		},
		"build space": func(m map[string][]byte) {
			m[string(tf.ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.IndexBuildSpaceKey), "x"}))] = []byte{1}
		},
		"not a tuple":           func(m map[string][]byte) { m[string(append(slices.Clone(tf.ts.Subspace.Bytes()), 0x7f))] = []byte{} },
		"split first part gone": func(m map[string][]byte) { delete(m, bigKeys[0]) },
		"split last part gone":  func(m map[string][]byte) { delete(m, bigKeys[len(bigKeys)-1]) },
		"extra split suffix": func(m map[string][]byte) {
			m[string(recordSubspace(tf.ts).Pack(append(slices.Clone(big.pk), int64(len(bigKeys)+1))))] = []byte{1}
		},
		"split and unsplit": func(m map[string][]byte) {
			m[string(recordSubspace(tf.ts).Pack(append(slices.Clone(big.pk), int64(0))))] = []byte{1}
		},
		"short split chunk": func(m map[string][]byte) { m[bigKeys[0]] = m[bigKeys[0]][:splitSize-1] },
		"unsplit stored as split": func(m map[string][]byte) {
			v := m[smallKeys[0]]
			delete(m, smallKeys[0])
			m[string(recordSubspace(tf.ts).Pack(append(slices.Clone(small.pk), int64(1))))] = v
		},
		"negative suffix": func(m map[string][]byte) {
			v := m[smallKeys[0]]
			delete(m, smallKeys[0])
			m[string(recordSubspace(tf.ts).Pack(append(slices.Clone(small.pk), int64(-1))))] = v
		},
		"pk with an extra component": func(m map[string][]byte) {
			v := m[smallKeys[0]]
			delete(m, smallKeys[0])
			m[string(recordSubspace(tf.ts).Pack(append(slices.Clone(small.pk), "extra", int64(0))))] = v
		},
		"record bytes changed": func(m map[string][]byte) {
			k := tf.recordKeys(1)[0]
			v := slices.Clone(m[k])
			v[len(v)-1] ^= 1
			m[k] = v
		},
		"oversized record": func(m map[string][]byte) { m[smallKeys[0]] = append(slices.Clone(m[smallKeys[0]]), 0) },
		"identity outside the corpus": func(m map[string][]byte) {
			m[outsideKey] = spanUnion(tf.ts, outsideRow.rowWire)
		},
		// The physical totals agree, but an identity is missing and another is extra.
		"one row swapped for an outsider": func(m map[string][]byte) {
			delete(m, smallKeys[0])
			m[outsideKey] = spanUnion(tf.ts, outsideRow.rowWire)
		},
	} {
		mutated := maps.Clone(kvs)
		mutate(mutated)
		if maps.EqualFunc(mutated, kvs, bytes.Equal) {
			t.Fatalf("%s did not change the tenant", name)
		}
		if c := tf.census(t, mutated); c.result().Matched {
			t.Errorf("%s passed the census", name)
		}
	}
	// A known identity whose stored value fails still counts its actual KVs, stays
	// unresolved, and its validated physical expectation is not invented.
	mutated := maps.Clone(kvs)
	k := tf.recordKeys(1)[0]
	mutated[k] = append([]byte{2}, mutated[k]...)
	r := tf.census(t, mutated).result()
	if r.Matched || r.Actual.PhysicalSpanRecordKVs != got.Actual.PhysicalSpanRecordKVs || r.Actual.SpanRecordIdentities != 5 ||
		r.ValidatedSpanTransforms.Unresolved != 1 || r.Expected.PhysicalSpanRecordKVs != got.Expected.PhysicalSpanRecordKVs-1 {
		t.Fatalf("an invalid representation: %+v %+v", r.Actual, r.ValidatedSpanTransforms)
	}
}

func servePages(all []fdb.KeyValue) func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error) {
	return func(_ context.Context, kr fdb.KeyRange, limit int) ([]fdb.KeyValue, error) {
		var out []fdb.KeyValue
		for _, kv := range all {
			if bytes.Compare(kv.Key, kr.Begin.FDBKey()) >= 0 && bytes.Compare(kv.Key, kr.End.FDBKey()) < 0 && len(out) < limit {
				out = append(out, kv)
			}
		}
		return out, nil
	}
}

func tenantRange(tf *tenantFixture) fdb.KeyRange { return prefixRange(tf.ts.Subspace) }

// boundaryFrame is a real Go-projected row whose union exceeds a split by enough that a
// compressed representation can land on either side of the split.
func boundaryFrame(t *testing.T) []byte {
	t.Helper()
	// Bytes attributes are projected and kept in the OTLP: the noise is stored twice,
	// beyond deflate's 32 KiB window, so it compresses to about 2 x 45000 bytes.
	return frameWith(t, 2, bytesAttr("noise", noise(45_000, 11)), strAttr("padding", strings.Repeat("y", 12_000)))
}

// Two complete streams of one union, compressed below and above a split, are the
// same legal data: each validates with its own chunk count, in both directions.
func TestCensusAcceptsAlternativeStreamsAcrossTheSplit(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 2)
	for _, mode := range []string{"zlib-1", "zlib-default"} {
		tf := newTenantFixture(t, mode, append(frames, boundaryFrame(t)))
		row := 2
		union := tf.union(tf.rows[row])
		u := len(union)
		if u <= splitSize+5 {
			t.Fatalf("the boundary union is %d bytes", u)
		}
		low := zlibForm(u, deflate(t, union, zlib.BestCompression, 0))
		if len(low) > splitSize {
			t.Fatalf("the compact stream is %d bytes, not below the split", len(low))
		}
		// Empty sync blocks lengthen a valid stream past the split while it still saves.
		step := len(zlibForm(u, deflate(t, union, zlib.BestCompression, 2))) - len(zlibForm(u, deflate(t, union, zlib.BestCompression, 1)))
		if step <= 0 {
			t.Fatal("a sync flush did not lengthen the stream")
		}
		flushes := (splitSize-len(low))/step + 2
		high := zlibForm(u, deflate(t, union, zlib.BestCompression, flushes))
		if len(high) <= splitSize || len(high) >= u+1 {
			t.Fatalf("the long stream is %d bytes for a %d-byte union", len(high), u)
		}
		for _, sh := range [][]byte{low, high} {
			if tr, err := validateStored(sh, mode, u, sha256.Sum256(union)); err != nil || tr != zlibV1 {
				t.Fatalf("%s: a %d-byte stream is not legal: %v", mode, len(sh), err)
			}
		}
		for _, c := range []struct {
			name   string
			first  []byte
			second []byte
		}{{"low then high", low, high}, {"high then low", high, low}} {
			tf.stored[row] = c.first
			before := tf.kvs()
			tf.stored[row] = c.second
			after := tf.kvs()
			if maps.EqualFunc(before, after, bytes.Equal) {
				t.Fatalf("%s %s: the representations are the same tenant", mode, c.name)
			}
			want := int64(len(tf.rows) - 1 + chunkCount(len(c.second)))
			for _, arm := range []string{"go_pure", "go_native", "rust_native"} {
				j := &job{Arm: arm, RecordCompression: mode}
				exp := newJobExpectation(j, tf.ts, tf.base)
				for _, r := range tf.rows {
					if err := exp.add(r); err != nil {
						t.Fatal(err)
					}
				}
				c2 := newCensus(exp)
				for _, k := range slices.Sorted(maps.Keys(after)) {
					c2.add(fdb.KeyValue{Key: fdb.Key(k), Value: after[k]})
				}
				c2.finish()
				r := c2.result()
				if !r.Matched || r.Expected.PhysicalSpanRecordKVs != want || r.Actual.PhysicalSpanRecordKVs != want || r.ValidatedSpanTransforms.ZlibV1 != int64(len(tf.rows)) {
					t.Fatalf("%s %s %s: %v %+v %+v", mode, c.name, arm, r.Problems, r.Expected, r.ValidatedSpanTransforms)
				}
			}
		}
		if chunkCount(len(low)) != 1 || chunkCount(len(high)) != 2 {
			t.Fatalf("chunk counts %d and %d, want 1 and 2", chunkCount(len(low)), chunkCount(len(high)))
		}
	}
}

// Either zlib representation is legal whatever Go would have chosen; the disclosure
// counts only fully validated values.
func TestCensusTransformDisclosure(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 4)
	tf := newTenantFixture(t, "zlib-1", frames)
	if c := tf.census(t, tf.kvs()); !c.result().Matched || c.result().ValidatedSpanTransforms != (transformCounts{ZlibV1: 4}) {
		t.Fatalf("Go's own representations: %v %+v", c.problems, c.result().ValidatedSpanTransforms)
	}
	if tf.stored[0][0] != 4 {
		t.Fatal("the premise is a row Go compresses")
	}
	tf.stored[0] = append([]byte{2}, tf.union(tf.rows[0])...)
	tf.stored[1] = zlibForm(len(tf.union(tf.rows[1])), deflate(t, tf.union(tf.rows[1]), zlib.BestCompression, 1))
	r := tf.census(t, tf.kvs()).result()
	if !r.Matched || r.ValidatedSpanTransforms != (transformCounts{ClearFallback: 1, ZlibV1: 3}) {
		t.Fatalf("mixed representations: %v %+v", r.Problems, r.ValidatedSpanTransforms)
	}
	// A third record with a legal prefix and a broken stream stays unresolved.
	tf.stored[2] = zlibForm(len(tf.union(tf.rows[2])), []byte{0x78, 0x9c, 1, 2, 3})
	r = tf.census(t, tf.kvs()).result()
	if r.Matched || r.ValidatedSpanTransforms != (transformCounts{ClearFallback: 1, ZlibV1: 2, Unresolved: 1}) {
		t.Fatalf("a malformed compressed record: %+v", r.ValidatedSpanTransforms)
	}
	// Under none only the bare union is legal, for the same rows.
	none := newTenantFixture(t, "none", frames)
	none.stored[0] = append([]byte{2}, none.union(none.rows[0])...)
	if none.census(t, none.kvs()).result().Matched {
		t.Error("a prefixed record passed under compression none")
	}
}

func TestGoStored(t *testing.T) {
	t.Parallel()
	union := []byte(strings.Repeat("compressible ", 100))
	if got, compressed, _ := goStored(union, "none"); !bytes.Equal(got, union) || compressed {
		t.Fatal("none must store the bare union")
	}
	for _, mode := range []string{"zlib-1", "zlib-default"} {
		got, compressed, err := goStored(union, mode)
		if err != nil || !compressed || got[0] != 4 || got[1] != 1 || int(binary.BigEndian.Uint32(got[2:6])) != len(union) {
			t.Fatalf("%s: % x %v", mode, got[:6], err)
		}
		r, err := zlib.NewReader(bytes.NewReader(got[6:]))
		if err != nil {
			t.Fatal(err)
		}
		back, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(back, union) {
			t.Fatalf("%s does not decode to the union", mode)
		}
	}
	tiny := []byte{0x0a, 1, 2}
	if got, compressed, _ := goStored(tiny, "zlib-1"); compressed || !bytes.Equal(got, append([]byte{2}, tiny...)) {
		t.Fatalf("an incompressible union must be clear-prefixed: % x", got)
	}
}

func TestPageThroughProgresses(t *testing.T) {
	t.Parallel()
	var all []fdb.KeyValue
	for i := range 10 {
		all = append(all, fdb.KeyValue{Key: fdb.Key{byte('a' + i)}, Value: []byte{byte(i)}})
	}
	r := fdb.KeyRange{Begin: fdb.Key("a"), End: fdb.Key("z")}
	serve := servePages(all)
	var seen []string
	if err := pageThrough(t.Context(), serve, r, 3, func(kv fdb.KeyValue) { seen = append(seen, string(kv.Key)) }); err != nil || len(seen) != 10 {
		t.Fatalf("paged %d of 10 keys: %v", len(seen), err)
	}
	for name, fetch := range map[string]func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error){
		"repeats the first page": func(ctx context.Context, _ fdb.KeyRange, limit int) ([]fdb.KeyValue, error) {
			return serve(ctx, r, limit)
		},
		"out of order": func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error) {
			return []fdb.KeyValue{all[2], all[1]}, nil
		},
		"over its limit": func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error) {
			return all[:4], nil
		},
		"outside the range": func(context.Context, fdb.KeyRange, int) ([]fdb.KeyValue, error) {
			return []fdb.KeyValue{{Key: fdb.Key("zz")}}, nil
		},
	} {
		if err := pageThrough(t.Context(), fetch, r, 3, func(fdb.KeyValue) {}); err == nil {
			t.Errorf("a page that %s was accepted", name)
		}
	}
}

// manifest is the layout setup would seal for the fixture's tenant.
func (tf *tenantFixture) manifest(t *testing.T) layoutManifest {
	t.Helper()
	digest, err := descriptorDigest()
	if err != nil {
		t.Fatal(err)
	}
	m := layoutManifest{Version: 1, DescriptorSHA256: digest, Messages: "none", RecordPrefix: hex.EncodeToString(recordSubspace(tf.ts).Bytes()), IndexPrefixes: map[string]string{}}
	for _, name := range spanIndexes {
		m.IndexPrefixes[name] = hex.EncodeToString(indexSubspace(tf.ts, tf.ts.MetaData.GetIndex(name)).Bytes())
	}
	for _, k := range slices.Sorted(maps.Keys(tf.base.kvs)) {
		v := hex.EncodeToString(tf.base.kvs[k])
		m.Checks = append(m.Checks, expectedValue{Key: hex.EncodeToString([]byte(k)), Value: &v})
	}
	absent := hex.EncodeToString([]byte("catalog-absent"))
	m.Checks = append(m.Checks, expectedValue{Key: absent})
	for k := recordlayer.IndexSecondarySpaceKey; k <= recordlayer.IndexSlidingWindowSpaceKey; k++ {
		r := prefixRange(tf.ts.Subspace.Sub(int64(k)))
		m.EmptyRanges = append(m.EmptyRanges, [2]string{hex.EncodeToString(r.Begin.FDBKey()), hex.EncodeToString(r.End.FDBKey())})
	}
	return m
}

func TestManifestMatchesTheTenant(t *testing.T) {
	t.Parallel()
	tf := newTenantFixture(t, "zlib-1", nil)
	m := tf.manifest(t)
	if _, err := validateManifest(tf.ts, tf.template, m, "zlib-1"); err != nil {
		t.Fatalf("the sealed manifest was refused: %v", err)
	}
	b := encode(t, m)
	if back, err := parseManifest(b); err != nil || len(back.Checks) != len(m.Checks) {
		t.Fatalf("manifest round trip: %v", err)
	}
	text := string(b)
	for name, bad := range map[string]string{
		"unknown field":       strings.Replace(text, `{`, `{"extra":1,`, 1),
		"duplicate field":     strings.Replace(text, `{`, `{"version":1,`, 1),
		"missing messages":    strings.Replace(text, `,"messages":"none"`, ``, 1),
		"unknown check key":   strings.Replace(text, `"checks":[{`, `"checks":[{"note":1,`, 1),
		"two values":          text + text,
		"wrong value type":    strings.Replace(text, `"version":1`, `"version":"1"`, 1),
		"null index_prefixes": strings.Replace(text, `"index_prefixes":{`, `"index_prefixes":null,"x":{`, 1),
	} {
		if _, err := parseManifest([]byte(bad)); err == nil {
			t.Errorf("manifest with %s was parsed", name)
		}
	}
	for name, mutate := range map[string]func(*layoutManifest){
		"version":            func(m *layoutManifest) { m.Version = 2 },
		"messages":           func(m *layoutManifest) { m.Messages = "deltas" },
		"descriptor":         func(m *layoutManifest) { m.DescriptorSHA256 = strings.Repeat("0", 64) },
		"record prefix":      func(m *layoutManifest) { m.RecordPrefix = "00" },
		"missing index":      func(m *layoutManifest) { delete(m.IndexPrefixes, "SPANS_TIME") },
		"index prefix":       func(m *layoutManifest) { m.IndexPrefixes["SPANS_TIME"] = "00" },
		"missing state seal": func(m *layoutManifest) { m.EmptyRanges = m.EmptyRanges[1:] },
		"baseline value": func(m *layoutManifest) {
			v := *m.Checks[0].Value + "00"
			m.Checks[0].Value = &v
		},
		"missing baseline key": func(m *layoutManifest) { m.Checks = m.Checks[1:] },
		"extra tenant seal": func(m *layoutManifest) {
			v := ""
			m.Checks = append(m.Checks, expectedValue{Key: hex.EncodeToString(append(slices.Clone(tf.ts.Subspace.Bytes()), 0x15, 9)), Value: &v})
		},
	} {
		bad := tf.manifest(t)
		mutate(&bad)
		if _, err := validateManifest(tf.ts, tf.template, bad, "zlib-1"); err == nil {
			t.Errorf("a manifest with a changed %s was accepted", name)
		}
	}
	// The baseline is derived for the job's compression: another mode's bytes differ.
	if _, err := validateManifest(tf.ts, tf.template, m, "none"); err == nil {
		t.Error("a zlib-1 baseline validated as compression none")
	}
	// The manifest is checked only with an exact stored template: none, or the bound form.
	if _, err := validateManifest(tf.ts, nil, m, "zlib-1"); err == nil {
		t.Error("a manifest validated without a stored template")
	}
	if _, err := validateManifest(tf.ts, tf.ts.MetaData, m, "zlib-1"); err == nil {
		t.Error("a manifest validated against the bound metadata as its template")
	}
}

// The exact comparison is made on the unbound stored template: the compiled binding keeps
// only structural invariants, which alone accept a changed descriptor or index expression.
func TestMetadataIsComparedBeforeTheCompiledBinding(t *testing.T) {
	t.Parallel()
	template, err := expectedTemplate()
	if err != nil {
		t.Fatal(err)
	}
	bound, err := expectedMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if err := exactTemplate(template); err != nil {
		t.Fatalf("the raw OTEL_V1 template: %v", err)
	}
	if err := validateMetadata(bound, template.Version()); err != nil {
		t.Fatalf("its compiled binding: %v", err)
	}
	// Record Layer refuses to serialize a binding, so it is never the compared form.
	if _, err := bound.ToProto(); err == nil {
		t.Fatal("bound metadata serialized")
	}
	if err := exactTemplate(bound); err == nil {
		t.Fatal("bound metadata was accepted as the stored template")
	}
	if err := validateMetadata(bound, template.Version()+1); err == nil {
		t.Fatal("a binding of another template version was accepted")
	}
	original, err := template.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	index := func(p *gen.MetaData, name string) *gen.Index {
		for _, idx := range p.GetIndexes() {
			if idx.GetName() == name {
				return idx
			}
		}
		t.Fatalf("no index %s", name)
		return nil
	}
	spans := func(p *gen.MetaData) *descriptorpb.DescriptorProto {
		for _, m := range p.GetRecords().GetMessageType() {
			if m.GetName() == "SPANS" {
				return m
			}
		}
		t.Fatal("no SPANS message")
		return nil
	}
	hasField := func(p *gen.MetaData, name string) bool {
		return slices.ContainsFunc(spans(p).GetField(), func(f *descriptorpb.FieldDescriptorProto) bool { return f.GetName() == name })
	}
	cases := []struct {
		name   string
		change func(*gen.MetaData)
		// applied checks the mutation is in the built metadata's own proto.
		applied func(got *gen.MetaData, md *recordlayer.RecordMetaData) bool
		// structural reports whether the binding invariants alone still accept it.
		structural bool
	}{
		{
			"an optional descriptor field",
			func(p *gen.MetaData) {
				m := spans(p)
				next := int32(0)
				for _, f := range m.GetField() {
					next = max(next, f.GetNumber())
				}
				m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
					Name: proto.String("extra_field"), Number: proto.Int32(next + 1),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				})
			},
			func(got *gen.MetaData, _ *recordlayer.RecordMetaData) bool {
				return hasField(got, "extra_field") && !hasField(original, "extra_field")
			},
			true,
		},
		{
			"two index expressions swapped",
			func(p *gen.MetaData) {
				a, b := index(p, "SPANS_TIME"), index(p, "SPANS_SPAN_ID")
				a.RootExpression, b.RootExpression = b.RootExpression, a.RootExpression
			},
			func(got *gen.MetaData, _ *recordlayer.RecordMetaData) bool {
				return proto.Equal(index(got, "SPANS_TIME").GetRootExpression(), index(original, "SPANS_SPAN_ID").GetRootExpression()) &&
					!proto.Equal(index(got, "SPANS_TIME").GetRootExpression(), index(original, "SPANS_TIME").GetRootExpression())
			},
			true,
		},
		{
			"record versions stored",
			func(p *gen.MetaData) { p.StoreRecordVersions = proto.Bool(true) },
			func(_ *gen.MetaData, md *recordlayer.RecordMetaData) bool {
				return md.IsStoreRecordVersions() && !original.GetStoreRecordVersions()
			},
			false,
		},
		{
			"long records not split",
			func(p *gen.MetaData) { p.SplitLongRecords = proto.Bool(false) },
			func(_ *gen.MetaData, md *recordlayer.RecordMetaData) bool {
				return !md.IsSplitLongRecords() && original.GetSplitLongRecords()
			},
			false,
		},
	}
	if len(cases) != 4 {
		t.Fatalf("%d metadata mutations", len(cases))
	}
	for _, c := range cases {
		p := proto.Clone(original).(*gen.MetaData)
		c.change(p)
		if proto.Equal(p, original) {
			t.Fatalf("%s: the mutation changed nothing", c.name)
		}
		md, err := recordlayer.RecordMetaDataFromProto(p)
		if err != nil {
			t.Fatalf("%s: the mutated template does not build: %v", c.name, err)
		}
		got, err := md.ToProto()
		if err != nil || !c.applied(got, md) {
			t.Fatalf("%s: the built metadata does not carry the mutation: %v", c.name, err)
		}
		if err := validateMetadata(md, md.Version()); (err == nil) != c.structural {
			t.Fatalf("%s: structural checks alone: %v", c.name, err)
		}
		if err := exactTemplate(md); err == nil {
			t.Errorf("%s: a changed template passed the exact comparison", c.name)
		}
	}
}

// recordKey is a record key of pk with raw suffix bytes, canonical or not.
func recordKey(tf *tenantFixture, pk tuple.Tuple, suffix []byte) string {
	return string(slices.Concat(recordSubspace(tf.ts).Bytes(), pk.Pack(), suffix))
}

// A malformed record key under a known identity is that identity's raw KV: it counts in
// the actual physical and identity totals, fails the identity, and is never unexpected.
func TestCensusCountsMalformedKnownIdentities(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 4)
	tf := newTenantFixture(t, "none", frames)
	kvs := tf.kvs()
	good := tf.census(t, kvs).result()
	if !good.Matched || good.Actual.SpanRecordIdentities != 4 || good.Actual.PhysicalSpanRecordKVs != 4 || good.Actual.BaselineRecordKVs != int64(tf.base.records) {
		t.Fatalf("the control tenant: %+v %v", good.Actual, good.Problems)
	}
	row, canonicalKey := tf.rows[1], tf.recordKeys(1)[0]
	if canonicalKey != recordKey(tf, row.pk, tuple.Tuple{int64(0)}.Pack()) {
		t.Fatal("the premise is an unsplit row keyed by suffix 0")
	}
	// 0x16 0x00 0x01 decodes as int64 1 in two bytes; 0x15 0x00 decodes as uint64 0.
	nonminimal, unsignedZero := []byte{0x16, 0x00, 0x01}, []byte{0x15, 0x00}
	for _, c := range []struct {
		suffix []byte
		want   any
	}{{nonminimal, int64(1)}, {unsignedZero, uint64(0)}} {
		got, err := tf.ts.Subspace.Unpack(fdb.Key(recordKey(tf, row.pk, c.suffix)))
		if err != nil || got[len(got)-1] != c.want {
			t.Fatalf("the premise is a decodable noncanonical suffix %x: %v %v", c.suffix, got, err)
		}
	}
	for _, c := range []struct {
		name     string
		suffixes [][]byte
		keep     bool
		// physical is the identity's raw KVs plus the three intact rows'.
		physical int64
	}{
		{"negative suffix", [][]byte{tuple.Tuple{int64(-1)}.Pack()}, false, 4},
		{"string suffix", [][]byte{tuple.Tuple{"0"}.Pack()}, false, 4},
		{"nonminimal integer suffix", [][]byte{nonminimal}, false, 4},
		{"unsigned zero suffix", [][]byte{unsignedZero}, false, 4},
		{"canonical and negative", [][]byte{tuple.Tuple{int64(-1)}.Pack()}, true, 5},
		{"canonical and nonminimal", [][]byte{nonminimal}, true, 5},
		{"canonical and three malformed", [][]byte{tuple.Tuple{int64(-1)}.Pack(), tuple.Tuple{"0"}.Pack(), unsignedZero}, true, 7},
	} {
		mutated := maps.Clone(kvs)
		value := mutated[canonicalKey]
		if !c.keep {
			delete(mutated, canonicalKey)
		}
		for _, s := range c.suffixes {
			mutated[recordKey(tf, row.pk, s)] = value
		}
		r := tf.census(t, mutated).result()
		if r.Matched || r.Actual.SpanRecordIdentities != 4 || r.Actual.PhysicalSpanRecordKVs != c.physical || r.Actual.UnexpectedKVs != 0 ||
			r.Expected.PhysicalSpanRecordKVs != 3 || r.ValidatedSpanTransforms != (transformCounts{BareUnion: 3, Unresolved: 1}) ||
			!slices.Contains(r.Problems, "1 source identities are missing or did not validate") {
			t.Errorf("%s: actual %+v expected %+v transforms %+v problems %v", c.name, r.Actual, r.Expected, r.ValidatedSpanTransforms, r.Problems)
		}
	}

	// Expected physical KVs sum only validated identities, so a raw count cannot equal
	// it while one is unresolved; arranged equal anyway, the unresolved identity fails.
	mutated := maps.Clone(kvs)
	mutated[recordKey(tf, row.pk, tuple.Tuple{int64(-1)}.Pack())] = mutated[canonicalKey]
	delete(mutated, canonicalKey)
	exp := newExpectation(tf.ts, tf.base, tf.mode)
	for _, src := range tf.rows {
		if err := exp.add(src); err != nil {
			t.Fatal(err)
		}
	}
	c := newCensus(exp)
	for _, k := range slices.Sorted(maps.Keys(mutated)) {
		c.add(fdb.KeyValue{Key: fdb.Key(k), Value: mutated[k]})
	}
	c.flush()
	if c.actual.PhysicalSpanRecordKVs != 4 || c.actual.SpanRecordIdentities != 4 {
		t.Fatalf("raw actual %+v", c.actual)
	}
	c.actual.PhysicalSpanRecordKVs = 3
	if c.finish() || c.result().Matched || !reflect.DeepEqual(c.actual, c.exp.counts) || c.transforms().Unresolved != 1 ||
		!slices.Contains(c.problems, "1 source identities are missing or did not validate") {
		t.Fatalf("equal counts hid an unresolved identity: %+v %+v %v", c.actual, c.exp.counts, c.problems)
	}

	// The sealed baseline's own record under a malformed suffix still counts as the
	// baseline's KV and fails it.
	layoutRecord := recordKey(tf, tf.base.layoutPK, tuple.Tuple{int64(0)}.Pack())
	if _, ok := kvs[layoutRecord]; !ok || tf.base.records != 1 {
		t.Fatal("the premise is an unsplit baseline record keyed by suffix 0")
	}
	mutated = maps.Clone(kvs)
	mutated[recordKey(tf, tf.base.layoutPK, tuple.Tuple{int64(-1)}.Pack())] = mutated[layoutRecord]
	delete(mutated, layoutRecord)
	r := tf.census(t, mutated).result()
	if r.Matched || r.Actual.BaselineRecordIdentities != 1 || r.Actual.BaselineRecordKVs != 1 || r.Actual.UnexpectedKVs != 0 ||
		!slices.Contains(r.Problems, "the STORE_META(layout) record differs from the sealed baseline") {
		t.Fatalf("a malformed baseline key: %+v %v", r.Actual, r.Problems)
	}

	// An unknown identity is not coerced into a known one: its KVs are unexpected.
	_, outsideFrames := testCorpus(t, 6)
	outside, err := buildRow(tf.ts, outsideFrames[5])
	if err != nil {
		t.Fatal(err)
	}
	mutated = maps.Clone(kvs)
	mutated[recordKey(tf, outside.pk, tuple.Tuple{int64(-1)}.Pack())] = []byte{1}
	mutated[recordKey(tf, outside.pk, tuple.Tuple{int64(0)}.Pack())] = []byte{1}
	r = tf.census(t, mutated).result()
	if r.Matched || r.Actual.UnexpectedKVs != 2 || r.Actual.SpanRecordIdentities != 4 || r.Actual.PhysicalSpanRecordKVs != 4 {
		t.Fatalf("an outsider: %+v", r.Actual)
	}
}

// feed adds kv to c and returns the group it landed in, if any.
func feed(c *census, key string, value []byte) *recordGroup {
	c.add(fdb.KeyValue{Key: fdb.Key(key), Value: value})
	return c.group
}

// Values are bounded by the identity's legal stored size before they are retained: a
// chunk that would cross it, an empty chunk or one over a split invalidates the group
// and releases what it held; an unknown identity is unexpected at once and retains
// nothing.
func TestCensusBoundsBeforeRetaining(t *testing.T) {
	t.Parallel()
	_, frames := testCorpus(t, 1)
	big := frameWith(t, 1, strAttr("big", strings.Repeat("x", 150_000)))
	newCensusOf := func(mode string) (*census, *tenantFixture) {
		tf := newTenantFixture(t, mode, [][]byte{frames[0], big})
		exp := newExpectation(tf.ts, tf.base, mode)
		for _, row := range tf.rows {
			if err := exp.add(row); err != nil {
				t.Fatal(err)
			}
		}
		return newCensus(exp), tf
	}
	key := func(tf *tenantFixture, row int, suffix int64) string {
		return recordKey(tf, tf.rows[row].pk, tuple.Tuple{suffix}.Pack())
	}

	// The union of row 0 is U bytes: none stores at most U, either zlib mode U+1.
	c, tf := newCensusOf("none")
	u := len(tf.union(tf.rows[0]))
	if g := feed(c, key(tf, 0, 0), make([]byte, u+1)); g == nil || !g.invalid || g.chunks != nil || g.n != 1 {
		t.Fatalf("none retained U+1 bytes: %+v", g)
	}
	c, tf = newCensusOf("zlib-1")
	g := feed(c, key(tf, 0, 1), make([]byte, u+1))
	if g == nil || g.invalid || len(g.chunks) != 1 || g.size != u+1 {
		t.Fatalf("zlib did not retain U+1 bytes: %+v", g)
	}
	// The next chunk crosses the bound: the group is invalid and holds nothing.
	if g = feed(c, key(tf, 0, 2), []byte{1}); !g.invalid || g.chunks != nil || g.n != 2 {
		t.Fatalf("a crossing chunk was retained: %+v", g)
	}
	if g = feed(c, key(tf, 0, 3), []byte{1}); !g.invalid || g.chunks != nil || g.n != 3 {
		t.Fatalf("a chunk after the crossing one was retained: %+v", g)
	}
	if c.finish() || c.actual.PhysicalSpanRecordKVs != 3 || c.actual.UnexpectedKVs != 0 || c.transforms().Unresolved != 2 {
		t.Fatalf("an over-bound identity: %+v %+v", c.actual, c.transforms())
	}

	for name, bad := range map[string]struct {
		row   int
		value []byte
	}{
		"an empty chunk":             {0, []byte{}},
		"a chunk over a split":       {1, make([]byte, splitSize+1)},
		"a chunk past the row bound": {0, make([]byte, u+2)},
	} {
		cs, tf := newCensusOf("zlib-1")
		if g := feed(cs, key(tf, bad.row, 1), bad.value); g == nil || !g.invalid || g.chunks != nil || g.n != 1 {
			t.Errorf("%s was retained: %+v", name, g)
		}
	}
	// A full split under a row whose bound allows it is retained.
	c, tf = newCensusOf("none")
	if g := feed(c, key(tf, 1, 1), make([]byte, splitSize)); g == nil || g.invalid || len(g.chunks) != 1 {
		t.Fatalf("a full split was not retained: %+v", g)
	}

	// An unknown identity is counted unexpected per KV, at once, and retains nothing.
	c, tf = newCensusOf("none")
	_, outsideFrames := testCorpus(t, 6)
	outside, err := buildRow(tf.ts, outsideFrames[5])
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		g := feed(c, recordKey(tf, outside.pk, tuple.Tuple{int64(i)}.Pack()), []byte{1})
		if g == nil || g.exp != nil || g.base || g.chunks != nil || c.actual.UnexpectedKVs != int64(i+1) {
			t.Fatalf("outsider KV %d: %+v unexpected %d", i, g, c.actual.UnexpectedKVs)
		}
	}
	// A malformed key of a known identity is counted for it and retains nothing.
	c, tf = newCensusOf("none")
	if g := feed(c, recordKey(tf, tf.rows[0].pk, tuple.Tuple{int64(-1)}.Pack()), []byte{1}); g == nil || g.exp == nil || !g.invalid || g.chunks != nil || g.n != 1 {
		t.Fatalf("a malformed known key: %+v", g)
	}
	if c.actual.UnexpectedKVs != 0 {
		t.Fatal("a malformed known key was counted unexpected")
	}
}

// The source union bound is the paired Rust writer's: a row whose union is exactly
// 16 MiB is an obligation, one byte more is refused before any obligation exists.
func TestSourceUnionBound(t *testing.T) {
	t.Parallel()
	tf := newTenantFixture(t, "none", nil)
	probe := len(spanUnion(tf.ts, make([]byte, maxUnionBytes-64)))
	overhead := probe - (maxUnionBytes - 64)
	at := make([]byte, maxUnionBytes-overhead)
	if len(spanUnion(tf.ts, at)) != maxUnionBytes || len(spanUnion(tf.ts, append(at, 0))) != maxUnionBytes+1 {
		t.Fatal("the premise is a union of exactly the bound and one byte more")
	}
	exp := newExpectation(tf.ts, tf.base, "none")
	if err := exp.add(&sourceRow{pk: tuple.Tuple{"bound", int64(1)}, rowWire: at, indexKeys: make([][]string, len(spanIndexes))}); err != nil {
		t.Fatalf("a union at the bound: %v", err)
	}
	over := tuple.Tuple{"bound", int64(2)}
	if err := exp.add(&sourceRow{pk: over, rowWire: append(at, 0), indexKeys: make([][]string, len(spanIndexes))}); err == nil {
		t.Fatal("a union over the bound was accepted")
	}
	if e := exp.spans[string(tuple.Tuple{"bound", int64(1)}.Pack())]; e == nil || e.unionLen != maxUnionBytes || len(exp.spans) != 1 || exp.counts.SpanRecordIdentities != 1 {
		t.Fatalf("obligations %d, identities %d", len(exp.spans), exp.counts.SpanRecordIdentities)
	}
	if exp.spans[string(over.Pack())] != nil {
		t.Fatal("a refused row left an obligation")
	}
}
