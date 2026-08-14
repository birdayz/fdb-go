package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// dummyCollisionPred is any non-nil join predicate — the guard branches on
// presence, not content.
func dummyCollisionPred() predicates.QueryPredicate {
	leftType := &values.RecordType{Fields: []values.Field{{Name: "C", Ordinal: 0, FieldType: values.NullableString}}}
	rightType := &values.RecordType{Fields: []values.Field{{Name: "X", Ordinal: 0, FieldType: values.NullableString}}}
	left, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("ST"), leftType)
	if err != nil {
		panic(err)
	}
	right, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("X"), rightType)
	if err != nil {
		panic(err)
	}
	leftField, err := values.ResolveFieldOrdinals(left, []int{0})
	if err != nil {
		panic(err)
	}
	rightField, err := values.ResolveFieldOrdinals(right, []int{0})
	if err != nil {
		panic(err)
	}
	return predicates.NewComparisonPredicate(
		leftField,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: rightField,
		},
	)
}

// TestExistsGuardNarrowing pins existsInnerScopeCollidesOuter's CLEAN-PATH
// SKIP: an esq with a nil JoinPredicate whose plan the rename can
// re-identify (existsInnerSafeToRename) contributes NO collision even when
// its source alias equals an outer leg's — the FOD rebinds under esq.Alias
// and the self-contained plan has no cross-scope predicate. Everything the
// skip does NOT cover keeps the conservative name-model decline: a non-nil
// JoinPredicate (name-keyed refs the merged row could mis-serve on the
// unminted shapes) and a rename-declined plan (join/CTE inners route by
// source-alias name keys).
func TestExistsGuardNarrowing(t *testing.T) {
	t.Parallel()
	outer := map[string]struct{}{"ST": {}, "X": {}}
	scanST := logical.NewScan("ST", "ST")
	joinPlan := logical.NewJoin(logical.NewScan("ST", "ST"), logical.NewScan("OT", "OT"), logical.JoinInner, "")
	somePred := dummyCollisionPred()

	for _, tc := range []struct {
		name string
		esq  logical.ExistsSubquery
		want bool
	}{
		// The narrowing: clean-path colliding single-table inner → skipped.
		{"nilpred_safe_colliding_skips", logical.ExistsSubquery{Plan: scanST}, false},
		// A join-pred-carrying esq with a colliding SOURCE alias still
		// counts. (The minted fallback never produces this — its plan alias
		// is Q$N — but the guard must stay conservative for anything that
		// does.)
		{"pred_colliding_counts", logical.ExistsSubquery{Plan: scanST, JoinPredicate: somePred}, true},
		// A rename-declined plan (multi-source join) with a colliding leg
		// still counts even with a nil JoinPredicate.
		{"nilpred_unsafe_colliding_counts", logical.ExistsSubquery{Plan: joinPlan}, true},
		// No collision at all → false regardless.
		{"noncolliding_never_counts", logical.ExistsSubquery{Plan: logical.NewScan("OT", "OT"), JoinPredicate: somePred}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := existsInnerScopeCollidesOuter([]logical.ExistsSubquery{tc.esq}, outer)
			if got != tc.want {
				t.Fatalf("existsInnerScopeCollidesOuter(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
