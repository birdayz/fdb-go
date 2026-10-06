package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestImplementInUnionRule_PreserveYieldsNothing pins RFC-257 WS-F 4.3 item 4:
// with no ordering requested the rule builds no in-union, as Java's
// ImplementInUnionRule skips a PRESERVE request; the IN is an in-join there.
// Go's former unordered arm, a merge with no comparison key, is gone.
func TestImplementInUnionRule_PreserveYieldsNothing(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1), int64(2), int64(3))))
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	if results := mustInRuleFire(t, NewImplementInUnionRule(), outerRef); len(results) != 0 {
		t.Fatalf("no requested ordering yielded %d expression(s), want none", len(results))
	}

	// WS-F 4.3 item 6: the size comes from the planner configuration, and a
	// rule call without a planner context is a harness defect, not size 0.
	rule := NewImplementInUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sel)
	if len(bindings) == 0 {
		t.Fatal("fixture select does not bind the in-union matcher")
	}
	call := &ImplementationRuleCall{Bindings: bindings[0], Reference: outerRef}
	rule.OnMatch(call)
	if call.Err() == nil {
		t.Fatal("a rule call without a planner context did not fail")
	}
}

func TestImplementInUnionRuleSeparatesFixedAndDirectionalRichOrderings(t *testing.T) {
	t.Parallel()

	explodeAlias := values.UniqueCorrelationIdentifier()
	equality := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	})
	if !equality.Complete() || equality.Range == nil {
		t.Fatal("construct exact IN-binding equality range")
	}

	// Keyed by x, so the merge key (a, x) identifies the records it merges.
	index := func(ranges []*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
		return mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
			"IDX_A", ranges, []string{"T"}, inRuleRowType(), false)).
			WithKeyComponentTypes([]values.Type{values.NotNullLong}).
			WithIndexMetadata([]string{"a"}, []string{"x"}, false).
			WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
			WithDistinctRecordsSignal(false)
	}
	unbound := index(nil)
	boundIndex := index([]*predicates.ComparisonRange{equality.Range})
	// Make the fixed-bound member locally more expensive. Before the rich-
	// ordering roll-up, the mixed partition's cost pick therefore chooses the
	// unbounded directional scan even while deriving comparison keys from the
	// fixed member (or vice versa, depending on insertion order).
	bound := plans.RecordQueryPlan(boundIndex)
	for range 2 {
		// Real conjuncts: TRUE counts no residual conjunct, as in Java's
		// NormalizedResidualPredicateProperty.
		bound = mustInRuleConstruct(plans.NewRecordQueryPredicatesFilterPlan(
			bound, []predicates.QueryPredicate{rungPredicate("K")}))
	}
	boundExpr := bound.(expressions.RelationalExpression)
	if !PlanningCostModelLess(unbound, boundExpr) {
		t.Fatal("fixture must make the unbounded member cheaper than the fixed-bound member")
	}

	// Force the two members into the same RAW partition, reproducing the
	// reduced-ordering collision independently of future changes to the plain
	// Ordering property. Their full RichOrdering values remain production-
	// derived and differ exactly at A: sorted versus fixed-to-explode.
	unboundProps := computeWrapperProperties(unbound)
	boundProps := computeWrapperProperties(boundExpr.(physicalPlanExpression))
	unboundRich := unboundProps[properties.PropRichOrdering].(*properties.RichOrdering)
	// The wrapper chain is intentionally expensive, but a standalone wrapper's
	// private child references are not planner-property-populated in this direct
	// fixture. Stamp the production-derived fixed ordering of the bounded leaf;
	// the rule still has to select/pin the concrete wrapper spine carrying it.
	boundRich := boundIndex.HintRichOrdering()
	boundProps[properties.PropRichOrdering] = boundRich
	unboundBindings, unboundOK := bindingsForStructuralKey(unboundRich, unboundRich.GetKeys()[0])
	boundBindings, boundOK := bindingsForStructuralKey(boundRich, boundRich.GetKeys()[0])
	if len(unboundRich.GetKeys()) != 2 || len(boundRich.GetKeys()) != 2 ||
		!unboundOK || !boundOK || properties.AreAllBindingsFixed(unboundBindings) ||
		!properties.AreAllBindingsFixed(boundBindings) {
		t.Fatal("fixture must differ only in directional-versus-fixed rich binding for A")
	}
	sharedPlain := properties.Ordering{IsKnown: true, Keys: unboundRich.GetKeys()}
	unboundProps[properties.PropOrdering] = sharedPlain
	boundProps[properties.PropOrdering] = sharedPlain

	innerRef := expressions.InitialOf(unbound)
	innerRef.Insert(boundExpr)
	propertyMap := NewPlanPropertiesMap()
	propertyMap.Set(unbound, unboundProps)
	propertyMap.Set(boundExpr, boundProps)
	innerRef.SetPlanProperties(propertyMap)
	if raw := ToPlanPartitions(innerRef); len(raw) != 1 {
		t.Fatalf("fixture raw partitions = %d, want one reduced-ordering collision", len(raw))
	}
	if rich := RollUpPlanPartitions(ToPlanPartitions(innerRef), properties.PropRichOrdering); len(rich) != 2 {
		t.Fatalf("rich-ordering roll-up = %d partitions, want fixed and directional", len(rich))
	}

	innerQ := expressions.ForEachQuantifier(innerRef)
	explodeQ := expressions.NamedForEachQuantifier(
		explodeAlias,
		expressions.InitialOf(inRuleExplode(
			inRuleArray(values.NotNullLong, int64(1), int64(2), int64(3)))),
	)
	selectExpr := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)
	selectRef := expressions.InitialOf(selectExpr)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     boundRich.GetKeys()[0],
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	constraints := NewConstraintMap()
	Set(constraints, selectRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{requested})

	yielded, err := FireImplementationRule(NewImplementInUnionRule(), selectRef, constraints)
	if err != nil {
		t.Fatalf("fire InUnion rule: %v", err)
	}
	for _, expression := range yielded {
		inUnion, ok := expression.(*plans.RecordQueryInUnionPlan)
		if !ok {
			continue
		}
		var hasBoundIndex func(plans.RecordQueryPlan) bool
		hasBoundIndex = func(plan plans.RecordQueryPlan) bool {
			if indexPlan, isIndex := plan.(*plans.RecordQueryIndexPlan); isIndex {
				return len(indexPlan.GetScanComparisons()) == 1
			}
			for _, child := range plan.GetChildren() {
				if hasBoundIndex(child) {
					return true
				}
			}
			return false
		}
		if hasBoundIndex(inUnion.GetInner()) {
			return
		}
	}
	t.Fatal("rule did not yield an ordered InUnion over the fixed-bound index partition")
}

// TestImplementInUnionRule_RecordTypeHorizon pins RFC-257 WS-F 4.3 item 2. An
// index scan's primary-key suffix that lies past the record-type coordinate of
// the primary key still orders the scan, but the in-union rule builds no merge
// for a request naming it: Java's data access stops at that coordinate and its
// reference holds no such leg. A request on the IN column builds the merge, the
// marked key its free comparison-key suffix, and a primary key with no
// record-type coordinate (a record-layer type) keeps the merge ordered by it.
func TestImplementInUnionRule_RecordTypeHorizon(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		recordTypePrefix bool
	}{
		{"primary key after the record type", true},
		{"primary key without a record type", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explodeAlias := values.UniqueCorrelationIdentifier()
			equality := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: inRuleQOV(explodeAlias, values.NotNullLong),
			})
			if !equality.Complete() || equality.Range == nil {
				t.Fatal("construct exact IN-binding equality range")
			}
			base := inRuleQOV(values.UniqueCorrelationIdentifier(), inRuleRowType())
			pk := []values.Value{inRuleField(base, 2)}
			if tc.recordTypePrefix {
				pk = append([]values.Value{values.NewRecordTypeValue(base)}, pk...)
			}
			index := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
				"IDX_A", []*predicates.ComparisonRange{equality.Range}, []string{"T"}, inRuleRowType(), false)).
				WithKeyComponentTypes([]values.Type{values.NotNullLong}).
				WithIndexMetadata([]string{"a"}, []string{"x"}, false).
				WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}).
				WithDistinctRecordsSignal(false).
				WithCommonPrimaryKey(pk)

			rich := index.HintRichOrdering()
			if len(rich.GetKeys()) != 2 {
				t.Fatalf("index ordering keys = %v, want (a, x)", rich.GetKeys())
			}
			a, x := rich.GetKeys()[0], rich.GetKeys()[1]
			if rich.IsPastRecordTypeHorizon(a) || rich.IsPastRecordTypeHorizon(x) != tc.recordTypePrefix {
				t.Fatalf("marks: a=%v x=%v, want x marked exactly when the primary key begins with the record type",
					rich.IsPastRecordTypeHorizon(a), rich.IsPastRecordTypeHorizon(x))
			}
			request := func(v values.Value) *properties.RequestedOrdering {
				return properties.NewRequestedOrdering(
					[]properties.RequestedOrderingPart{{Value: v, SortOrder: properties.RequestedSortOrderAscending}},
					properties.DistinctnessPreserveDistinctness, false)
			}
			if !rich.Satisfies(request(x)) {
				t.Fatal("the primary-key suffix must still satisfy ORDER BY x (the read extension)")
			}

			innerRef := expressions.InitialOf(index)
			pm := NewPlanPropertiesMap()
			pm.Add(index)
			innerRef.SetPlanProperties(pm)
			innerQ := expressions.ForEachQuantifier(innerRef)
			explodeQ := expressions.NamedForEachQuantifier(explodeAlias, expressions.InitialOf(
				inRuleExplode(inRuleArray(values.NotNullLong, int64(1), int64(2)))))
			selectRef := expressions.InitialOf(inRuleSelect(
				inRuleFlowedObject(innerQ), []expressions.Quantifier{explodeQ, innerQ}, nil))
			inUnions := func(requested *properties.RequestedOrdering) int {
				constraints := NewConstraintMap()
				Set(constraints, selectRef, RequestedOrderingConstraintKey,
					[]*properties.RequestedOrdering{requested})
				yielded, err := FireImplementationRule(NewImplementInUnionRule(), selectRef, constraints)
				if err != nil {
					t.Fatalf("fire InUnion rule: %v", err)
				}
				n := 0
				for _, expression := range yielded {
					if _, ok := expression.(*plans.RecordQueryInUnionPlan); ok {
						n++
					}
				}
				return n
			}
			if got := inUnions(request(x)); (got > 0) == tc.recordTypePrefix {
				t.Errorf("ORDER BY x yielded %d in-union(s); want one exactly when x is not past the record type", got)
			}
			if got := inUnions(request(a)); got == 0 {
				t.Error("ORDER BY a yielded no in-union; the marked key is a free comparison-key suffix")
			}
		})
	}
}

func TestImplementInUnionRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	innerRef := expressions.InitialOf(inRuleLogicalScan())
	innerAlias := values.NamedCorrelationIdentifier("STRICT")
	innerQ := expressions.NamedForEachStrictSingleQuantifier(innerAlias, innerRef)
	explodeQ := expressions.ForEachQuantifier(expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1), int64(2)))))
	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	results := mustInRuleFire(t,
		NewImplementInUnionRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		t.Fatalf("strict-single IN shape yielded %d InUnion implementation(s), want zero", len(results))
	}
}

func TestImplementInUnionRule_SkipsSingleQuantifier(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	sel := inRuleSelect(
		inRuleFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with single quantifier, got %d", len(results))
	}
}

func TestImplementInUnionRule_SkipsWithPredicates(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1))))
	eq := expressions.ForEachQuantifier(explodeRef)

	pred := []predicates.QueryPredicate{predicates.NewComparisonPredicate(
		inRuleField(inRuleFlowedObject(q), 2),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)}
	sel := inRuleSelect(
		inRuleFlowedObject(q),
		[]expressions.Quantifier{eq, q},
		pred,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with predicates, got %d", len(results))
	}
}

func TestAdjustBindingsForInUnion_PromotesExplodeAlias(t *testing.T) {
	t.Parallel()
	explodeAlias := values.NamedCorrelationIdentifier("explode_1")
	explodeAliases := map[values.CorrelationIdentifier]struct{}{explodeAlias: {}}

	eqComp := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Complete() || result.Range == nil {
		t.Fatal("merge should succeed")
	}
	eqRange := result.Range

	a := inRuleField(
		inRuleQOV(values.NamedCorrelationIdentifier("row_1"), inRuleRowType()), 0)
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.FixedBinding(eqRange)},
		},
		[]values.Value{a},
		properties.NotDistinct())

	req := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)

	adjusted := adjustBindingsForInUnion(ordering, explodeAliases, req)
	if adjusted == nil {
		t.Fatal("adjustment should succeed")
	}

	bindings := adjusted.GetBindingMap()[a]
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if !bindings[0].IsSorted() {
		t.Fatal("fixed binding referencing explode alias should be promoted to sorted")
	}
	if bindings[0].GetSortOrder() != properties.ProvidedSortOrderAscending {
		t.Fatal("promoted binding should match requested ascending direction")
	}
}

func TestAdjustBindingsForInUnion_KeepsNonExplodeFixed(t *testing.T) {
	t.Parallel()
	explodeAliases := map[values.CorrelationIdentifier]struct{}{}

	a := inRuleField(
		inRuleQOV(values.NamedCorrelationIdentifier("row_2"), inRuleRowType()), 0)
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.FixedBinding(nil)},
		},
		[]values.Value{a},
		properties.NotDistinct())

	req := properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false)
	adjusted := adjustBindingsForInUnion(ordering, explodeAliases, req)
	if adjusted == nil {
		t.Fatal("adjustment should succeed")
	}

	bindings := adjusted.GetBindingMap()[a]
	if len(bindings) != 1 || !bindings[0].IsFixed() {
		t.Fatal("non-explode fixed binding should remain fixed")
	}
}

func TestAdjustBindingsForInUnionPreservesFixedPrefixIndependence(t *testing.T) {
	t.Parallel()

	explodeAlias := values.NamedCorrelationIdentifier("explode_prefix")
	equality := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	})
	if !equality.Complete() || equality.Range == nil {
		t.Fatal("construct exact IN-binding equality range")
	}

	row := inRuleQOV(values.NamedCorrelationIdentifier("row_prefix"), inRuleRowType())
	customerID := inRuleField(row, 1)
	id := inRuleField(row, 0)
	provided := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			customerID: {properties.FixedBinding(equality.Range)},
			id:         {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{customerID, id},
		properties.NotDistinct(),
	)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     id,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	adjusted := adjustBindingsForInUnion(
		provided,
		map[values.CorrelationIdentifier]struct{}{explodeAlias: {}},
		requested,
	)
	if adjusted == nil {
		t.Fatal("adjustment should succeed")
	}
	bindings := adjusted.GetBindingMap()[customerID]
	if len(bindings) != 1 || !bindings[0].IsChoose() {
		t.Fatalf("IN-correlated unrequested prefix bindings = %v, want one CHOOSE", bindings)
	}

	keys := adjusted.EnumerateSatisfyingComparisonKeyValues(requested)
	if len(keys) == 0 || len(keys[0]) != 2 {
		t.Fatalf("comparison-key enumerations = %v, want ID-first two-key ordering", keys)
	}
	if !values.SameOrderingColumn(keys[0][0], id) {
		t.Fatalf("first comparison key = %s, want requested ID first",
			values.ExplainValue(keys[0][0]))
	}
}

func TestAdjustBindingsForInUnion_NilOrdering(t *testing.T) {
	t.Parallel()
	result := adjustBindingsForInUnion(nil, nil, properties.PreserveOrdering())
	if result != nil {
		t.Fatal("nil ordering should return nil")
	}
}
