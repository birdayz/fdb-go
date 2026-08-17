package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFilterPlan_RelinkRebasesPredicateProgramsWithoutMutatingSource(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("filter_old")
	newAlias := values.NamedCorrelationIdentifier("filter_new")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	predicate := predicates.NewComparisonPredicate(
		testFieldIn(t, rowType, oldAlias.Name(), "V"),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	original := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlanFromQuantifier(
			[]predicates.QueryPredicate{predicate}, oldQ)
	})

	relinkedExpr, err := original.WithQuantifiers([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryFilterPlan)
	requireOnlyPredicateCorrelation(t, relinked.GetPredicates()[0], newAlias)
	requireOnlyPredicateCorrelation(t, original.GetPredicates()[0], oldAlias)
	if relinked.GetPredicates()[0] == original.GetPredicates()[0] {
		t.Fatal("relink retained the predicate node despite changing its input edge")
	}
}

func TestFilterPlan_RelinkRejectsChildTypeDrift(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	oldScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("filter_type_old")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(oldScan, expressions.StageCanonical))
	original := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlanFromQuantifier([]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				testFieldIn(t, rowType, oldAlias.Name(), "ID"),
				predicates.Comparison{Type: predicates.ComparisonIsNotNull},
			),
		}, oldQ)
	})

	otherType := values.NewRecordType("other_filter_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	otherScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, otherType, false)
	})
	otherQ := expressions.NamedPhysicalQuantifier(
		values.NamedCorrelationIdentifier("filter_type_new"),
		expressions.FinalOfAtStage(otherScan, expressions.StageCanonical),
	)
	if _, err := original.WithQuantifiers([]expressions.Quantifier{otherQ}); err == nil {
		t.Fatal("WithQuantifiers accepted a child with a different exact type")
	}
}

func TestPredicatesFilterPlan_NormalizesPhysicalAndBindingAliases(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("predicates_filter_old")
	newAlias := values.NamedCorrelationIdentifier("predicates_filter_new")
	logicalAlias := values.NamedCorrelationIdentifier("predicates_filter_logical")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	physicalPredicate := predicates.NewComparisonPredicate(
		testFieldIn(t, rowType, oldAlias.Name(), "V"),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	logicalPredicate := predicates.NewComparisonPredicate(
		testFieldIn(t, rowType, logicalAlias.Name(), "ID"),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	original := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			oldQ,
			[]predicates.QueryPredicate{physicalPredicate, logicalPredicate},
			logicalAlias,
		)
	})

	relinkedExpr, err := original.WithQuantifiers([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryPredicatesFilterPlan)
	if relinked.GetInnerAlias() != logicalAlias {
		t.Fatalf("logical binding alias = %s, want preserved %s", relinked.GetInnerAlias(), logicalAlias)
	}
	requireOnlyPredicateCorrelation(t, relinked.GetPredicates()[0], values.CurrentCorrelation())
	requireOnlyPredicateCorrelation(t, relinked.GetPredicates()[1], values.CurrentCorrelation())
	requireOnlyPredicateCorrelation(t, original.GetPredicates()[0], values.CurrentCorrelation())
	requireOnlyPredicateCorrelation(t, original.GetPredicates()[1], values.CurrentCorrelation())
}

func TestPredicatesFilterPlan_SelectedOrdinalityExplodeNormalizesBindingNames(t *testing.T) {
	t.Parallel()
	array := values.NewArrayConstructorValue(values.NullableLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	explode := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlanWithOrdinality(array, true)
	})
	alias := values.NamedCorrelationIdentifier("UNNEST")
	logicalType := values.NewRecordType("", false, []values.Field{
		{Name: "ELEMENT", FieldType: values.NullableLong},
		{Name: "ORDINAL", FieldType: values.NotNullInt},
	})
	logical := mustOrdinalLayoutQOV(t, alias, logicalType)
	ordinal, err := values.ResolveFieldOrdinals(logical, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	predicate := predicates.NewComparisonPredicate(
		ordinal, predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlanWithAlias(explode, []predicates.QueryPredicate{predicate}, alias)
	})

	explodeLayout := requireProvidedLayout(t, explode)
	assertPredicateHasExactRoot(
		t, filter.GetPredicates()[0], alias, explodeLayout.Carrier().FlowedType())
	assertPredicateLacksRoot(t, filter.GetPredicates()[0], alias, logicalType)
	var normalizedField values.FieldValue
	_, err = predicates.TransformEmbeddedValuesChecked(
		filter.GetPredicates()[0],
		func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				field, fieldOK := values.AsFieldValue(node)
				if !fieldOK {
					return true
				}
				root, rootOK := values.AsQuantifiedObjectValue(field.ChildValue())
				if rootOK && root.Correlation() == alias &&
					root.FlowedType().Equals(explodeLayout.Carrier().FlowedType()) {
					normalizedField = field
				}
				return true
			})
			return value, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedField == nil {
		t.Fatal("ordinal predicate has no normalized physical field")
	}
	if got := normalizedField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("ordinal predicate path = %v, want [1]", got)
	}
	if field, ok := values.AsFieldValue(ordinal); !ok || field.ChildValue() != logical {
		t.Fatal("filter construction mutated the logical ordinal field")
	}

	plain := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(array)
	})
	declined, err := translateOrdinalityBindingNames(
		predicate, QuantifierOverPlan(plain), alias)
	if err != nil {
		t.Fatal(err)
	}
	if declined != predicate {
		t.Fatal("plain Explode admitted the WITH ORDINALITY binding-name bridge")
	}
}

func TestPredicatesFilterPlan_RelinkRetypesNominalInputOntoExactPhysicalEdge(t *testing.T) {
	t.Parallel()
	physicalType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
	nominalType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
	retainedWindowType := values.NewRecordType("T_WINDOW", false, []values.Field{
		{Name: "V", FieldType: values.NullableLong, Ordinal: 0},
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, physicalType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("physical_old")
	newAlias := values.UniqueCorrelationIdentifier()
	foreignAlias := values.NamedCorrelationIdentifier("foreign")
	logicalAlias := values.NamedCorrelationIdentifier("logical")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))

	nominalField := testFieldIn(t, nominalType, oldAlias.Name(), "V")
	foreignField := testFieldIn(t, nominalType, foreignAlias.Name(), "ID")
	retainedWindowField := testFieldIn(t, retainedWindowType, oldAlias.Name(), "V")
	original := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			oldQ,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					nominalField,
					predicates.Comparison{Type: predicates.ComparisonEquals, Operand: foreignField},
				),
				predicates.NewComparisonPredicate(
					retainedWindowField,
					predicates.Comparison{Type: predicates.ComparisonIsNotNull},
				),
			},
			logicalAlias,
		)
	})

	relinkedExpression, err := original.WithQuantifiers([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpression.(*RecordQueryPredicatesFilterPlan)
	if relinked.GetInnerAlias() != logicalAlias {
		t.Fatalf("logical binding alias = %s, want %s", relinked.GetInnerAlias(), logicalAlias)
	}

	newLayout := requireProvidedLayout(t, scan)
	assertPredicateHasExactRoot(
		t, relinked.GetPredicates()[0], values.CurrentCorrelation(), newLayout.Carrier().FlowedType())
	assertPredicateHasExactRoot(t, relinked.GetPredicates()[0], foreignAlias, nominalType)
	assertPredicateHasExactRoot(t, relinked.GetPredicates()[1], oldAlias, retainedWindowType)
	assertPredicateHasExactRoot(
		t, original.GetPredicates()[0], values.CurrentCorrelation(), newLayout.Carrier().FlowedType())
	assertPredicateLacksRoot(t, original.GetPredicates()[0], oldAlias, nominalType)
	assertPredicateLacksRoot(t, original.GetPredicates()[0], newAlias, physicalType)
}

func TestPredicatesFilterPlan_SelectedJoinNormalizesRetainedSourcesThroughProducer(t *testing.T) {
	t.Parallel()
	outerType := values.NewRecordType("BOOKS", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "AUTHOR_ID", Ordinal: 1, FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("AWARDS", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "BOOK_ID", Ordinal: 1, FieldType: values.NullableLong},
	})
	fixture := newRetainedJoinFixture(
		t, "predicates_filter_join",
		values.NamedCorrelationIdentifier("B"),
		values.NamedCorrelationIdentifier("W"), outerType, innerType)
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			fixture.outerQ, fixture.innerQ, nil, JoinInner,
			fixture.outerAlias, fixture.innerAlias, fixture.result)
	})
	joinQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(),
		expressions.FinalOfAtStage(join, expressions.StageCanonical))

	logicalOuter := mustOrdinalLayoutQOV(t, fixture.outerAlias,
		values.NewRecordType("", false, outerType.Fields))
	logicalInner := mustOrdinalLayoutQOV(t, fixture.innerAlias,
		values.NewRecordType("", false, innerType.Fields))
	outerAuthorID, err := values.ResolveFieldOrdinals(logicalOuter, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	innerBookID, err := values.ResolveFieldOrdinals(logicalInner, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	foreignType := values.NewRecordType("FOREIGN", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
	})
	foreignRoot := mustOrdinalLayoutQOV(
		t, values.NamedCorrelationIdentifier("FOREIGN"), foreignType)
	foreignID, err := values.ResolveFieldOrdinals(foreignRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}

	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlanFromQuantifier(joinQ, []predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				outerAuthorID,
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: innerBookID},
			),
			predicates.NewComparisonPredicate(
				foreignID,
				predicates.Comparison{Type: predicates.ComparisonIsNotNull},
			),
		})
	})
	joinLayout := requireProvidedLayout(t, join)
	assertPredicateHasExactRoot(
		t, filter.GetPredicates()[0], values.CurrentCorrelation(), joinLayout.Carrier().FlowedType())
	assertPredicateLacksRoot(t, filter.GetPredicates()[0], fixture.outerAlias, outerType)
	assertPredicateLacksRoot(t, filter.GetPredicates()[0], fixture.innerAlias, innerType)
	assertPredicateHasExactRoot(t, filter.GetPredicates()[1], foreignRoot.Correlation(), foreignType)
	if !predicatePathsEqual(
		t, filter.GetPredicates()[0], joinLayout.Carrier(), [][]int{{1}, {3}}) {
		t.Fatal("selected join predicate did not retain the exact B.AUTHOR_ID/W.BOOK_ID output ordinals")
	}
	assertPredicateHasExactRoot(t, filter.GetPredicates()[1], foreignRoot.Correlation(), foreignType)
	if !predicatePathsEqual(t, filter.GetPredicates()[1], foreignRoot, [][]int{{0}}) {
		t.Fatal("foreign predicate root/path was rewritten through the selected join")
	}
}

func TestPredicatesFilterPlan_SelectedNestedFlatMapNormalizesBuriedSource(t *testing.T) {
	t.Parallel()
	baseType := values.NewRecordType("T4", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
	})
	xType := values.NewRecordType("X_ROW", false, []values.Field{
		{Name: "XV", Ordinal: 0, FieldType: values.NullableInt},
	})
	yType := values.NewRecordType("Y_ROW", false, []values.Field{
		{Name: "YV", Ordinal: 0, FieldType: values.NullableInt},
	})
	baseAlias := values.NamedCorrelationIdentifier("T4")
	xAlias := values.NamedCorrelationIdentifier("X")
	yAlias := values.NamedCorrelationIdentifier("Y")
	newLeg := func(alias values.CorrelationIdentifier, typ values.Type) expressions.Quantifier {
		t.Helper()
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias.Name()}, typ, false)
		})
		return expressions.NamedPhysicalQuantifier(
			alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	}
	baseQ := newLeg(baseAlias, baseType)
	xQ := newLeg(xAlias, xType)
	yQ := newLeg(yAlias, yType)
	baseRoot, err := baseQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	xRoot, err := xQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	yRoot, err := yQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, resolveErr := values.ResolveFieldOrdinals(root, path)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return field
	}
	lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			baseQ, xQ, baseAlias, xAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(baseRoot, 0)},
				values.RecordConstructorField{Name: "K", Value: resolve(baseRoot, 1)},
				values.RecordConstructorField{Name: "X", Value: xRoot}), false)
	})
	// The upper FlatMap binds the lower materialized row as X, deliberately
	// shadowing the lower element alias. Its nearest result program therefore
	// mentions only X/Y even though the selected lower producer still proves
	// the buried T4 declaration.
	lowerQ := expressions.NamedPhysicalQuantifier(
		xAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
	lowerRoot, err := lowerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			lowerQ, yQ, xAlias, yAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: resolve(lowerRoot, 0)},
				values.RecordConstructorField{Name: "K", Value: resolve(lowerRoot, 1)},
				values.RecordConstructorField{Name: "X", Value: resolve(lowerRoot, 2)},
				values.RecordConstructorField{Name: "Y", Value: yRoot}), false)
	})

	buriedID := resolve(baseRoot, 0)
	foreignRoot := mustOrdinalLayoutQOV(
		t, values.NamedCorrelationIdentifier("FOREIGN_T4"), baseType)
	foreignID := resolve(foreignRoot, 0)
	wrongType := values.NewRecordType("T4_WRONG", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
	})
	wrongRoot := mustOrdinalLayoutQOV(t, baseAlias, wrongType)
	wrongID := resolve(wrongRoot, 0)
	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(upper, []predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				buriedID, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
			predicates.NewComparisonPredicate(
				foreignID, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
			predicates.NewComparisonPredicate(
				wrongID, predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
		})
	})

	upperLayout := requireProvidedLayout(t, upper)
	if provided, provideErr := values.LayoutProvides(upperLayout, lowerRoot); provideErr != nil || !provided {
		t.Fatalf("upper direct whole-row X = (%t, %v), want direct result authority", provided, provideErr)
	}
	if provided, provideErr := values.LayoutProvides(upperLayout, xRoot); provideErr == nil || provided {
		t.Fatalf("buried same-correlation X = (%t, %v), want exact-type conflict behind direct whole row",
			provided, provideErr)
	}
	assertPredicateHasExactRoot(
		t, filter.GetPredicates()[0], values.CurrentCorrelation(), upperLayout.Carrier().FlowedType())
	assertPredicateLacksRoot(t, filter.GetPredicates()[0], baseAlias, baseType)
	if !predicatePathsEqual(t, filter.GetPredicates()[0], upperLayout.Carrier(), [][]int{{0}}) {
		t.Fatal("buried T4.ID did not cross both selected FlatMap materializers")
	}
	assertPredicateHasExactRoot(t, filter.GetPredicates()[1], foreignRoot.Correlation(), baseType)
	assertPredicateHasExactRoot(t, filter.GetPredicates()[2], baseAlias, wrongType)
	if !predicatePathsEqual(t, filter.GetPredicates()[1], foreignRoot, [][]int{{0}}) ||
		!predicatePathsEqual(t, filter.GetPredicates()[2], wrongRoot, [][]int{{0}}) {
		t.Fatal("foreign or wrong-type source was guessed through the materializer chain")
	}
	if original, ok := values.AsFieldValue(buriedID); !ok || original.ChildValue() != baseRoot {
		t.Fatal("filter construction mutated the buried source field")
	}
}

// TestFlatMapRetainedSources_DirectFieldModeShadowsBuriedSameCorrelationWithoutRecordInner
// pins the direct-source precedence independently of the direct-record trigger.
// The upper FlatMap retains its selected lower row under X field-by-field, while
// that lower producer also carries an older record-valued X window. Its inner is
// deliberately scalar, so no bare direct record can make retained-source
// discovery happen accidentally. The direct row is the nearest X declaration;
// the buried X_ROW must not conflict with it or replace it.
func TestFlatMapRetainedSources_DirectFieldModeShadowsBuriedSameCorrelationWithoutRecordInner(t *testing.T) {
	t.Parallel()
	baseType := values.NewRecordType("DIRECT_BASE", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
	})
	buriedType := values.NewRecordType("X_ROW", false, []values.Field{
		{Name: "XV", Ordinal: 0, FieldType: values.NullableInt},
	})
	baseAlias := values.NamedCorrelationIdentifier("DIRECT_BASE")
	xAlias := values.NamedCorrelationIdentifier("X")
	scalarAlias := values.NamedCorrelationIdentifier("SCALAR_INNER")
	newRecordLeg := func(alias values.CorrelationIdentifier, typ values.Type) expressions.Quantifier {
		t.Helper()
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias.Name()}, typ, false)
		})
		return expressions.NamedPhysicalQuantifier(
			alias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	}
	baseQ := newRecordLeg(baseAlias, baseType)
	buriedQ := newRecordLeg(xAlias, buriedType)
	baseRoot, err := baseQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	buriedRoot, err := buriedQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(root values.Value, path ...int) values.Value {
		t.Helper()
		field, resolveErr := values.ResolveFieldOrdinals(root, path)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		return field
	}
	lowerResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: resolve(baseRoot, 0)},
		values.RecordConstructorField{Name: "K", Value: resolve(baseRoot, 1)},
		values.RecordConstructorField{Name: "X", Value: buriedRoot})
	lower := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			baseQ, buriedQ, baseAlias, xAlias, lowerResult, false)
	})
	lowerLayout := requireProvidedLayout(t, lower)
	if provided, provideErr := values.LayoutProvides(lowerLayout, buriedRoot); provideErr != nil || !provided {
		t.Fatalf("lower buried X = (%t, %v), want exact child source authority", provided, provideErr)
	}

	lowerQ := expressions.NamedPhysicalQuantifier(
		xAlias, expressions.FinalOfAtStage(lower, expressions.StageCanonical))
	directRoot, err := lowerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	scalarArray := values.NewArrayConstructorValue(
		values.NotNullLong, []values.Value{values.LiteralValue(int64(1))})
	scalarPlan := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(scalarArray)
	})
	scalarQ := expressions.NamedPhysicalQuantifier(
		scalarAlias, expressions.FinalOfAtStage(scalarPlan, expressions.StageCanonical))
	scalarRoot, err := scalarQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	if scalarRoot.FlowedType().Code() == values.TypeCodeRecord {
		t.Fatalf("inner control is %s, want non-record", scalarRoot.FlowedType())
	}
	upperResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: resolve(directRoot, 0)},
		values.RecordConstructorField{Name: "K", Value: resolve(directRoot, 1)},
		values.RecordConstructorField{Name: "X", Value: resolve(directRoot, 2)},
		values.RecordConstructorField{Name: "S", Value: scalarRoot})
	for _, field := range upperResult.Fields {
		if root, isRoot := values.AsQuantifiedObjectValue(field.Value); isRoot &&
			root.FlowedType().Code() == values.TypeCodeRecord {
			t.Fatalf("fixture contains a bare direct record %s; it no longer pins field-mode discovery", root.FlowedType())
		}
	}
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			lowerQ, scalarQ, xAlias, scalarAlias, upperResult, false)
	})
	upperLayout := requireProvidedLayout(t, upper)
	if provided, provideErr := values.LayoutProvides(upperLayout, directRoot); provideErr != nil || !provided {
		t.Fatalf("upper direct field-mode X = (%t, %v), want nearest result authority", provided, provideErr)
	}
	if provided, provideErr := values.LayoutProvides(upperLayout, buriedRoot); provideErr == nil || provided {
		t.Fatalf("upper buried same-correlation X = (%t, %v), want exact-type conflict behind direct X",
			provided, provideErr)
	}
	if got := upper.GetResultValue(); got != upperResult {
		t.Fatal("FlatMap construction replaced or mutated the direct result program")
	}
	if got := lower.GetResultValue(); got != lowerResult || buriedRoot.Correlation() != xAlias {
		t.Fatal("FlatMap construction mutated the selected child or buried source declaration")
	}
}

func TestPredicatesFilterPlan_SelectedProjectionDoesNotCaptureForeignOuterField(t *testing.T) {
	t.Parallel()
	baseType := values.NewRecordType("U", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"U"}, baseType, false)
	})
	scanLayout := requireProvidedLayout(t, scan)
	baseID, err := values.ResolveFieldOrdinals(scanLayout.Carrier(), []int{0})
	if err != nil {
		t.Fatal(err)
	}
	derived := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{baseID}, []string{"A.ID"}, scan)
	})
	derivedQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(),
		expressions.FinalOfAtStage(derived, expressions.StageCanonical),
	)
	innerAlias := values.NamedCorrelationIdentifier("S")
	innerType := values.NewRecordType("", false, []values.Field{
		{Name: "A.ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	innerRoot := mustOrdinalLayoutQOV(t, innerAlias, innerType)
	innerID, err := values.ResolveFieldOrdinals(innerRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	outerAlias := values.NamedCorrelationIdentifier("A")
	outerType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	outerRoot := mustOrdinalLayoutQOV(t, outerAlias, outerType)
	outerID, err := values.ResolveFieldOrdinals(outerRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}

	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			derivedQ,
			[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
				outerID,
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: innerID},
			)},
			innerAlias,
		)
	})
	derivedLayout := requireProvidedLayout(t, derived)
	assertPredicateHasExactRoot(t, filter.GetPredicates()[0], outerAlias, outerType)
	assertPredicateHasExactRoot(
		t, filter.GetPredicates()[0], values.CurrentCorrelation(), derivedLayout.Carrier().FlowedType())
	assertPredicateLacksRoot(t, filter.GetPredicates()[0], innerAlias, innerType)
	if !predicatePathsEqual(
		t, filter.GetPredicates()[0], outerRoot, [][]int{{0}}) {
		t.Fatal("foreign outer A.ID was captured by the one-slot derived producer")
	}
	if !predicatePathsEqual(
		t, filter.GetPredicates()[0], derivedLayout.Carrier(), [][]int{{0}}) {
		t.Fatal("owned S.\"A.ID\" did not move onto the exact derived carrier")
	}
	if field, ok := values.AsFieldValue(outerID); !ok || field.ChildValue() != outerRoot {
		t.Fatal("filter construction mutated the foreign outer field")
	}
}

func TestProjectionAcrossSelectedPredicatesFilterUsesRetainedJoinProducer(t *testing.T) {
	t.Parallel()
	outerType := values.NewRecordType("A_ROW", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("B_ROW", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NullableLong},
	})
	fixture := newRetainedJoinFixture(
		t, "projection_filter_join",
		values.NamedCorrelationIdentifier("A"),
		values.NamedCorrelationIdentifier("B"), outerType, innerType)
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			fixture.outerQ, fixture.innerQ, nil, JoinInner,
			fixture.outerAlias, fixture.innerAlias, fixture.result)
	})
	filter := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(join, nil)
	})
	logicalOuter := mustOrdinalLayoutQOV(t, fixture.outerAlias,
		values.NewRecordType("", false, outerType.Fields))
	logicalID, err := values.ResolveFieldOrdinals(logicalOuter, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	projection := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{logicalID}, filter)
	})
	joinLayout := requireProvidedLayout(t, join)
	projected, ok := values.AsFieldValue(projection.GetProjections()[0])
	if !ok || projected.ChildValue() != joinLayout.Carrier() {
		t.Fatalf("projection root = %T/%v, want exact retained-join carrier %p",
			projection.GetProjections()[0], projected, joinLayout.Carrier())
	}
	if got := projected.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("projection path = %v, want retained outer ID at [0]", got)
	}
	if field, fieldOK := values.AsFieldValue(logicalID); !fieldOK || field.ChildValue() != logicalOuter {
		t.Fatal("projection construction mutated the logical source field")
	}
}

func predicatePathsEqual(
	t *testing.T,
	predicate predicates.QueryPredicate,
	root values.QuantifiedObjectValue,
	want [][]int,
) bool {
	t.Helper()
	var got [][]int
	_, err := predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				field, ok := values.AsFieldValue(node)
				if ok && field.ChildValue() == root {
					got = append(got, field.Path().Ordinals())
				}
				return true
			})
			return value, nil
		},
	)
	if err != nil {
		t.Fatalf("inspect predicate paths: %v", err)
	}
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

func assertPredicateHasExactRoot(
	t *testing.T,
	predicate predicates.QueryPredicate,
	wantCorrelation values.CorrelationIdentifier,
	wantType values.Type,
) {
	t.Helper()
	if !predicateHasExactRoot(t, predicate, wantCorrelation, wantType) {
		t.Fatalf("predicate has no exact root %s / %s", wantCorrelation, wantType)
	}
}

func assertPredicateLacksRoot(
	t *testing.T,
	predicate predicates.QueryPredicate,
	wantCorrelation values.CorrelationIdentifier,
	wantType values.Type,
) {
	t.Helper()
	if predicateHasExactRoot(t, predicate, wantCorrelation, wantType) {
		t.Fatalf("predicate unexpectedly has exact root %s / %s", wantCorrelation, wantType)
	}
}

func predicateHasExactRoot(
	t *testing.T,
	predicate predicates.QueryPredicate,
	wantCorrelation values.CorrelationIdentifier,
	wantType values.Type,
) bool {
	t.Helper()
	found := false
	_, err := predicates.TransformEmbeddedValuesChecked(
		predicate,
		func(value values.Value) (values.Value, error) {
			values.WalkValue(value, func(node values.Value) bool {
				root, ok := values.AsQuantifiedObjectValue(node)
				if ok && root.Correlation() == wantCorrelation && root.FlowedType().Equals(wantType) {
					found = true
				}
				return true
			})
			return value, nil
		},
	)
	if err != nil {
		t.Fatalf("inspect predicate roots: %v", err)
	}
	return found
}

func requireOnlyPredicateCorrelation(
	t *testing.T,
	predicate predicates.QueryPredicate,
	want values.CorrelationIdentifier,
) {
	t.Helper()
	correlated := predicate.GetCorrelatedTo()
	if len(correlated) != 1 {
		t.Fatalf("predicate correlations = %v, want only %s", correlated, want)
	}
	if _, ok := correlated[want]; !ok {
		t.Fatalf("predicate correlations = %v, want %s", correlated, want)
	}
}
