package executor

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// floatKeyDomain is every structurally distinct region of a FLOAT/DOUBLE key
// coordinate, including BOTH NaN signs. The negative NaNs are the whole reason
// this file exists: they sort below -Inf physically and above +Inf logically,
// so a test domain without them cannot express the defect and any single-range
// binding looks correct.
func floatKeyDomain() []float64 {
	return []float64{
		math.Float64frombits(0xFFFFFFFFFFFFFFFF), // most negative NaN encoding
		math.Float64frombits(0xFFF8000000000000), // canonical negative NaN
		math.Inf(-1),
		-8.0, -4.0, -1.0,
		math.Copysign(0, -1), 0.0,
		1.0, 4.0, 8.0,
		math.Inf(1),
		math.Float64frombits(0x7FF8000000000000), // canonical positive NaN
		math.Float64frombits(0x7FFFFFFFFFFFFFFF), // largest NaN encoding
	}
}

// logicallySelects asks the ENGINE'S OWN predicate evaluator whether a stored
// key qualifies, and deliberately does not reimplement the semantics.
//
// THERE ARE TWO FLOAT COMPARATORS IN THIS ENGINE AND THEY DISAGREE ON PURPOSE:
//
//   - predicates.Comparison.Eval / cmpAny is the PREDICATE comparator. It
//     checks IEEE equality FIRST, so -0.0 == +0.0, and only falls back to the
//     total order (NaN greatest) when that fails.
//   - values.CompareFloat64 is the SORT comparator. It puts -0.0 BELOW +0.0 so
//     that sort order matches FDB tuple / index order.
//
// Picking the wrong one here is a live trap, not a hypothetical: the first
// version of this test built its oracle on CompareFloat64 and reported the
// binder as broken on every signed-zero threshold — "-0 is returned but should
// not be" — while the binder was correct and the ORACLE was wrong. If this test
// ever starts failing only on ±0 thresholds, suspect the oracle before the
// binder.
func logicallySelects(
	t *testing.T,
	comparison predicates.ComparisonType,
	stored, threshold float64,
) bool {
	t.Helper()
	c := predicates.Comparison{
		Type:    comparison,
		Operand: &values.ConstantValue{Value: threshold, Typ: values.UnknownType},
	}
	result, err := c.Eval(stored)
	if err != nil {
		t.Fatalf("evaluate %v %v against %v: %v", comparison, threshold, stored, err)
	}
	return result == predicates.TriTrue
}

// physicallySelected reports whether the packed key falls inside a materialized
// tuple range, by the same byte comparison FDB performs.
func physicallySelected(r recordlayer.TupleRange, key []byte) bool {
	lowOK := true
	switch r.LowEndpoint {
	case recordlayer.EndpointTypeTreeStart:
		lowOK = true
	case recordlayer.EndpointTypeRangeInclusive:
		lowOK = bytes.Compare(key, r.Low.Pack()) >= 0
	case recordlayer.EndpointTypeRangeExclusive:
		lowOK = bytes.Compare(key, r.Low.Pack()) > 0
	}
	highOK := true
	switch r.HighEndpoint {
	case recordlayer.EndpointTypeTreeEnd:
		highOK = true
	case recordlayer.EndpointTypeRangeInclusive:
		highOK = bytes.Compare(key, r.High.Pack()) <= 0
	case recordlayer.EndpointTypeRangeExclusive:
		highOK = bytes.Compare(key, r.High.Pack()) < 0
	}
	return lowOK && highOK
}

// TestFloatKeyTupleOrderIsIEEETotalOrder pins the physical fact every range
// decision in this file rests on. If tuple encoding ever stopped preserving
// NaN sign and payload, the decompositions below would become wrong in a way no
// other test would notice — so the ordering is asserted directly.
func TestFloatKeyTupleOrderIsIEEETotalOrder(t *testing.T) {
	t.Parallel()
	domain := floatKeyDomain()
	for i := 1; i < len(domain); i++ {
		previous := tuple.Tuple{domain[i-1]}.Pack()
		current := tuple.Tuple{domain[i]}.Pack()
		if bytes.Compare(previous, current) >= 0 {
			t.Fatalf(
				"packed tuple order broke at index %d: %v (% x) is not below %v (% x).\n"+
					"Every float range decomposition here assumes negNaN < -Inf < finite < +Inf < posNaN",
				i, domain[i-1], previous, domain[i], current)
		}
	}
}

// TestOrderedFloatRangeSetSelectsExactlyTheLogicalRows is the exactness proof
// for RFC-208's ordered-float access path: for every ordered comparison against
// every interesting threshold, the union of the materialized physical ranges
// must select EXACTLY the keys the query comparator selects — no missing row
// (which would be silent data loss) and no extra row (which would be a wrong
// answer the scan cannot compensate).
//
// A single physical range cannot do this for `>`/`>=`: all NaNs are logically
// greatest, but the negative ones are physically the smallest keys there are.
// That is the case a one-range binding gets wrong and a full-table-scan
// fallback merely hides at O(N) cost.
func TestOrderedFloatRangeSetSelectsExactlyTheLogicalRows(t *testing.T) {
	t.Parallel()
	comparisons := []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	}
	thresholds := []float64{
		math.Inf(-1), -8.0, -1.0, math.Copysign(0, -1), 0.0, 1.0, 8.0, math.Inf(1),
	}
	domain := floatKeyDomain()

	for _, comparison := range comparisons {
		for _, threshold := range thresholds {
			name := fmt.Sprintf("%v_%v", comparison, threshold)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				operand := &values.ConstantValue{Value: threshold, Typ: values.UnknownType}
				spec, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparison, operand),
					},
					[]values.Type{values.NotNullDouble}, nil, false, "ordered-float-exactness",
				)
				if err != nil {
					t.Fatalf("bind %v %v: %v", comparison, threshold, err)
				}
				ranges := materializeAllRanges(t, spec)
				if len(ranges) == 0 && !spec.empty {
					t.Fatalf("%v %v produced no ranges and is not empty", comparison, threshold)
				}
				for _, stored := range domain {
					key := tuple.Tuple{stored}.Pack()
					want := logicallySelects(t, comparison, stored, threshold)
					got := false
					hits := 0
					for _, r := range ranges {
						if physicallySelected(r, key) {
							got = true
							hits++
						}
					}
					if got != want {
						verb := "MISSES a qualifying key (silent data loss)"
						if got {
							verb = "returns a NON-qualifying key (wrong answer)"
						}
						t.Fatalf(
							"%v %v: stored %v (bits %#016x) — physical range set %s.\nlogical=%v physical=%v across %d range(s)",
							comparison, threshold, stored, math.Float64bits(stored),
							verb, want, got, len(ranges))
					}
					if hits > 1 {
						t.Fatalf(
							"%v %v: stored %v is covered by %d ranges; the range set must be DISJOINT or a row is returned twice",
							comparison, threshold, stored, hits)
					}
				}
			})
		}
	}
}

// TestOrderedFloatRangeSetKeepsIndexAccess pins the COST half of the same
// decision: the point of the decomposition is that an ordered float predicate
// still binds a bounded index range instead of degrading to a full scan. A
// binding that returned one unbounded [TreeStart,TreeEnd] range would satisfy
// the exactness test above and still be the regression this work exists to undo.
func TestOrderedFloatRangeSetKeepsIndexAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		comparison predicates.ComparisonType
		wantRanges int
	}{
		{predicates.ComparisonLessThan, 1},
		{predicates.ComparisonLessThanOrEq, 1},
		{predicates.ComparisonGreaterThan, 2},
		{predicates.ComparisonGreaterThanEq, 2},
	} {
		operand := &values.ConstantValue{Value: 4.0, Typ: values.UnknownType}
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, test.comparison, operand),
			},
			[]values.Type{values.NotNullDouble}, nil, false, "ordered-float-access",
		)
		if err != nil {
			t.Fatalf("bind %v: %v", test.comparison, err)
		}
		ranges := materializeAllRanges(t, spec)
		if len(ranges) != test.wantRanges {
			t.Fatalf("%v produced %d ranges, want %d", test.comparison, len(ranges), test.wantRanges)
		}
		for i, r := range ranges {
			if r.LowEndpoint == recordlayer.EndpointTypeTreeStart &&
				r.HighEndpoint == recordlayer.EndpointTypeTreeEnd {
				t.Fatalf(
					"%v range %d is unbounded on both ends — that is a full scan wearing a range set, "+
						"which is exactly the access-path regression this decomposition removes",
					test.comparison, i)
			}
		}
	}
}

// TestUnboundFloatRangeSetIsCompleteAndLogicallyOrdered pins the ordering-only
// decomposition. An unbound float coordinate has no predicate to be exact
// about, so the two properties that matter are:
//
//  1. COMPLETENESS — every stored key is still returned exactly once. Splitting
//     a scan into blocks is the easiest possible way to silently drop rows, and
//     an ORDER BY that loses the NaN rows would look perfectly healthy.
//  2. LOGICAL ORDER — the blocks come back in the order the query comparator
//     would sort them, NULLs first, every NaN last. That is the whole reason
//     the split exists: without it the index cannot serve an ORDER BY and the
//     query pays a full scan plus a blocking sort.
func TestUnboundFloatRangeSetIsCompleteAndLogicallyOrdered(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		nil, []values.Type{values.NullableDouble}, nil, false, "unbound-float-order",
	)
	if err != nil {
		t.Fatalf("bind unbound float coordinate: %v", err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) < 2 {
		t.Fatalf("unbound float coordinate produced %d range(s); one raw range puts the "+
			"negative NaNs between the NULLs and -Inf and cannot serve an ORDER BY", len(ranges))
	}

	// Completeness, including the NULL key, which lives below every float.
	domain := floatKeyDomain()
	for _, stored := range append([]any{nil}, float64SliceToAny(domain)...) {
		key := tuple.Tuple{stored}.Pack()
		hits := 0
		for _, r := range ranges {
			if physicallySelected(r, key) {
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("stored %v is covered by %d ranges, want exactly 1 — the block split must "+
				"partition the coordinate, or an unbounded scan drops or duplicates rows", stored, hits)
		}
	}

	// Logical order: walking the ranges in order must visit the keys in the
	// order the SORT comparator would put them. NULLs come first; all NaNs are
	// logically tied and come last.
	var visited []any
	for _, r := range ranges {
		for _, stored := range append([]any{nil}, float64SliceToAny(domain)...) {
			if physicallySelected(r, tuple.Tuple{stored}.Pack()) {
				visited = append(visited, stored)
			}
		}
	}
	rank := func(v any) int {
		f, ok := v.(float64)
		switch {
		case !ok:
			return 0 // NULL sorts first
		case math.IsNaN(f):
			return 2 // every NaN is logically greatest, and mutually tied
		default:
			return 1
		}
	}
	for i := 1; i < len(visited); i++ {
		if rank(visited[i-1]) > rank(visited[i]) {
			t.Fatalf("range order visits %v before %v, which the sort comparator orders the other way; "+
				"the blocks must be concatenated in LOGICAL order or the scan cannot claim an ordering",
				visited[i-1], visited[i])
		}
	}
}

func float64SliceToAny(in []float64) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// materializeAllRanges walks the spec's odometer and returns every physical
// range it enumerates.
func materializeAllRanges(t *testing.T, spec scanRangeSetSpec) []recordlayer.TupleRange {
	t.Helper()
	if spec.empty {
		return nil
	}
	counts := spec.alternativeCounts
	choices := make([]uint32, len(counts))
	var out []recordlayer.TupleRange
	for {
		r, err := spec.materialize(choices)
		if err != nil {
			t.Fatalf("materialize %v: %v", choices, err)
		}
		out = append(out, r)
		position := len(counts) - 1
		for position >= 0 {
			choices[position]++
			if choices[position] < counts[position] {
				break
			}
			choices[position] = 0
			position--
		}
		if position < 0 {
			return out
		}
	}
}
