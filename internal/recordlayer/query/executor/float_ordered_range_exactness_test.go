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

// TestFloatSuffixedScanOpensOneRange pins the COST of a float coordinate that
// nothing constrains, as a structural fact rather than a wall clock.
//
// An unbound float coordinate used to be split into NULL / [-Inf..+Inf] / the
// two NaN blocks so a single-column float ORDER BY could drop its sort. That
// charged FOUR range opens to every scan whose first unbound coordinate is a
// float — ordering-consuming or not — and bought a plan change no test could
// observe (0 of 2489 in the plan-shape golden). It was removed.
//
// A wall-clock budget tells you THAT something regressed, weeks later and on a
// contended machine. This tells you WHAT: reinstating the split turns this red
// immediately, next to the comment explaining what it would have to earn.
func TestFloatSuffixedScanOpensOneRange(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		comps []*predicates.ComparisonRange
		types []values.Type
	}{
		{
			name:  "unbound DOUBLE coordinate",
			types: []values.Type{values.NullableDouble},
		},
		{
			name:  "unbound FLOAT coordinate",
			types: []values.Type{values.NullableFloat},
		},
		{
			name:  "equality prefix then an unbound DOUBLE suffix",
			comps: []*predicates.ComparisonRange{scanRangeTestComparison(t, predicates.ComparisonEquals, &values.ConstantValue{Value: int64(1), Typ: values.UnknownType})},
			types: []values.Type{values.NullableLong, values.NullableDouble},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			spec, err := bindScanComparisonsToRangeSet(
				test.comps, test.types, nil, false, "float-suffix-cost",
			)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			ranges := materializeAllRanges(t, spec)
			if len(ranges) != 1 {
				t.Fatalf("opened %d ranges, want 1 — an unconstrained float coordinate must not "+
					"be split into NaN blocks; that costs a round trip per block on every such "+
					"scan and buys an ordering claim nothing measured", len(ranges))
			}
		})
	}
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
