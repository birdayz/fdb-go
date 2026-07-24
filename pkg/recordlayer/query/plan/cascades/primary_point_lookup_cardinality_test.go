package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputeCardinalities_PrimaryPointLookupBounded pins RFC-189 B1
// (finding 10 M3-followup): computeCardinalities' RecordQueryScanPlan arm always
// returned UnknownMaxCardinality, so a full-PK-equality primary scan (a point
// lookup, max 1 — which Java's CardinalitiesProperty bounds) was reported
// unknown, and the M3 whole-plan-cardinality outer guard over-abstained for a
// bounded-primary-vs-unbounded comparison. The scan arm now bounds a full-PK
// equality bind at 1, cross-checking len(comps) == len(PK) so a prefix bind
// (which can match many rows) stays unknown — the safe direction.
func TestComputeCardinalities_PrimaryPointLookupBounded(t *testing.T) {
	t.Parallel()

	// Two PK columns → a composite primary key. The values' contents are
	// irrelevant to the length-coverage check.
	pk2 := []values.Value{
		&values.ConstantValue{Value: int64(0), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(0), Typ: values.NullableLong},
	}

	t.Run("full_pk_equality_is_point_lookup", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		if !wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("full-PK-equality primary scan must be a bounded point lookup (max 1)")
		}
	})

	t.Run("prefix_bind_stays_unknown", func(t *testing.T) {
		t.Parallel()
		// One equality comparison against a 2-column PK → a prefix scan that can
		// match many rows. scanProvableMaxCard owns the stamped-PK arity check,
		// so the cardinality property cannot mistake the bound prefix for the
		// complete key.
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))}).
			WithPrimaryKey(pk2)
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("a composite-PK prefix bind must stay unknown (matches many rows)")
		}
	})

	t.Run("no_pk_values_stays_unknown", func(t *testing.T) {
		t.Parallel()
		// All-equality but PK metadata absent → full coverage unprovable → unknown.
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("without PK values, full-PK coverage is unprovable → unknown")
		}
	})

	t.Run("bare_full_scan_is_unknown", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("bare full scan must be unknown")
		}
	})

	// In a multi-record-type store the primary point scan is wrapped in a
	// RecordQueryTypeFilterPlan (primary_scan_match_candidate) whose child edge is
	// a SNAPSHOT quantifier — its Reference carries no populated property map, so
	// the transparent-wrapper arm's cardinalitiesForRef reported unknown and the
	// point lookup lost its proven AtMostOne through the type filter (the booked
	// M3-followup). The arm now falls back to the concrete embedded child, so the
	// bound propagates.
	t.Run("type_filter_over_point_lookup_propagates", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		tf := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
		if !wholePlanMaxCardinalityKnown(tf) {
			t.Fatal("a full-PK-equality point lookup under a TypeFilter must stay bounded (max 1)")
		}
	})

	t.Run("type_filter_over_prefix_stays_unknown", func(t *testing.T) {
		t.Parallel()
		// A prefix bind under a type filter can still match many rows — the
		// fallback must not manufacture a bound the child doesn't prove.
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))}).
			WithPrimaryKey(pk2)
		tf := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
		if wholePlanMaxCardinalityKnown(tf) {
			t.Fatal("a composite-PK prefix bind under a TypeFilter must stay unknown")
		}
	})

	// The concrete-child fallback must NEVER descend into a multi-member child
	// reference with no winner: plan.GetChildren() → planFromQuantifier PANICS on
	// a reference with multiple plan-typed final members. When the transparent
	// wrapper's child ref is not a proven singleton, the arm returns unknown
	// instead of reading (and crashing on) an ambiguous group.
	t.Run("multi_member_child_ref_does_not_panic", func(t *testing.T) {
		t.Parallel()
		bound := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		other := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		// A child reference holding TWO plan-typed final members and no winner.
		childRef := expressions.FinalOf(bound)
		if !childRef.InsertFinal(other) {
			t.Fatal("failed to build a two-member child reference")
		}
		tf := plans.NewRecordQueryTypeFilterPlanFromQuantifier([]string{"T"},
			expressions.ForEachQuantifier(childRef))
		// Must not panic; the ambiguous group yields unknown.
		if wholePlanMaxCardinalityKnown(tf) {
			t.Fatal("a multi-member no-winner child ref must yield unknown (not a tightened bound)")
		}
	})

	// The data-access path can present the composite as a scanPlanExpression
	// adapter that reports NO child quantifier (GetQuantifiers()==nil) even though
	// the wrapped plan has a child. The fallback must resolve the child off the
	// PLAN, not the wrapper, so a full-PK point lookup keeps its bound.
	t.Run("scan_plan_adapter_len0_propagates", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		tf := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
		adapter := &scanPlanExpression{plan: tf}
		if len(adapter.GetQuantifiers()) != 0 {
			t.Fatal("precondition: the adapter must expose no child quantifier")
		}
		if !wholePlanMaxCardinalityKnown(adapter) {
			t.Fatal("a scanPlanExpression(TypeFilter(pointScan)) must propagate AtMostOne through the plan's child")
		}
	})

	// A scanPlanExpression adapter exposes no wrapper quantifier, so the populated
	// property map must be consulted off the PLAN's child, not the wrapper's. A
	// mixed bounded/unbounded group legitimately weakens to unknown; the fallback
	// must trust that, NOT follow the bounded winner and report AtMostOne (which
	// would wrongly enable the cost model's point-lookup criterion).
	t.Run("scan_adapter_populated_mixed_group_stays_unknown", func(t *testing.T) {
		t.Parallel()
		bounded := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		unbounded := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		childRef := expressions.FinalOf(bounded)
		if !childRef.InsertFinal(unbounded) {
			t.Fatal("failed to build the mixed child reference")
		}
		pm := NewPlanPropertiesMap()
		pm.Add(bounded)
		pm.Add(unbounded)
		childRef.SetPlanProperties(pm)
		childRef.SetWinner(bounded) // winner is the bounded leg
		tf := plans.NewRecordQueryTypeFilterPlanFromQuantifier([]string{"T"},
			expressions.ForEachQuantifier(childRef))
		adapter := &scanPlanExpression{plan: tf}
		if wholePlanMaxCardinalityKnown(adapter) {
			t.Fatal("a populated mixed bounded/unbounded group must stay unknown, not follow the bounded winner")
		}
	})

	// A child ref with ONE distinct plan-typed FINAL member plus an exploratory
	// (non-final) member is unambiguous — planFromQuantifier cannot panic — so the
	// bound must still propagate (the guard keys on distinct FINAL plans, not on
	// AllMembers()).
	t.Run("one_final_plan_plus_exploratory_resolves", func(t *testing.T) {
		t.Parallel()
		bound := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		childRef := expressions.FinalOf(bound)
		// A non-final exploratory member alongside the single final plan.
		childRef.Insert(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
		tf := plans.NewRecordQueryTypeFilterPlanFromQuantifier([]string{"T"},
			expressions.ForEachQuantifier(childRef))
		if !wholePlanMaxCardinalityKnown(tf) {
			t.Fatal("one final plan + exploratory members must still resolve the point-lookup bound")
		}
	})

	// planFromQuantifier resolves a lone EXPLORATORY member (empty final set, one
	// member plan — the MemoizeExpression InitialOf(plan) shape) via
	// firstPlanFromMembers. childPlanIfUnambiguous must match that: a
	// FinalMembers()-only scan would drop the bound.
	t.Run("exploratory_only_singleton_resolves", func(t *testing.T) {
		t.Parallel()
		bound := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		childRef := expressions.InitialOf(bound) // Members=[bound], FinalMembers=[]
		if len(childRef.FinalMembers()) != 0 {
			t.Fatal("precondition: InitialOf must leave the final set empty")
		}
		tf := plans.NewRecordQueryTypeFilterPlanFromQuantifier([]string{"T"},
			expressions.ForEachQuantifier(childRef))
		if !wholePlanMaxCardinalityKnown(tf) {
			t.Fatal("a lone exploratory member plan must resolve the point-lookup bound (Members fallback)")
		}
	})
}
