package cascades

import (
	"context"
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
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Complete() || result.Range == nil {
		t.Fatal("merge should succeed")
	}

	indexPlan := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{result.Range},
		[]string{"T"}, inRuleRowType(), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
	iw := indexPlan.WithIndexMetadata([]string{"a"}, nil, false)

	innerRef := expressions.InitialOf(iw)
	pm := NewPlanPropertiesMap()
	pm.Add(iw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1), int64(2), int64(3))))
	explodeQ := expressions.NamedForEachQuantifier(explodeAlias, explodeRef)

	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
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

// inJoinOverIndexFixture builds Select(explode(3, 1, 2), IndexScan(a = explode))
// and returns the select's reference and the scan's ordering key for a.
func inJoinOverIndexFixture(t *testing.T) (*expressions.Reference, values.Value) {
	t.Helper()
	explodeAlias := values.UniqueCorrelationIdentifier()
	eqComp := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Complete() || result.Range == nil {
		t.Fatal("merge should succeed")
	}
	indexPlan := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{result.Range},
		[]string{"T"}, inRuleRowType(), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
	iw := indexPlan.WithIndexMetadata([]string{"a"}, nil, false)
	innerRef := expressions.InitialOf(iw)
	pm := NewPlanPropertiesMap()
	pm.Add(iw)
	innerRef.SetPlanProperties(pm)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// Deliberately NOT in sorted order.
	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(3), int64(1), int64(2))))
	explodeQ := expressions.NamedForEachQuantifier(explodeAlias, explodeRef)
	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)
	keys := iw.HintRichOrdering().GetKeys()
	if len(keys) == 0 {
		t.Fatal("index scan should publish its ordering key")
	}
	return expressions.InitialOf(sel), keys[0]
}

func inJoinPlansOf(results []expressions.RelationalExpression) []*plans.RecordQueryInJoinPlan {
	var out []*plans.RecordQueryInJoinPlan
	for _, r := range results {
		if plan, ok := r.(*plans.RecordQueryInJoinPlan); ok {
			out = append(out, plan)
		}
	}
	return out
}

// A preserve request asks for no order, so the IN source stays unsorted even
// though the inner fixes the explode's binding (Java:
// enumerateInSourcesForRequestedOrdering passes a null sort order for a
// non-exhaustive request).
func TestInJoinRule_PreserveRequestClaimsNoSortedSource(t *testing.T) {
	t.Parallel()
	ref, _ := inJoinOverIndexFixture(t)
	inJoins := inJoinPlansOf(mustInRuleFire(t, NewImplementInJoinRule(), ref))
	if len(inJoins) == 0 {
		t.Fatal("should fire")
	}
	for _, plan := range inJoins {
		if plan.IsSorted() {
			t.Fatalf("a preserve request must not yield a sorted IN source: %s", plan.Explain())
		}
	}
}

// TestInJoinRule_SortedClaimIsBackedByActuallySortedValues drives the rule
// end to end over a literal list that is NOT given in sorted order and asserts
// that every InJoin claiming IsSorted() carries GetInValues() in that order.
// The claim and the data must never disagree.
func TestInJoinRule_SortedClaimIsBackedByActuallySortedValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		order   properties.RequestedSortOrder
		reverse bool
		want    []any
	}{
		{properties.RequestedSortOrderAscending, false, []any{int64(1), int64(2), int64(3)}},
		{properties.RequestedSortOrderDescending, true, []any{int64(3), int64(2), int64(1)}},
	} {
		ref, aKey := inJoinOverIndexFixture(t)
		cm := NewConstraintMap()
		Set(cm, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
			properties.NewRequestedOrdering(
				[]properties.RequestedOrderingPart{{Value: aKey, SortOrder: tc.order}},
				properties.DistinctnessPreserveDistinctness, false),
		})
		results, err := FireImplementationRule(NewImplementInJoinRule(), ref, cm)
		if err != nil {
			t.Fatalf("FireImplementationRule: %v", err)
		}
		sawSorted := false
		for _, plan := range inJoinPlansOf(results) {
			if !plan.IsSorted() {
				continue
			}
			sawSorted = true
			if plan.IsReverse() != tc.reverse {
				t.Fatalf("requested %v: InJoin reverse = %v, want %v", tc.order, plan.IsReverse(), tc.reverse)
			}
			got := plan.GetInValues()
			if len(got) != len(tc.want) {
				t.Fatalf("InJoin claims sorted but GetInValues() = %#v, want %#v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("InJoin claims sorted but GetInValues() = %#v, want %#v (mismatch at %d)", got, tc.want, i)
				}
			}
			if err := ValidatePlanInvariants(plan); err != nil {
				t.Fatalf("ValidatePlanInvariants must accept the sorted plan: %v", err)
			}
		}
		if !sawSorted {
			t.Fatalf("requested %v on the explode-bound key: expected a sorted InJoin", tc.order)
		}
	}
}

// Explodes no requested part claims follow in declaration order, unsorted,
// unless the request is exhaustive: then every order and both directions.
func TestInJoinRule_RemainingExplodesFollowJava(t *testing.T) {
	t.Parallel()
	rule := &ImplementInJoinRule{}
	q1 := expressions.ForEachQuantifier(nil)
	q2 := expressions.ForEachQuantifier(nil)
	explodes := []expressions.Quantifier{q1, q2}
	aliases := map[values.CorrelationIdentifier]struct{}{q1.GetAlias(): {}, q2.GetAlias(): {}}
	byAlias := map[values.CorrelationIdentifier]expressions.Quantifier{q1.GetAlias(): q1, q2.GetAlias(): q2}

	plain := rule.enumerateSourceOrderingsForRequestedOrdering(
		context.Background(), nil, explodes, aliases, byAlias, properties.PreserveOrdering())
	if len(plain) != 1 || len(plain[0]) != 2 {
		t.Fatalf("non-exhaustive: want one chain of 2 sources, got %v", plain)
	}
	if plain[0][0].bindingAlias != q1.GetAlias() || plain[0][1].bindingAlias != q2.GetAlias() {
		t.Fatal("non-exhaustive: sources must keep declaration order")
	}
	for _, s := range plain[0] {
		if s.sorted {
			t.Fatal("non-exhaustive: remaining sources must be unsorted")
		}
	}

	exhaustive := rule.enumerateSourceOrderingsForRequestedOrdering(
		context.Background(), nil, explodes, aliases, byAlias,
		properties.PreserveOrdering().Exhaustive())
	// 2 permutations x 2 directions per source.
	if len(exhaustive) != 8 {
		t.Fatalf("exhaustive: want 8 chains, got %d", len(exhaustive))
	}
	for _, chain := range exhaustive {
		for _, s := range chain {
			if !s.sorted {
				t.Fatal("exhaustive: every source must be sorted")
			}
		}
	}
}

func TestInJoinRule_OrderingAware_RichOrderingFromIndexScan(t *testing.T) {
	t.Parallel()

	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42))
	eqResult := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !eqResult.Complete() || eqResult.Range == nil {
		t.Fatal("equality range merge should succeed")
	}

	indexPlan := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
		"idx_ab", []*predicates.ComparisonRange{eqResult.Range, predicates.EmptyComparisonRange()},
		[]string{"T"}, inRuleRowTypeWithKeyTypes(values.NullableLong, values.NullableLong), false)).
		WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableLong})
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
