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

func descendingInUnionRowType(name string) *values.RecordType {
	return values.NewRecordType(name, false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "K", FieldType: values.NullableLong, Ordinal: 1},
	})
}

func descendingInUnionQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustConstruct(t, qov, err)
}

func descendingInUnionField(
	t testing.TB,
	root values.Value,
	ordinal int,
) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, field, err)
}

func descendingInUnionExplode(t testing.TB) *expressions.ExplodeExpression {
	t.Helper()
	collection := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
		values.LiteralValue(int64(2)),
	})
	explode, err := expressions.NewExplodeExpression(collection)
	return mustConstruct(t, explode, err)
}

func descendingInUnionFullScan(
	t testing.TB,
	recordTypes []string,
	flowedType values.Type,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression(recordTypes, flowedType)
	return mustConstruct(t, scan, err)
}

func descendingInUnionSelect(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
) *expressions.SelectExpression {
	t.Helper()
	selectExpression, err := expressions.NewSelectExpression(result, quantifiers, nil)
	return mustConstruct(t, selectExpression, err)
}

func descendingInUnionScan(
	t testing.TB,
	recordTypes []string,
	flowedType values.Type,
	reverse bool,
) *plans.RecordQueryScanPlan {
	t.Helper()
	scan, err := plans.NewRecordQueryScanPlan(recordTypes, flowedType, reverse)
	return mustConstruct(t, scan, err)
}

func descendingInUnionFlowedObject(
	t testing.TB,
	quantifier expressions.Quantifier,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := quantifier.RequireFlowedObjectValue()
	return mustConstruct(t, qov, err)
}

func descendingInUnionLogicalUnion(
	t testing.TB,
	quantifiers []expressions.Quantifier,
) *expressions.LogicalUnionExpression {
	t.Helper()
	union, err := expressions.NewLogicalUnionExpression(quantifiers)
	return mustConstruct(t, union, err)
}

func descendingInUnionLogicalDistinct(
	t testing.TB,
	inner expressions.Quantifier,
) *expressions.LogicalDistinctExpression {
	t.Helper()
	distinct, err := expressions.NewLogicalDistinctExpression(inner)
	return mustConstruct(t, distinct, err)
}

func descendingInUnionKeyTypes(size int) []values.Type {
	types := make([]values.Type, size)
	for i := range types {
		types[i] = values.NullableLong
	}
	return types
}

func fireDescendingConstraintOnlyRule(
	t testing.TB,
	rule ImplementationRule,
	subject expressions.RelationalExpression,
	ref *expressions.Reference,
	constraints *ConstraintMap,
) {
	t.Helper()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), subject)
	if len(bindings) != 1 {
		t.Fatalf("%T matcher produced %d bindings, want one", rule, len(bindings))
	}
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ref,
		Constraints:    constraints,
		constraintOnly: true,
	}
	rule.OnMatch(call)
	if err := call.Err(); err != nil {
		t.Fatalf("%T.OnMatch() unexpected error: %v", rule, err)
	}
	call.applyPendingConstraints()
}

func requireDescendingPushedOrdering(
	t testing.TB,
	constraints *ConstraintMap,
	ref *expressions.Reference,
) *properties.RequestedOrdering {
	t.Helper()
	pushed, ok := Get(constraints, ref, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("requested ordering was not pushed to the child reference")
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed %d requested orderings, want one", len(pushed))
	}
	return pushed[0]
}

// The three dimensions below are the ones that let `WHERE pk IN (...) ORDER BY
// pk DESC` plan an in-memory sort over a forward IN-join while its ascending
// mirror planned a sort-free IN-union. Each was invisible to the corpus,
// because each on its own only removes a CANDIDATE — the query still returned
// the right rows through the slower plan.

// TestPrimaryScanMatchCandidateReportsKeyOrder pins that a primary-scan match
// reports its key order at all. It reported none, so it could never satisfy a
// requested ordering, so it never got a reverse variant, so no descending
// access path over a primary key existed anywhere in the memo. Java's
// PrimaryScanMatchCandidate inherits this from ValueIndexLikeMatchCandidate.
func TestPrimaryScanMatchCandidateReportsKeyOrder(t *testing.T) {
	t.Parallel()

	idAlias := values.UniqueCorrelationIdentifier()
	kAlias := values.UniqueCorrelationIdentifier()
	rowType := descendingInUnionRowType("TBL")
	candidate := NewPrimaryScanMatchCandidate(
		nil,
		[]values.CorrelationIdentifier{idAlias, kAlias},
		[]string{"TBL"},
		[]string{"TBL"},
		[]string{"ID", "K"},
		true,
		rowType,
	).WithKeyComponentTypes(descendingInUnionKeyTypes(2))

	eqComparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	equality := predicates.EmptyComparisonRange().Merge(&eqComparison)
	if !equality.Ok {
		t.Fatal("fixture: equality range did not merge")
	}
	matchInfo := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{idAlias: equality.Range},
		nil, nil, nil, nil, nil, nil, nil,
	)

	for _, tc := range []struct {
		name    string
		reverse bool
		want    MatchedSortOrder
	}{
		{name: "forward", reverse: false, want: MatchedSortOrderAscending},
		{name: "reverse", reverse: true, want: MatchedSortOrderDescending},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts := candidate.ComputeMatchedOrderingParts(
				matchInfo,
				[]values.CorrelationIdentifier{idAlias, kAlias},
				tc.reverse,
			)
			if len(parts) != 2 {
				t.Fatalf("got %d ordering parts, want one per primary-key column", len(parts))
			}
			for i, wantCol := range []string{"ID", "K"} {
				field, ok := values.AsFieldValue(parts[i].GetValue())
				if !ok {
					t.Fatalf("part %d value = %T, want exact FieldValue", i, parts[i].GetValue())
				}
				if got := field.DisplayName(); got != wantCol {
					t.Fatalf("part %d orders by %q, want %q", i, got, wantCol)
				}
				if parts[i].GetMatchedSortOrder() != tc.want {
					t.Fatalf("part %d sort order = %v, want %v",
						i, parts[i].GetMatchedSortOrder(), tc.want)
				}
			}
			// The bound equality must ride along: SatisfiesRequestedOrdering
			// skips equality-bound columns when it resolves the scan
			// direction, and it can only do that if the range is carried.
			if !parts[0].GetComparisonRange().IsEquality() {
				t.Fatal("the bound primary-key column lost its equality range")
			}
			if !parts[1].GetComparisonRange().IsEmpty() {
				t.Fatal("the unbound primary-key column invented a range")
			}
		})
	}
}

// TestPrimaryScanMatchCandidateStopsAtUnknownParameter pins the prefix rule: a
// sort parameter that is not a key column ends the reported order rather than
// being skipped over. Continuing past it would claim an order over
// non-contiguous key positions, which the records do not have.
func TestPrimaryScanMatchCandidateStopsAtUnknownParameter(t *testing.T) {
	t.Parallel()

	idAlias := values.UniqueCorrelationIdentifier()
	kAlias := values.UniqueCorrelationIdentifier()
	stranger := values.UniqueCorrelationIdentifier()
	rowType := descendingInUnionRowType("TBL")
	candidate := NewPrimaryScanMatchCandidate(
		nil,
		[]values.CorrelationIdentifier{idAlias, kAlias},
		[]string{"TBL"},
		[]string{"TBL"},
		[]string{"ID", "K"},
		true,
		rowType,
	).WithKeyComponentTypes(descendingInUnionKeyTypes(2))

	parts := candidate.ComputeMatchedOrderingParts(
		nil,
		[]values.CorrelationIdentifier{idAlias, stranger, kAlias},
		false,
	)
	if len(parts) != 1 {
		t.Fatalf("got %d ordering parts, want the prefix up to the unknown parameter", len(parts))
	}
	field, ok := values.AsFieldValue(parts[0].GetValue())
	if !ok {
		t.Fatalf("reported prefix value = %T, want exact FieldValue", parts[0].GetValue())
	}
	if got := field.DisplayName(); got != "ID" {
		t.Fatalf("reported prefix orders by %q, want ID", got)
	}
}

// TestSelectRulePushesOrderingWhenResultIsOneChildsRow pins that a SELECT whose
// result value IS one child's row pushes its parent's ordering request down to
// that child. An IN-like SELECT has this shape (explode quantifiers plus the
// inner, result = the inner's row) and used to downgrade the request to
// Preserve, because Go's sort keys carry no correlation to attribute them by.
//
// The downgrade was not a no-op: the Preserve joined the concrete request in
// the base reference's constraint set, every data access there then reported
// that EITHER direction satisfied the set, and "either" deliberately yields a
// forward access only.
func TestSelectRulePushesOrderingWhenResultIsOneChildsRow(t *testing.T) {
	t.Parallel()

	explodeRef := expressions.InitialOf(descendingInUnionExplode(t))
	explodeQ := expressions.ForEachQuantifier(explodeRef)
	rowType := descendingInUnionRowType("TBL")
	innerRef := expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"TBL"}, rowType),
	)
	innerQ := expressions.ForEachQuantifier(innerRef)
	innerRow := descendingInUnionFlowedObject(t, innerQ)
	sel := descendingInUnionSelect(
		t,
		innerRow,
		[]expressions.Quantifier{explodeQ, innerQ},
	)
	selRef := expressions.InitialOf(sel)

	// The request names the SELECT.s own output row, which is the reserved-current
	// carrier: the sort above rebased it there before pushing. The
	// result-is-child-row arm must carry it through to the passthrough child.
	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     descendingInUnionCurrentField(t, innerRow, 0),
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(constraints, selRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering})

	fireDescendingConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughSelectRule(),
		sel,
		selRef,
		constraints,
	)

	pushed := requireDescendingPushedOrdering(t, constraints, innerRef)
	if pushed.IsPreserve() {
		t.Fatal("the inner child was asked to preserve order, not to produce the requested one")
	}
	parts := pushed.GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed %d ordering parts, want one", len(parts))
	}
	if parts[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed sort order = %v, want descending", parts[0].SortOrder)
	}
}

// TestSelectRuleDeclinesUnownedOrderingForCompositeResult is the other side of
// the same guard. RFC-232 makes every admitted field's owner explicit, so the
// exact equivalent pins that a key owned by one leg is not claimed by a
// same-shaped sibling leg.
func TestSelectRuleDeclinesUnownedOrderingForCompositeResult(t *testing.T) {
	t.Parallel()

	leftType := descendingInUnionRowType("L")
	leftRef := expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"L"}, leftType),
	)
	leftQ := expressions.ForEachQuantifier(leftRef)
	leftRow := descendingInUnionFlowedObject(t, leftQ)
	rightType := descendingInUnionRowType("R")
	rightRef := expressions.InitialOf(
		descendingInUnionFullScan(t, []string{"R"}, rightType),
	)
	rightQ := expressions.ForEachQuantifier(rightRef)
	rightRow := descendingInUnionFlowedObject(t, rightQ)
	leftID := descendingInUnionField(t, leftRow, 0)
	rightID := descendingInUnionField(t, rightRow, 0)
	sel := descendingInUnionSelect(
		t,
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "L_ID",
				Value: leftID,
			},
			values.RecordConstructorField{
				Name:  "R_ID",
				Value: rightID,
			},
		),
		[]expressions.Quantifier{leftQ, rightQ},
	)
	selRef := expressions.InitialOf(sel)

	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     leftID,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(constraints, selRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering})

	fireDescendingConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughSelectRule(),
		sel,
		selRef,
		constraints,
	)

	if pushed, ok := Get(constraints, rightRef, RequestedOrderingConstraintKey); ok {
		for _, o := range pushed {
			if !o.IsPreserve() {
				t.Fatalf("a same-named column was claimed for the right leg: %#v", o.GetParts())
			}
		}
	}
}

// mixedDirectionInLikeSelect builds the shape that produces a MIXED-direction
// comparison key: an IN-like SELECT over `Scan(TBL, [ID = $explode])`, whose
// provided ordering is (ID equality-bound, K ascending). Asking it for
// (ID <idDir>, K ASC) leaves ID taking the request's direction while K keeps the
// scan's ascending one — so the two comparison parts disagree exactly when
// idDir is descending.
func mixedDirectionInLikeSelect(
	t *testing.T,
	scanReverse bool,
	idDir properties.RequestedSortOrder,
	kDir properties.RequestedSortOrder,
) (*expressions.SelectExpression, *expressions.Reference, *ConstraintMap) {
	t.Helper()

	explodeRef := expressions.InitialOf(descendingInUnionExplode(t))
	explodeQ := expressions.ForEachQuantifier(explodeRef)
	explodeValue := descendingInUnionFlowedObject(t, explodeQ)

	// The scan's ID column is equality-bound to the explode binding — that
	// correlation is what lets the rule promote ID to a directional key.
	comparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: explodeValue,
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("fixture: equality range did not merge")
	}
	rowType := descendingInUnionRowType("TBL")
	innerAlias := values.UniqueCorrelationIdentifier()
	innerRow := descendingInUnionQOV(t, innerAlias, rowType)
	idValue := descendingInUnionField(t, innerRow, 0)
	kValue := descendingInUnionField(t, innerRow, 1)
	scan := descendingInUnionScan(t, []string{"TBL"}, rowType, scanReverse).
		WithPrimaryKey([]values.Value{idValue, kValue}).
		WithScanComparisons([]*predicates.ComparisonRange{merged.Range}).
		WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableLong})

	innerRef := expressions.FinalOf(scan)
	// The rule reads plan PARTITIONS, which the planner populates on the
	// reference before implementation rules run.
	computeRefPlanProperties(innerRef)
	innerQ := expressions.NamedForEachQuantifier(innerAlias, innerRef)
	sel := descendingInUnionSelect(
		t,
		innerRow,
		[]expressions.Quantifier{explodeQ, innerQ},
	)
	selRef := expressions.InitialOf(sel)

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: idValue, SortOrder: idDir},
			{Value: kValue, SortOrder: kDir},
		},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	cm := NewConstraintMap()
	Set(cm, selRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{requested})
	return sel, selRef, cm
}

func fireInUnionRule(
	t *testing.T,
	_ *expressions.SelectExpression,
	selRef *expressions.Reference,
	cm *ConstraintMap,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireImplementationRule(NewImplementInUnionRule(), selRef, cm)
	if err != nil {
		t.Fatalf("FireImplementationRule() unexpected error: %v", err)
	}
	return yielded
}

// TestInUnionRuleRefusesMixedDirectionMerge drives ImplementInUnionRule itself,
// not the helper it calls. The comparison key it would otherwise build is
// (ID DESC, K ASC) with a FORWARD merge — a merge front comparing a descending
// key the wrong way round, which returns rows in an order the plan then
// advertises as ascending. The rule must yield nothing and leave the query to
// the in-memory sort.
//
// The ascending-K control is what keeps this honest: the SAME fixture with
// (ID DESC, K DESC) must still yield an in-union, so a failure to yield in the
// mixed case is the gate and not a broken fixture.
func TestInUnionRuleRefusesMixedDirectionMerge(t *testing.T) {
	t.Parallel()

	t.Run("mixed directions yield no merge", func(t *testing.T) {
		t.Parallel()
		sel, selRef, cm := mixedDirectionInLikeSelect(t, false,
			properties.RequestedSortOrderDescending,
			properties.RequestedSortOrderAscending)
		for _, yielded := range fireInUnionRule(t, sel, selRef, cm) {
			if inUnion, isInUnion := yielded.(*plans.RecordQueryInUnionPlan); isInUnion {
				t.Fatalf("rule built a mixed-direction merge: %s (reverse=%v)",
					inUnion.Explain(), inUnion.IsReverse())
			}
		}
	})

	t.Run("uniform ascending still yields a merge", func(t *testing.T) {
		t.Parallel()
		sel, selRef, cm := mixedDirectionInLikeSelect(t, false,
			properties.RequestedSortOrderAscending,
			properties.RequestedSortOrderAscending)
		found := false
		for _, yielded := range fireInUnionRule(t, sel, selRef, cm) {
			if _, isInUnion := yielded.(*plans.RecordQueryInUnionPlan); isInUnion {
				found = true
			}
		}
		if !found {
			t.Fatal("control: the fixture never reaches the yield, so the mixed-direction case proves nothing")
		}
	})

	t.Run("uniform descending still yields a merge", func(t *testing.T) {
		t.Parallel()
		sel, selRef, cm := mixedDirectionInLikeSelect(t, true,
			properties.RequestedSortOrderDescending,
			properties.RequestedSortOrderDescending)
		found := false
		for _, yielded := range fireInUnionRule(t, sel, selRef, cm) {
			if _, isInUnion := yielded.(*plans.RecordQueryInUnionPlan); isInUnion {
				found = true
			}
		}
		if !found {
			t.Fatal("control: the fixture never reaches the yield, so the mixed-direction case proves nothing")
		}
	})
}

// mixedDirectionDistinctUnion builds Distinct(Union(leg1, leg2)) whose merged
// ordering is (ID equality-bound, K ascending). The legs bind ID to DIFFERENT
// values, so the equality binding is not common to both and survives into the
// merged ordering as a key the request can give a direction to.
func mixedDirectionDistinctUnion(
	t *testing.T,
	idDir properties.RequestedSortOrder,
	kDir properties.RequestedSortOrder,
) (*expressions.Reference, *ConstraintMap) {
	t.Helper()

	rowType := descendingInUnionRowType("T")
	keyRoot := descendingInUnionQOV(
		t, values.UniqueCorrelationIdentifier(), rowType)
	idValue := descendingInUnionField(t, keyRoot, 0)
	kValue := descendingInUnionField(t, keyRoot, 1)
	leg := func(literal int64) *expressions.Reference {
		comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("fixture: equality range did not merge")
		}
		scan := descendingInUnionScan(t, []string{"T"}, rowType, false).
			WithPrimaryKey([]values.Value{idValue, kValue}).
			WithScanComparisons([]*predicates.ComparisonRange{merged.Range}).
			WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableLong})
		ref := expressions.FinalOf(scan)
		computeRefPlanProperties(ref)
		return ref
	}

	union := descendingInUnionLogicalUnion(t, []expressions.Quantifier{
		expressions.ForEachQuantifier(leg(1)),
		expressions.ForEachQuantifier(leg(2)),
	})
	distinct := descendingInUnionLogicalDistinct(t,
		expressions.ForEachQuantifier(expressions.InitialOf(union)),
	)
	distinctRef := expressions.InitialOf(distinct)

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: idValue, SortOrder: idDir},
			{Value: kValue, SortOrder: kDir},
		},
		properties.DistinctnessPreserveDistinctness,
		false,
	)
	cm := NewConstraintMap()
	Set(cm, distinctRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{requested})
	return distinctRef, cm
}

// TestDistinctUnionMergedOrderingCarriesNoEqualityBoundKeys pins WHY the
// sibling gate in ImplementDistinctUnionRule cannot be driven the same way, so
// that "it declines nothing" stays a measured fact rather than an assumption.
//
// A mixed-direction key needs a NON-directional key for the request to give a
// direction to. The union merge has none to give: an equality binding common to
// every leg is stripped as redundant, and one that differs between legs does
// not survive the merge either. Every key that reaches the gate therefore
// carries a leg's own uniform scan direction. If this ever stops holding, the
// fence in that rule becomes live and needs the rule-level test its in-union
// twin has.
func TestDistinctUnionMergedOrderingCarriesNoEqualityBoundKeys(t *testing.T) {
	t.Parallel()

	rowType := descendingInUnionRowType("T")
	keyRoot := descendingInUnionQOV(
		t, values.UniqueCorrelationIdentifier(), rowType)
	idValue := descendingInUnionField(t, keyRoot, 0)
	kValue := descendingInUnionField(t, keyRoot, 1)
	legOrdering := func(literal int64) *properties.RichOrdering {
		comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("fixture: equality range did not merge")
		}
		scan := descendingInUnionScan(t, []string{"T"}, rowType, false).
			WithPrimaryKey([]values.Value{idValue, kValue}).
			WithScanComparisons([]*predicates.ComparisonRange{merged.Range}).
			WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableLong})
		return computeWrapperRichOrdering(scan)
	}

	// Precondition: each leg on its own DOES carry the equality-bound key.
	legs := []*properties.RichOrdering{legOrdering(1), legOrdering(2)}
	if _, bound := legs[0].GetEqualityBoundValues()[idValue]; !bound {
		t.Fatal("fixture: the leg scan does not report ID as equality-bound")
	}

	legs = removeCommonEqualityBoundParts(legs)
	merged := properties.MergeOrderings(properties.CreateUnionOrdering(legs[0]), legs[1])
	for _, key := range merged.GetKeys() {
		if !properties.SortOrderOf(merged.GetBindingMap()[key]).IsDirectional() {
			t.Fatalf("the union merge now carries a non-directional key (%s) — "+
				"the mixed-direction gate in ImplementDistinctUnionRule is reachable "+
				"and needs a rule-level test",
				values.ExplainValue(key))
		}
	}
}

// descendingInUnionCurrentField is descendingInUnionField on the RESERVED-CURRENT
// carrier for the same row. A requested ordering names the row of the group it
// is attached to, and that row is always the reserved-current one — Java rebases
// every part onto Quantifier.current() before pushing (see
// requestedOrderingAtInnerCurrent).
func descendingInUnionCurrentField(
	t testing.TB,
	root values.Value,
	ordinal int,
) values.Value {
	t.Helper()
	rootQOV, ok := values.AsQuantifiedObjectValue(root)
	if !ok {
		t.Fatalf("root %T is not a quantified object value", root)
	}
	carrier, err := values.CurrentPhaseCarrierForEdge(rootQOV)
	if err != nil {
		t.Fatalf("current carrier: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(carrier, []int{ordinal})
	return mustConstruct(t, field, err)
}
