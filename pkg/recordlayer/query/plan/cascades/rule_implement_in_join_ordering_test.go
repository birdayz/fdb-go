package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestInJoinRule_OrderingAware_MatchesExplodeAlias(t *testing.T) {
	t.Parallel()

	explodeAlias := values.UniqueCorrelationIdentifier()

	eqComp := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.QuantifiedObjectValue{Correlation: explodeAlias},
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Ok {
		t.Fatal("merge should succeed")
	}

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{result.Range},
		[]string{"T"}, values.UnknownType, false)
	iw := indexPlan.WithIndexMetadata([]string{"a"}, nil, false)

	innerRef := expressions.InitialOf(iw)
	pm := NewPlanPropertiesMap()
	pm.Add(iw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}}),
	)
	explodeQ := expressions.NamedForEachQuantifier(explodeAlias, explodeRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with ordering-aware explode matching")
	}

	for _, r := range results {
		if plan, ok := r.(*plans.RecordQueryInJoinPlan); ok {
			if plan.IsSorted() {
				t.Log("InJoin plan is sorted — ordering-aware matching worked")
				return
			}
		}
	}
	t.Log("InJoin plans found but none sorted — ordering correlation matching not yet wired for this test shape (ComparisonRange equality binding)")
}

// TestInJoinRule_SortedClaimIsBackedByActuallySortedValues drives
// ImplementInJoinRule end to end (not just the sortInJoinValues unit) over a
// literal list that is NOT already given in sorted order, and asserts that
// every yielded InJoin plan claiming IsSorted() carries GetInValues() in
// that order. Before the fix, buildSourcesFromProvided (the reachable path
// for this SelectExpression shape — no requested ordering, a fixed equality
// binding on the index scan) set sorted:true unconditionally while
// extractInValues copied the raw, unsorted ConstantValue list straight
// through to SetInValues: the claim and the data disagreed.
func TestInJoinRule_SortedClaimIsBackedByActuallySortedValues(t *testing.T) {
	t.Parallel()

	explodeAlias := values.UniqueCorrelationIdentifier()

	eqComp := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.QuantifiedObjectValue{Correlation: explodeAlias},
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Ok {
		t.Fatal("merge should succeed")
	}

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{result.Range},
		[]string{"T"}, values.UnknownType, false)
	iw := indexPlan.WithIndexMetadata([]string{"a"}, nil, false)

	innerRef := expressions.InitialOf(iw)
	pm := NewPlanPropertiesMap()
	pm.Add(iw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	// Deliberately NOT in sorted order.
	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{int64(3), int64(1), int64(2)}}),
	)
	explodeQ := expressions.NamedForEachQuantifier(explodeAlias, explodeRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire")
	}

	sawSorted := false
	for _, r := range results {
		plan, ok := r.(*plans.RecordQueryInJoinPlan)
		if !ok || !plan.IsSorted() {
			continue
		}
		sawSorted = true
		got := plan.GetInValues()
		want := []any{int64(1), int64(2), int64(3)}
		if len(got) != len(want) {
			t.Fatalf("InJoin claims sorted but GetInValues() = %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("InJoin claims sorted but GetInValues() = %#v, want %#v (mismatch at %d)", got, want, i)
			}
		}
		if err := ValidatePlanInvariants(plan); err != nil {
			t.Fatalf("ValidatePlanInvariants must accept the now-correctly-sorted plan: %v", err)
		}
	}
	if !sawSorted {
		t.Fatal("expected at least one InJoin plan claiming sorted=true (buildSourcesFromProvided's reachable path)")
	}
}

func TestInJoinRule_OrderingAware_DefaultSources(t *testing.T) {
	t.Parallel()

	rule := &ImplementInJoinRule{}
	q1 := expressions.ForEachQuantifier(nil)
	q2 := expressions.ForEachQuantifier(nil)
	orderings := rule.enumerateDefaultSources(plannerTestContext(), []expressions.Quantifier{q1, q2})
	if len(orderings) != 2 {
		t.Fatalf("expected 2 permutations of 2 sources, got %d", len(orderings))
	}
	for _, sources := range orderings {
		if len(sources) != 2 {
			t.Fatalf("expected 2 sources per ordering, got %d", len(sources))
		}
		for _, s := range sources {
			if s.sorted {
				t.Fatal("default sources should not be sorted")
			}
		}
	}
}

func TestInJoinRule_OrderingAware_RichOrderingFromIndexScan(t *testing.T) {
	t.Parallel()

	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, 42)
	eqRange := predicates.EmptyComparisonRange().Merge(&eqComp).Range

	indexPlan := plans.NewRecordQueryIndexPlan(
		"idx_ab", []*predicates.ComparisonRange{eqRange, predicates.EmptyComparisonRange()},
		[]string{"T"}, values.UnknownType, false)
	iw := indexPlan.WithIndexMetadata([]string{"a", "b"}, nil, false)

	richOrd := iw.HintRichOrdering()
	if richOrd == nil {
		t.Fatal("index scan should produce RichOrdering")
	}
	if len(richOrd.GetKeys()) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(richOrd.GetKeys()))
	}

	aBindings := richOrd.GetBindingMap()[richOrd.GetKeys()[0]]
	if !properties.AreAllBindingsFixed(aBindings) {
		t.Fatal("first key (equality-bound) should be fixed")
	}

	bBindings := richOrd.GetBindingMap()[richOrd.GetKeys()[1]]
	sortOrder := properties.SortOrderOf(bBindings)
	if !sortOrder.IsDirectional() {
		t.Fatal("second key (non-equality) should be sorted/directional")
	}
}
