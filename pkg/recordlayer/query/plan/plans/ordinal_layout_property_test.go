package plans

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlanProvidedOutputLayoutExactIdentity(t *testing.T) {
	t.Parallel()

	t.Run("record fields are one exact flat tile", func(t *testing.T) {
		t.Parallel()
		rowType := values.NewRecordType("identity_row", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "NAME", Ordinal: 1, FieldType: values.NullableString},
		})
		plan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
		})

		layout := requireProvidedLayout(t, plan)
		if layout.CarrierKind() != values.OrdinalCarrierRecord {
			t.Fatalf("carrier kind = %v, want record", layout.CarrierKind())
		}
		if !layout.Carrier().FlowedType().Equals(rowType) {
			t.Fatalf("carrier type = %v, want %v", layout.Carrier().FlowedType(), rowType)
		}
		resultRoot, ok := values.AsQuantifiedObjectValue(plan.GetResultValue())
		if !ok || resultRoot != layout.Carrier() {
			t.Fatal("type-owning plan result QOV is not its layout's exact owner carrier")
		}
		want, err := values.NewOrdinalLayout(layout.Carrier(), []values.OrdinalTileSpec{{
			Start: 0,
			Width: 2,
			Kind:  values.OrdinalTileFlat,
		}}, nil)
		if err != nil {
			t.Fatalf("expected identity layout: %v", err)
		}
		if !layout.RawEqual(want) {
			t.Fatal("record plan did not provide the exact flat identity layout")
		}
		if again := requireProvidedLayout(t, plan); again != layout {
			t.Fatal("provided layout changed across reads")
		}
	})

	t.Run("zero-field record has no tiles", func(t *testing.T) {
		t.Parallel()
		rowType := values.NewRecordType("unit", false, nil)
		plan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"UNIT"}, rowType, false)
		})
		layout := requireProvidedLayout(t, plan)
		want, err := values.NewOrdinalLayout(layout.Carrier(), nil, nil)
		if err != nil {
			t.Fatalf("expected zero-field identity layout: %v", err)
		}
		if !layout.RawEqual(want) {
			t.Fatal("zero-field plan did not provide the empty record identity layout")
		}
	})

	t.Run("scalar is a direct scalar carrier", func(t *testing.T) {
		t.Parallel()
		stream := &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}
		plan := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
			return NewRecordQueryTableFunctionPlan(stream)
		})
		layout := requireProvidedLayout(t, plan)
		if layout.CarrierKind() != values.OrdinalCarrierScalar {
			t.Fatalf("carrier kind = %v, want scalar", layout.CarrierKind())
		}
		want, err := values.NewScalarOrdinalLayout(layout.Carrier())
		if err != nil {
			t.Fatalf("expected scalar identity layout: %v", err)
		}
		if !layout.RawEqual(want) {
			t.Fatal("scalar plan did not provide the exact scalar identity layout")
		}
	})
}

func TestJoinPlansPublishRetainedSourceLayouts(t *testing.T) {
	t.Parallel()

	outerType, innerType := retainedJoinTypes()
	fixture := newRetainedJoinFixture(
		t, "layout", values.NamedCorrelationIdentifier("OUTER"),
		values.NamedCorrelationIdentifier("INNER"), outerType, innerType)

	t.Run("nested loop agrees with executor retained-result build", func(t *testing.T) {
		plan := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
			return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				fixture.outerQ, fixture.innerQ, nil, JoinLeftOuter,
				fixture.outerAlias, fixture.innerAlias, fixture.result)
		})
		got := requireProvidedLayout(t, plan)
		want, err := values.NewFlatOrdinalLayoutForRetainedResult(
			fixture.result, []values.QuantifiedObjectValue{fixture.innerSource})
		if err != nil {
			t.Fatalf("executor retained-result layout: %v", err)
		}
		if !got.RawEqual(want) {
			t.Fatal("NLJ plan layout disagrees with the executor retained-result build")
		}

		rebuiltExpression, err := plan.WithQuantifiers([]expressions.Quantifier{
			fixture.outerQ, fixture.innerQ,
		})
		if err != nil {
			t.Fatalf("WithQuantifiers: %v", err)
		}
		if rebuilt := rebuiltExpression.(*RecordQueryNestedLoopJoinPlan); requireProvidedLayout(t, rebuilt) != got {
			t.Fatal("NLJ reconstruction did not preserve its exact layout handle")
		}
	})

	t.Run("flat map lowering states null-supplying inner", func(t *testing.T) {
		ordinary := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiers(
				fixture.outerQ, fixture.innerQ, fixture.outerAlias, fixture.innerAlias,
				fixture.result, false)
		})
		nullSupplying := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
				fixture.outerQ, fixture.innerQ, fixture.outerAlias, fixture.innerAlias,
				fixture.result, false)
		})

		wantOrdinary, err := values.NewFlatOrdinalLayoutForRetainedResult(fixture.result, nil)
		if err != nil {
			t.Fatalf("ordinary retained-result layout: %v", err)
		}
		wantNull, err := values.NewFlatOrdinalLayoutForRetainedResult(
			fixture.result, []values.QuantifiedObjectValue{fixture.innerSource})
		if err != nil {
			t.Fatalf("null-supplying retained-result layout: %v", err)
		}
		ordinaryLayout := requireProvidedLayout(t, ordinary)
		nullLayout := requireProvidedLayout(t, nullSupplying)
		if !ordinaryLayout.RawEqual(wantOrdinary) || !nullLayout.RawEqual(wantNull) {
			t.Fatal("FlatMap plan layout disagrees with its explicit retained-result build")
		}
		if ordinaryLayout.RawEqual(nullLayout) || expressions.MemoEqual(ordinary, nullSupplying) {
			t.Fatal("FlatMaps with different null-supplying layouts deduplicated")
		}

		rebuiltExpression, err := nullSupplying.WithQuantifiers([]expressions.Quantifier{
			fixture.outerQ, fixture.innerQ,
		})
		if err != nil {
			t.Fatalf("WithQuantifiers: %v", err)
		}
		rebuiltLayout := requireProvidedLayout(t, rebuiltExpression.(*RecordQueryFlatMapPlan))
		if rebuiltLayout == nullLayout || !rebuiltLayout.RawEqual(nullLayout) {
			t.Fatalf(
				"FlatMap reconstruction layout identity/equality = (%t, %t), carriers = (%s, %s), hashes = (%d, %d)",
				rebuiltLayout == nullLayout, rebuiltLayout.RawEqual(nullLayout),
				nullLayout.Carrier().FlowedType(), rebuiltLayout.Carrier().FlowedType(),
				nullLayout.AliasFreeHash(), rebuiltLayout.AliasFreeHash())
		}
	})

	t.Run("pass-through chain keeps windowed join carrier evaluable", func(t *testing.T) {
		join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
			return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				fixture.outerQ, fixture.innerQ, nil, JoinInner,
				fixture.outerAlias, fixture.innerAlias, fixture.result)
		})
		pkDistinct := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
			return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(join)
		})
		filtered := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
			return NewRecordQueryPredicatesFilterPlan(pkDistinct, nil)
		})
		layout := requireProvidedLayout(t, join)
		if requireProvidedLayout(t, pkDistinct) != layout ||
			requireProvidedLayout(t, filtered) != layout ||
			filtered.GetResultValue() != layout.Carrier() {
			t.Fatal("pass-through chain detached from the windowed join carrier")
		}

		row := &planLayoutTestRow{slots: []any{"outer-a", "outer-b", "inner-a", "inner-b"}}
		binder, err := values.NewOrdinalObjectBinder(layout, row, nil, nil)
		if err != nil {
			t.Fatalf("NewOrdinalObjectBinder(windowed join): %v", err)
		}
		ctx := &values.RowEvalContext{Objects: binder}
		gotRow, err := filtered.GetResultValue().Evaluate(ctx)
		if err != nil || gotRow != row {
			t.Fatalf("windowed pass-through result Evaluate = (%T %v, %v), want row %p", gotRow, gotRow, err, row)
		}
		field, err := values.ResolveFieldOrdinals(filtered.GetResultValue(), []int{2})
		if err != nil {
			t.Fatalf("ResolveFieldOrdinals(windowed pass-through): %v", err)
		}
		gotField, err := field.Evaluate(ctx)
		if err != nil || gotField != "inner-a" {
			t.Fatalf("windowed pass-through field Evaluate = (%v, %v), want inner-a", gotField, err)
		}
	})
}

func TestFlatMapRelinkRetypesLogicalBindingsAndRebuildsNullLayout(t *testing.T) {
	t.Parallel()
	fields := []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "LABEL", Ordinal: 1, FieldType: values.NullableString},
	}
	logicalOuterType := values.NewRecordType("logical_outer", false, fields)
	logicalInnerType := values.NewRecordType("logical_inner", true, fields)
	physicalOuterType := values.NewRecordType("", false, fields)
	physicalInnerType := values.NewRecordType("", true, fields)
	outerAlias := values.NamedCorrelationIdentifier("OUTER_BINDING")
	innerAlias := values.NamedCorrelationIdentifier("INNER_BINDING")
	logicalOuter := mustOrdinalLayoutQOV(t, outerAlias, logicalOuterType)
	logicalInner := mustOrdinalLayoutQOV(t, innerAlias, logicalInnerType)
	outerID, err := values.ResolveOrdinalSeedField(logicalOuter, 0)
	if err != nil {
		t.Fatalf("outer ID: %v", err)
	}
	innerLabel, err := values.ResolveOrdinalSeedField(logicalInner, 1)
	if err != nil {
		t.Fatalf("inner LABEL: %v", err)
	}
	logicalResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "OUTER_ID", Value: outerID},
		values.RecordConstructorField{Name: "INNER_LABEL", Value: innerLabel},
	)
	outerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"OUTER"}, physicalOuterType, false)
	})
	innerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"INNER"}, physicalInnerType, false)
	})
	oldOuterQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(), expressions.FinalOfAtStage(outerPlan, expressions.StageCanonical))
	oldInnerQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(), expressions.FinalOfAtStage(innerPlan, expressions.StageCanonical))
	original := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
			oldOuterQ, oldInnerQ, outerAlias, innerAlias, logicalResult, false)
	})
	originalLayout := requireProvidedLayout(t, original)

	newOuterQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(), expressions.FinalOfAtStage(outerPlan, expressions.StageCanonical))
	newInnerQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(), expressions.FinalOfAtStage(innerPlan, expressions.StageCanonical))
	relinkedExpression, err := original.WithQuantifiers([]expressions.Quantifier{newOuterQ, newInnerQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpression.(*RecordQueryFlatMapPlan)
	if relinked.GetOuterAlias() != outerAlias || relinked.GetInnerAlias() != innerAlias {
		t.Fatalf("runtime binding aliases changed to (%s, %s)", relinked.GetOuterAlias(), relinked.GetInnerAlias())
	}
	assertResultSourceType := func(plan *RecordQueryFlatMapPlan, alias values.CorrelationIdentifier, want values.Type) {
		t.Helper()
		source, sourceErr := exactQOVForResultSource(alias, plan.GetResultValue())
		if sourceErr != nil {
			t.Fatalf("result source %s: %v", alias, sourceErr)
		}
		if source == nil || source.Correlation() != alias || !source.FlowedType().Equals(want) {
			t.Fatalf("result source %s = %#v, want exact type %s", alias, source, want)
		}
	}
	assertResultSourceType(original, outerAlias, logicalOuterType)
	assertResultSourceType(original, innerAlias, logicalInnerType)
	assertResultSourceType(relinked, outerAlias, physicalOuterType)
	assertResultSourceType(relinked, innerAlias, physicalInnerType)

	relinkedLayout := requireProvidedLayout(t, relinked)
	physicalInner := mustOrdinalLayoutQOV(t, innerAlias, physicalInnerType)
	wantLayout, err := values.NewFlatOrdinalLayoutForRetainedResult(
		relinked.GetResultValue(), []values.QuantifiedObjectValue{physicalInner})
	if err != nil {
		t.Fatalf("expected relinked layout: %v", err)
	}
	if relinkedLayout == originalLayout || !relinkedLayout.RawEqual(wantLayout) {
		t.Fatal("relink retained the stale layout or lost the null-supplying inner marker")
	}

	driftedOuterType := values.NewRecordType("drifted_outer", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	driftedOuter := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"DRIFTED"}, driftedOuterType, false)
	})
	driftedOuterQ := expressions.NamedPhysicalQuantifier(
		values.UniqueCorrelationIdentifier(), expressions.FinalOfAtStage(driftedOuter, expressions.StageCanonical))
	if rebuilt, rebuildErr := original.WithQuantifiers([]expressions.Quantifier{driftedOuterQ, newInnerQ}); rebuildErr == nil || rebuilt != nil {
		t.Fatalf("WithQuantifiers accepted outer type drift: (%T, %v)", rebuilt, rebuildErr)
	}
}

func TestFlatMapRelinkPreservesSemanticWholeObjectNullability(t *testing.T) {
	t.Parallel()

	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	nestedType := values.NewRecordType("", true, []values.Field{
		{Name: "_0", Ordinal: 0, FieldType: values.NullableLong},
	})
	physicalInnerType := values.NewRecordType("", false, []values.Field{
		{Name: "_0", Ordinal: 0, FieldType: nestedType},
	})
	semanticInnerType := values.WithNullability(physicalInnerType, true)
	outerAlias := values.NamedCorrelationIdentifier("OUTER")
	innerAlias := values.NamedCorrelationIdentifier("SCALAR_INNER")

	outerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"OUTER"}, outerType, false)
	})
	innerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"INNER"}, physicalInnerType, false)
	})
	semanticInner := mustOrdinalLayoutQOV(t, innerAlias, semanticInnerType)
	retainedWholeField, err := values.ResolveOrdinalSeedField(semanticInner, 0)
	if err != nil {
		t.Fatalf("semantic inner field: %v", err)
	}
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "SCALAR_ROW", Value: retainedWholeField})
	original := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			QuantifierOverPlan(outerPlan), QuantifierOverPlan(innerPlan),
			outerAlias, innerAlias, result, false)
	})

	rebuiltExpression, err := original.WithQuantifiers([]expressions.Quantifier{
		QuantifierOverPlan(outerPlan), QuantifierOverPlan(innerPlan),
	})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	rebuilt := rebuiltExpression.(*RecordQueryFlatMapPlan)
	rebuiltSource, err := exactQOVForResultSource(innerAlias, rebuilt.GetResultValue())
	if err != nil {
		t.Fatalf("rebuilt inner source: %v", err)
	}
	if rebuiltSource == nil || !rebuiltSource.FlowedType().Equals(semanticInnerType) {
		t.Fatalf("rebuilt inner source = %#v, want semantic nullable type %s", rebuiltSource, semanticInnerType)
	}
	rebuiltResult := rebuilt.GetResultValue().(*values.RecordConstructorValue)
	if !rebuiltResult.Fields[0].Value.Type().Equals(retainedWholeField.Type()) {
		t.Fatalf("rebuilt whole field type = %s, want unchanged %s",
			rebuiltResult.Fields[0].Value.Type(), retainedWholeField.Type())
	}
	originalSource, err := exactQOVForResultSource(innerAlias, original.GetResultValue())
	if err != nil || originalSource != semanticInner {
		t.Fatalf("source plan mutated: source = %#v, err = %v", originalSource, err)
	}

	wrongNestedType := values.NewRecordType("", true, []values.Field{
		{Name: "_0", Ordinal: 0, FieldType: values.NullableString},
	})
	wrongSemanticType := values.NewRecordType("", true, []values.Field{
		{Name: "_0", Ordinal: 0, FieldType: wrongNestedType},
	})
	wrongInner := mustOrdinalLayoutQOV(t, innerAlias, wrongSemanticType)
	wrongField, err := values.ResolveOrdinalSeedField(wrongInner, 0)
	if err != nil {
		t.Fatalf("wrong semantic inner field: %v", err)
	}
	wrongResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "SCALAR_ROW", Value: wrongField})
	wrongPlan := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			QuantifierOverPlan(outerPlan), QuantifierOverPlan(innerPlan),
			outerAlias, innerAlias, wrongResult, false)
	})
	if rebuiltWrong, rebuildErr := wrongPlan.WithQuantifiers([]expressions.Quantifier{
		QuantifierOverPlan(outerPlan), QuantifierOverPlan(innerPlan),
	}); rebuildErr == nil || rebuiltWrong != nil {
		t.Fatalf("WithQuantifiers accepted nested leaf drift: (%T, %v)", rebuiltWrong, rebuildErr)
	}
}

func TestJoinPlanLayoutIdentityIsAlphaAware(t *testing.T) {
	t.Parallel()

	outerType, innerType := retainedJoinTypes()
	left := newRetainedJoinFixture(
		t, "alpha", values.NamedCorrelationIdentifier("LEFT_OUTER"),
		values.NamedCorrelationIdentifier("LEFT_INNER"), outerType, innerType)
	right := newRetainedJoinFixture(
		t, "alpha", values.NamedCorrelationIdentifier("RIGHT_OUTER"),
		values.NamedCorrelationIdentifier("RIGHT_INNER"), outerType, innerType)

	t.Run("nested loop", func(t *testing.T) {
		leftPlan := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
			return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				left.outerQ, left.innerQ, nil, JoinLeftOuter,
				left.outerAlias, left.innerAlias, left.result)
		})
		rightPlan := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
			return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				right.outerQ, right.innerQ, nil, JoinLeftOuter,
				right.outerAlias, right.innerAlias, right.result)
		})
		assertAlphaEquivalentJoinPlans(t, leftPlan, rightPlan)
	})

	t.Run("flat map", func(t *testing.T) {
		leftPlan := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
				left.outerQ, left.innerQ, left.outerAlias, left.innerAlias,
				left.result, false)
		})
		rightPlan := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
			return NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
				right.outerQ, right.innerQ, right.outerAlias, right.innerAlias,
				right.result, false)
		})
		assertAlphaEquivalentJoinPlans(t, leftPlan, rightPlan)
	})
}

func TestJoinPlanLayoutPathParticipatesInMemoIdentity(t *testing.T) {
	t.Parallel()

	outerType, innerType := retainedJoinTypes()
	fixture := newRetainedJoinFixture(
		t, "path", values.NamedCorrelationIdentifier("OUTER"),
		values.NamedCorrelationIdentifier("INNER"), outerType, innerType)
	plan := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplyingInner(
			fixture.outerQ, fixture.innerQ, fixture.outerAlias, fixture.innerAlias,
			fixture.result, false)
	})
	original := requireProvidedLayout(t, plan)
	changedLayout, err := values.NewOrdinalLayout(
		original.Carrier(),
		[]values.OrdinalTileSpec{{Start: 0, Width: 4, Kind: values.OrdinalTileFlat}},
		[]values.OrdinalWindowSpec{
			{Source: fixture.outerSource, FieldPaths: [][]int{{1}, {0}}},
			{Source: fixture.innerSource, FieldPaths: [][]int{{2}, {3}}, NullSupplying: true},
		},
	)
	if err != nil {
		t.Fatalf("changed-path layout: %v", err)
	}
	changed := *plan
	changed.PlanExprBase, err = newPlanExprBaseForProvidedLayout(
		"path-changed FlatMap", changed.resultValue, changedLayout)
	if err != nil {
		t.Fatalf("changed-path base: %v", err)
	}
	if original.RawEqual(changedLayout) || plan.EqualsPlanWithoutChildren(&changed) ||
		expressions.MemoEqual(plan, &changed) {
		t.Fatal("different retained-source paths deduplicated")
	}
}

func TestPlanProvidedOutputLayoutWithQuantifiers(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("row", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	first := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"FIRST"}, rowType, false)
	})
	replacement := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"REPLACEMENT"}, rowType, false)
	})
	firstLayout := requireProvidedLayout(t, first)
	replacementLayout := requireProvidedLayout(t, replacement)
	if firstLayout == replacementLayout {
		t.Fatal("independent result producers unexpectedly share a layout handle")
	}

	t.Run("pass-through adopts replacement child layout", func(t *testing.T) {
		t.Parallel()
		limit := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
			return NewRecordQueryLimitPlan(first, 1, 0)
		})
		if got := requireProvidedLayout(t, limit); got != firstLayout {
			t.Fatal("pass-through constructor did not preserve the child layout handle")
		}
		if limit.GetResultValue() != firstLayout.Carrier() {
			t.Fatal("pass-through result Value is not the preserved layout's exact current carrier")
		}
		assertPassThroughResultEvaluates(t, limit, int64(41))

		rebuiltExpression, err := limit.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{replacement}))
		if err != nil {
			t.Fatalf("WithQuantifiers: %v", err)
		}
		rebuilt, ok := rebuiltExpression.(*RecordQueryLimitPlan)
		if !ok {
			t.Fatalf("WithQuantifiers returned %T, want *RecordQueryLimitPlan", rebuiltExpression)
		}
		if got := requireProvidedLayout(t, rebuilt); got != replacementLayout {
			t.Fatal("rebuilt pass-through did not adopt the replacement child layout handle")
		}
		if rebuilt.GetResultValue() != replacementLayout.Carrier() {
			t.Fatal("rebuilt pass-through result Value is not the replacement layout's carrier")
		}
		assertPassThroughResultEvaluates(t, rebuilt, int64(73))
		if got := requireProvidedLayout(t, limit); got != firstLayout {
			t.Fatal("WithQuantifiers mutated the original plan's layout")
		}
	})

	t.Run("pass-through chain keeps one evaluable carrier", func(t *testing.T) {
		t.Parallel()
		pkDistinct := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
			return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(first)
		})
		filtered := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
			return NewRecordQueryPredicatesFilterPlan(pkDistinct, nil)
		})
		if requireProvidedLayout(t, pkDistinct) != firstLayout ||
			requireProvidedLayout(t, filtered) != firstLayout {
			t.Fatal("PK-distinct/filter chain did not preserve the source carrier layout")
		}
		if pkDistinct.GetResultValue() != firstLayout.Carrier() ||
			filtered.GetResultValue() != firstLayout.Carrier() {
			t.Fatal("PK-distinct/filter chain published an unbound child-edge QOV")
		}
		assertPassThroughResultEvaluates(t, filtered, int64(99))
	})

	t.Run("output producer preserves its own layout", func(t *testing.T) {
		t.Parallel()
		result := &values.ConstantValue{Value: int64(9), Typ: values.NotNullLong}
		mapped := mustChecked(t, func() (*RecordQueryMapPlan, error) {
			return NewRecordQueryMapPlan(first, result)
		})
		outputLayout := requireProvidedLayout(t, mapped)

		rebuiltExpression, err := mapped.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{replacement}))
		if err != nil {
			t.Fatalf("WithQuantifiers: %v", err)
		}
		rebuilt, ok := rebuiltExpression.(*RecordQueryMapPlan)
		if !ok {
			t.Fatalf("WithQuantifiers returned %T, want *RecordQueryMapPlan", rebuiltExpression)
		}
		if got := requireProvidedLayout(t, rebuilt); got != outputLayout {
			t.Fatal("output-producing rebuild replaced its node-owned layout")
		}
	})
}

func TestPlanProvidedOutputLayoutDynamicAndMalformedAreLoud(t *testing.T) {
	t.Parallel()

	t.Run("exact AnyRecord is valid but requires physical refinement", func(t *testing.T) {
		t.Parallel()
		plan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan(
				[]string{"T", "U"}, values.NewAnyRecordType(false), false)
		})
		firstResult := plan.GetResultValue()
		if firstResult == nil || firstResult != plan.GetResultValue() {
			t.Fatal("AnyRecord plan did not retain one stable exact result Value")
		}
		assertUnavailableLayoutCode(t, plan, OrdinalLayoutDynamicCarrier)

		passThrough := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
			return NewRecordQueryLimitPlan(plan, 1, 0)
		})
		assertUnavailableLayoutCode(t, passThrough, OrdinalLayoutDynamicCarrier)
	})

	t.Run("unconstructed base is malformed", func(t *testing.T) {
		t.Parallel()
		layout, err := (PlanExprBase{}).ProvidedOutputLayout()
		if layout != nil {
			t.Fatalf("malformed base returned layout %T", layout)
		}
		var unavailable *OrdinalLayoutUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Code != OrdinalLayoutMalformedPlan {
			t.Fatalf("malformed base error = %T %v, want code %v", err, err, OrdinalLayoutMalformedPlan)
		}
	})

	t.Run("unknown result type is rejected at construction", func(t *testing.T) {
		t.Parallel()
		plan, err := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		if err == nil || plan != nil {
			t.Fatalf("scan(UnknownType) = (%#v, %v), want (nil, error)", plan, err)
		}
	})
}

func requireProvidedLayout(t testing.TB, plan RecordQueryPlan) values.OrdinalLayout {
	t.Helper()
	layout, err := plan.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("%T ProvidedOutputLayout: %v", plan, err)
	}
	if layout == nil {
		t.Fatalf("%T ProvidedOutputLayout returned nil, nil", plan)
	}
	return layout
}

func assertUnavailableLayoutCode(t testing.TB, plan RecordQueryPlan, want OrdinalLayoutAvailabilityCode) {
	t.Helper()
	layout, err := plan.ProvidedOutputLayout()
	if layout != nil {
		t.Fatalf("%T dynamic layout = %T, want nil", plan, layout)
	}
	var unavailable *OrdinalLayoutUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code != want {
		t.Fatalf("%T layout error = %T %v, want code %v", plan, err, err, want)
	}
}

type retainedJoinFixture struct {
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	outerSource values.QuantifiedObjectValue
	innerSource values.QuantifiedObjectValue
	outerQ      expressions.Quantifier
	innerQ      expressions.Quantifier
	result      values.Value
}

func retainedJoinTypes() (*values.RecordType, *values.RecordType) {
	fields := []values.Field{
		{Name: "FIRST", Ordinal: 0, FieldType: values.NullableString},
		{Name: "SECOND", Ordinal: 1, FieldType: values.NullableString},
	}
	return values.NewRecordType("layout_outer", false, fields),
		values.NewRecordType("layout_inner", true, fields)
}

func newRetainedJoinFixture(
	t testing.TB,
	scanPrefix string,
	outerAlias, innerAlias values.CorrelationIdentifier,
	outerType, innerType *values.RecordType,
) retainedJoinFixture {
	t.Helper()
	outerSource := mustOrdinalLayoutQOV(t, outerAlias, outerType)
	innerSource := mustOrdinalLayoutQOV(t, innerAlias, innerType)
	resultFields := make([]values.RecordConstructorField, 0, 4)
	for sourceIndex, source := range []values.QuantifiedObjectValue{outerSource, innerSource} {
		for ordinal := 0; ordinal < 2; ordinal++ {
			field, err := values.ResolveOrdinalSeedField(source, ordinal)
			if err != nil {
				t.Fatalf("ResolveOrdinalSeedField(source=%d, ordinal=%d): %v", sourceIndex, ordinal, err)
			}
			resultFields = append(resultFields, values.RecordConstructorField{
				Name:  []string{"OUTER_FIRST", "OUTER_SECOND", "INNER_FIRST", "INNER_SECOND"}[sourceIndex*2+ordinal],
				Value: field,
			})
		}
	}
	outerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{scanPrefix + "_OUTER"}, outerType, false)
	})
	innerPlan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{scanPrefix + "_INNER"}, innerType, false)
	})
	return retainedJoinFixture{
		outerAlias:  outerAlias,
		innerAlias:  innerAlias,
		outerSource: outerSource,
		innerSource: innerSource,
		outerQ: expressions.NamedForEachQuantifier(
			outerAlias, expressions.FinalOf(outerPlan)),
		innerQ: expressions.NamedForEachQuantifier(
			innerAlias, expressions.FinalOf(innerPlan)),
		result: values.NewRecordConstructorValue(resultFields...),
	}
}

func mustOrdinalLayoutQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", alias.Name(), err)
	}
	return qov
}

func assertAlphaEquivalentJoinPlans(t testing.TB, left, right RecordQueryPlan) {
	t.Helper()
	if left.EqualsPlanWithoutChildren(right) {
		t.Fatal("raw plan equality ignored independently named source aliases")
	}
	if left.HashCodeWithoutChildren() != right.HashCodeWithoutChildren() {
		t.Fatalf("alpha-equivalent hashes differ: %d vs %d",
			left.HashCodeWithoutChildren(), right.HashCodeWithoutChildren())
	}
	leftExpression := left.(expressions.RelationalExpression)
	rightExpression := right.(expressions.RelationalExpression)
	if !expressions.MemoEqual(leftExpression, rightExpression) {
		t.Fatal("independently constructed alpha-equivalent join plans did not deduplicate")
	}
}

type planLayoutTestRow struct{ slots []any }

func (r *planLayoutTestRow) Get(ordinal int) (any, bool) {
	if r == nil || ordinal < 0 || ordinal >= len(r.slots) {
		return nil, false
	}
	return r.slots[ordinal], true
}

func assertPassThroughResultEvaluates(t testing.TB, plan RecordQueryPlan, fieldValue int64) {
	t.Helper()
	layout := requireProvidedLayout(t, plan)
	row := &planLayoutTestRow{slots: []any{fieldValue}}
	binder, err := values.NewOrdinalObjectBinder(layout, row, nil, nil)
	if err != nil {
		t.Fatalf("NewOrdinalObjectBinder: %v", err)
	}
	ctx := &values.RowEvalContext{Objects: binder}
	gotRow, err := plan.GetResultValue().Evaluate(ctx)
	if err != nil {
		t.Fatalf("pass-through result Evaluate: %v", err)
	}
	if gotRow != row {
		t.Fatalf("pass-through result Evaluate = %T %v, want emitted row %p", gotRow, gotRow, row)
	}
	field, err := values.ResolveFieldOrdinals(plan.GetResultValue(), []int{0})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(pass-through result): %v", err)
	}
	gotField, err := field.Evaluate(ctx)
	if err != nil {
		t.Fatalf("downstream field Evaluate: %v", err)
	}
	if gotField != fieldValue {
		t.Fatalf("downstream field Evaluate = %v, want %v", gotField, fieldValue)
	}
}
