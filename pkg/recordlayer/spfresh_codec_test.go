package recordlayer

import (
	"bytes"
	"math"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

func TestSPFreshCentroidRowRoundTrip(t *testing.T) {
	t.Parallel()
	vec := []float64{1.5, -2.25, 0, 65504, 1e-7}
	data := encodeCentroidRow(spfreshStateForward, 7, 100, 101, vec)
	row, err := decodeCentroidRow(data)
	if err != nil {
		t.Fatal(err)
	}
	if row.state != spfreshStateForward || row.epoch != 7 || row.childA != 100 || row.childB != 101 {
		t.Fatalf("header mismatch: %+v", row)
	}
	got, err := row.vector()
	if err != nil {
		t.Fatal(err)
	}
	want, err := vectorcodec.Deserialize(vectorcodec.SerializeHalf(vec))
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("component %d: got %v, want %v", i, got[i], want[i])
		}
	}
	// Negative IDs and zero ("none") survive.
	d2 := encodeCentroidRow(spfreshStateActive, -1, 0, 0, nil)
	r2, err := decodeCentroidRow(d2)
	if err != nil || r2.epoch != -1 || r2.childA != 0 || r2.childB != 0 {
		t.Fatalf("zero/negative round-trip: %+v err=%v", r2, err)
	}
	// Invalid state rejected.
	bad := encodeCentroidRow(spfreshStateActive, 0, 0, 0, nil)
	bad[0] = 99
	if _, err := decodeCentroidRow(bad); err == nil {
		t.Fatal("invalid state must error")
	}
	// Truncated rejected.
	if _, err := decodeCentroidRow(data[:10]); err == nil {
		t.Fatal("truncated row must error")
	}
}

func TestSPFreshMembershipRoundTrip(t *testing.T) {
	t.Parallel()
	for _, ids := range [][]int64{nil, {1}, {1, 2, 3}, {-5, 0, math.MaxInt64}} {
		data := encodeMembership(ids)
		got, err := decodeMembership(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(ids) {
			t.Fatalf("len mismatch: %d vs %d", len(got), len(ids))
		}
		for i := range ids {
			if got[i] != ids[i] {
				t.Errorf("id %d: got %d, want %d", i, got[i], ids[i])
			}
		}
	}
	if _, err := decodeMembership([]byte{1, 2, 3}); err == nil {
		t.Fatal("non-multiple-of-8 must error")
	}
}

func TestSPFreshTaskRowRoundTrip(t *testing.T) {
	t.Parallel()
	rows := []spfreshTaskRow{
		{},
		{owner: "writer-1", leaseDeadlineMs: 1234567890123, state: spfreshCellfinFinalized, childA: 5, childB: 6},
		{owner: "", leaseDeadlineMs: -1, state: 255, childA: -9, childB: 0},
	}
	for _, r := range rows {
		got, err := decodeTaskRow(encodeTaskRow(r))
		if err != nil {
			t.Fatal(err)
		}
		if got != r {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, r)
		}
	}
	if _, err := decodeTaskRow([]byte{0xff, 0xff}); err == nil {
		t.Fatal("garbage task row must error")
	}
}

func TestSPFreshHDRRoundTrip(t *testing.T) {
	t.Parallel()
	cell, a, b, err := decodePostingHDR(encodePostingHDR(3, 10, 11))
	if err != nil || cell != 3 || a != 10 || b != 11 {
		t.Fatalf("posting HDR: %d %d %d err=%v", cell, a, b, err)
	}
	ca, cb, err := decodeCellHDR(encodeCellHDR(20, 21))
	if err != nil || ca != 20 || cb != 21 {
		t.Fatalf("cell HDR: %d %d err=%v", ca, cb, err)
	}
	if _, _, _, err := decodePostingHDR(encodeCellHDR(1, 2)); err == nil {
		t.Fatal("2-element payload must not parse as posting HDR")
	}
}

func TestSPFreshDeltaRoundTrip(t *testing.T) {
	t.Parallel()
	deltas := []spfreshDelta{
		{op: spfreshOpAddFine, ids: []int64{1, 2}},
		{op: spfreshOpForwardFine, ids: []int64{2, 3, 4}},
		{op: spfreshOpDeadFine, ids: []int64{2}},
		{op: spfreshOpAddCell, ids: []int64{9}},
		{op: spfreshOpForwardCell, ids: []int64{9, 10, 11}},
		{op: spfreshOpDeadCell, ids: []int64{9}},
		{op: spfreshOpGeneration, ids: []int64{3}},
	}
	for _, d := range deltas {
		got, err := decodeDelta(encodeDelta(d))
		if err != nil {
			t.Fatalf("op %d: %v", d.op, err)
		}
		if got.op != d.op || len(got.ids) != len(d.ids) {
			t.Fatalf("op %d: round-trip mismatch %+v", d.op, got)
		}
		for i := range d.ids {
			if got.ids[i] != d.ids[i] {
				t.Errorf("op %d id %d: got %d want %d", d.op, i, got.ids[i], d.ids[i])
			}
		}
	}
	// Wrong arity rejected.
	if _, err := decodeDelta(encodeDelta(spfreshDelta{op: spfreshOpDeadFine, ids: []int64{1, 2}})); err == nil {
		t.Fatal("wrong-arity delta must error")
	}
}

// Fuzz targets: decoders must never panic on arbitrary bytes (repo standard:
// 0 panics; errors are the only acceptable failure mode).

func FuzzSPFreshDecodeCentroidRow(f *testing.F) {
	f.Add(encodeCentroidRow(spfreshStateActive, 1, 0, 0, []float64{1, 2}))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 40))
	f.Fuzz(func(t *testing.T, data []byte) {
		row, err := decodeCentroidRow(data)
		if err == nil {
			_, _ = row.vector()
		}
	})
}

func FuzzSPFreshDecodeMembership(f *testing.F) {
	f.Add(encodeMembership([]int64{1, 2}))
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeMembership(data)
	})
}

func FuzzSPFreshDecodeTaskRow(f *testing.F) {
	f.Add(encodeTaskRow(spfreshTaskRow{owner: "x", leaseDeadlineMs: 1}))
	f.Add([]byte{0x02, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeTaskRow(data)
	})
}

func FuzzSPFreshDecodeDelta(f *testing.F) {
	f.Add(encodeDelta(spfreshDelta{op: spfreshOpAddFine, ids: []int64{1, 2}}))
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeDelta(data)
		_, _, _, _ = decodePostingHDR(data)
		_, _, _ = decodeCellHDR(data)
	})
}

// encodeCentroidRowRaw must preserve the vector byte-for-byte across a state
// rewrite. The append(encodeCentroidRow(..., nil), vecBytes...) pattern it
// replaces produced a DOUBLE vectorcodec header (SerializeHalf(nil) still
// emits its type byte): every SEAL/FORWARD flip silently corrupted the
// preserved vector, which then decoded to garbage dimensions and panicked
// k-means in the 094.3 coarse split (caught by the cold-start e2e).
func TestSPFreshCentroidRowRawPreservesVector(t *testing.T) {
	t.Parallel()
	vec := []float64{1.5, -2.25, 8}
	original := encodeCentroidRow(spfreshStateActive, 7, 0, 0, vec)
	row, err := decodeCentroidRow(original)
	if err != nil {
		t.Fatal(err)
	}

	resealed := encodeCentroidRowRaw(spfreshStateSealed, row.epoch, 0, 0, row.vecBytes)
	sealedRow, err := decodeCentroidRow(resealed)
	if err != nil {
		t.Fatal(err)
	}
	if sealedRow.state != spfreshStateSealed || sealedRow.epoch != 7 {
		t.Fatalf("header not rewritten: %+v", sealedRow)
	}
	got, err := sealedRow.vector()
	if err != nil {
		t.Fatalf("vector corrupted by re-header: %v", err)
	}
	if len(got) != len(vec) {
		t.Fatalf("vector dims changed across rewrite: got %d want %d (the double-header bug)", len(got), len(vec))
	}
	want, _ := row.vector()
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("vector component %d changed: got %v want %v", i, got[i], want[i])
		}
	}

	// The buggy pattern, pinned as buggy: double header decodes to the WRONG
	// shape (or errors) — never to the original vector.
	corrupt := append(encodeCentroidRow(spfreshStateSealed, 7, 0, 0, nil), row.vecBytes...)
	corruptRow, err := decodeCentroidRow(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if cv, cerr := corruptRow.vector(); cerr == nil && len(cv) == len(vec) {
		same := true
		for i := range cv {
			if cv[i] != vec[i] {
				same = false
			}
		}
		if same {
			t.Fatal("the double-header pattern unexpectedly round-trips — regression premise broken")
		}
	}
}
