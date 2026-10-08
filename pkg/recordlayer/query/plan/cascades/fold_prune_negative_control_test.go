package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestFoldPrune_NegativeControl is WS-E design 5.4(j)'s negative control: the
// fold of a conjunction that an access path could probe, ranked directly by
// both cost models. The REWRITING comparator keeps the fold ([FALSE], one
// conjunct, against the probe's conjunct plus the annulled one), and PLANNING's
// comparator over the implemented plans prefers the unfolded PROBE (a bounded
// scan against a full scan under a FALSE filter). So it is the REWRITING prune
// that decides these folds (the yamsql rows of fold_prune_regime.yaml and
// fold_prune_unique_regime.yaml answer as the fold does), and without it
// PLANNING would decide the other way: the probe would evaluate the annulled
// conjunct on the probed rows. The groups are the design's: primary key,
// primary-key IN, UNIQUE index and a union leg.
func TestFoldPrune_NegativeControl(t *testing.T) {
	t.Parallel()

	annulled := rungPredicate("K") // stands for COALESCE(1 / 0, 5) IS NULL
	fold := predicates.NewConstantPredicate(predicates.TriFalse)
	probe := rungPredicate("ID")

	// REWRITING: the original select keeps the probe's conjunct and the
	// annulled one; the fold keeps FALSE alone.
	scanRef := expressions.InitialOf(rungFullScan("T"))
	original := makeRewritingRungSelect(scanRef, []predicates.QueryPredicate{probe, annulled})
	folded := makeRewritingRungSelect(scanRef, []predicates.QueryPredicate{fold})
	assertStrictPlanningPreference(t, RewritingCostModelLess, folded, original)

	// PLANNING over the implemented members of each group.
	eqFive := func() []*predicates.ComparisonRange {
		return []*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(5))}
	}
	inBinding := func() []*predicates.ComparisonRange {
		return []*predicates.ComparisonRange{rungEqualityRange(t, mustRungConstruct(
			values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("in_q"), values.NotNullLong)))}
	}
	foldedScan := func() plans.RecordQueryPlan { return rungFilter(rungScan("T"), fold) }
	otherLeg := func() plans.RecordQueryPlan {
		return rungScan("T").WithScanComparisons([]*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(1))})
	}
	for _, g := range []struct {
		name            string
		probed, foldedP plans.RecordQueryPlan
	}{
		{
			"primary key",
			rungFilter(rungScan("T").WithScanComparisons(eqFive()), annulled),
			foldedScan(),
		},
		{
			"primary-key IN",
			rungInJoin(rungFilter(rungScan("T").WithScanComparisons(inBinding()), annulled), "in_q"),
			foldedScan(),
		},
		{
			"union leg",
			rungUnion(rungFilter(rungScan("T").WithScanComparisons(eqFive()), annulled), otherLeg()),
			rungUnion(foldedScan(), otherLeg()),
		},
	} {
		t.Run(g.name, func(t *testing.T) {
			assertStrictPlanningPreference(t, PlanningCostModelLess, g.probed, g.foldedP)
		})
	}

	// UNIQUE index: a fully equality-bound unique index bounds the probe at one
	// row, so the fetch over it beats the full scan on the provable-cardinality
	// rung (#2). The design leaves the NON-unique rows out: there the target's
	// PLANNING picks the fold too (measured), as Go's does, the fetch losing to
	// the primary scan (#10).
	t.Run("unique index", func(t *testing.T) {
		unique := costModelIndex(t, "T_UN", eqFive()).
			WithKeyComponentTypes([]values.Type{values.NullableLong}).
			WithIndexMetadata([]string{"A"}, nil, true)
		ctx := &indexTestPlanContext{candidates: []MatchCandidate{
			newKnownDistinctValueIndexCandidate("T_UN", []string{"T"}, []string{"A"}, nil, costModelRowType(), true, nil),
		}}
		probed := rungFilter(mustCostConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
			unique, nil, costModelRowType(), plans.FetchIndexRecordsPrimaryKey)), annulled)
		// The filter reads its child's bound off the child Reference's plan
		// properties, which PLANNING computes after implementing the group; a
		// client-built tree has none until they are computed.
		computeRefPlanProperties(probed.GetQuantifiers()[0].GetRangesOver())
		if !wholePlanMaxCardinalityKnown(probed) {
			t.Fatal("premise broken: the unique probe's whole-plan max cardinality is unknown")
		}
		assertStrictPlanningPreference(t, NewPlanningCostModelLessWithContext(nil, ctx),
			probed, rungFilter(costModelScan(t, "T"), fold))
	})
}
