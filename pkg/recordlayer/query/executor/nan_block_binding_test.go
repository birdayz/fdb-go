package executor

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
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

// A NaN followed by constrained components binds as the two NaN blocks plus a
// key filter on the later components: the union of ranges and filter selects
// exactly the keys the predicates select, for an equality, a range tail and a
// second NaN component (RFC-257 WS-E 5.3).
func TestNaNEqualityKeyFilterSelectsExactly(t *testing.T) {
	t.Parallel()
	nan := math.Float64frombits(0x7ff8000000000000)
	greaterThan := func(v any) *predicates.ComparisonRange {
		return scanRangeTestComparison(t, predicates.ComparisonGreaterThan, values.LiteralValue(v))
	}
	gValues := []float64{math.Float64frombits(0xfff8000000000000), -1, math.Copysign(0, -1), 0, 1, 2, math.Inf(1), nan}
	for _, c := range []struct {
		name  string
		later *predicates.ComparisonRange
		want  func(g float64) bool
	}{
		{"equality", scanRangeTestEq(t, 1.0), func(g float64) bool { return g == 1 }},
		{"zero equality", scanRangeTestEq(t, 0.0), func(g float64) bool { return g == 0 }},
		{"range tail", greaterThan(0.0), func(g float64) bool { return g > 0 || math.IsNaN(g) }},
		{"second NaN", scanRangeTestEq(t, math.Float64frombits(0x7ff800000000abcd)), math.IsNaN},
	} {
		for _, reverse := range []bool{false, true} {
			spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
				[]*predicates.ComparisonRange{scanRangeTestEq(t, nan), c.later},
				[]values.Type{values.NotNullDouble, values.NotNullDouble}, nil, reverse, "nan-then-"+c.name,
			)
			if err != nil {
				t.Fatalf("%s: bind: %v", c.name, err)
			}
			if spec.keyFilter == nil {
				t.Fatalf("%s: no key filter for the component after the NaN", c.name)
			}
			for _, f := range floatKeyDomain() {
				for _, g := range gValues {
					key := tuple.Tuple{f, g, int64(7)}
					admitted, err := spec.keyFilter.admits(key, 0)
					if err != nil {
						t.Fatalf("%s: admits: %v", c.name, err)
					}
					got := selectedByRanges(t, spec, key.Pack()) == 1 && admitted
					if want := math.IsNaN(f) && c.want(g); got != want {
						t.Fatalf("%s reverse=%t: key (%016x, %v) selected=%t, want %t",
							c.name, reverse, math.Float64bits(f), g, got, want)
					}
				}
			}
		}
	}
	// The filter's comparands are part of the scan's identity; NaN payloads
	// and zero signs are not.
	fp := func(later *predicates.ComparisonRange) []byte {
		spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
			[]*predicates.ComparisonRange{scanRangeTestEq(t, nan), later},
			[]values.Type{values.NotNullDouble, values.NotNullDouble}, nil, false, "fp",
		)
		if err != nil {
			t.Fatal(err)
		}
		return spec.fingerprint
	}
	if bytes.Equal(fp(scanRangeTestEq(t, 1.0)), fp(scanRangeTestEq(t, 2.0))) {
		t.Error("two different filter comparands share a scan fingerprint")
	}
	if !bytes.Equal(fp(scanRangeTestEq(t, 0.0)), fp(scanRangeTestEq(t, math.Copysign(0, -1)))) {
		t.Error("+0 and -0 filter comparands are one comparison but fingerprint apart")
	}
	if !bytes.Equal(fp(scanRangeTestEq(t, nan)), fp(scanRangeTestEq(t, math.Float64frombits(0xfff8000000000001)))) {
		t.Error("two NaN filter comparands are one comparison but fingerprint apart")
	}
	// A NULL comparand after the NaN admits nothing.
	spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, nan), scanRangeTestComparison(t, predicates.ComparisonEquals, &values.ConstantValue{Value: nil, Typ: values.NullableDouble})},
		[]values.Type{values.NotNullDouble, values.NotNullDouble}, nil, false, "nan-then-null",
	)
	if err != nil || !spec.empty {
		t.Fatalf("NaN then `= NULL`: empty=%t, err=%v; want an empty scan", spec.empty, err)
	}
}

// The filter returns the inner cursor's own results: a rejected entry is read
// but never returned, a returned entry carries its own continuation, and a
// limit that expires on a rejected entry resumes after it.
func TestKeyFilterCursorForwardsContinuations(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	spec, err := bindScanComparisonsToRangeSetWithNaNBlocks(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, nan), scanRangeTestEq(t, int64(1))},
		[]values.Type{values.NotNullDouble, values.NotNullLong}, nil, false, "cursor",
	)
	if err != nil {
		t.Fatal(err)
	}
	keys := []tuple.Tuple{{nan, int64(1)}, {nan, int64(2)}, {nan, int64(1)}, {nan, int64(3)}}
	var results []recordlayer.RecordCursorResult[tuple.Tuple]
	for i, k := range keys {
		results = append(results, recordlayer.NewResultWithValue(k, recordlayer.NewBytesContinuation([]byte{byte('a' + i)})))
	}
	// The scan limit expires right after the rejected (nan, 3).
	results = append(results, recordlayer.NewResultNoNext[tuple.Tuple](recordlayer.ScanLimitReached, recordlayer.NewBytesContinuation([]byte("d"))))
	cursor := filterScanKeys(spec, recordlayer.RecordCursor[tuple.Tuple](&scriptedCursor[tuple.Tuple]{results: results}),
		func(k tuple.Tuple) tuple.Tuple { return k }, 0)
	var got []string
	for {
		r, err := cursor.OnNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cont, _ := r.GetContinuation().ToBytes()
		if !r.HasNext() {
			if r.GetNoNextReason() != recordlayer.ScanLimitReached || string(cont) != "d" {
				t.Fatalf("terminal result: %v %q; want the limit with the rejected entry's continuation", r.GetNoNextReason(), cont)
			}
			break
		}
		got = append(got, string(cont))
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("returned continuations %v, want [a c]", got)
	}
}

type scriptedCursor[T any] struct {
	results []recordlayer.RecordCursorResult[T]
	next    int
}

func (c *scriptedCursor[T]) OnNext(context.Context) (recordlayer.RecordCursorResult[T], error) {
	r := c.results[c.next]
	if c.next < len(c.results)-1 {
		c.next++
	}
	return r, nil
}

func (c *scriptedCursor[T]) Close() error   { return nil }
func (c *scriptedCursor[T]) IsClosed() bool { return false }

// An aggregate-index or vector-partition scan keeps the refusal: both fail
// loudly before storage.
func TestNaNEqualityRefusals(t *testing.T) {
	t.Parallel()
	var want *UnsupportedPhysicalFloatEquivalenceError
	_, err := bindScanComparisonsToRangeSet(
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
