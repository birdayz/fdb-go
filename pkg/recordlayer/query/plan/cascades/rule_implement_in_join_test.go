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

func mustInRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct IN-rule fixture: " + err.Error())
	}
	return value
}

func mustInRuleFire(
	t testing.TB,
	rule ImplementationRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	got, err := FireImplementationRule(rule, ref)
	if err != nil {
		t.Fatalf("FireImplementationRule() unexpected error: %v", err)
	}
	return got
}

func inRuleRowType() *values.RecordType {
	return inRuleRowTypeWithKeyTypes(values.NotNullLong, values.NotNullLong)
}

func inRuleRowTypeWithKeyTypes(aType, bType values.Type) *values.RecordType {
	return values.NewRecordType("in_rule_row", false, []values.Field{
		{Name: "a", FieldType: aType, Ordinal: 0},
		{Name: "b", FieldType: bType, Ordinal: 1},
		{Name: "x", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "computed", FieldType: values.NotNullLong, Ordinal: 3},
	})
}

func inRuleScanPlan() *plans.RecordQueryScanPlan {
	return mustInRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, inRuleRowType(), false))
}

func inRuleLogicalScan() *expressions.FullUnorderedScanExpression {
	return mustInRuleConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, inRuleRowType()))
}

func inRuleArray(elementType values.Type, elements ...any) *values.ConstantValue {
	return &values.ConstantValue{
		Value: elements,
		Typ:   values.NewArrayType(false, elementType),
	}
}

func inRuleExplode(collection values.Value) *expressions.ExplodeExpression {
	return mustInRuleConstruct(expressions.NewExplodeExpression(collection))
}

func inRuleFlowedObject(q expressions.Quantifier) values.QuantifiedObjectValue {
	return mustInRuleConstruct(q.RequireFlowedObjectValue())
}

func inRuleQOV(
	alias values.CorrelationIdentifier,
	flowedType values.Type,
) values.QuantifiedObjectValue {
	return mustInRuleConstruct(values.NewQuantifiedObjectValue(alias, flowedType))
}

func inRuleField(root values.Value, ordinal int) values.Value {
	return mustInRuleConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func inRuleSelect(
	result values.Value,
	quantifiers []expressions.Quantifier,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return mustInRuleConstruct(expressions.NewSelectExpression(result, quantifiers, preds))
}

func TestImplementInJoinRule_MatchesSelectExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementInJoinRule()
	scanRef := expressions.InitialOf(inRuleLogicalScan())
	q := expressions.ForEachQuantifier(scanRef)
	sel := inRuleSelect(
		inRuleFlowedObject(q),
		[]expressions.Quantifier{q},
		nil,
	)
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sel)
	if len(bindings) == 0 {
		t.Fatal("should match SelectExpression")
	}
}

func TestImplementInJoinRule_SkipsWithPredicates(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1), int64(2), int64(3))))
	eq := expressions.ForEachQuantifier(explodeRef)

	pred := predicates.NewComparisonPredicate(
		inRuleField(inRuleFlowedObject(q), 2),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	sel := inRuleSelect(
		inRuleFlowedObject(q),
		[]expressions.Quantifier{eq, q},
		[]predicates.QueryPredicate{pred},
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with predicates, got %d", len(results))
	}
}

func TestImplementInJoinRule_SkipsSingleQuantifier(t *testing.T) {
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
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with single quantifier, got %d", len(results))
	}
}

func TestImplementInJoinRule_FiresWithExplodeAndInner(t *testing.T) {
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
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with explode + inner quantifier")
	}

	found := false
	for _, r := range results {
		if inJoin, ok := r.(*plans.RecordQueryInJoinPlan); ok {
			found = true
			if inJoin.GetBindingAlias() != explodeQ.GetAlias() {
				t.Fatalf("InJoin binding alias = %v, want exact explode alias %v",
					inJoin.GetBindingAlias(), explodeQ.GetAlias())
			}
			if inJoin.GetBindingAlias() == values.NamedCorrelationIdentifier(explodeQ.GetAlias().Name()) {
				t.Fatal("implementation round-tripped a Unique explode alias through its display spelling")
			}
			break
		}
	}
	if !found {
		t.Fatal("should yield *RecordQueryInJoinPlan")
	}
}

func TestImplementInJoinRule_StrictSingleFailsClosed(t *testing.T) {
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
		NewImplementInJoinRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		t.Fatalf("strict-single IN shape yielded %d InJoin implementation(s), want zero", len(results))
	}
}

func TestImplementInJoinRule_SkipsWhenResultNotQOV(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1))))
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := inRuleSelect(
		inRuleField(inRuleFlowedObject(innerQ), 3),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire when result is not QOV for inner, got %d", len(results))
	}
}

func TestIsExplodeExpression_True(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1))))
	if getExplodeExpression(ref) == nil {
		t.Fatal("should detect ExplodeExpression")
	}
}

func TestIsExplodeExpression_False(t *testing.T) {
	t.Parallel()
	ref := expressions.InitialOf(inRuleLogicalScan())
	if getExplodeExpression(ref) != nil {
		t.Fatal("should not detect scan as ExplodeExpression")
	}
}

func TestIsSupportedExplodeValue(t *testing.T) {
	t.Parallel()
	quantifiedCollection := inRuleQOV(
		values.UniqueCorrelationIdentifier(),
		values.NewArrayType(false, values.NotNullLong),
	)
	row := inRuleQOV(values.UniqueCorrelationIdentifier(), inRuleRowType())
	tests := []struct {
		name string
		val  values.Value
		ok   bool
	}{
		{"constant", inRuleArray(values.NotNullLong, int64(1), int64(2)), true},
		{"quantified", quantifiedCollection, true},
		{"field", inRuleField(row, 2), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := isSupportedExplodeValue(tc.val); got != tc.ok {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestImplementInJoinRule_MultipleExplodes(t *testing.T) {
	t.Parallel()
	scan := inRuleScanPlan()
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef1 := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullLong, int64(1), int64(2))))
	explodeRef2 := expressions.InitialOf(inRuleExplode(
		inRuleArray(values.NotNullString, "a", "b")))
	eq1 := expressions.ForEachQuantifier(explodeRef1)
	eq2 := expressions.ForEachQuantifier(explodeRef2)

	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{eq1, eq2, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with multiple explodes + inner quantifier")
	}
}

// TestClassifyInSourceKind_ConstantValueCollection verifies that an
// ExplodeExpression wrapping a ConstantValue classifies as InSourceValues.
func TestClassifyInSourceKind_ConstantValueCollection(t *testing.T) {
	t.Parallel()

	collection := inRuleArray(values.NotNullLong, int64(1), int64(2), int64(3))
	explode := inRuleExplode(collection)
	ref := expressions.InitialOf(explode)
	q := expressions.ForEachQuantifier(ref)

	got := classifyInSourceKind(q)
	if got != plans.InSourceValues {
		t.Errorf("classifyInSourceKind(ConstantValue collection) = %v, want InSourceValues (%v)", got, plans.InSourceValues)
	}
}

func TestImplementInJoinRule_RecordValuedExplodeIsRelationSource(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullInt},
		{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullInt)},
	})
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(1)}},
		values.RecordConstructorField{Name: "ARR", Value: values.NewArrayConstructorValue(
			values.NotNullInt, []values.Value{&values.ConstantValue{Typ: values.NotNullInt, Value: int32(101)}})},
	)
	collection := values.NewArrayConstructorValue(rowType, []values.Value{row})
	originalElementType := collection.ElementType
	originalElement := collection.Elements[0]
	if isSupportedExplodeValue(collection) {
		t.Fatal("record-valued constant Explode classified as a scalar IN source")
	}
	if collection.ElementType != originalElementType || collection.Elements[0] != originalElement ||
		!row.Type().Equals(rowType) {
		t.Fatal("record-valued classification mutated its source collection")
	}

	innerPlan := inRuleScanPlan()
	innerRef := expressions.InitialOf(innerPlan)
	pm := NewPlanPropertiesMap()
	pm.Add(innerPlan)
	innerRef.SetPlanProperties(pm)
	innerQ := expressions.ForEachQuantifier(innerRef)
	recordExplodeQ := expressions.ForEachQuantifier(expressions.InitialOf(inRuleExplode(collection)))
	selectExpr := inRuleSelect(inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{recordExplodeQ, innerQ}, nil)
	if got := mustInRuleFire(t, NewImplementInJoinRule(), expressions.InitialOf(selectExpr)); len(got) != 0 {
		t.Fatalf("record-valued relation source yielded %d InJoin alternatives, want none", len(got))
	}

	scalar := inRuleArray(values.NotNullLong, int64(1), int64(2))
	if !isSupportedExplodeValue(scalar) {
		t.Fatal("scalar constant array stopped classifying as an IN source")
	}
	scalarExplodeQ := expressions.ForEachQuantifier(expressions.InitialOf(inRuleExplode(scalar)))
	scalarSelect := inRuleSelect(inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{scalarExplodeQ, innerQ}, nil)
	if got := mustInRuleFire(t, NewImplementInJoinRule(), expressions.InitialOf(scalarSelect)); len(got) == 0 {
		t.Fatal("scalar constant array no longer yields an InJoin")
	}
}

// TestClassifyInSourceKind_QuantifiedObjectValueCollection verifies that
// an ExplodeExpression wrapping a QuantifiedObjectValue classifies as
// InSourceParameter.
func TestClassifyInSourceKind_QuantifiedObjectValueCollection(t *testing.T) {
	t.Parallel()

	collection := inRuleQOV(
		values.NamedCorrelationIdentifier("param"),
		values.NewArrayType(false, values.NotNullLong),
	)
	explode := inRuleExplode(collection)
	ref := expressions.InitialOf(explode)
	q := expressions.ForEachQuantifier(ref)

	got := classifyInSourceKind(q)
	if got != plans.InSourceParameter {
		t.Errorf("classifyInSourceKind(QuantifiedObjectValue) = %v, want InSourceParameter (%v)", got, plans.InSourceParameter)
	}
}

// TestClassifyInSourceKind_NilRef verifies that when the quantifier has
// no Reference (nil ranges-over), the function defaults to InSourceValues.
func TestClassifyInSourceKind_NilRef(t *testing.T) {
	t.Parallel()

	q := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("noref"), nil)

	got := classifyInSourceKind(q)
	if got != plans.InSourceValues {
		t.Errorf("classifyInSourceKind(nil ref) = %v, want InSourceValues (%v)", got, plans.InSourceValues)
	}
}

// TestClassifyInSourceKind_NonExplodeReference verifies that when the
// Reference holds a non-Explode expression, the default InSourceValues
// is returned.
func TestClassifyInSourceKind_NonExplodeReference(t *testing.T) {
	t.Parallel()

	scan := inRuleLogicalScan()
	ref := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(ref)

	got := classifyInSourceKind(q)
	if got != plans.InSourceValues {
		t.Errorf("classifyInSourceKind(non-explode ref) = %v, want InSourceValues (%v)", got, plans.InSourceValues)
	}
}

// TestNewExplodeExpression_NilCollectionFailsClosed verifies that the RFC-232
// constructor rejects the malformed shape before classifyInSourceKind can see
// an ExplodeExpression with no exact collection layout.
func TestNewExplodeExpression_NilCollectionFailsClosed(t *testing.T) {
	t.Parallel()

	explode, err := expressions.NewExplodeExpression(nil)
	if err == nil || explode != nil {
		t.Fatalf("NewExplodeExpression(nil) = (%v, %v), want (nil, error)", explode, err)
	}
}

func TestImplementInJoinRule_WithIndexScanInner(t *testing.T) {
	t.Parallel()
	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	eqResult := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !eqResult.Ok || eqResult.Range == nil {
		t.Fatal("equality range merge should succeed")
	}
	indexPlan := mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
		"idx_a", []*predicates.ComparisonRange{eqResult.Range},
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
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := mustInRuleFire(t, NewImplementInJoinRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with index scan inner")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryInJoinPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield *RecordQueryInJoinPlan with index scan inner")
	}
}

// A MIXED PARTITION MUST NOT ANSWER THE `sorted` QUESTION, and this is the
// InJoin twin of the IN-union rule's separation test — the same defect, on the
// rule next door, found the same way.
//
// The raw partition key carries only the REDUCED Ordering. An equality-bound
// index access and a residual filter over a full scan therefore collide in it:
// the index's bound prefix is dropped from the plain key sequence, leaving the
// primary-key suffix, which is exactly what the scan advertises. Their RICH
// orderings differ materially — A fixed to the IN binding versus A absent — and
// the rich binding is the ONLY thing that makes an explode alias a SORTED
// in-source.
//
// So reading the first member's rich ordering out of a mixed partition answers
// with whichever member the memo happened to list first. That is not merely a
// lost optimization in the unlucky direction: the InJoin's inner edge is the
// whole group, so a `sorted` claim derived from the bound member can be
// extracted over the unbounded one — a plan that promises an order it does not
// execute.
//
// The fixture below is deliberately built so the FIRST member is the one
// WITHOUT the fixed binding. Without the roll-up the rule reads that member,
// finds no fixed binding correlated to the explode alias, and emits an
// unsorted InJoin — which is exactly the shape that shipped.
func TestImplementInJoinRule_SortedClaimComesFromAHomogeneousPartition(t *testing.T) {
	t.Parallel()

	explodeAlias := values.UniqueCorrelationIdentifier()
	equality := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: inRuleQOV(explodeAlias, values.NotNullLong),
	})
	if !equality.Ok || equality.Range == nil {
		t.Fatal("fixture: construct the exact IN-binding equality range")
	}
	index := func(ranges []*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
		return mustInRuleConstruct(plans.NewRecordQueryIndexPlan(
			"IDX_A", ranges, []string{"T"}, inRuleRowType(), false)).
			WithKeyComponentTypes([]values.Type{values.NotNullLong}).
			WithIndexMetadata([]string{"a"}, nil, false)
	}
	unbound := index(nil)
	bound := index([]*predicates.ComparisonRange{equality.Range})

	unboundProps := computeWrapperProperties(unbound)
	boundProps := computeWrapperProperties(bound)
	unboundRich := unboundProps[properties.PropRichOrdering].(*properties.RichOrdering)
	boundRich := boundProps[properties.PropRichOrdering].(*properties.RichOrdering)
	unboundBindings, unboundOK := bindingsForStructuralKey(unboundRich, unboundRich.GetKeys()[0])
	boundBindings, boundOK := bindingsForStructuralKey(boundRich, boundRich.GetKeys()[0])
	if !unboundOK || !boundOK ||
		properties.AreAllBindingsFixed(unboundBindings) ||
		!properties.AreAllBindingsFixed(boundBindings) {
		t.Fatal("fixture must differ exactly in directional-versus-FIXED rich binding for A — " +
			"the fixed binding is the only thing that can make an explode alias sorted")
	}

	// Force the reduced-ordering collision explicitly rather than relying on the
	// plain Ordering property continuing to drop the bound prefix. The rich
	// values stay production-derived.
	sharedPlain := properties.Ordering{IsKnown: true, Keys: unboundRich.GetKeys()}
	unboundProps[properties.PropOrdering] = sharedPlain
	boundProps[properties.PropOrdering] = sharedPlain

	// UNBOUND FIRST: the member the rule would read without the roll-up.
	innerRef := expressions.InitialOf(unbound)
	innerRef.Insert(bound)
	pm := NewPlanPropertiesMap()
	pm.Set(unbound, unboundProps)
	pm.Set(bound, boundProps)
	innerRef.SetPlanProperties(pm)

	if raw := ToPlanPartitions(innerRef); len(raw) != 1 {
		t.Fatalf("fixture raw partitions = %d, want ONE reduced-ordering collision — "+
			"with the two members already separated there is nothing to read wrongly", len(raw))
	}
	if rich := RollUpPlanPartitions(ToPlanPartitions(innerRef), properties.PropRichOrdering); len(rich) != 2 {
		t.Fatalf("rich-ordering roll-up = %d partition(s), want 2 (fixed and directional)", len(rich))
	}

	innerQ := expressions.ForEachQuantifier(innerRef)
	explodeQ := expressions.NamedForEachQuantifier(
		explodeAlias,
		expressions.InitialOf(inRuleExplode(
			inRuleArray(values.NotNullLong, int64(3), int64(1), int64(2)))),
	)
	sel := inRuleSelect(
		inRuleFlowedObject(innerQ),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	results := mustInRuleFire(t, NewImplementInJoinRule(), expressions.InitialOf(sel))
	sawInJoin, sawSorted := false, false
	for _, r := range results {
		inJoin, ok := r.(*plans.RecordQueryInJoinPlan)
		if !ok {
			continue
		}
		sawInJoin = true
		if inJoin.IsSorted() {
			sawSorted = true
		}
	}
	if !sawInJoin {
		t.Fatal("the rule yielded no InJoin at all, so nothing below is being tested")
	}
	if !sawSorted {
		t.Fatal("no yielded InJoin claims a SORTED in-source.\n" +
			"  The fixed-bound member is in the group and its binding is correlated to\n" +
			"  the explode alias, so the fixed partition must produce a sorted InJoin.\n" +
			"  Reading the first member's rich ordering out of the mixed partition\n" +
			"  finds the DIRECTIONAL member instead, and the sorted alternative is\n" +
			"  never enumerated — the InJoin then iterates the IN-list in SQL text\n" +
			"  order while the index it probes is ordered.")
	}
}
