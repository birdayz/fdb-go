package plans

import (
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestStreamingAggregationPlan_RelinkRebasesProgramsAndRebuildsResult(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("stream_agg_old")
	newAlias := values.NamedCorrelationIdentifier("stream_agg_new")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	key := testFieldIn(t, rowType, oldAlias.Name(), "K")
	operand := testFieldIn(t, rowType, oldAlias.Name(), "V")
	aggregates := []expressions.AggregateSpec{
		{
			Function:       expressions.AggCount,
			Alias:          "row_count",
			OperandName:    "*",
			OperandIntType: values.TypeCodeInt,
		},
		{
			Function:       expressions.AggSum,
			Operand:        operand,
			Alias:          "value_sum",
			OperandName:    "V",
			OperandIntType: values.TypeCodeLong,
		},
	}
	original := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlanFromQuantifier(
			oldQ, []values.Value{key}, aggregates)
	})

	relinkedExpr, err := original.WithChildren([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryStreamingAggregationPlan)
	if relinked.GetInnerQuantifier().GetAlias() != newAlias {
		t.Fatalf("inner alias = %s, want %s", relinked.GetInnerQuantifier().GetAlias(), newAlias)
	}
	childLayout, err := scan.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("scan output layout: %v", err)
	}
	requireStreamingAggregationRoot(t, relinked.GetGroupingKeys()[0], childLayout.Carrier())
	requireStreamingAggregationRoot(t, relinked.GetAggregates()[1].Operand, childLayout.Carrier())

	gotAggregates := relinked.GetAggregates()
	if gotAggregates[0].Operand != nil {
		t.Fatalf("COUNT(*) operand = %v, want nil", gotAggregates[0].Operand)
	}
	for i := range aggregates {
		if gotAggregates[i].Function != aggregates[i].Function ||
			gotAggregates[i].Alias != aggregates[i].Alias ||
			gotAggregates[i].OperandName != aggregates[i].OperandName ||
			gotAggregates[i].OperandIntType != aggregates[i].OperandIntType {
			t.Fatalf("aggregate %d metadata = %#v, want %#v", i, gotAggregates[i], aggregates[i])
		}
	}
	if relinked.PlanExprBase.resultValue != relinked.resultValue {
		t.Fatal("rebuilt base and aggregate result Value disagree")
	}
	if relinked.resultValue == original.resultValue ||
		relinked.PlanExprBase.resultValue == original.PlanExprBase.resultValue {
		t.Fatal("relink retained the original aggregate result Value/base")
	}
	if !relinked.GetResultType().Equals(original.GetResultType()) {
		t.Fatalf("rebuilt result type = %s, want %s", relinked.GetResultType(), original.GetResultType())
	}

	// The checked rewrite is copy-on-write: neither retained program on the
	// source plan may acquire the replacement alias.
	requireStreamingAggregationRoot(t, original.GetGroupingKeys()[0], childLayout.Carrier())
	requireStreamingAggregationRoot(t, original.GetAggregates()[1].Operand, childLayout.Carrier())
}

func TestStreamingAggregationPlan_RelinkFreezesComputedKeyOutputSchema(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("computed_key_source")
	newAlias := values.NamedCorrelationIdentifier("computed_key_relinked")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	key := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  testFieldIn(t, rowType, oldAlias.Name(), "V"),
		Right: &values.ConstantValue{Value: int64(100), Typ: values.NotNullLong},
	}
	original := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlanFromQuantifier(
			oldQ, []values.Value{key}, []expressions.AggregateSpec{{Function: expressions.AggCount}})
	})
	wantType := original.OutputRecordType()
	wantNames := recordFieldNames(wantType)

	relinkedExpr, err := original.WithQuantifiers([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryStreamingAggregationPlan)
	if got := recordFieldNames(relinked.OutputRecordType()); !slices.Equal(got, wantNames) {
		t.Fatalf("relinked output names = %v, want frozen %v", got, wantNames)
	}
	if !relinked.OutputRecordType().Equals(wantType) {
		t.Fatalf("relinked output type = %s, want frozen %s", relinked.OutputRecordType(), wantType)
	}
	if got := values.ColumnNameValue(relinked.GetGroupingKeys()[0]); got == values.ColumnNameValue(key) {
		t.Fatalf("mutation control did not change the rebased evaluation rendering: %q", got)
	}
	if got := expressions.GroupByOutputColumnNames(relinked.GetGroupingKeys(), relinked.GetAggregates()); slices.Equal(got, wantNames) {
		t.Fatalf("mutation control: re-derived schema unexpectedly equals frozen names %v", wantNames)
	}
}

// TestStreamingAggregationPlan_RelinkPreservesSameAliasSourceWindowUntilMaterializer
// pins the duplicate-name outer-join hazard at the physical child relink.
//
// During exploration the aggregate edge conventionally carries the rightmost
// join-leg alias (B). That whole edge and B's source window therefore have the
// same correlation but different exact types. Relinking the edge by alias alone
// changes B.ID into an edge-rooted ordinal 0 before the selected sort can map
// the source window into the merged row. Since both legs call their first field
// ID, that mutation silently reads A.ID. The exact edge rewrite must retain the
// narrower B window until the selected sort's producer lineage maps it to merged
// ordinal 2.
func TestStreamingAggregationPlan_RelinkPreservesSameAliasSourceWindowUntilMaterializer(t *testing.T) {
	t.Parallel()
	outerAlias := values.NamedCorrelationIdentifier("A")
	innerAlias := values.NamedCorrelationIdentifier("B")
	outerType := values.NewRecordType("A", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NullableLong},
	})
	innerType := values.NewRecordType("B", true, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "K", Ordinal: 1, FieldType: values.NullableLong},
	})
	outerSource := mustOrdinalLayoutQOV(t, outerAlias, outerType)
	innerSource := mustOrdinalLayoutQOV(t, innerAlias, innerType)
	seedField := func(source values.QuantifiedObjectValue, ordinal int) values.Value {
		t.Helper()
		field, err := values.ResolveOrdinalSeedField(source, ordinal)
		if err != nil {
			t.Fatalf("ResolveOrdinalSeedField(%s, %d): %v", source.Correlation(), ordinal, err)
		}
		return field
	}
	joinResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: seedField(outerSource, 0)},
		values.RecordConstructorField{Name: "K", Value: seedField(outerSource, 1)},
		values.RecordConstructorField{Name: "ID", Value: seedField(innerSource, 0)},
		values.RecordConstructorField{Name: "K", Value: seedField(innerSource, 1)},
	)
	outerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"A"}, outerType, false)
	})
	innerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"B"}, innerType, false)
	})
	outerQ := expressions.NamedPhysicalQuantifier(
		outerAlias, expressions.FinalOfAtStage(outerScan, expressions.StageCanonical))
	innerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(innerScan, expressions.StageCanonical))
	join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
		return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
			outerQ, innerQ, nil, JoinLeftOuter,
			outerAlias, innerAlias, joinResult)
	})
	sortPlan := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(join, nil)
	})

	// InitialOf is the live exploratory edge shape: its sole member is not yet
	// a concrete selection and may still gain alternatives.
	oldQ := expressions.NamedPhysicalQuantifier(innerAlias, expressions.InitialOf(sortPlan))
	oldEdge, err := oldQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("old aggregate edge: %v", err)
	}
	wholeEdgeK, err := values.ResolveFieldOrdinals(oldEdge, []int{1})
	if err != nil {
		t.Fatalf("whole-edge K: %v", err)
	}
	innerID, err := values.ResolveFieldOrdinals(innerSource, []int{0})
	if err != nil {
		t.Fatalf("inner-source ID: %v", err)
	}
	original := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlanFromQuantifier(
			oldQ,
			[]values.Value{wholeEdgeK},
			[]expressions.AggregateSpec{{Function: expressions.AggCount, Operand: innerID}},
		)
	})
	originalOperand := original.GetAggregates()[0].Operand
	originalField, ok := values.AsFieldValue(originalOperand)
	if !ok || originalField.ChildValue() != innerSource {
		t.Fatalf("exploratory operand = %T/%v, want retained exact B source %p",
			originalOperand, originalField, innerSource)
	}

	newAlias := values.NamedCorrelationIdentifier("selected_aggregate_input")
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(sortPlan, expressions.StageCanonical))
	relinkedExpr, err := original.WithQuantifiers([]expressions.Quantifier{newQ})
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryStreamingAggregationPlan)
	sortLayout := requireProvidedLayout(t, sortPlan)
	assertCurrentPath := func(label string, value values.Value, wantOrdinal int) {
		t.Helper()
		field, isField := values.AsFieldValue(value)
		if !isField {
			t.Fatalf("%s = %T, want FieldValue", label, value)
		}
		if field.ChildValue() != sortLayout.Carrier() {
			t.Fatalf("%s root = %p (%v), want selected sort carrier %p",
				label, field.ChildValue(), field.ChildValue(), sortLayout.Carrier())
		}
		if got := field.Path().Ordinals(); !slices.Equal(got, []int{wantOrdinal}) {
			t.Fatalf("%s path = %v, want [%d]", label, got, wantOrdinal)
		}
		if !field.Path().IsFrontierPinned() {
			t.Fatalf("%s is rooted at the selected materializer but remains leg-relative", label)
		}
	}
	// Whole-edge K proves the declared edge still moves normally. B.ID is the
	// mutation-sensitive assertion: alias-only rebasing leaves it at ordinal 0;
	// exact source preservation lets the sort map it past A's two columns.
	assertCurrentPath("whole-edge K", relinked.GetGroupingKeys()[0], 1)
	assertCurrentPath("B.ID", relinked.GetAggregates()[0].Operand, 2)
	originalField, originalOK := values.AsFieldValue(innerID)
	if !originalOK {
		t.Fatal("source B.ID is not an exact FieldValue")
	}
	if originalField.Path().IsFrontierPinned() {
		t.Fatal("relink mutated the source B.ID into a frontier-owned field")
	}
}

// TestStreamingAggregationPlan_RelinkKeepsExternalRootsAndRejectsMismatchedOnes
// pins which roots a relink moves, keeps and refuses. The declared input edge
// moves onto the new input. A root under a correlation the input neither is nor
// retains a window for is an enclosing block's row — Java's grouping keys and
// aggregate operands may be correlated to any outer quantifier, and its
// translateCorrelations leaves a root outside the translation map as it is — so
// it is kept, untouched, and stays in the plan's correlations for the enclosing
// FlatMap to bind; refusing it failed `MAX(a.x)` in a lateral derived table over
// `a` at extraction (TestFDB_GroupedSubqueryProjectsAnOuterColumn). A root that
// claims the input, or one of the windows the input retains, with another exact
// type is refused: rebasing it would bind a different row shape.
func TestStreamingAggregationPlan_RelinkKeepsExternalRootsAndRejectsMismatchedOnes(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAlias := values.NamedCorrelationIdentifier("stream_agg_guard_old")
	newAlias := values.NamedCorrelationIdentifier("stream_agg_guard_new")
	oldQ := expressions.NamedPhysicalQuantifier(
		oldAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	newQ := expressions.NamedPhysicalQuantifier(
		newAlias, expressions.FinalOfAtStage(scan, expressions.StageCanonical))

	t.Run("external alias", func(t *testing.T) {
		externalAlias := values.NamedCorrelationIdentifier("stream_agg_external")
		externalKey := testFieldIn(t, rowType, externalAlias.Name(), "K")
		externalOperand := testFieldIn(t, rowType, externalAlias.Name(), "K")
		plan := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
			return NewRecordQueryStreamingAggregationPlanFromQuantifier(oldQ, []values.Value{externalKey},
				[]expressions.AggregateSpec{{Function: expressions.AggMax, Operand: externalOperand}})
		})
		relinkedExpr, err := plan.WithQuantifiers([]expressions.Quantifier{newQ})
		if err != nil {
			t.Fatalf("WithQuantifiers refused an external root: %v", err)
		}
		relinked := relinkedExpr.(*RecordQueryStreamingAggregationPlan)
		if got := relinked.GetGroupingKeys()[0]; got != externalKey {
			t.Errorf("the external key was rewritten to %v", got)
		}
		if got := relinked.GetAggregates()[0].Operand; got != externalOperand {
			t.Errorf("the external operand was rewritten to %v", got)
		}
		if _, ok := relinked.GetCorrelatedToWithoutChildren()[externalAlias]; !ok {
			t.Errorf("the relinked plan no longer reports %s among its correlations: %v",
				externalAlias.Name(), relinked.GetCorrelatedToWithoutChildren())
		}
	})

	t.Run("mismatched exact type", func(t *testing.T) {
		wrongType := values.NewRecordType("wrong_stream_agg_row", false, []values.Field{
			{Name: "K", FieldType: values.NotNullLong, Ordinal: 0},
		})
		wrongKey := testFieldIn(t, wrongType, oldAlias.Name(), "K")
		plan := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
			return NewRecordQueryStreamingAggregationPlanFromQuantifier(oldQ, []values.Value{wrongKey}, nil)
		})
		if _, err := plan.WithQuantifiers([]expressions.Quantifier{newQ}); err == nil {
			t.Fatal("WithQuantifiers accepted an input root with the wrong exact type")
		} else if !strings.Contains(err.Error(), "disagrees with input edge type") {
			t.Fatalf("WithQuantifiers error = %v, want exact-type diagnostic", err)
		}
	})

	t.Run("retained window with another type", func(t *testing.T) {
		outerAlias := values.NamedCorrelationIdentifier("A")
		innerAlias := values.NamedCorrelationIdentifier("B")
		sourceType := func(name string) *values.RecordType {
			return values.NewRecordType(name, true, []values.Field{
				{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
				{Name: "K", Ordinal: 1, FieldType: values.NullableLong},
			})
		}
		outerType, innerType := sourceType("A"), sourceType("B")
		outerSource := mustOrdinalLayoutQOV(t, outerAlias, outerType)
		innerSource := mustOrdinalLayoutQOV(t, innerAlias, innerType)
		seedField := func(source values.QuantifiedObjectValue, ordinal int) values.Value {
			t.Helper()
			field, err := values.ResolveOrdinalSeedField(source, ordinal)
			if err != nil {
				t.Fatalf("ResolveOrdinalSeedField(%s, %d): %v", source.Correlation(), ordinal, err)
			}
			return field
		}
		joinResult := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: seedField(outerSource, 0)},
			values.RecordConstructorField{Name: "K", Value: seedField(outerSource, 1)},
			values.RecordConstructorField{Name: "ID", Value: seedField(innerSource, 0)},
			values.RecordConstructorField{Name: "K", Value: seedField(innerSource, 1)},
		)
		outerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"A"}, outerType, false)
		})
		innerScan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{"B"}, innerType, false)
		})
		join := mustChecked(t, func() (*RecordQueryNestedLoopJoinPlan, error) {
			return NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				expressions.NamedPhysicalQuantifier(outerAlias, expressions.FinalOfAtStage(outerScan, expressions.StageCanonical)),
				expressions.NamedPhysicalQuantifier(innerAlias, expressions.FinalOfAtStage(innerScan, expressions.StageCanonical)),
				nil, JoinLeftOuter, outerAlias, innerAlias, joinResult)
		})
		retained := false
		for _, source := range requireProvidedLayout(t, join).WindowSources() {
			retained = retained || source.Correlation() == outerAlias
		}
		if !retained {
			t.Fatal("the join retains no A window; this arm would test the external one")
		}
		// The arm is driven directly: a constructor over a selected input
		// re-anchors an A-rooted value onto the input's carrier by name, so a
		// retained-window root reaches the relink only from a plan built over a
		// live edge that has a selection by the time it is relinked.
		selectedQ := expressions.NamedPhysicalQuantifier(
			values.NamedCorrelationIdentifier("stream_agg_selected"),
			expressions.FinalOfAtStage(join, expressions.StageCanonical))
		edge, err := selectedQ.RequireFlowedObjectValue()
		if err != nil {
			t.Fatal(err)
		}
		rightA := testFieldIn(t, outerType, outerAlias.Name(), "K")
		wrongA := testFieldIn(t, values.NewRecordType("A", true, []values.Field{
			{Name: "K", Ordinal: 0, FieldType: values.NullableLong},
		}), outerAlias.Name(), "K")
		external := testFieldIn(t, outerType, "stream_agg_external", "K")
		if err := validateStreamingAggregationOldInputRoots(rightA, edge, selectedQ); err != nil {
			t.Errorf("the retained A window at its own type was refused: %v", err)
		}
		if err := validateStreamingAggregationOldInputRoots(external, edge, selectedQ); err != nil {
			t.Errorf("an external root was refused: %v", err)
		}
		if err := validateStreamingAggregationOldInputRoots(wrongA, edge, selectedQ); err == nil {
			t.Error("a root under the retained A window's correlation with another type was accepted")
		} else if !strings.Contains(err.Error(), "foreign to input edge") {
			t.Errorf("error = %v, want the retained-window diagnostic", err)
		}
	})
}

func requireStreamingAggregationRoot(
	t *testing.T,
	value values.Value,
	want values.QuantifiedObjectValue,
) {
	t.Helper()
	count := 0
	values.WalkValue(value, func(node values.Value) bool {
		qov, ok := values.AsQuantifiedObjectValue(node)
		if !ok {
			return true
		}
		count++
		if qov != want {
			t.Errorf("root handle = %p (%s), want exact layout carrier %p (%s)",
				qov, qov.Correlation(), want, want.Correlation())
		}
		if !qov.FlowedType().Equals(want.FlowedType()) {
			t.Errorf("root type = %s, want %s", qov.FlowedType(), want.FlowedType())
		}
		return true
	})
	if count != 1 {
		t.Fatalf("exact QOV root count = %d, want 1", count)
	}
}

// recordFieldNames reads a stated row's column names in ordinal order — the
// names the aggregate cursor keys its PositionalRow by.
func recordFieldNames(rt *values.RecordType) []string {
	if rt == nil {
		return nil
	}
	names := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		names[i] = f.Name
	}
	return names
}
