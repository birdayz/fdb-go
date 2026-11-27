package values

import (
	"errors"
	"testing"
)

func requiredBindingsRecord(fields ...Field) *RecordType {
	return NewRecordType("", false, fields)
}

func mustRequiredQOV(t testing.TB, name string, typ Type) QuantifiedObjectValue {
	t.Helper()
	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(name), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", name, err)
	}
	return qov
}

func mustRequiredField(t testing.TB, qov QuantifiedObjectValue, ordinal int) Value {
	t.Helper()
	field, err := ResolveFieldOrdinals(qov, []int{ordinal})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%s,%d): %v", qov.Correlation(), ordinal, err)
	}
	return field
}

func requiredBindingsFixture(t testing.TB) (
	OrdinalLayout,
	[]Value,
	TypedEdgeDeclaration,
	TypedExternalDeclaration,
	QuantifiedObjectValue,
) {
	t.Helper()
	sourceType := requiredBindingsRecord(Field{Name: "S", FieldType: NotNullLong})
	edgeType := requiredBindingsRecord(Field{Name: "E", FieldType: NotNullString})
	externalType := requiredBindingsRecord(Field{Name: "X", FieldType: NullableDouble})
	carrierType := requiredBindingsRecord(
		Field{Name: "C", FieldType: NotNullLong},
		Field{Name: "S", FieldType: NotNullLong},
	)
	source := mustRequiredQOV(t, "SOURCE", sourceType)
	edgeQOV := mustRequiredQOV(t, "EDGE", edgeType)
	externalQOV := mustRequiredQOV(t, "EXTERNAL", externalType)

	layout, err := NewOrdinalLayoutForCarrierType(
		carrierType,
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}},
		[]OrdinalWindowSpec{{Source: source, FieldPaths: [][]int{{1}}}},
	)
	if err != nil {
		t.Fatalf("NewOrdinalLayoutForCarrierType: %v", err)
	}
	edge, err := NewTypedEdgeDeclaration(edgeQOV)
	if err != nil {
		t.Fatalf("NewTypedEdgeDeclaration: %v", err)
	}
	external, err := NewTypedExternalDeclaration(externalQOV)
	if err != nil {
		t.Fatalf("NewTypedExternalDeclaration: %v", err)
	}
	roots := []Value{
		mustRequiredField(t, layout.Carrier(), 0),
		mustRequiredField(t, source, 0),
		mustRequiredField(t, edgeQOV, 0),
		mustRequiredField(t, externalQOV, 0),
	}
	return layout, roots, edge, external, source
}

func TestRequiredBindingsClassifiesOriginsAndValidatesLayout(t *testing.T) {
	t.Parallel()
	layout, roots, edge, external, source := requiredBindingsFixture(t)
	required, err := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge}, []TypedExternalDeclaration{external})
	if err != nil {
		t.Fatalf("CollectRequiredBindings: %v", err)
	}
	windows := required.WindowSources()
	if len(windows) != 1 || windows[0] != source {
		t.Fatalf("WindowSources = %#v, want exact SOURCE QOV", windows)
	}
	windows[0] = nil
	if again := required.WindowSources(); len(again) != 1 || again[0] != source {
		t.Fatal("mutating WindowSources result changed immutable manifest")
	}
	if ok, validateErr := required.ValidateAgainst(layout); validateErr != nil || !ok {
		t.Fatalf("ValidateAgainst = (%v,%v), want (true,nil)", ok, validateErr)
	}
	if ok, validateErr := LayoutSatisfies(layout, required); validateErr != nil || !ok {
		t.Fatalf("LayoutSatisfies = (%v,%v), want (true,nil)", ok, validateErr)
	}
}

func TestRequiredBindingsRejectsMissingExtraAndWrongOwnerLayouts(t *testing.T) {
	t.Parallel()
	layout, roots, edge, external, source := requiredBindingsFixture(t)
	required, err := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge}, []TypedExternalDeclaration{external})
	if err != nil {
		t.Fatalf("CollectRequiredBindings: %v", err)
	}
	carrierType := layout.Carrier().FlowedType()
	missing, err := NewOrdinalLayoutForCarrierType(
		carrierType, []OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("missing layout: %v", err)
	}
	if ok, validateErr := LayoutSatisfies(missing, required); ok || validateErr == nil {
		t.Fatalf("independent current layout = (%v,%v), want LayoutCarrierMismatch", ok, validateErr)
	} else {
		var coded interface{ Code() ResolutionErrorCode }
		if !errors.As(validateErr, &coded) || coded.Code() != LayoutCarrierMismatch {
			t.Fatalf("independent current layout = (%v,%v), want LayoutCarrierMismatch", ok, validateErr)
		}
	}

	// Use the exact owner handle but omit its one required source window.
	exactCarrier := layout.Carrier().(*quantifiedObjectValue)
	withoutSource := &ordinalLayout{
		carrier: exactCarrier, carrierKind: OrdinalCarrierRecord,
		tiles:    []ordinalTile{{start: 0, width: 2, kind: OrdinalTileFlat}},
		bySource: make(map[CorrelationIdentifier]int),
	}
	withoutSource.hash = hashOrdinalLayout(withoutSource)
	if ok, validateErr := LayoutSatisfies(withoutSource, required); ok || validateErr != nil {
		t.Fatalf("missing source = (%v,%v), want (false,nil)", ok, validateErr)
	}

	extraQOV := mustRequiredQOV(t, "EXTRA", requiredBindingsRecord(Field{Name: "S", FieldType: NotNullLong}))
	extraLayout, err := NewOrdinalLayout(
		layout.Carrier(),
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}},
		[]OrdinalWindowSpec{
			{Source: source, FieldPaths: [][]int{{1}}},
			{Source: extraQOV, FieldPaths: [][]int{{1}}},
		},
	)
	if err != nil {
		t.Fatalf("extra layout: %v", err)
	}
	if ok, validateErr := LayoutSatisfies(extraLayout, required); ok || validateErr == nil {
		t.Fatalf("extra source = (%v,%v), want typed error", ok, validateErr)
	}
}

func TestRequiredBindingsRejectsOriginAndTypeCollisions(t *testing.T) {
	t.Parallel()
	layout, roots, edge, _, _ := requiredBindingsFixture(t)
	external, err := NewTypedExternalDeclaration(edge.QOV())
	if err != nil {
		t.Fatalf("NewTypedExternalDeclaration: %v", err)
	}
	if result, collectErr := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge}, []TypedExternalDeclaration{external},
	); result != nil || collectErr == nil {
		t.Fatalf("colliding origins = (%T,%v), want nil,error", result, collectErr)
	}

	conflicting := mustRequiredQOV(t, edge.QOV().Correlation().Name(),
		requiredBindingsRecord(Field{Name: "E", FieldType: NotNullLong}))
	conflictingEdge, err := NewTypedEdgeDeclaration(conflicting)
	if err != nil {
		t.Fatalf("conflicting declaration: %v", err)
	}
	if result, collectErr := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge, conflictingEdge}, nil,
	); result != nil || collectErr == nil {
		t.Fatalf("conflicting edge types = (%T,%v), want nil,error", result, collectErr)
	}

	if binding, bindErr := BindTypedEdge(edge, nil); binding != nil || bindErr == nil {
		t.Fatalf("non-nullable nil edge = (%T,%v), want nil,error", binding, bindErr)
	}
}

func TestRequiredBindingsRejectsRetainedForeignCurrent(t *testing.T) {
	t.Parallel()
	layout, roots, edge, external, _ := requiredBindingsFixture(t)
	twin, err := NewOrdinalLayoutForCarrierType(
		layout.Carrier().FlowedType(),
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("twin layout: %v", err)
	}
	roots = append(roots, mustRequiredField(t, twin.Carrier(), 0))
	if result, collectErr := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge}, []TypedExternalDeclaration{external},
	); result != nil || collectErr == nil {
		t.Fatalf("foreign current = (%T,%v), want nil,error", result, collectErr)
	}
}

func TestRequiredOrdinalObjectBinderConsumesTheSameOriginManifest(t *testing.T) {
	t.Parallel()
	layout, roots, edge, external, source := requiredBindingsFixture(t)
	required, err := CollectRequiredBindings(
		layout.Carrier(), roots,
		[]TypedEdgeDeclaration{edge}, []TypedExternalDeclaration{external})
	if err != nil {
		t.Fatalf("CollectRequiredBindings: %v", err)
	}
	edgeRow := &materializedOrdinalWindow{names: []string{"E"}, slots: []any{"edge"}}
	edgeBinding, err := BindTypedEdge(edge, edgeRow)
	if err != nil {
		t.Fatalf("BindTypedEdge: %v", err)
	}
	carrier := &materializedOrdinalWindow{names: []string{"C", "S"}, slots: []any{int64(1), int64(2)}}
	binder, err := NewRequiredOrdinalObjectBinder(
		layout, carrier, nil, required, []TypedEdgeBinding{edgeBinding}, nil)
	if err != nil {
		t.Fatalf("NewRequiredOrdinalObjectBinder: %v", err)
	}
	if got, present, lookupErr := binder.GetQuantifiedBinding(edge.QOV()); lookupErr != nil || !present || got != edgeRow {
		t.Fatalf("edge lookup = (%v,%v,%v), want exact whole edge row", got, present, lookupErr)
	}
	if got, present, lookupErr := binder.GetQuantifiedBinding(source); lookupErr != nil || !present {
		t.Fatalf("window lookup = (%v,%v,%v), want present source row", got, present, lookupErr)
	}
	if _, missingErr := NewRequiredOrdinalObjectBinder(layout, carrier, nil, required, nil, nil); missingErr == nil {
		t.Fatal("binder accepted a missing declared edge")
	}
	unexpectedQOV := mustRequiredQOV(t, "UNEXPECTED", requiredBindingsRecord(Field{Name: "U", FieldType: NotNullLong}))
	unexpectedDecl, err := NewTypedEdgeDeclaration(unexpectedQOV)
	if err != nil {
		t.Fatalf("unexpected declaration: %v", err)
	}
	unexpectedBinding, err := BindTypedEdge(unexpectedDecl, &materializedOrdinalWindow{slots: []any{int64(3)}})
	if err != nil {
		t.Fatalf("unexpected binding: %v", err)
	}
	if result, bindErr := NewRequiredOrdinalObjectBinder(
		layout, carrier, nil, required,
		[]TypedEdgeBinding{edgeBinding, unexpectedBinding}, nil,
	); result != nil || bindErr == nil {
		t.Fatalf("unexpected runtime edge = (%T,%v), want nil,error", result, bindErr)
	}
}

type panicRequiredValue struct{}

func (*panicRequiredValue) Children() []Value         { panic("hostile children") }
func (*panicRequiredValue) Type() Type                { panic("hostile type") }
func (*panicRequiredValue) Name() string              { return "hostile" }
func (*panicRequiredValue) Evaluate(any) (any, error) { panic("hostile evaluate") }

func TestRequiredBindingsConvertsHostileTraversalPanicToTypedError(t *testing.T) {
	t.Parallel()
	layout, _, _, _, _ := requiredBindingsFixture(t)
	result, err := CollectRequiredBindings(layout.Carrier(), []Value{&panicRequiredValue{}}, nil, nil)
	if result != nil || err == nil {
		t.Fatalf("hostile traversal = (%T,%v), want nil,error", result, err)
	}
	var coded interface{ Code() ResolutionErrorCode }
	if !errors.As(err, &coded) || coded.Code() != LayoutForeignValue {
		t.Fatalf("hostile traversal code = %v, want LayoutForeignValue", err)
	}
}
