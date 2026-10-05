package executor

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// selectedByRanges reports whether key falls in any range of the spec, through
// the same TupleRange.ToFDBRange the scans open (an exclusive low is the
// Strinc of its packed bound, so it skips every key it prefixes).
func selectedByRanges(t *testing.T, spec scanRangeSetSpec, key []byte) int {
	t.Helper()
	hits := 0
	for _, r := range materializeAllRanges(t, spec) {
		kr := r.ToFDBRange(subspace.FromBytes(nil))
		begin, end := kr.FDBRangeKeys()
		if bytes.Compare(key, begin.FDBKey()) >= 0 && bytes.Compare(key, end.FDBKey()) < 0 {
			hits++
		}
	}
	return hits
}

// A terminal NaN equality on a value-index or primary-key coordinate selects
// exactly the stored NaNs, of every sign and payload, whatever the probe's
// payload (RFC-257 WS-E 5.3: Java probes its NaN's bits; Go returns every NaN,
// as its per-row `=` does). Index entries carry a primary-key suffix, so the
// +Inf boundary must exclude +Inf's own entries.
func TestNaNEqualitySelectsBothNaNBlocks(t *testing.T) {
	t.Parallel()
	probes := []float64{
		math.Float64frombits(0x7ff8000000000000),
		math.Float64frombits(0xfff8000000000000),
		math.Float64frombits(0x7ff800000000abcd),
	}
	var fingerprints [][]byte
	for _, probe := range probes {
		for _, reverse := range []bool{false, true} {
			spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
				[]*predicates.ComparisonRange{scanRangeTestEq(t, int64(5)), scanRangeTestEq(t, probe)},
				[]values.Type{values.NotNullLong, values.NotNullDouble}, nil, reverse, "nan-blocks",
			)
			if err != nil {
				t.Fatalf("bind NaN %016x: %v", math.Float64bits(probe), err)
			}
			if !reverse {
				fingerprints = append(fingerprints, spec.fingerprint)
			}
			for _, prefix := range []int64{4, 5, 6} {
				for _, stored := range floatKeyDomain() {
					for _, pk := range []int64{1, 2} {
						key := tuple.Tuple{prefix, stored, pk}.Pack()
						want := 0
						if prefix == 5 && math.IsNaN(stored) {
							want = 1
						}
						if got := selectedByRanges(t, spec, key); got != want {
							t.Fatalf("probe %016x reverse=%t: key (%d, %016x, %d) selected by %d ranges, want %d",
								math.Float64bits(probe), reverse, prefix, math.Float64bits(stored), pk, got, want)
						}
					}
				}
			}
		}
	}
	for _, f := range fingerprints[1:] {
		if !bytes.Equal(f, fingerprints[0]) {
			t.Fatal("the scan's fingerprint depends on the probe's NaN payload; a resume under another payload must be the same scan")
		}
	}
	// FLOAT keys hold float32 NaNs.
	spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, math.NaN())},
		[]values.Type{values.NotNullFloat}, nil, false, "nan-blocks-float",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, bits := range []uint32{0xffffffff, 0xffc00000, 0x7fc00000, 0x7fffffff} {
		if selectedByRanges(t, spec, tuple.Tuple{math.Float32frombits(bits), int64(1)}.Pack()) != 1 {
			t.Errorf("FLOAT NaN %08x not selected", bits)
		}
	}
	for _, v := range []float32{float32(math.Inf(-1)), -1, 0, 1, float32(math.Inf(1))} {
		if selectedByRanges(t, spec, tuple.Tuple{v, int64(1)}.Pack()) != 0 {
			t.Errorf("FLOAT %v selected by a NaN equality", v)
		}
	}
}

// A NaN followed by a constrained component cannot be two ranges (its suffix
// would need a key filter across both blocks), and an aggregate-index or
// vector-partition scan keeps the refusal: both fail loudly before storage.
func TestNaNEqualityRefusals(t *testing.T) {
	t.Parallel()
	var want *UnsupportedPhysicalFloatEquivalenceError
	_, err := bindScanComparisonsToRangeSetWithNaNBlocks(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, math.NaN()), scanRangeTestEq(t, int64(1))},
		[]values.Type{values.NotNullDouble, values.NotNullLong}, nil, false, "nan-then-eq",
	)
	if !errors.As(err, &want) {
		t.Errorf("NaN then an equality: %v, want UnsupportedPhysicalFloatEquivalenceError", err)
	}
	_, err = bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, math.NaN())},
		[]values.Type{values.NotNullDouble}, nil, false, "aggregate",
	)
	if !errors.As(err, &want) {
		t.Errorf("aggregate-index binder: %v, want UnsupportedPhysicalFloatEquivalenceError", err)
	}
	_, err = bindScanComparisonsToRangeSetWithTerminalWidening(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, math.NaN())},
		[]values.Type{values.NotNullDouble}, nil, false, "vector", false,
	)
	if !errors.As(err, &want) {
		t.Errorf("vector-partition binder: %v, want UnsupportedPhysicalFloatEquivalenceError", err)
	}
}
