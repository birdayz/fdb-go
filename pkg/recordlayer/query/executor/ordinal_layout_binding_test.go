package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type hostileOrdinalCarrier struct {
	layout values.OrdinalLayout
}

func (h *hostileOrdinalCarrier) OrdinalLayout() values.OrdinalLayout { return h.layout }
func (*hostileOrdinalCarrier) Get(int) (any, bool)                   { return nil, false }

type executorLayoutCode interface {
	Code() values.ResolutionErrorCode
}

func mustExecutorCurrentLayout(t testing.TB, typ values.Type, windows []values.OrdinalWindowSpec) values.OrdinalLayout {
	t.Helper()
	// Current QOVs are deliberately not externally mintable. A zero-tile record
	// layout is obtained through a tiny owner-shaped RC-free test hook in the
	// values package only in same-package tests, so executor fixtures derive it
	// from a one-field source layout factory below.
	return mustExecutorLayoutForType(t, typ, windows)
}

func mustExecutorLayoutForType(t testing.TB, typ values.Type, windows []values.OrdinalWindowSpec) values.OrdinalLayout {
	t.Helper()
	layout, err := values.NewOrdinalLayoutForCarrierType(typ, []values.OrdinalTileSpec{{
		Start: 0, Width: len(typ.(*values.RecordType).Fields), Kind: values.OrdinalTileFlat,
	}}, windows)
	if err != nil {
		t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
	}
	return layout
}

func TestAttachProvidedOutputLayoutPublishesExactPlanProperty(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	plan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("scan plan: %v", err)
	}
	layout, err := plan.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	original := &PositionalRow{Type: rowType, Slots: []any{int64(42)}}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{{Positional: original}}))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("next = (%v, %v), want row", result, err)
	}
	attached := result.GetValue().Positional
	if attached == original {
		t.Fatal("layout publication mutated/reused the child row instead of copying")
	}
	if original.Layout != nil {
		t.Fatal("layout publication mutated the input row")
	}
	if attached.Layout != layout {
		t.Fatal("emitted row did not retain the exact plan layout handle")
	}
	evalContext := rowEvalContextForPositional(attached, nil)
	got, err := layout.Carrier().Evaluate(evalContext)
	if err != nil || got != attached {
		t.Fatalf("current carrier Evaluate = (%T, %v), want attached row", got, err)
	}
}

func TestAttachProvidedOutputLayoutRejectsWrongRuntimeShape(t *testing.T) {
	t.Parallel()

	declared := values.NewRecordType("declared", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	wrong := values.NewRecordType("wrong", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullString, Ordinal: 0},
	})
	plan, err := plans.NewRecordQueryScanPlan([]string{"T"}, declared, false)
	if err != nil {
		t.Fatalf("scan plan: %v", err)
	}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList([]QueryResult{{
		Positional: &PositionalRow{Type: wrong, Slots: []any{"wrong"}},
	}}))
	if err != nil {
		t.Fatalf("construct adapter: %v", err)
	}
	result, err := cursor.OnNext(context.Background())
	if err == nil || result.HasNext() {
		t.Fatalf("wrong row next = (%v, %v), want loud layout mismatch", result, err)
	}
}

func TestOrdinalLayoutRowContextBindsExactCurrentAndSourceWindow(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: values.NullableLong},
	}}
	source, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("source"),
		&values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NotNullLong}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	layout := mustExecutorCurrentLayout(t, rowType, []values.OrdinalWindowSpec{{
		Source: source, FieldPaths: [][]int{{0}},
	}})
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatal(err)
	}
	row.Slots[0], row.Slots[1] = int64(7), nil
	context, err := ordinalLayoutRowContext(layout, row, nil, &values.RowEvalContext{})
	if err != nil {
		t.Fatal(err)
	}
	current, err := layout.Carrier().Evaluate(context)
	if err != nil || current != row {
		t.Fatalf("current = (%T, %v), want exact positional row", current, err)
	}
	field, err := values.ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	got, err := field.Evaluate(context)
	if err != nil || got != int64(7) {
		t.Fatalf("source field = (%v, %v), want (7, nil)", got, err)
	}
}

func TestFrontierLayoutDoesNotFallBackForAnOmittedSource(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	sourceType := &values.RecordType{Fields: []values.Field{{Name: "V", Ordinal: 0, FieldType: values.NotNullLong}}}
	sourceA, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A"), sourceType)
	if err != nil {
		t.Fatal(err)
	}
	sourceB, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B"), sourceType)
	if err != nil {
		t.Fatal(err)
	}
	layout := mustExecutorLayoutForType(t, rowType, []values.OrdinalWindowSpec{{
		Source: sourceA, FieldPaths: [][]int{{0}},
	}})
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatal(err)
	}
	row.Slots[0], row.Slots[1] = int64(10), int64(20)
	ctx, err := frontierRowContext(row, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	fieldA, err := values.ResolveFieldOrdinals(sourceA, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fieldA.Evaluate(ctx)
	if err != nil || got != int64(10) {
		t.Fatalf("declared source A = (%v, %v), want (10, nil)", got, err)
	}
	fieldB, err := values.ResolveFieldOrdinals(sourceB, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	got, err = fieldB.Evaluate(ctx)
	var coded executorLayoutCode
	if got != nil || !errors.As(err, &coded) || coded.Code() != values.UnboundCorrelation {
		t.Fatalf("omitted source B = (%v, %v), want UnboundCorrelation (never ambient slot 0/1)", got, err)
	}
}

func TestOrdinalLayoutRowContextRejectsForeignAndEqualButDistinctLayouts(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NotNullLong}}}
	first := mustExecutorCurrentLayout(t, rowType, nil)
	second := mustExecutorCurrentLayout(t, rowType, nil)
	if !first.RawEqual(second) {
		t.Fatal("independently built control layouts must be raw-equal")
	}
	row, err := NewLayoutPositionalRow(rowType, first)
	if err != nil {
		t.Fatal(err)
	}
	for name, carrier := range map[string]any{
		"foreign embedded view": &hostileOrdinalCarrier{layout: first},
		"distinct equal layout": row,
	} {
		name, carrier := name, carrier
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			layout := first
			if name == "distinct equal layout" {
				layout = second
			}
			context, err := ordinalLayoutRowContext(layout, carrier, nil, nil)
			if context != nil {
				t.Fatalf("failure returned context %T", context)
			}
			var coded executorLayoutCode
			if !errors.As(err, &coded) || (coded.Code() != values.LayoutForeignValue && coded.Code() != values.LayoutCarrierMismatch) {
				t.Fatalf("error = %v, want foreign/carrier mismatch", err)
			}
		})
	}
}

func TestScalarLayoutRowContextBindsExactCurrentAndDeclaredEdges(t *testing.T) {
	t.Parallel()

	layout, err := values.NewScalarOrdinalLayoutForCarrierType(values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("ELEMENT"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	row := scalarPositionalRowOfType(int64(42), values.NotNullLong)
	ctx, err := scalarLayoutRowContext(layout, row, nil, edge)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]values.Value{
		"current carrier": layout.Carrier(),
		"declared edge":   edge,
	} {
		got, evalErr := value.Evaluate(ctx)
		if evalErr != nil || got != int64(42) {
			t.Fatalf("%s Evaluate = (%v, %v), want (42, nil)", name, got, evalErr)
		}
	}
	wrongEdge, err := values.NewQuantifiedObjectValue(edge.Correlation(), values.NotNullInt)
	if err != nil {
		t.Fatal(err)
	}
	if got, evalErr := wrongEdge.Evaluate(ctx); got != nil || evalErr == nil {
		t.Fatalf("wrong-type edge Evaluate = (%v, %v), want loud rejection", got, evalErr)
	}
}

func TestBindOuterLayoutSourcesSeparatesRetainedScalarFromWholeRow(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("ELEMENT")
	scalar, err := values.NewQuantifiedObjectValue(alias, values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "OUTER_ID",
			Value: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
		},
		values.RecordConstructorField{Name: "ELEMENT", Value: scalar},
	)
	layout, err := values.NewFlatOrdinalLayoutForRetainedResult(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	rowType, ok := layout.Carrier().FlowedType().(*values.RecordType)
	if !ok || rowType == nil {
		t.Fatalf("layout carrier type = %T, want exact record", layout.Carrier().FlowedType())
	}
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatal(err)
	}
	row.Slots[0], row.Slots[1] = int64(7), int64(42)
	whole, err := values.NewQuantifiedObjectValue(alias, rowType)
	if err != nil {
		t.Fatal(err)
	}
	base, err := EmptyEvaluationContext().withQuantifiedBinding(whole, row, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, evalErr := scalar.Evaluate(base); got != nil || evalErr == nil {
		t.Fatalf("scalar before retained-window bind = (%v, %v), want loud type rejection", got, evalErr)
	}
	bound, err := bindOuterLayoutSources(base, row)
	if err != nil {
		t.Fatal(err)
	}
	if got, evalErr := whole.Evaluate(bound); evalErr != nil || got != row {
		t.Fatalf("whole row Evaluate = (%T, %v), want original row", got, evalErr)
	}
	if got, evalErr := scalar.Evaluate(bound); evalErr != nil || got != int64(42) {
		t.Fatalf("retained scalar Evaluate = (%v, %v), want (42, nil)", got, evalErr)
	}
	if got, evalErr := scalar.Evaluate(base); got != nil || evalErr == nil {
		t.Fatalf("retained-window bind mutated source context: (%v, %v)", got, evalErr)
	}
}

// TestEvaluationObjectBinderCarriesExplicitAbsenceThroughChildLayout pins the
// adapter boundary used by a correlated child with its own OrdinalLayout. A
// FirstOrDefault outer can bind a statically non-nullable QOV to SQL NULL only
// with an exact absence proof; the child binder must delegate that proof along
// with the nil value. A same-spelled foreign type cannot borrow it.
func TestEvaluationObjectBinderCarriesExplicitAbsenceThroughChildLayout(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("ABSENT_OUTER")
	outerType := values.NewRecordType("OUTER", false, []values.Field{{
		Name: "V", Ordinal: 0, FieldType: values.NullableLong,
	}})
	outer := mustTestQOV(t, outerAlias, outerType)
	absent, err := EmptyEvaluationContext().withQuantifiedBinding(outer, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	localType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	localLayout := mustExecutorCurrentLayout(t, localType, nil)
	localRow, err := NewLayoutPositionalRow(localType, localLayout)
	if err != nil {
		t.Fatal(err)
	}
	localRow.Slots[0] = int64(1)
	binder, err := values.NewOrdinalObjectBinder(
		localLayout, localRow, nil, &evaluationObjectBinder{base: absent})
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := binder.GetQuantifiedBinding(outer)
	if err != nil || !present || got != nil {
		t.Fatalf("absent outer lookup = (%v, %t, %v), want (nil, true, nil)", got, present, err)
	}
	proof, ok := binder.(values.ExplicitNullQuantifiedObjectBinder)
	if !ok {
		t.Fatal("ordinal binder does not expose explicit-null proof")
	}
	if explicit, proofErr := proof.IsExplicitNullQuantifiedBinding(outer); proofErr != nil || !explicit {
		t.Fatalf("absent outer proof = (%t, %v), want (true, nil)", explicit, proofErr)
	}
	if evaluated, evalErr := outer.Evaluate(&values.RowEvalContext{Objects: binder}); evalErr != nil || evaluated != nil {
		t.Fatalf("absent outer Evaluate = (%v, %v), want bound SQL NULL", evaluated, evalErr)
	}

	// FOREIGN is a different row SHAPE under the same alias — a RecordName does
	// not separate two types (Java's Type.Record.equals), so renaming alone
	// would hand the lookup the outer's own row and prove nothing.
	foreignType := values.NewRecordType("FOREIGN", false, []values.Field{{
		Name: "FOREIGN_V", Ordinal: 0, FieldType: values.NullableLong,
	}})
	foreign := mustTestQOV(t, outerAlias, foreignType)
	if got, present, lookupErr := binder.GetQuantifiedBinding(foreign); got != nil || present || lookupErr == nil {
		t.Fatalf("foreign lookup = (%v, %t, %v), want loud exact-type rejection", got, present, lookupErr)
	}
}

func TestOrdinalJoinBuildPublishesLayoutAndNullMatchPresence(t *testing.T) {
	t.Parallel()

	typeA := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	typeB := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
		{Name: "W", Ordinal: 1, FieldType: values.NullableLong},
	}}
	aliasA := values.NamedCorrelationIdentifier("A")
	// Generated physical edges use Unique correlations. The one-shot FlatMap
	// build must retain that discriminator; recreating aliasB from Name() as a
	// Named correlation produces the same display text but cannot satisfy this
	// layout window's exact source identity.
	aliasB := values.UniqueCorrelationIdentifier()
	qovA, err := values.NewQuantifiedObjectValue(aliasA, typeA)
	if err != nil {
		t.Fatal(err)
	}
	qovB, err := values.NewQuantifiedObjectValue(aliasB, typeB)
	if err != nil {
		t.Fatal(err)
	}
	field := func(qov values.QuantifiedObjectValue, ordinal int) values.Value {
		t.Helper()
		value, fieldErr := values.ResolveOrdinalSeedField(qov, ordinal)
		if fieldErr != nil {
			t.Fatal(fieldErr)
		}
		return value
	}
	seed := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "A.ID", Value: field(qovA, 0)},
		{Name: "A.V", Value: field(qovA, 1)},
		{Name: "B.ID", Value: field(qovB, 0)},
		{Name: "B.W", Value: field(qovB, 1)},
	}}
	build, err := newOrdinalJoinBuild(seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := build.configureNullSupplying(aliasB); err != nil {
		t.Fatal(err)
	}
	bWindowField := field(qovB, 1)
	rowA := NewPositionalRow(typeA)
	rowA.Slots[0], rowA.Slots[1] = int64(1), int64(10)

	t.Run("unmatched is bound SQL NULL", func(t *testing.T) {
		t.Parallel()
		row, evalErr := build.evaluateLegs(
			map[values.CorrelationIdentifier]values.OrdinalRow{aliasA: rowA, aliasB: nil}, nil, nil,
		)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		if row.Layout != build.OutputLayout || row.LayoutPresence == nil {
			t.Fatal("built row did not retain its exact output layout/presence")
		}
		matched, known := row.LayoutPresence.MatchState(qovB)
		if !known || matched {
			t.Fatalf("B presence = (matched=%t, known=%t), want (false,true)", matched, known)
		}
		ctx, ctxErr := frontierRowContext(row, nil, false)
		if ctxErr != nil {
			t.Fatal(ctxErr)
		}
		got, fieldErr := bWindowField.Evaluate(ctx)
		if got != nil || fieldErr != nil {
			t.Fatalf("unmatched B.W = (%v, %v), want bound SQL NULL", got, fieldErr)
		}
	})

	t.Run("matched all-null row remains matched", func(t *testing.T) {
		t.Parallel()
		rowB := NewPositionalRow(typeB)
		row, evalErr := build.evaluateLegs(
			map[values.CorrelationIdentifier]values.OrdinalRow{aliasA: rowA, aliasB: rowB}, nil, nil,
		)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		matched, known := row.LayoutPresence.MatchState(qovB)
		if !known || !matched {
			t.Fatalf("B presence = (matched=%t, known=%t), want (true,true)", matched, known)
		}
		ctx, ctxErr := frontierRowContext(row, nil, false)
		if ctxErr != nil {
			t.Fatal(ctxErr)
		}
		got, fieldErr := bWindowField.Evaluate(ctx)
		if got != nil || fieldErr != nil {
			t.Fatalf("matched all-null B.W = (%v, %v), want SQL NULL", got, fieldErr)
		}
	})

	t.Run("one-shot build preserves unique correlation kind", func(t *testing.T) {
		t.Parallel()
		rowB := NewPositionalRow(typeB)
		outer := QueryResult{Positional: rowA}
		inner := QueryResult{Positional: rowB}
		row, evalErr := build.evaluate(aliasA, aliasB, &outer, &inner, nil)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		matched, known := row.LayoutPresence.MatchState(qovB)
		if !known || !matched {
			t.Fatalf("unique B presence = (matched=%t, known=%t), want (true,true)", matched, known)
		}
	})
}

func TestOrdinalJoinBuildPreservesSelectedRetainedScalarPresence(t *testing.T) {
	outerType := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	innerType := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "V", Ordinal: 0, FieldType: values.NullableLong},
	}}
	outerAlias := values.NamedCorrelationIdentifier("OUTER")
	innerAlias := values.UniqueCorrelationIdentifier()
	outerSource := mustTestQOV(t, outerAlias, outerType)
	innerSource := mustTestQOV(t, innerAlias, innerType)
	retainedAlias := values.NamedCorrelationIdentifier("VAL")
	retainedScalar := mustTestQOV(t, retainedAlias, values.NullableLong)
	field := func(source values.QuantifiedObjectValue, ordinal int) values.Value {
		t.Helper()
		return mustExecutorConstruct(values.ResolveOrdinalSeedField(source, ordinal))
	}
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "VAL", Value: field(innerSource, 0)},
		values.RecordConstructorField{Name: "ID", Value: field(outerSource, 0)},
	)
	provided, err := values.NewFlatOrdinalLayoutForRetainedResultWithSources(
		result,
		[]values.QuantifiedObjectValue{innerSource},
		[]values.OrdinalOutputSource{{
			Source: retainedScalar, ObjectPath: []int{0}, NullSupplying: true,
		}},
	)
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	origins := map[values.CorrelationIdentifier]outputSourceOrigin{
		retainedAlias: {
			topLegAlias: innerAlias, childSource: retainedScalar,
		},
	}
	build, err := newOrdinalJoinBuildWithOutputLayout(result, nil, provided, origins)
	if err != nil {
		t.Fatalf("newOrdinalJoinBuildWithOutputLayout: %v", err)
	}
	// Construction snapshots the origin proof. Mutating the caller's map must
	// not redirect an unmatched inner source onto the present outer leg.
	origins[retainedAlias] = outputSourceOrigin{
		topLegAlias: outerAlias, childSource: retainedScalar,
	}
	if build.OutputLayout != provided || !build.OutputLayoutFromPlan {
		t.Fatal("ordinal build did not retain the exact selected plan layout")
	}
	if err := build.configureNullSupplying(innerAlias); err != nil {
		t.Fatalf("configureNullSupplying: %v", err)
	}
	if build.OutputLayout != provided {
		t.Fatal("null-supplying configuration reconstructed and lost the selected layout")
	}

	legacy, err := newOrdinalJoinBuild(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.configureNullSupplying(innerAlias); err != nil {
		t.Fatal(err)
	}
	if supplied, supplyErr := values.LayoutProvides(legacy.OutputLayout, retainedScalar); supplied || supplyErr == nil {
		t.Fatalf("RC-only derivation provides retained scalar = (%t, %v), want absent proof", supplied, supplyErr)
	}

	outerRow := NewPositionalRow(outerType)
	outerRow.Slots[0] = int64(7)
	innerLayout := mustExecutorCurrentLayout(t, innerType, []values.OrdinalWindowSpec{{
		Source: retainedScalar, ObjectPath: []int{0},
	}})
	evaluate := func(t *testing.T, inner values.OrdinalRow) *PositionalRow {
		t.Helper()
		row, evalErr := build.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
			outerAlias: outerRow,
			innerAlias: inner,
		}, nil, nil)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		if row.Layout != provided || row.LayoutPresence == nil {
			t.Fatal("built row lost selected layout or exact presence")
		}
		return row
	}
	assertScalar := func(t *testing.T, row *PositionalRow, wantMatched, wantExplicitAbsent bool) {
		t.Helper()
		matched, known := row.LayoutPresence.MatchState(retainedScalar)
		if !known || matched != wantMatched {
			t.Fatalf("retained scalar presence = (%t,%t), want (%t,true)", matched, known, wantMatched)
		}
		binder, bindErr := values.NewOrdinalObjectBinder(
			row.Layout, row, row.LayoutPresence, nil)
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		got, present, lookupErr := binder.GetQuantifiedBinding(retainedScalar)
		if lookupErr != nil || !present || got != nil {
			t.Fatalf("retained scalar lookup = (%v,%t,%v), want bound SQL NULL", got, present, lookupErr)
		}
		proof := binder.(values.ExplicitNullQuantifiedObjectBinder)
		explicitAbsent, proofErr := proof.IsExplicitNullQuantifiedBinding(retainedScalar)
		if proofErr != nil || explicitAbsent != wantExplicitAbsent {
			t.Fatalf("retained scalar absence = (%t,%v), want (%t,nil)",
				explicitAbsent, proofErr, wantExplicitAbsent)
		}
		wrongType := mustTestQOV(t, retainedAlias, values.NullableString)
		got, present, lookupErr = binder.GetQuantifiedBinding(wrongType)
		var coded executorLayoutCode
		if got != nil || present || !errors.As(lookupErr, &coded) ||
			coded.Code() != values.CorrelationTypeConflict {
			t.Fatalf("foreign same-alias scalar lookup = (%v,%t,%v), want CorrelationTypeConflict",
				got, present, lookupErr)
		}
	}

	t.Run("unmatched inner is explicit absence", func(t *testing.T) {
		assertScalar(t, evaluate(t, nil), false, true)
	})
	t.Run("matched SQL NULL remains present", func(t *testing.T) {
		assertScalar(t, evaluate(t,
			mustExecutorConstruct(NewLayoutPositionalRow(innerType, innerLayout))), true, false)
	})
}

func TestOrdinalJoinBuildPropagatesNestedSourcePresence(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("OUTER_BOX")
	innerAlias := values.NamedCorrelationIdentifier("INNER")
	preservedAlias := values.NamedCorrelationIdentifier("PRESERVED")
	optionalAlias := values.NamedCorrelationIdentifier("OPTIONAL")
	outerType := exactTestRowType(
		values.Field{Name: "P_ID", FieldType: values.NullableLong},
		values.Field{Name: "O_ID", FieldType: values.NullableLong},
	)
	innerType := &values.RecordType{Nullable: true, Fields: []values.Field{{
		Name: "I_ID", Ordinal: 0, FieldType: values.NullableLong,
	}}}
	preservedSource := mustTestQOV(t, preservedAlias, exactTestRowType(
		values.Field{Name: "P_ID", FieldType: values.NullableLong},
	))
	optionalSource := mustTestQOV(t, optionalAlias, &values.RecordType{
		Nullable: true, Fields: []values.Field{{Name: "O_ID", Ordinal: 0, FieldType: values.NullableLong}},
	})
	outerLayout := mustExecutorCurrentLayout(t, outerType,
		[]values.OrdinalWindowSpec{
			{Source: preservedSource, FieldPaths: [][]int{{0}}},
			{Source: optionalSource, FieldPaths: [][]int{{1}}, NullSupplying: true},
		})
	outerRow := mustExecutorConstruct(NewLayoutPositionalRow(outerType, outerLayout))
	outerRow.Slots[0], outerRow.Slots[1] = int64(1), nil
	innerSource := mustTestQOV(t, innerAlias, innerType)
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "P_ID", Value: mustExecutorConstruct(values.ResolveOrdinalSeedField(mustTestQOV(t, outerAlias, outerType), 0))},
		values.RecordConstructorField{Name: "O_ID", Value: mustExecutorConstruct(values.ResolveOrdinalSeedField(mustTestQOV(t, outerAlias, outerType), 1))},
		values.RecordConstructorField{Name: "I_ID", Value: mustExecutorConstruct(values.ResolveOrdinalSeedField(innerSource, 0))},
	)
	outputPreserved := mustTestQOV(t, preservedAlias,
		values.WithNullability(preservedSource.FlowedType(), true))
	provided := mustExecutorCurrentLayout(t, result.Type(), []values.OrdinalWindowSpec{
		{Source: outputPreserved, FieldPaths: [][]int{{0}}, NullSupplying: true},
		{Source: optionalSource, FieldPaths: [][]int{{1}}, NullSupplying: true},
	})
	outputSources := provided.WindowSources()
	origins := map[values.CorrelationIdentifier]outputSourceOrigin{
		preservedAlias: {topLegAlias: outerAlias, childSource: preservedSource},
		optionalAlias:  {topLegAlias: outerAlias, childSource: optionalSource, childNullSupplying: true},
	}
	build, err := newOrdinalJoinBuildWithOutputLayout(result, nil, provided, origins)
	if err != nil {
		t.Fatal(err)
	}

	evaluate := func(t *testing.T, outer values.OrdinalRow) (*PositionalRow, error) {
		t.Helper()
		return build.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
			outerAlias: outer, innerAlias: nil,
		}, nil, nil)
	}
	assertPresence := func(t *testing.T, row *PositionalRow, preserved, optional bool) {
		t.Helper()
		for i, want := range []bool{preserved, optional} {
			got, known := row.LayoutPresence.MatchState(outputSources[i])
			if !known || got != want {
				t.Fatalf("source %d presence = (%t,%t), want (%t,true)", i, got, known, want)
			}
		}
	}

	t.Run("nested optional absence propagates independently", func(t *testing.T) {
		outerRow.LayoutPresence = mustExecutorConstruct(values.NewWindowMatchPresence([]values.WindowMatch{{
			Source: optionalSource, Matched: false,
		}}))
		row, evalErr := evaluate(t, outerRow)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		assertPresence(t, row, true, false)
	})
	t.Run("nested optional match propagates independently", func(t *testing.T) {
		matchedOuter := *outerRow
		matchedOuter.LayoutPresence = mustExecutorConstruct(values.NewWindowMatchPresence([]values.WindowMatch{{
			Source: optionalSource, Matched: true,
		}}))
		row, evalErr := evaluate(t, &matchedOuter)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		assertPresence(t, row, true, true)
	})
	t.Run("whole top leg absence pads every retained source", func(t *testing.T) {
		row, evalErr := evaluate(t, nil)
		if evalErr != nil {
			t.Fatal(evalErr)
		}
		assertPresence(t, row, false, false)
	})
	t.Run("missing nested state fails closed", func(t *testing.T) {
		missing := *outerRow
		missing.LayoutPresence = nil
		row, evalErr := evaluate(t, &missing)
		var coded executorLayoutCode
		if row != nil || !errors.As(evalErr, &coded) || coded.Code() != values.LayoutPresenceMissing {
			t.Fatalf("missing nested presence = (%v,%v), want LayoutPresenceMissing", row, evalErr)
		}
	})
	t.Run("wrong nested source type fails closed", func(t *testing.T) {
		wrong := *build
		wrong.OutputSourceOrigins = map[values.CorrelationIdentifier]outputSourceOrigin{}
		for key, origin := range build.OutputSourceOrigins {
			wrong.OutputSourceOrigins[key] = origin
		}
		origin := wrong.OutputSourceOrigins[optionalAlias]
		origin.childSource = mustTestQOV(t, optionalAlias, values.NullableLong)
		wrong.OutputSourceOrigins[optionalAlias] = origin
		row, evalErr := wrong.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
			outerAlias: outerRow, innerAlias: nil,
		}, nil, nil)
		var coded executorLayoutCode
		if row != nil || !errors.As(evalErr, &coded) || coded.Code() != values.CorrelationTypeConflict {
			t.Fatalf("wrong nested source = (%v,%v), want CorrelationTypeConflict", row, evalErr)
		}
	})
	t.Run("foreign nested source fails closed", func(t *testing.T) {
		wrong := *build
		wrong.OutputSourceOrigins = map[values.CorrelationIdentifier]outputSourceOrigin{}
		for key, origin := range build.OutputSourceOrigins {
			wrong.OutputSourceOrigins[key] = origin
		}
		origin := wrong.OutputSourceOrigins[optionalAlias]
		origin.childSource = mustTestQOV(t,
			values.NamedCorrelationIdentifier("FOREIGN_OPTIONAL"), optionalSource.FlowedType())
		wrong.OutputSourceOrigins[optionalAlias] = origin
		row, evalErr := wrong.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
			outerAlias: outerRow, innerAlias: nil,
		}, nil, nil)
		var coded executorLayoutCode
		if row != nil || !errors.As(evalErr, &coded) || coded.Code() != values.LayoutSourceNotProvided {
			t.Fatalf("foreign nested source = (%v,%v), want LayoutSourceNotProvided", row, evalErr)
		}
	})
	t.Run("null-supplying descriptor disagreement fails closed", func(t *testing.T) {
		wrong := *build
		wrong.OutputSourceOrigins = map[values.CorrelationIdentifier]outputSourceOrigin{}
		for key, origin := range build.OutputSourceOrigins {
			wrong.OutputSourceOrigins[key] = origin
		}
		origin := wrong.OutputSourceOrigins[optionalAlias]
		origin.childNullSupplying = false
		wrong.OutputSourceOrigins[optionalAlias] = origin
		row, evalErr := wrong.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
			outerAlias: outerRow, innerAlias: nil,
		}, nil, nil)
		var coded executorLayoutCode
		if row != nil || !errors.As(evalErr, &coded) || coded.Code() != values.LayoutInvalidWindow {
			t.Fatalf("wrong null-supplying proof = (%v,%v), want LayoutInvalidWindow", row, evalErr)
		}
	})

	if outerRow.Layout != outerLayout || outerRow.Slots[0] != int64(1) || outerRow.Slots[1] != nil {
		t.Fatal("nested-source presence propagation mutated the selected child row")
	}
}

func TestOrdinalJoinBuildSelectedLayoutFlattensNestedChildRows(t *testing.T) {
	t.Parallel()

	leftLeafType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "V", FieldType: values.NotNullInt},
	)
	rightLeafType := exactTestRowType(
		values.Field{Name: "K", FieldType: values.NotNullLong},
		values.Field{Name: "W", FieldType: values.NotNullInt},
	)
	leftBoxType := exactTestRowType(values.Field{Name: "LEFT", FieldType: leftLeafType})
	rightBoxType := exactTestRowType(values.Field{Name: "RIGHT", FieldType: rightLeafType})
	leftAlias := values.NamedCorrelationIdentifier("LEFT_BOX")
	rightAlias := values.NamedCorrelationIdentifier("RIGHT_BOX")
	leftSource := mustTestQOV(t, leftAlias, leftBoxType)
	rightSource := mustTestQOV(t, rightAlias, rightBoxType)
	field := func(source values.Value, path ...int) values.Value {
		t.Helper()
		return mustExecutorConstruct(values.ResolveFieldOrdinals(source, path))
	}
	leftID := field(leftSource, 0, 0)
	leftV := field(leftSource, 0, 1)
	rightK := field(rightSource, 0, 0)
	rightW := field(rightSource, 0, 1)
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "LEFT_ID", Value: leftID},
		values.RecordConstructorField{Name: "LEFT_V", Value: leftV},
		values.RecordConstructorField{Name: "RIGHT_K", Value: rightK},
		values.RecordConstructorField{Name: "RIGHT_W", Value: rightW},
	)
	if values.ContainsBakedOrdinal(result) || values.IsPositionalMergeRC(result) {
		t.Fatal("fixture accidentally satisfies the legacy ordinal-build trigger")
	}
	legacy, err := newOrdinalJoinBuild(result, nil)
	if err != nil || legacy != nil {
		t.Fatalf("legacy ordinary RC build = (%v, %v), want (nil, nil)", legacy, err)
	}

	provided, err := values.NewFlatOrdinalLayoutForRetainedResult(result, nil)
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	build, err := newOrdinalJoinBuildWithOutputLayout(result, nil, provided, nil)
	if err != nil {
		t.Fatalf("selected-layout build: %v", err)
	}
	if !build.enabled() || build.RC != result || build.OutputLayout != provided ||
		!build.OutputLayoutFromPlan {
		t.Fatal("selected exact RC/layout did not become the authoritative output build")
	}

	leftLeaf := NewPositionalRow(leftLeafType)
	leftLeaf.Slots[0], leftLeaf.Slots[1] = int64(7), int32(8)
	leftBox := NewPositionalRow(leftBoxType)
	leftBox.Slots[0] = leftLeaf
	rightLeaf := NewPositionalRow(rightLeafType)
	rightLeaf.Slots[0], rightLeaf.Slots[1] = int64(9), int32(10)
	rightBox := NewPositionalRow(rightBoxType)
	rightBox.Slots[0] = rightLeaf
	row, err := build.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
		leftAlias:  leftBox,
		rightAlias: rightBox,
	}, nil, nil)
	if err != nil {
		t.Fatalf("evaluate selected layout: %v", err)
	}
	if row.Layout != provided || row.Type == nil ||
		!row.Type.Equals(provided.Carrier().FlowedType()) {
		t.Fatal("selected layout build emitted a row outside its exact carrier")
	}
	want := []any{int64(7), int32(8), int64(9), int32(10)}
	if len(row.Slots) != len(want) {
		t.Fatalf("flattened slots = %v, want %v", row.Slots, want)
	}
	for i := range want {
		if row.Slots[i] != want[i] {
			t.Fatalf("flattened slot %d = %v, want %v", i, row.Slots[i], want[i])
		}
	}
	leftIDField, ok := values.AsFieldValue(leftID)
	if !ok || leftIDField.ChildValue() != leftSource ||
		len(leftIDField.Path().Ordinals()) != 2 ||
		leftIDField.Path().Ordinals()[0] != 0 || leftIDField.Path().Ordinals()[1] != 0 ||
		leftBox.Slots[0] != leftLeaf || rightBox.Slots[0] != rightLeaf {
		t.Fatal("selected build mutated its source Values or nested input rows")
	}

	wrongResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ONLY", Value: &values.ConstantValue{
			Value: int64(1), Typ: values.NotNullLong,
		}},
	)
	wrongLayout, err := values.NewFlatOrdinalLayoutForRetainedResult(wrongResult, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong, wrongErr := newOrdinalJoinBuildWithOutputLayout(result, nil, wrongLayout, nil)
	var coded executorLayoutCode
	if wrong != nil || !errors.As(wrongErr, &coded) ||
		coded.Code() != values.LayoutCarrierMismatch {
		t.Fatalf("wrong selected layout = (%v, %v), want LayoutCarrierMismatch", wrong, wrongErr)
	}
}

func TestOrdinalJoinBuildSelectedLayoutDoesNotForceFlatOneLevelRC(t *testing.T) {
	t.Parallel()

	leftType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "V", FieldType: values.NotNullInt},
	)
	rightType := exactTestRowType(
		values.Field{Name: "K", FieldType: values.NotNullLong},
		values.Field{Name: "W", FieldType: values.NotNullInt},
	)
	leftSource := mustTestQOV(t, values.NamedCorrelationIdentifier("LEFT"), leftType)
	rightSource := mustTestQOV(t, values.NamedCorrelationIdentifier("RIGHT"), rightType)
	field := func(source values.Value, ordinal int) values.Value {
		t.Helper()
		return mustExecutorConstruct(values.ResolveFieldOrdinals(source, []int{ordinal}))
	}
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: field(leftSource, 0)},
		values.RecordConstructorField{Name: "V", Value: field(leftSource, 1)},
		values.RecordConstructorField{Name: "K", Value: field(rightSource, 0)},
		values.RecordConstructorField{Name: "W", Value: field(rightSource, 1)},
	)
	if values.ContainsBakedOrdinal(result) || values.IsPositionalMergeRC(result) ||
		recordConstructorReadsNestedLegPath(result) {
		t.Fatal("fixture accidentally satisfies an ordinal-build trigger")
	}
	provided, err := values.NewFlatOrdinalLayoutForRetainedResult(result, nil)
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	build, err := newOrdinalJoinBuildWithOutputLayout(result, nil, provided, nil)
	if err != nil || build != nil {
		t.Fatalf("flat one-level selected RC build = (%v, %v), want (nil, nil)", build, err)
	}

	leftField, ok := values.AsFieldValue(result.Fields[0].Value)
	if !ok || leftField.ChildValue() != leftSource || leftField.Path().Len() != 1 {
		t.Fatal("declining selected-layout probe mutated its source result program")
	}
}

func TestOrdinalJoinBuildSelectedLayoutRetainsWholeRecordSlot(t *testing.T) {
	t.Parallel()

	outerType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "HID", FieldType: values.NullableLong},
	)
	innerType := exactTestRowType(
		values.Field{Name: "K", FieldType: values.NotNullLong},
		values.Field{Name: "W", FieldType: values.NullableString},
	)
	outerAlias := values.NamedCorrelationIdentifier("RIGHT_DEEP_OUTER")
	innerAlias := values.NamedCorrelationIdentifier("RIGHT_DEEP_INNER")
	outerSource := mustTestQOV(t, outerAlias, outerType)
	innerSource := mustTestQOV(t, innerAlias, innerType)
	outerField := func(ordinal int) values.Value {
		t.Helper()
		return mustExecutorConstruct(values.ResolveFieldOrdinals(outerSource, []int{ordinal}))
	}
	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: outerField(0)},
		values.RecordConstructorField{Name: "HID", Value: outerField(1)},
		values.RecordConstructorField{Name: "INNER", Value: innerSource},
	)
	if values.ContainsBakedOrdinal(result) || values.IsPositionalMergeRC(result) ||
		recordConstructorReadsNestedLegPath(result) ||
		!recordConstructorRetainsWholeRecordSlot(result) {
		t.Fatal("fixture does not isolate the selected whole-record-slot trigger")
	}
	legacy, err := newOrdinalJoinBuild(result, nil)
	if err != nil || legacy != nil {
		t.Fatalf("legacy whole-record RC build = (%v, %v), want (nil, nil)", legacy, err)
	}

	provided, err := values.NewFlatOrdinalLayoutForRetainedResult(result, nil)
	if err != nil {
		t.Fatalf("provided layout: %v", err)
	}
	if supplied, supplyErr := values.LayoutProvides(provided, innerSource); !supplied || supplyErr != nil {
		t.Fatalf("selected layout does not provide exact inner object path = (%t, %v)", supplied, supplyErr)
	}
	foreign := mustTestQOV(t, values.NamedCorrelationIdentifier("FOREIGN_INNER"), innerType)
	if supplied, supplyErr := values.LayoutProvides(provided, foreign); supplied || supplyErr == nil {
		t.Fatalf("foreign whole-record source provided = (%t, %v), want rejected", supplied, supplyErr)
	}
	wrongInner := mustTestQOV(t, innerAlias, exactTestRowType(
		values.Field{Name: "K", FieldType: values.NotNullLong},
		values.Field{Name: "W", FieldType: values.NullableLong},
	))
	if supplied, supplyErr := values.LayoutProvides(provided, wrongInner); supplied || supplyErr == nil {
		t.Fatalf("wrong-type whole-record source provided = (%t, %v), want rejected", supplied, supplyErr)
	}

	build, err := newOrdinalJoinBuildWithOutputLayout(result, nil, provided, nil)
	if err != nil {
		t.Fatalf("selected-layout build: %v", err)
	}
	if !build.enabled() || build.RC != result || build.OutputLayout != provided ||
		!build.OutputLayoutFromPlan {
		t.Fatal("selected whole-record slot did not become an authoritative output build")
	}

	outerRow := NewPositionalRow(outerType)
	outerRow.Slots[0], outerRow.Slots[1] = int64(7), int64(70)
	innerRow := NewPositionalRow(innerType)
	innerRow.Slots[0], innerRow.Slots[1] = int64(9), "nine"
	row, err := build.evaluateLegs(map[values.CorrelationIdentifier]values.OrdinalRow{
		outerAlias: outerRow,
		innerAlias: innerRow,
	}, nil, nil)
	if err != nil {
		t.Fatalf("evaluate selected whole-record slot: %v", err)
	}
	if row.Layout != provided || row.Type == nil ||
		!row.Type.Equals(provided.Carrier().FlowedType()) || len(row.Slots) != 3 ||
		row.Slots[0] != int64(7) || row.Slots[1] != int64(70) || row.Slots[2] != innerRow {
		t.Fatalf("built row = type %v slots %v layout %p, want exact width-3 row with inner object",
			row.Type, row.Slots, row.Layout)
	}
	if outerRow.Slots[0] != int64(7) || outerRow.Slots[1] != int64(70) ||
		innerRow.Slots[0] != int64(9) || innerRow.Slots[1] != "nine" ||
		result.Fields[2].Value != innerSource {
		t.Fatal("selected whole-record build mutated its Values or input rows")
	}

	wrongCarrier := mustExecutorCurrentLayout(t, exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "HID", FieldType: values.NullableLong},
		values.Field{Name: "INNER", FieldType: wrongInner.FlowedType()},
	), nil)
	wrong, wrongErr := newOrdinalJoinBuildWithOutputLayout(result, nil, wrongCarrier, nil)
	var coded executorLayoutCode
	if wrong != nil || !errors.As(wrongErr, &coded) || coded.Code() != values.LayoutCarrierMismatch {
		t.Fatalf("wrong whole-record carrier = (%v, %v), want LayoutCarrierMismatch", wrong, wrongErr)
	}

	scalarSource := mustTestQOV(t, values.NamedCorrelationIdentifier("SCALAR_INNER"), values.NullableLong)
	scalarResult := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: outerField(0)},
		values.RecordConstructorField{Name: "VALUE", Value: scalarSource},
	)
	scalarLayout, err := values.NewFlatOrdinalLayoutForRetainedResult(scalarResult, nil)
	if err != nil {
		t.Fatal(err)
	}
	scalarBuild, scalarErr := newOrdinalJoinBuildWithOutputLayout(scalarResult, nil, scalarLayout, nil)
	if scalarErr != nil || scalarBuild != nil {
		t.Fatalf("scalar bare-QOV selected build = (%v, %v), want (nil, nil)", scalarBuild, scalarErr)
	}
}

func TestNestedLoopJoinPlanLayoutAgreesWithExecutorBuild(t *testing.T) {
	t.Parallel()

	outerType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "V", FieldType: values.NotNullString},
	)
	innerType := exactTestRowType(
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "W", FieldType: values.NotNullLong},
	)
	outer := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"A"}, outerType, false))
	inner := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"B"}, innerType, false))
	outerAlias := values.NamedCorrelationIdentifier("PLAN_BUILD_A")
	innerAlias := values.NamedCorrelationIdentifier("PLAN_BUILD_B")

	for _, test := range []struct {
		name     string
		joinType plans.JoinType
	}{
		{name: "inner", joinType: plans.JoinInner},
		{name: "left_outer", joinType: plans.JoinLeftOuter},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			outerSource := mustTestQOV(t, outerAlias, outerType)
			var innerFlowed values.Type = innerType
			if test.joinType == plans.JoinLeftOuter {
				innerFlowed = values.WithNullability(innerFlowed, true)
			}
			innerSource := mustTestQOV(t, innerAlias, innerFlowed)
			field := func(source values.QuantifiedObjectValue, ordinal int) values.Value {
				t.Helper()
				return mustExecutorConstruct(values.ResolveOrdinalSeedField(source, ordinal))
			}
			result := values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "A_ID", Value: field(outerSource, 0)},
				values.RecordConstructorField{Name: "A_V", Value: field(outerSource, 1)},
				values.RecordConstructorField{Name: "B_ID", Value: field(innerSource, 0)},
				values.RecordConstructorField{Name: "B_W", Value: field(innerSource, 1)},
			)

			build, err := newOrdinalJoinBuild(result, nil)
			if err != nil {
				t.Fatalf("newOrdinalJoinBuild: %v", err)
			}
			unconfigured := build.OutputLayout
			if test.joinType == plans.JoinLeftOuter {
				if err := build.configureNullSupplying(innerAlias); err != nil {
					t.Fatalf("configure null-supplying inner: %v", err)
				}
			}
			plan := mustExecutorConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
				outer, inner, nil, test.joinType, outerAlias, innerAlias, result,
			))
			provided, err := plan.ProvidedOutputLayout()
			if err != nil {
				t.Fatalf("plan ProvidedOutputLayout: %v", err)
			}
			if !provided.RawEqual(build.OutputLayout) {
				t.Fatal("plan-provided layout disagrees with the executor ordinal build")
			}
			if test.joinType == plans.JoinLeftOuter && provided.RawEqual(unconfigured) {
				t.Fatal("left-outer plan lost the executor build's null-supplying-inner configuration")
			}
		})
	}
}
