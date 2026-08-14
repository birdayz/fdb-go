package values

import (
	"errors"
	"testing"
)

type outputLayoutBinder map[CorrelationIdentifier]any

func (b outputLayoutBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	value, present := b[id]
	return value, present
}

func TestNewFlatOrdinalLayoutForResultDerivesWholeSourceWindows(t *testing.T) {
	t.Parallel()

	sourceType := &RecordType{Nullable: true, Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "NAME", Ordinal: 1, FieldType: NullableString},
	}}
	source, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("S"), sourceType)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	name, err := ResolveFieldOrdinals(source, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	result := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "COMPUTED", Value: &ConstantValue{Value: int64(99), Typ: NotNullLong}},
		{Name: "NAME", Value: name},
		{Name: "ID", Value: id},
	}}
	layout, err := NewFlatOrdinalLayoutForResult(result, []OrdinalOutputSource{{
		Source: source, NullSupplying: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if provided, err := LayoutProvides(layout, source); err != nil || !provided {
		t.Fatalf("LayoutProvides(source) = (%t, %v), want (true, nil)", provided, err)
	}
	row := &bindingTestRow{slots: []any{int64(99), "birdy", int64(7)}}
	presence, err := NewWindowMatchPresenceFromCorrelations(layout, outputLayoutBinder{source.Correlation(): row})
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewOrdinalObjectBinder(layout, row, presence, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &RowEvalContext{Objects: binder}
	if got, evalErr := id.Evaluate(ctx); evalErr != nil || got != int64(7) {
		t.Fatalf("source.ID = (%v, %v), want (7, nil)", got, evalErr)
	}
	if got, evalErr := name.Evaluate(ctx); evalErr != nil || got != "birdy" {
		t.Fatalf("source.NAME = (%v, %v), want (birdy, nil)", got, evalErr)
	}

	unmatched, err := NewWindowMatchPresenceFromCorrelations(layout, outputLayoutBinder{source.Correlation(): nil})
	if err != nil {
		t.Fatal(err)
	}
	nullBinder, err := NewOrdinalObjectBinder(layout, row, unmatched, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, evalErr := id.Evaluate(&RowEvalContext{Objects: nullBinder})
	if got != nil || evalErr != nil {
		t.Fatalf("unmatched source.ID = (%v, %v), want bound SQL NULL", got, evalErr)
	}
}

func TestNewFlatOrdinalLayoutForResultRejectsPartialDuplicateAndMissingPresence(t *testing.T) {
	t.Parallel()

	sourceType := &RecordType{Nullable: true, Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "NAME", Ordinal: 1, FieldType: NullableString},
	}}
	source, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("S"), sourceType)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string][]RecordConstructorField{
		"partial":   {{Name: "ID", Value: id}},
		"duplicate": {{Name: "ID1", Value: id}, {Name: "ID2", Value: id}},
	} {
		name, fields := name, fields
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			layout, buildErr := NewFlatOrdinalLayoutForResult(
				&RecordConstructorValue{Fields: fields},
				[]OrdinalOutputSource{{Source: source, NullSupplying: true}},
			)
			if layout != nil || buildErr == nil {
				t.Fatalf("layout = (%T, %v), want (nil, error)", layout, buildErr)
			}
		})
	}

	nameField, err := ResolveFieldOrdinals(source, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewFlatOrdinalLayoutForResult(
		&RecordConstructorValue{Fields: []RecordConstructorField{{Name: "ID", Value: id}, {Name: "NAME", Value: nameField}}},
		[]OrdinalOutputSource{{Source: source, NullSupplying: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	presence, err := NewWindowMatchPresenceFromCorrelations(layout, outputLayoutBinder{})
	var coded interface{ Code() ResolutionErrorCode }
	if presence != nil || !errors.As(err, &coded) || coded.Code() != LayoutPresenceMissing {
		t.Fatalf("missing presence = (%T, %v), want LayoutPresenceMissing", presence, err)
	}
}

func TestRetainedScalarQOVPublishesExactObjectWindow(t *testing.T) {
	t.Parallel()

	scalar, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("ELEMENT"), NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	result := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "OUTER_ID", Value: &ConstantValue{Value: int64(7), Typ: NotNullLong}},
		RecordConstructorField{Name: "ELEMENT", Value: scalar},
	)
	layout, err := NewFlatOrdinalLayoutForRetainedResult(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := LayoutProvides(layout, scalar); provideErr != nil || !provided {
		t.Fatalf("LayoutProvides(scalar) = (%t, %v), want (true, nil)", provided, provideErr)
	}
	windows := layout.WindowSources()
	if len(windows) != 1 || windows[0] != scalar {
		t.Fatalf("WindowSources = %v, want exact scalar source", windows)
	}
	windows[0] = nil
	if again := layout.WindowSources(); len(again) != 1 || again[0] != scalar {
		t.Fatal("WindowSources exposed mutable layout storage")
	}
	binder, err := NewOrdinalObjectBinder(layout, &bindingTestRow{slots: []any{int64(7), int64(42)}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, evalErr := scalar.Evaluate(&RowEvalContext{Objects: binder})
	if evalErr != nil || got != int64(42) {
		t.Fatalf("retained scalar Evaluate = (%v, %v), want (42, nil)", got, evalErr)
	}

	foreign, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("FOREIGN"), NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := LayoutProvides(layout, foreign); provided || provideErr == nil {
		t.Fatalf("LayoutProvides(foreign) = (%t, %v), want loud missing-source rejection", provided, provideErr)
	}
	wrongType, err := NewQuantifiedObjectValue(scalar.Correlation(), NotNullInt)
	if err != nil {
		t.Fatal(err)
	}
	if provided, provideErr := LayoutProvides(layout, wrongType); provideErr == nil || provided {
		t.Fatalf("LayoutProvides(wrong type) = (%t, %v), want loud rejection", provided, provideErr)
	}
}

func TestFlatOrdinalLayoutRejectsDistinctSourcesAtOneObjectPathInEitherOrder(t *testing.T) {
	t.Parallel()
	left, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("LEFT"), NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("RIGHT"), NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	result := NewRawRecordConstructorValue(RecordConstructorField{
		Name: "VALUE", Value: &ConstantValue{Value: int64(7), Typ: NotNullLong},
	})
	for _, sources := range [][]OrdinalOutputSource{
		{{Source: left, ObjectPath: []int{0}}, {Source: right, ObjectPath: []int{0}}},
		{{Source: right, ObjectPath: []int{0}}, {Source: left, ObjectPath: []int{0}}},
	} {
		layout, buildErr := NewFlatOrdinalLayoutForResult(result, sources)
		var coded interface{ Code() ResolutionErrorCode }
		if layout != nil || !errors.As(buildErr, &coded) || coded.Code() != LayoutInvalidWindow {
			t.Fatalf("shared object path = (%T, %v), want LayoutInvalidWindow", layout, buildErr)
		}
	}
}

func TestRetainedResultRejectsDuplicateAndConflictingNullSourcesInEitherOrder(t *testing.T) {
	t.Parallel()

	alias := NamedCorrelationIdentifier("SOURCE")
	recordType := NewRecordType("SOURCE", true, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
	})
	source, err := NewQuantifiedObjectValue(alias, recordType)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	result := NewRawRecordConstructorValue(RecordConstructorField{Name: "ID", Value: id})
	conflictingType := NewRecordType("OTHER", true, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
	})
	conflicting, err := NewQuantifiedObjectValue(alias, conflictingType)
	if err != nil {
		t.Fatal(err)
	}
	for name, sources := range map[string][]QuantifiedObjectValue{
		"duplicate":              {source, source},
		"matching then conflict": {source, conflicting},
		"conflict then matching": {conflicting, source},
	} {
		_, layoutErr := NewFlatOrdinalLayoutForRetainedResult(result, sources)
		var coded interface{ Code() ResolutionErrorCode }
		if !errors.As(layoutErr, &coded) {
			t.Fatalf("%s error = %v, want typed rejection", name, layoutErr)
		}
		want := CorrelationTypeConflict
		if name == "duplicate" {
			want = LayoutDuplicateSource
		}
		if coded.Code() != want {
			t.Fatalf("%s code = %v, want %v", name, coded.Code(), want)
		}
	}
}

func TestQOVSourceLayoutIsImmutableNonSemanticAndBecomesPhysicalWindows(t *testing.T) {
	t.Parallel()

	legCorrelation := NamedCorrelationIdentifier("L")
	boxType := &RecordType{Fields: []Field{
		{Name: "L_ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "OTHER", Ordinal: 1, FieldType: NotNullString},
	}, Legs: []RecordTypeLeg{
		NewRecordTypeLeg(LegKindFlatRun, legCorrelation, "L", 0, 1),
	}}
	box, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("BOX"), boxType)
	if err != nil {
		t.Fatal(err)
	}
	semanticTwin, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier("BOX"),
		&RecordType{Fields: append([]Field(nil), boxType.Fields...)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !SemanticEqualsUnderAliasMap(box, semanticTwin, EmptyAliasMap()) ||
		SemanticHashCode(box) != SemanticHashCode(semanticTwin) {
		t.Fatal("physical source layout entered QOV semantic equality/hash")
	}
	if flowed := box.FlowedType().(*RecordType); len(flowed.Legs) != 0 {
		t.Fatalf("QOV.FlowedType leaked %d physical legs", len(flowed.Legs))
	}

	fields := make([]RecordConstructorField, len(boxType.Fields))
	for i := range fields {
		field, resolveErr := ResolveFieldOrdinals(box, []int{i})
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		fields[i] = RecordConstructorField{Name: boxType.Fields[i].Name, Value: field}
	}
	layout, err := NewFlatOrdinalLayoutForRetainedResult(
		NewRawRecordConstructorValue(fields...), nil)
	if err != nil {
		t.Fatal(err)
	}
	leg, err := NewQuantifiedObjectValue(legCorrelation, &RecordType{Fields: []Field{
		{Name: "L_ID", Ordinal: 0, FieldType: NotNullLong},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if provided, providesErr := LayoutProvides(layout, leg); providesErr != nil || !provided {
		t.Fatalf("buried source LayoutProvides = (%t, %v), want (true, nil)", provided, providesErr)
	}

	// Caller mutation after QOV construction cannot redirect the admitted
	// physical source. A fresh layout still provides L and not MUTATED.
	boxType.Legs[0].Alias = NamedCorrelationIdentifier("MUTATED")
	mutated, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier("MUTATED"), leg.FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	layoutAfterMutation, err := NewFlatOrdinalLayoutForRetainedResult(
		NewRawRecordConstructorValue(fields...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if provided, providesErr := LayoutProvides(layoutAfterMutation, leg); providesErr != nil || !provided {
		t.Fatalf("input mutation removed frozen L window: (%t, %v)", provided, providesErr)
	}
	if provided, _ := LayoutProvides(layoutAfterMutation, mutated); provided {
		t.Fatal("input mutation redirected the frozen physical source window")
	}
}

func TestRetainedWholeObjectPublishesTwoLevelExactWindows(t *testing.T) {
	t.Parallel()
	leafAlias := NamedCorrelationIdentifier("LEAF")
	middleAlias := NamedCorrelationIdentifier("MIDDLE")
	outerAlias := NamedCorrelationIdentifier("OUTER")
	leafType := NewRecordType("LEAF_ROW", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "NAME", Ordinal: 1, FieldType: NullableString},
	})
	middleType := NewRecordType("MIDDLE_ROW", false, []Field{
		{Name: "LEAF_BOX", Ordinal: 0, FieldType: leafType},
		{Name: "MIDDLE_ID", Ordinal: 1, FieldType: NotNullLong},
	})
	middleType.Legs = []RecordTypeLeg{
		NewRecordTypeLeg(LegKindNested, leafAlias, "LEAF", 0, 1),
	}
	outerType := NewRecordType("OUTER_ROW", false, []Field{
		{Name: "MIDDLE_BOX", Ordinal: 0, FieldType: middleType},
		{Name: "OUTER_ID", Ordinal: 1, FieldType: NotNullLong},
	})
	outerType.Legs = []RecordTypeLeg{
		NewRecordTypeLeg(LegKindNested, middleAlias, "MIDDLE", 0, 1),
	}
	outer := mustLayoutSourceQOV(t, outerAlias.Name(), outerType)
	layout, err := NewFlatOrdinalLayoutForRetainedResult(
		NewRawRecordConstructorValue(RecordConstructorField{Name: "BOX", Value: outer}), nil)
	if err != nil {
		t.Fatal(err)
	}
	middle := mustLayoutSourceQOV(t, middleAlias.Name(), middleType)
	leaf := mustLayoutSourceQOV(t, leafAlias.Name(), leafType)
	for name, source := range map[string]QuantifiedObjectValue{
		"outer": outer, "middle": middle, "leaf": leaf,
	} {
		provided, provideErr := LayoutProvides(layout, source)
		if provideErr != nil || !provided {
			t.Fatalf("LayoutProvides(%s) = (%t, %v), want (true, nil)", name, provided, provideErr)
		}
	}
	leafName, err := ResolveFieldOrdinals(leaf, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	reanchored, err := ReanchorFieldValue(mustReanchorField(t, leafName), layout.Carrier(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := reanchored.Path().Ordinals(); len(got) != 4 ||
		got[0] != 0 || got[1] != 0 || got[2] != 0 || got[3] != 1 {
		t.Fatalf("two-level leaf path = %v, want [0 0 0 1]", got)
	}

	foreign := mustLayoutSourceQOV(t, "FOREIGN_LEAF", leafType)
	foreignName, err := ResolveFieldOrdinals(foreign, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := ReanchorFieldValue(mustReanchorField(t, foreignName), layout.Carrier(), layout)
	requireReanchorError(t, failed, err, ReanchorUnmappedSource)

	mismatched := mustLayoutSourceQOV(t, leafAlias.Name(),
		NewRecordType("LEAF_ROW", false, []Field{
			{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
			{Name: "NAME", Ordinal: 1, FieldType: NotNullString},
		}))
	if provided, provideErr := LayoutProvides(layout, mismatched); provideErr == nil || provided {
		t.Fatalf("mismatched leaf LayoutProvides = (%t, %v), want (false, error)", provided, provideErr)
	}
}
