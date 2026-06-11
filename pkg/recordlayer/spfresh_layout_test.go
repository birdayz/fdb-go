package recordlayer

import (
	"bytes"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TestSPFreshHDRSortsBeforeEveryPK is the property the posting-header design
// leans on (RFC-094 §3): the HDR key element (tuple nil, 0x00) must sort
// strictly before EVERY legal primary-key encoding, across all pk element
// types the record layer admits, so a posting range read always yields the
// header first and the fetch cap never drops it. Sound because nulls are
// rejected in primary-key components — no pk element can encode below 0x00.
func TestSPFreshHDRSortsBeforeEveryPK(t *testing.T) {
	t.Parallel()
	s := newSPFreshStorage(subspace.Sub("spfresh_hdr_test"), 1)

	const fineID = int64(42)
	hdr := s.postingHDRKey(fineID)

	pks := []tuple.Tuple{
		{int64(0)},
		{int64(-1)},
		{int64(1) << 62},
		{int64(-1) << 62},
		{""},
		{"a"},
		{"\x00"}, // string containing a NUL still encodes with type byte 0x02
		{[]byte{}},
		{[]byte{0x00}},
		{float64(0)},
		{float64(-1e300)},
		{true},
		{false},
		{tuple.UUID{}},
		{int64(1), "composite", []byte{0xff}},
		{tuple.Tuple{int64(7), "nested"}},
		{"\xff\xff\xff"},
	}
	for _, pk := range pks {
		key := s.postingKey(fineID, pk)
		if bytes.Compare(hdr, key) >= 0 {
			t.Errorf("HDR key %x does not sort before posting key %x (pk=%v)", hdr, key, pk)
		}
		// And both must stay inside the posting's range.
		r, err := s.postingRange(fineID)
		if err != nil {
			t.Fatal(err)
		}
		begin, end := r.FDBRangeKeys()
		if bytes.Compare(key, begin.FDBKey()) < 0 || bytes.Compare(key, end.FDBKey()) >= 0 {
			t.Errorf("posting key %x (pk=%v) outside posting range [%x, %x)", key, pk, begin.FDBKey(), end.FDBKey())
		}
		if bytes.Compare(hdr, begin.FDBKey()) < 0 || bytes.Compare(hdr, end.FDBKey()) >= 0 {
			t.Errorf("HDR key %x outside posting range", hdr)
		}
	}
}

// TestSPFreshPostingPKRoundTrip pins key building/parsing: postingKey ->
// postingPK is the identity on the pk, and the HDR parses as ok=false.
func TestSPFreshPostingPKRoundTrip(t *testing.T) {
	t.Parallel()
	s := newSPFreshStorage(subspace.Sub("spfresh_pk_test"), 3)

	pks := []tuple.Tuple{
		{int64(1)},
		{int64(1), int64(2)},
		{"user", int64(99)},
		{[]byte{1, 2, 3}, "x", int64(-5)},
	}
	for _, pk := range pks {
		key := s.postingKey(7, pk)
		got, ok, err := s.postingPK(key)
		if err != nil || !ok {
			t.Fatalf("postingPK(%v): ok=%v err=%v", pk, ok, err)
		}
		if !bytes.Equal(got.Pack(), pk.Pack()) {
			t.Errorf("round-trip mismatch: got %v, want %v", got, pk)
		}
	}

	_, ok, err := s.postingPK(s.postingHDRKey(7))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("HDR key must parse as ok=false, not as a pk")
	}
}

// TestSPFreshGenerationIsolation pins that two generations of the same index
// never overlap, and that a generation's clear-range covers every subspace
// (the abandoned-build GC contract) while excluding META.
func TestSPFreshGenerationIsolation(t *testing.T) {
	t.Parallel()
	root := subspace.Sub("spfresh_gen_test")
	g1 := newSPFreshStorage(root, 1)
	g2 := newSPFreshStorage(root, 2)

	r1, err := g1.generationRange()
	if err != nil {
		t.Fatal(err)
	}
	b1, e1 := r1.FDBRangeKeys()

	inG1 := func(k []byte) bool {
		return bytes.Compare(k, b1.FDBKey()) >= 0 && bytes.Compare(k, e1.FDBKey()) < 0
	}

	pk := tuple.Tuple{int64(9)}
	g1Keys := [][]byte{
		g1.centroidKey(1, 2), g1.centroidHDRKey(1), g1.coarseKey(1),
		g1.postingKey(2, pk), g1.postingHDRKey(2), g1.membershipKey(pk),
		g1.counterKey(spfreshCounterFine, 2), g1.taskKey(spfreshTaskSplit, 2),
		g1.sidecarKey(pk), g1.stagingKey(1, pk),
	}
	for _, k := range g1Keys {
		if !inG1(k) {
			t.Errorf("g1 key %x not covered by g1's generation range", k)
		}
	}
	g2Keys := [][]byte{g2.centroidKey(1, 2), g2.postingKey(2, pk), g2.membershipKey(pk)}
	for _, k := range g2Keys {
		if inG1(k) {
			t.Errorf("g2 key %x inside g1's generation range (generations must be disjoint)", k)
		}
	}
	// META must NOT be inside any generation range (it survives GC).
	for k := spfreshMetaGeneration; k <= spfreshMetaBuild; k++ {
		if inG1(g1.metaKey(k)) {
			t.Errorf("META key %d inside the generation range — would be destroyed by abandoned-build GC", k)
		}
	}
}

// TestValidateSPFreshConfig pins the config invariants the RFC's sizing and
// lifecycle arguments depend on.
func TestValidateSPFreshConfig(t *testing.T) {
	t.Parallel()
	if err := ValidateSPFreshConfig(DefaultSPFreshConfig(768)); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if err := ValidateSPFreshConfig(DefaultSPFreshConfig(4096)); err == nil {
		// 4096 dims at default Lmax overflows the reply budget — must be caught.
		t.Fatal("4096-dim defaults exceed the reply budget and must fail validation")
	}

	bad := func(mutate func(*SPFreshConfig), wantSubstr string) {
		t.Helper()
		c := DefaultSPFreshConfig(128)
		mutate(&c)
		err := ValidateSPFreshConfig(c)
		if err == nil {
			t.Errorf("expected validation error (%s), got nil", wantSubstr)
		}
	}
	bad(func(c *SPFreshConfig) { c.NumDimensions = 0 }, "numDimensions")
	bad(func(c *SPFreshConfig) { c.Lmax = 8 }, "lmax")
	bad(func(c *SPFreshConfig) { c.LminRatio = 1 }, "lminRatio")
	bad(func(c *SPFreshConfig) { c.CellMax = c.CellTarget }, "cellMax")
	bad(func(c *SPFreshConfig) { c.Replication = 5 }, "replication")
	// The alpha=1.0 closure bug (RFC-094 §5): with r>1, alpha<=1.0 silently
	// admits only the nearest centroid. Must be rejected.
	bad(func(c *SPFreshConfig) { c.Alpha = 1.0 }, "alpha")
	bad(func(c *SPFreshConfig) { c.Kn = 0 }, "kn")
	bad(func(c *SPFreshConfig) { c.NumExBits = 9 }, "exBits")
	// Reply-budget rule: huge Lmax must be rejected even when individually legal.
	bad(func(c *SPFreshConfig) { c.Lmax = 4096; c.NumDimensions = 768 }, "reply")

	// alpha=1.0 IS legal at r=1 (no closure, nothing to admit).
	c := DefaultSPFreshConfig(128)
	c.Replication = 1
	c.Alpha = 1.0
	if err := ValidateSPFreshConfig(c); err != nil {
		t.Errorf("alpha=1.0 with r=1 must validate, got %v", err)
	}
}

// TestParseSPFreshConfig pins option parsing round-trip and defaults.
func TestParseSPFreshConfig(t *testing.T) {
	t.Parallel()
	idx := &Index{
		Name: "v",
		Type: IndexTypeVectorSPFresh,
		Options: map[string]string{
			IndexOptionSPFreshNumDimensions: "768",
			IndexOptionSPFreshMetric:        "COSINE_METRIC",
			IndexOptionSPFreshLmax:          "128",
			IndexOptionSPFreshAlpha:         "1.5",
			IndexOptionSPFreshSidecar:       "false",
		},
	}
	c := parseSPFreshConfig(idx)
	if c.NumDimensions != 768 || c.Metric != VectorMetricCosine || c.Lmax != 128 ||
		c.Alpha != 1.5 || c.Sidecar {
		t.Fatalf("parse mismatch: %+v", c)
	}
	// Absent options take RFC defaults.
	if c.CellTarget != spfreshDefaultCellTarget || c.Replication != spfreshDefaultReplication ||
		c.Kn != spfreshDefaultKn || c.LminRatio != spfreshDefaultLminRatio {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

// FuzzSPFreshPostingPKSpan pins span-extraction equivalence with the decoding
// postingPK: same entry/HDR classification, and the span IS the packed pk
// (so sidecarKeyFromSpan(span) == sidecarKey(pk) byte-for-byte).
func FuzzSPFreshPostingPKSpan(f *testing.F) {
	f.Add(int64(7), int64(42), "user", false)
	f.Add(int64(1), int64(-9), "", true)
	f.Fuzz(func(t *testing.T, fineID, pkInt int64, pkStr string, hdr bool) {
		s := newSPFreshStorage(subspace.Sub("fuzz-span"), 1)
		var key fdb.Key
		if hdr {
			key = s.postingHDRKey(fineID)
		} else {
			key = s.postingKey(fineID, tuple.Tuple{pkInt, pkStr})
		}
		prefixLen := len(s.postings.Pack(tuple.Tuple{fineID}))

		pk, okPK, errPK := s.postingPK(key)
		span, okSpan, errSpan := s.postingPKSpan(key, prefixLen)
		if (errPK == nil) != (errSpan == nil) {
			t.Fatalf("error divergence: postingPK=%v postingPKSpan=%v", errPK, errSpan)
		}
		if errPK != nil {
			return
		}
		if okPK != okSpan {
			t.Fatalf("entry/HDR divergence: postingPK=%v postingPKSpan=%v", okPK, okSpan)
		}
		if !okPK {
			return
		}
		if string(s.sidecarKey(pk)) != string(s.sidecarKeyFromSpan(string(span))) {
			t.Fatalf("sidecar key divergence for pk %v", pk)
		}
		got, derr := tuple.Unpack(span)
		if derr != nil {
			t.Fatalf("span must decode: %v", derr)
		}
		if len(got) != len(pk) {
			t.Fatalf("span decodes to %d elements, postingPK gave %d", len(got), len(pk))
		}
	})
}
