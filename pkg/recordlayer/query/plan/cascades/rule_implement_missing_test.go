package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustImplementMissingConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct missing-implementation fixture: " + err.Error())
	}
	return value
}

func implementMissingRowType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "PRICE", FieldType: values.NotNullLong},
	})
}

func implementMissingScan() *expressions.FullUnorderedScanExpression {
	return mustImplementMissingConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, implementMissingRowType()))
}

func implementMissingPrice(quantifier expressions.Quantifier) values.Value {
	root := mustImplementMissingConstruct(quantifier.RequireFlowedObjectValue())
	return mustImplementMissingConstruct(values.ResolveFieldOrdinals(root, []int{0}))
}

func implementProjectionReuseChild(t testing.TB) *plans.RecordQueryProjectionPlan {
	t.Helper()
	rowType := values.NewRecordType("ProjectionReuseInput", false, []values.Field{
		{Name: "SOURCE_A", FieldType: values.NotNullLong},
		{Name: "SOURCE_B", FieldType: values.NotNullLong},
	})
	scan := mustImplementMissingConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, rowType, false))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	root := mustImplementMissingConstruct(scanQ.RequireFlowedObjectValue())
	projected := []values.Value{
		mustImplementMissingConstruct(values.ResolveFieldOrdinals(root, []int{0})),
		mustImplementMissingConstruct(values.ResolveFieldOrdinals(root, []int{1})),
	}
	return mustImplementMissingConstruct(
		plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
			projected, nil, nil, []string{"A", "B"}, scanQ))
}

func implementProjectionReuseOuter(
	t testing.TB,
	child plans.RecordQueryPlan,
	ordinals []int,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
) *expressions.LogicalProjectionExpression {
	t.Helper()
	childQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	root := mustImplementMissingConstruct(childQ.RequireFlowedObjectValue())
	projected := make([]values.Value, len(ordinals))
	for i, ordinal := range ordinals {
		projected[i] = mustImplementMissingConstruct(values.ResolveFieldOrdinals(
			root, []int{ordinal}))
	}
	return mustImplementMissingConstruct(
		expressions.NewLogicalProjectionExpressionWithOutputSchema(
			projected, aliases, aliasMinted, outputNames, childQ))
}

func TestImplementProjectionRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scan := implementMissingScan()
	innerRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	proj := mustImplementMissingConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{implementMissingPrice(innerQ)},
		innerQ,
	))
	topRef := expressions.InitialOf(proj)

	mustFireExpressionRule(t, NewPrimaryScanRule(), innerRef)
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	yielded := mustFireExpressionRule(t, NewImplementProjectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryProjectionPlan", yielded[0])
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("projection plan inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

func TestImplementProjectionRule_ReusesExactPositionalProjectionChild(t *testing.T) {
	t.Parallel()
	child := implementProjectionReuseChild(t)
	logical := implementProjectionReuseOuter(
		t, child, []int{0, 1}, nil, nil, []string{"A", "B"})

	yielded := mustFireExpressionRule(
		t, NewImplementProjectionRule(), expressions.InitialOf(logical))
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionRule yielded %d members, want one exact child reuse", len(yielded))
	}
	if yielded[0] != child {
		t.Fatalf("yield = %T %p, want existing projection child %p", yielded[0], yielded[0], child)
	}
	if _, nested := child.GetInner().(*plans.RecordQueryProjectionPlan); nested {
		t.Fatal("fixture child unexpectedly contains a nested projection")
	}
}

func TestImplementProjectionFinalRule_ReusesExactPositionalProjectionChild(t *testing.T) {
	t.Parallel()
	child := implementProjectionReuseChild(t)
	logical := implementProjectionReuseOuter(
		t, child, []int{0, 1}, nil, nil, []string{"A", "B"})

	yielded := mustFireImplementationRule(
		t, NewImplementProjectionFinalRule(), expressions.InitialOf(logical))
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionFinalRule yielded %d members, want one exact child reuse", len(yielded))
	}
	if yielded[0] != child {
		t.Fatalf("yield = %T %p, want existing projection child %p", yielded[0], yielded[0], child)
	}
}

func TestImplementProjectionRule_ExactReuseDeclinesSemanticChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		ordinals    []int
		aliases     []string
		aliasMinted []bool
		outputNames []string
	}{
		{
			name:        "reordered",
			ordinals:    []int{1, 0},
			outputNames: []string{"A", "B"},
		},
		{
			name:        "narrowed",
			ordinals:    []int{0},
			outputNames: []string{"A"},
		},
		{
			name:        "different output name",
			ordinals:    []int{0, 1},
			outputNames: []string{"X", "B"},
		},
		{
			name:        "same output name but user alias metadata",
			ordinals:    []int{0, 1},
			aliases:     []string{"A", ""},
			aliasMinted: []bool{false, false},
			outputNames: []string{"A", "B"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			child := implementProjectionReuseChild(t)
			logical := implementProjectionReuseOuter(
				t, child, test.ordinals, test.aliases, test.aliasMinted, test.outputNames)
			yielded := mustFireExpressionRule(
				t, NewImplementProjectionRule(), expressions.InitialOf(logical))
			if len(yielded) != 1 {
				t.Fatalf("ImplementProjectionRule yielded %d members, want one wrapper", len(yielded))
			}
			wrapper, wrapped := yielded[0].(*plans.RecordQueryProjectionPlan)
			if !wrapped || wrapper == child {
				t.Fatalf("yield = %T %p, want a new projection wrapper", yielded[0], yielded[0])
			}
			if wrapper.GetInner() != child {
				t.Fatalf("wrapper inner = %T %p, want original child %p", wrapper.GetInner(), wrapper.GetInner(), child)
			}
		})
	}
}

func TestImplementProjectionRule_ExactReuseDeclinesForeignRoot(t *testing.T) {
	t.Parallel()
	child := implementProjectionReuseChild(t)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	foreignQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	foreignRoot := mustImplementMissingConstruct(foreignQ.RequireFlowedObjectValue())
	logical := mustImplementMissingConstruct(
		expressions.NewLogicalProjectionExpressionWithOutputSchema(
			[]values.Value{
				mustImplementMissingConstruct(values.ResolveFieldOrdinals(foreignRoot, []int{0})),
				mustImplementMissingConstruct(values.ResolveFieldOrdinals(foreignRoot, []int{1})),
			},
			nil, nil, []string{"A", "B"}, innerQ))

	yielded := mustFireExpressionRule(
		t, NewImplementProjectionRule(), expressions.InitialOf(logical))
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionRule yielded %d members, want one wrapper", len(yielded))
	}
	if yielded[0] == child {
		t.Fatal("foreign-root projection was incorrectly treated as this edge's identity")
	}
}

func TestImplementProjectionRule_ExactReuseDeclinesScalarQOVWrapper(t *testing.T) {
	t.Parallel()
	array := &values.ConstantValue{
		Value: []any{int64(1)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}
	child := mustImplementMissingConstruct(plans.NewRecordQueryExplodePlan(array))
	childQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
	scalar := mustImplementMissingConstruct(childQ.RequireFlowedObjectValue())
	logical := mustImplementMissingConstruct(
		expressions.NewLogicalProjectionExpressionWithOutputSchema(
			[]values.Value{scalar}, nil, nil, []string{"VALUE"}, childQ))

	yielded := mustFireExpressionRule(
		t, NewImplementProjectionRule(), expressions.InitialOf(logical))
	if len(yielded) != 1 {
		t.Fatalf("ImplementProjectionRule yielded %d members, want one wrapper", len(yielded))
	}
	if yielded[0] == child {
		t.Fatal("scalar QOV wrapper was incorrectly erased")
	}
	if _, wrapped := yielded[0].(*plans.RecordQueryProjectionPlan); !wrapped {
		t.Fatalf("yield = %T, want scalar RecordQueryProjectionPlan wrapper", yielded[0])
	}
}

func TestImplementProjectionRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := implementMissingScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := mustImplementMissingConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{implementMissingPrice(innerQ)},
		innerQ,
	))
	topRef := expressions.InitialOf(proj)

	yielded := mustFireExpressionRule(t, NewImplementProjectionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementProjectionRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementValuesRule_Fires(t *testing.T) {
	t.Parallel()
	ve := mustImplementMissingConstruct(expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
	}))
	ref := expressions.InitialOf(ve)

	yielded := mustFireExpressionRule(t, NewImplementValuesRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementValuesRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryValuesPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryValuesPlan", yielded[0])
	}
	if plan == nil {
		t.Fatal("nil values plan")
	}
}

func TestImplementTempTableScanRule_Fires(t *testing.T) {
	t.Parallel()
	scan := mustImplementMissingConstruct(expressions.NewTempTableScanExpression(
		values.NamedCorrelationIdentifier("tt_scan"), implementMissingRowType()))
	ref := expressions.InitialOf(scan)

	yielded := mustFireExpressionRule(t, NewImplementTempTableScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableScanRule yielded %d, want 1", len(yielded))
	}
	// RFC-184 W2: the temp-table scan is its own bare plan expression.
	wrap, ok := yielded[0].(*plans.RecordQueryTempTableScanPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTempTableScanPlan", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("plan is nil")
	}
}

func TestImplementTempTableInsertRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scan := implementMissingScan()
	innerRef := expressions.InitialOf(scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	insert := mustImplementMissingConstruct(expressions.NewTempTableInsertExpression(
		innerQ, values.NamedCorrelationIdentifier("tt_insert"), true))
	topRef := expressions.InitialOf(insert)

	mustFireExpressionRule(t, NewPrimaryScanRule(), innerRef)
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	yielded := mustFireExpressionRule(t, NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableInsertRule yielded %d, want 1", len(yielded))
	}
	// RFC-184 W2: the temp-table insert is its own bare plan expression.
	wrap, ok := yielded[0].(*plans.RecordQueryTempTableInsertPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTempTableInsertPlan", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("plan is nil")
	}
}

func TestImplementTempTableInsertRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := implementMissingScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	insert := mustImplementMissingConstruct(expressions.NewTempTableInsertExpression(
		innerQ, values.NamedCorrelationIdentifier("tt_insert"), true))
	topRef := expressions.InitialOf(insert)

	yielded := mustFireExpressionRule(t, NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTempTableInsertRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveDfsJoinRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scanA := implementMissingScan()
	scanB := implementMissingScan()
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	initialQ := expressions.ForEachQuantifier(refA)
	recursiveQ := expressions.ForEachQuantifier(refB)
	recUnion := mustImplementMissingConstruct(expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalPreorder,
	))
	topRef := expressions.InitialOf(recUnion)

	mustFireExpressionRule(t, NewPrimaryScanRule(), refA)
	mustFireExpressionRule(t, NewPrimaryScanRule(), refB)
	if got := len(refA.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refA has %d members, want 2", got)
	}
	if got := len(refB.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refB has %d members, want 2", got)
	}

	yielded := mustFireExpressionRule(t, NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveDfsJoinRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*plans.RecordQueryRecursiveDfsJoinPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryRecursiveDfsJoinPlan", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementRecursiveDfsJoinRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scanA := implementMissingScan()
	scanB := implementMissingScan()
	initialQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	recursiveQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	recUnion := mustImplementMissingConstruct(expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalPreorder,
	))
	topRef := expressions.InitialOf(recUnion)

	yielded := mustFireExpressionRule(t, NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveDfsJoinRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveLevelUnionRule_FiresAfterInnerImplemented(t *testing.T) {
	t.Parallel()
	scanA := implementMissingScan()
	scanB := implementMissingScan()
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	initialQ := expressions.ForEachQuantifier(refA)
	recursiveQ := expressions.ForEachQuantifier(refB)
	recUnion := mustImplementMissingConstruct(expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalLevel,
	))
	topRef := expressions.InitialOf(recUnion)

	mustFireExpressionRule(t, NewPrimaryScanRule(), refA)
	mustFireExpressionRule(t, NewPrimaryScanRule(), refB)
	if got := len(refA.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refA has %d members, want 2", got)
	}
	if got := len(refB.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, refB has %d members, want 2", got)
	}

	yielded := mustFireExpressionRule(t, NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveLevelUnionRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*plans.RecordQueryRecursiveLevelUnionPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryRecursiveLevelUnionPlan", yielded[0])
	}
	if wrap.GetRecordQueryPlan() == nil {
		t.Fatal("wrapper has no plan")
	}
}

func TestImplementRecursiveLevelUnionRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scanA := implementMissingScan()
	scanB := implementMissingScan()
	initialQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	recursiveQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	recUnion := mustImplementMissingConstruct(expressions.NewRecursiveUnionExpression(
		initialQ, recursiveQ,
		values.NamedCorrelationIdentifier("scan_tt"),
		values.NamedCorrelationIdentifier("insert_tt"),
		expressions.TraversalLevel,
	))
	topRef := expressions.InitialOf(recUnion)

	yielded := mustFireExpressionRule(t, NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveLevelUnionRule fired without physical inner; yielded %d", len(yielded))
	}
}
