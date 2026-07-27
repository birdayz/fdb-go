package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestInJoinHintCost_ScalesOffChildCardinalityWhenNotAPointProbe pins the
// real production defect: ImplementInJoinRule (rule_implement_in_join.go)
// matches ANY equality-bound inner, unique or not, so an InJoin over a
// non-unique secondary index (`category IN ('a','b','c')`) is a reachable
// plan shape. HintCost previously set Cardinality = len(inValues)
// unconditionally, discarding the child's own (non-unit) cardinality — the
// same principle plan_properties.go's computeCardinalities already applies
// for RecordQueryInJoinPlan (inSize.Times(child.GetMaxCardinality())).
//
// Pinned as a PROPERTY across several bucket sizes and IN-list lengths, not
// one hardcoded example: for a non-unique bind the ratio
// Cardinality / (inListLen * childCardinality) must stay exactly 1.
func TestInJoinHintCost_ScalesOffChildCardinalityWhenNotAPointProbe(t *testing.T) {
	t.Parallel()

	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))

	tests := []struct {
		name        string
		tableCard   float64
		inValues    []any
		wantScanCPU bool
	}{
		{name: "3-way IN over 1000-row table", tableCard: 1000, inValues: []any{int64(1), int64(2), int64(3)}},
		{name: "1-way IN (degenerate) over 1000-row table", tableCard: 1000, inValues: []any{int64(1)}},
		{name: "5-way IN over 50-row table", tableCard: 50, inValues: []any{int64(1), int64(2), int64(3), int64(4), int64(5)}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stats := properties.FixedStatistics{Cardinality: test.tableCard}
			inner := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{eq}, []string{"T"}, values.UnknownType, false).
				WithIndexMetadata([]string{"CATEGORY"}, []string{"ID"}, false /* non-unique */)

			childCost := inner.HintCost(nil, stats)
			if childCost.Cardinality <= 1 {
				t.Fatalf("test setup: child cardinality = %v, want a bucket (>1) — must not itself be a point probe", childCost.Cardinality)
			}

			inJoin := NewRecordQueryInJoinPlan(inner, "x", false, false)
			inJoin.SetInValues(test.inValues)

			got := inJoin.HintCost([]properties.Cost{childCost}, stats)

			inListLen := float64(len(test.inValues))
			wantCardinality := inListLen * childCost.Cardinality
			if got.Cardinality != wantCardinality {
				t.Fatalf("cardinality = %v, want %v (inListLen(%v) * childCardinality(%v)) — the child's cardinality must NOT be discarded for a non-unique bind",
					got.Cardinality, wantCardinality, inListLen, childCost.Cardinality)
			}
			if got.Cardinality == inListLen {
				t.Fatalf("cardinality regressed to the bare IN-list length (%v) — the non-unique-bind fix did not fire", inListLen)
			}
		})
	}
}

// TestInJoinHintCost_StaysInValuesLenForProvableUniqueBind is the mirror
// direction: when the inner IS a provable point probe (full-PK or
// full-unique-index equality bind), the IN-list length must remain the exact
// output cardinality regardless of what the child's OWN standalone cost
// reports — pinned across several IN-list lengths, not one example.
func TestInJoinHintCost_StaysInValuesLenForProvableUniqueBind(t *testing.T) {
	t.Parallel()

	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}}
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))

	tests := []struct {
		name     string
		inValues []any
	}{
		{name: "3-way IN", inValues: []any{int64(1), int64(2), int64(3)}},
		{name: "1-way IN", inValues: []any{int64(1)}},
		{name: "7-way IN", inValues: []any{int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7)}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithPrimaryKey(pk).
				WithScanComparisons([]*predicates.ComparisonRange{eq})

			inJoin := NewRecordQueryInJoinPlan(inner, "x", false, false)
			inJoin.SetInValues(test.inValues)

			// Child cost deliberately bogus/large: a proven point probe must
			// ignore it entirely, not scale by it.
			got := inJoin.HintCost([]properties.Cost{{Cardinality: 12345}}, properties.FixedStatistics{Cardinality: 1000})

			wantCardinality := float64(len(test.inValues))
			if got.Cardinality != wantCardinality {
				t.Fatalf("cardinality = %v, want %v (the IN-list length) — a proven point probe must not scale off a bogus child cardinality",
					got.Cardinality, wantCardinality)
			}
		})
	}
}

// TestInJoinInUnion_AgreeOnCardinalityForSameNonUniqueChild pins that InJoin
// and InUnion — genuine memo alternatives for the SAME logical IN node
// (ImplementInJoinRule and ImplementInUnionRule both match it) — must not
// disagree about output cardinality merely because of which physical
// representation was chosen. Before the fix, InJoin reported Cardinality=3
// while InUnion correctly reported 3*childCardinality for the identical
// non-unique-index child — a 100x undercost at a 1000-row/3-element-IN
// reproduction that fed straight into GetBest's scalar tiebreak.
func TestInJoinInUnion_AgreeOnCardinalityForSameNonUniqueChild(t *testing.T) {
	t.Parallel()

	stats := properties.FixedStatistics{Cardinality: 1000}
	eq := scanCostRange(t, predicates.ComparisonEquals, int64(5))
	newInner := func() *RecordQueryIndexPlan {
		return NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{eq}, []string{"T"}, values.UnknownType, false).
			WithIndexMetadata([]string{"CATEGORY"}, []string{"ID"}, false /* non-unique */)
	}

	childCost := newInner().HintCost(nil, stats)
	if childCost.Cardinality <= 1 {
		t.Fatalf("test setup: child cardinality = %v, want a bucket (>1)", childCost.Cardinality)
	}

	inJoin := NewRecordQueryInJoinPlan(newInner(), "x", false, false)
	inJoin.SetInValues([]any{int64(1), int64(2), int64(3)})
	inJoinCost := inJoin.HintCost([]properties.Cost{childCost}, stats)

	inUnion := NewRecordQueryInUnionPlan(newInner(), []string{"x"}, nil, false)
	inUnion.SetInSources([][]any{{int64(1), int64(2), int64(3)}})
	inUnionCost := inUnion.HintCost([]properties.Cost{childCost}, stats)

	if inJoinCost.Cardinality != inUnionCost.Cardinality {
		t.Fatalf("InJoin and InUnion disagree on cardinality for the same logical IN over the same child: InJoin=%v, InUnion=%v",
			inJoinCost.Cardinality, inUnionCost.Cardinality)
	}
	wantCardinality := 3.0 * childCost.Cardinality
	if inJoinCost.Cardinality != wantCardinality {
		t.Fatalf("InJoin cardinality = %v, want %v", inJoinCost.Cardinality, wantCardinality)
	}
}
