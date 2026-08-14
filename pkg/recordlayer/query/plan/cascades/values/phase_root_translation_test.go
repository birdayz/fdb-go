package values

import "testing"

func TestTranslatePhaseRootPreservesResolvedPathsAndUsesExactHandles(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("", false, []Field{
		{Name: "A", FieldType: NotNullLong},
		{Name: "N", FieldType: NewRecordType("", false, []Field{{Name: "B", FieldType: NullableString}})},
	})
	sourceLayout, err := NewOrdinalLayoutForCarrierType(
		rowType,
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("source layout: %v", err)
	}
	targetLayout, err := NewOrdinalLayoutForCarrierType(
		rowType,
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target layout: %v", err)
	}
	field, err := ResolveFieldOrdinals(sourceLayout.Carrier(), []int{1, 0})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals: %v", err)
	}
	translated, err := TranslatePhaseRoot(field, sourceLayout.Carrier(), targetLayout.Carrier())
	if err != nil {
		t.Fatalf("TranslatePhaseRoot: %v", err)
	}
	translatedField, ok := AsFieldValue(translated)
	if !ok {
		t.Fatalf("translated field type = %T, want FieldValue", translated)
	}
	if translatedField.ChildValue() != targetLayout.Carrier() {
		t.Fatal("translated field did not retain the exact target current handle")
	}
	if got := translatedField.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Fatalf("translated path = %v, want [1 0]", got)
	}
	if !translatedField.ResultType().Equals(field.Type()) {
		t.Fatalf("translated type = %s, want %s", translatedField.ResultType(), field.Type())
	}
	if same, sameErr := TranslatePhaseRoot(field, sourceLayout.Carrier(), sourceLayout.Carrier()); sameErr != nil || same != field {
		t.Fatalf("exact no-op = (%p,%v), want original %p", same, sameErr, field)
	}
}

func TestTranslatePhaseRootRejectsShapeDriftWithoutPartialResult(t *testing.T) {
	t.Parallel()
	sourceType := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullLong}})
	targetType := NewRecordType("", false, []Field{{Name: "A", FieldType: NullableLong}})
	source, err := NewOrdinalLayoutForCarrierType(sourceType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	target, err := NewOrdinalLayoutForCarrierType(targetType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	field, err := ResolveFieldOrdinals(source.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	if translated, translateErr := TranslatePhaseRoot(field, source.Carrier(), target.Carrier()); translated != nil || translateErr == nil {
		t.Fatalf("shape-drift translation = (%T,%v), want nil,error", translated, translateErr)
	}
}

func TestTranslatePhaseRootDoesNotRewriteRawEqualDistinctNonSource(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullLong}})
	source, _ := NewOrdinalLayoutForCarrierType(rowType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	twin, _ := NewOrdinalLayoutForCarrierType(rowType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	target, _ := NewOrdinalLayoutForCarrierType(rowType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	field, err := ResolveFieldOrdinals(twin.Carrier(), []int{0})
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	translated, err := TranslatePhaseRoot(field, source.Carrier(), target.Carrier())
	if err != nil {
		t.Fatalf("TranslatePhaseRoot: %v", err)
	}
	if translated != field {
		t.Fatal("translation rewrote a semantically equal but allocation-distinct non-source handle")
	}
}

func TestTranslateDeclaredEdgeRootMatchesCorrelationAndExactType(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("edge-row", false, []Field{{
		Name: "A", Ordinal: 0, FieldType: NotNullLong,
	}})
	alias := NamedCorrelationIdentifier("physical-edge")
	declaration := mustLayoutSourceQOV(t, alias.Name(), rowType)
	// Quantifier snapshots are allocation-distinct, so matching only the exact
	// declaration pointer would leave a valid edge program unbindable.
	snapshot := mustLayoutSourceQOV(t, alias.Name(), rowType)
	field, err := ResolveFieldOrdinals(snapshot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	output, err := NewOrdinalLayoutForCarrierType(
		rowType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	translated, err := TranslateDeclaredEdgeRoot(field, declaration, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	translatedField := mustReanchorField(t, translated)
	if translatedField.ChildValue() != output.Carrier() {
		t.Fatal("declared edge snapshot did not move to the exact output carrier")
	}

	// The same correlation can also name a retained source window of another
	// exact type. It is not this whole-edge declaration and must remain intact.
	otherType := NewRecordType("edge-leg", false, []Field{{
		Name: "A", Ordinal: 0, FieldType: NullableLong,
	}})
	otherRoot := mustLayoutSourceQOV(t, alias.Name(), otherType)
	otherField, err := ResolveFieldOrdinals(otherRoot, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := TranslateDeclaredEdgeRoot(otherField, declaration, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if retained != otherField {
		t.Fatal("same-correlation different-type source window was rewritten as the whole edge")
	}
}

func TestTranslateLogicalSourceRootReresolvesExactPhysicalPath(t *testing.T) {
	t.Parallel()
	logicalType := NewRecordType("B", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "V", Ordinal: 1, FieldType: NullableLong},
	})
	physicalType := NewRecordType("", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "V", Ordinal: 1, FieldType: NullableLong},
	})
	alias := NamedCorrelationIdentifier("B")
	declaration, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	logicalSnapshot, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := NewQuantifiedObjectValue(UniqueCorrelationIdentifier(), physicalType)
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(logicalSnapshot, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	translated, err := TranslateLogicalSourceRoot(field, declaration, physical)
	if err != nil {
		t.Fatal(err)
	}
	translatedField := mustReanchorField(t, translated)
	if translatedField.ChildValue() != physical {
		t.Fatal("logical field did not move to the exact physical source handle")
	}
	if got := translatedField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("translated path = %v, want [1]", got)
	}
	if !translatedField.ResultType().Equals(NullableLong) {
		t.Fatalf("translated result type = %s, want nullable LONG", translatedField.ResultType())
	}

	bare, err := TranslateLogicalSourceRoot(logicalSnapshot, declaration, physical)
	if err != nil {
		t.Fatal(err)
	}
	if bare != physical {
		t.Fatal("bare logical source did not become the exact physical source")
	}
}

func TestTranslateLogicalSourceRootKeepsForeignWindowAndFailsClosedOnSlotDrift(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("B")
	logicalType := NewRecordType("B", false, []Field{{
		Name: "V", Ordinal: 0, FieldType: NullableLong,
	}})
	declaration, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	foreignType := NewRecordType("retained-window", false, []Field{{
		Name: "V", Ordinal: 0, FieldType: NullableLong,
	}})
	foreign, err := NewQuantifiedObjectValue(alias, foreignType)
	if err != nil {
		t.Fatal(err)
	}
	foreignField, err := ResolveFieldOrdinals(foreign, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	physicalType := NewRecordType("", false, []Field{{
		Name: "V", Ordinal: 0, FieldType: NullableLong,
	}})
	physical, err := NewQuantifiedObjectValue(UniqueCorrelationIdentifier(), physicalType)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := TranslateLogicalSourceRoot(foreignField, declaration, physical)
	if err != nil {
		t.Fatal(err)
	}
	if retained != foreignField {
		t.Fatal("same-correlation different-type retained window was rewritten")
	}

	logicalField, err := ResolveFieldOrdinals(declaration, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	driftedType := NewRecordType("", false, []Field{{
		Name: "V", Ordinal: 0, FieldType: NullableString,
	}})
	drifted, err := NewQuantifiedObjectValue(UniqueCorrelationIdentifier(), driftedType)
	if err != nil {
		t.Fatal(err)
	}
	if result, translateErr := TranslateLogicalSourceRoot(logicalField, declaration, drifted); result != nil || translateErr == nil {
		t.Fatalf("slot-drift translation = (%T,%v), want nil,error", result, translateErr)
	}
}

func TestTranslateLogicalSourceNameNormalizationChangesOnlyTopLevelRecordName(t *testing.T) {
	t.Parallel()
	source := NamedCorrelationIdentifier("physical-edge")
	logicalType := NewRecordType("T", false, []Field{
		{Name: "ID", FieldType: NotNullLong},
		{Name: "V", FieldType: NullableLong},
	})
	physicalType := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong},
		{Name: "V", FieldType: NullableLong},
	})
	logical, err := NewQuantifiedObjectValue(source, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewQuantifiedObjectValue(UniqueCorrelationIdentifier(), physicalType)
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(logical, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	translated, err := TranslateLogicalSourceNameNormalization(field, source, target)
	if err != nil {
		t.Fatal(err)
	}
	translatedField := mustReanchorField(t, translated)
	if translatedField.ChildValue() != target {
		t.Fatal("nominal logical row did not move to the exact physical target")
	}
	if got := translatedField.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("translated path = %v, want [1]", got)
	}
	originalField := mustReanchorField(t, field)
	if originalField.ChildValue() != logical {
		t.Fatal("name normalization mutated the source FieldValue")
	}

	bare, err := TranslateLogicalSourceNameNormalization(logical, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if bare != target {
		t.Fatal("bare nominal QOV did not become the exact physical target")
	}

	foreign, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("foreign"), logicalType)
	if err != nil {
		t.Fatal(err)
	}
	foreignField, err := ResolveFieldOrdinals(foreign, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := TranslateLogicalSourceNameNormalization(foreignField, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if retained != foreignField {
		t.Fatal("foreign correlation was rewritten")
	}

	narrowType := NewRecordType("T_WINDOW", false, []Field{{
		Name: "V", FieldType: NullableLong,
	}})
	narrow, err := NewQuantifiedObjectValue(source, narrowType)
	if err != nil {
		t.Fatal(err)
	}
	narrowField, err := ResolveFieldOrdinals(narrow, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	retained, err = TranslateLogicalSourceNameNormalization(narrowField, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if retained != narrowField {
		t.Fatal("narrow same-correlation source window was rewritten as the whole edge")
	}

	fieldNameDrift := NewRecordType("T", false, []Field{
		{Name: "OTHER_ID", FieldType: NotNullLong},
		{Name: "V", FieldType: NullableLong},
	})
	drifted, err := NewQuantifiedObjectValue(source, fieldNameDrift)
	if err != nil {
		t.Fatal(err)
	}
	driftedField, err := ResolveFieldOrdinals(drifted, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	retained, err = TranslateLogicalSourceNameNormalization(driftedField, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if retained != driftedField {
		t.Fatal("field-name structural drift was accepted as top-level name normalization")
	}

	if result, translateErr := TranslateLogicalSourceNameNormalization(
		field, CurrentCorrelation(), target,
	); result != nil || translateErr == nil {
		t.Fatalf("current-correlation source = (%T,%v), want nil,error", result, translateErr)
	}
}

func TestTranslateLogicalSourceNameNormalizationFailsClosedOnSelectedPathDrift(t *testing.T) {
	t.Parallel()
	source := NamedCorrelationIdentifier("physical-edge")
	logicalType := NewRecordType("T", false, []Field{
		{Name: "ID", FieldType: NotNullLong},
		{Name: "V", FieldType: NullableLong},
	})
	targetType := NewRecordType("", false, []Field{
		{Name: "ID", FieldType: NotNullLong},
		{Name: "V", FieldType: NullableString},
	})
	logical, err := NewQuantifiedObjectValue(source, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewQuantifiedObjectValue(UniqueCorrelationIdentifier(), targetType)
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(logical, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	retained, err := TranslateLogicalSourceNameNormalization(field, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if retained != field {
		t.Fatal("exact leaf-type drift was accepted as top-level record-name normalization")
	}
}

func TestTranslateProjectionInputNameNormalizationUsesExactOrdinal(t *testing.T) {
	t.Parallel()
	// Duplicate SQL output names are legal at this logical boundary. Construct
	// the ordinary record explicitly: NewRecordType intentionally rejects
	// duplicate lookup names, while the exact QOV snapshot admits them because
	// this bridge is ordinal-only.
	logicalType := &RecordType{Fields: []Field{
		{Name: "X", Ordinal: 0, FieldType: NullableLong},
		{Name: "X", Ordinal: 1, FieldType: NullableLong},
		{Name: "Y", Ordinal: 2, FieldType: NullableLong},
	}}
	physicalType := NewRecordType("", false, []Field{
		{Name: "X", FieldType: NullableLong},
		{Name: "X_2", FieldType: NullableLong},
		{Name: "Y", FieldType: NullableLong},
	})
	alias := UniqueCorrelationIdentifier()
	declaration, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	logicalSnapshot, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewQuantifiedObjectValue(alias, physicalType)
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(logicalSnapshot, []int{2})
	if err != nil {
		t.Fatal(err)
	}

	translated, err := TranslateProjectionInputNameNormalization(field, declaration, target)
	if err != nil {
		t.Fatal(err)
	}
	translatedField := mustReanchorField(t, translated)
	if translatedField.ChildValue() != target {
		t.Fatal("projection input field did not move to the exact physical edge QOV")
	}
	if got := translatedField.Path().Ordinals(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("translated path = %v, want [2]", got)
	}
	if !translatedField.ResultType().Equals(NullableLong) {
		t.Fatalf("translated result type = %s, want nullable LONG", translatedField.ResultType())
	}
	targetRecord, ok := translatedField.ChildValue().Type().(*RecordType)
	if !ok || len(targetRecord.Fields) != 3 || targetRecord.Fields[1].Name != "X_2" {
		t.Fatalf("translated root type = %v, want producer-normalized [X,X_2,Y]", translatedField.ChildValue().Type())
	}

	foreignRoot, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("foreign"), logicalType)
	if err != nil {
		t.Fatal(err)
	}
	foreignField, err := ResolveFieldOrdinals(foreignRoot, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := TranslateProjectionInputNameNormalization(foreignField, declaration, target)
	if err != nil {
		t.Fatal(err)
	}
	if retained != foreignField {
		t.Fatal("foreign-correlation field was rewritten by projection input normalization")
	}
}

func TestTranslateProjectionInputNameNormalizationToCorrelationIsExactAndFailClosed(t *testing.T) {
	t.Parallel()
	logicalType := NewRecordType("", false, []Field{
		{Name: "ELEMENT", FieldType: NullableLong},
		{Name: "ORDINAL", FieldType: NotNullInt},
	})
	physicalType := NewRecordType("", false, []Field{
		{Name: "_0", FieldType: NullableLong},
		{Name: "_1", FieldType: NotNullInt},
	})
	alias := NamedCorrelationIdentifier("UNNEST")
	logical, err := NewQuantifiedObjectValue(alias, logicalType)
	if err != nil {
		t.Fatal(err)
	}
	ordinal, err := ResolveFieldOrdinals(logical, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	translated, err := TranslateProjectionInputNameNormalizationToCorrelation(
		ordinal, alias, physicalType)
	if err != nil {
		t.Fatal(err)
	}
	field := mustReanchorField(t, translated)
	root, ok := AsQuantifiedObjectValue(field.ChildValue())
	if !ok || root.Correlation() != alias || !root.FlowedType().Equals(physicalType) {
		t.Fatalf("translated root = %T/%v, want exact UNNEST physical row", field.ChildValue(), root)
	}
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("translated path = %v, want [1]", got)
	}
	if !field.ResultType().Equals(NotNullInt) {
		t.Fatalf("translated result = %v, want INT NOT NULL", field.ResultType())
	}
	original := mustReanchorField(t, ordinal)
	if original.ChildValue() != logical || !original.ChildValue().Type().Equals(logicalType) {
		t.Fatal("ordinality name normalization mutated its logical source")
	}

	foreign, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("FOREIGN"), logicalType)
	if err != nil {
		t.Fatal(err)
	}
	foreignOrdinal, err := ResolveFieldOrdinals(foreign, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	current := mustLayoutCurrentQOV(t, logicalType)
	currentOrdinal, err := ResolveFieldOrdinals(current, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := NewQuantifiedObjectValue(alias, physicalType)
	if err != nil {
		t.Fatal(err)
	}
	physicalOrdinal, err := ResolveFieldOrdinals(physical, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	declines := []struct {
		name   string
		value  Value
		target Type
	}{
		{name: "foreign named owner", value: foreignOrdinal, target: physicalType},
		{name: "independent current owner", value: currentOrdinal, target: physicalType},
		{name: "same names", value: physicalOrdinal, target: physicalType},
		{name: "width", value: ordinal, target: NewRecordType("", false, []Field{
			{Name: "_0", FieldType: NullableLong},
		})},
		{name: "leaf type", value: ordinal, target: NewRecordType("", false, []Field{
			{Name: "_0", FieldType: NullableLong},
			{Name: "_1", FieldType: NotNullLong},
		})},
		{name: "leaf nullability", value: ordinal, target: NewRecordType("", false, []Field{
			{Name: "_0", FieldType: NullableLong},
			{Name: "_1", FieldType: NullableLong},
		})},
		{name: "record nullability", value: ordinal, target: NewRecordType("", true, []Field{
			{Name: "_0", FieldType: NullableLong},
			{Name: "_1", FieldType: NotNullInt},
		})},
		{name: "record identity", value: ordinal, target: NewRecordType("OTHER", false, []Field{
			{Name: "_0", FieldType: NullableLong},
			{Name: "_1", FieldType: NotNullInt},
		})},
	}
	for _, test := range declines {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, translateErr := TranslateProjectionInputNameNormalizationToCorrelation(
				test.value, alias, test.target)
			if translateErr != nil {
				t.Fatal(translateErr)
			}
			if got != test.value {
				t.Fatalf("declined normalization rebuilt %s", test.name)
			}
		})
	}
}

func TestTranslateProjectionInputNameNormalizationRejectsStructuralDrift(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("derived")
	declarationType := NewRecordType("derived-row", false, []Field{
		{Name: "A", FieldType: NotNullLong},
		{Name: "B", FieldType: NullableString},
	})
	declaration, err := NewQuantifiedObjectValue(alias, declarationType)
	if err != nil {
		t.Fatal(err)
	}
	field, err := ResolveFieldOrdinals(declaration, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		targetAlias CorrelationIdentifier
		targetType  Type
	}{
		{
			name:        "foreign alias",
			targetAlias: NamedCorrelationIdentifier("other"),
			targetType: NewRecordType("derived-row", false, []Field{
				{Name: "A_1", FieldType: NotNullLong},
				{Name: "B_1", FieldType: NullableString},
			}),
		},
		{
			name:        "width",
			targetAlias: alias,
			targetType: NewRecordType("derived-row", false, []Field{
				{Name: "A_1", FieldType: NotNullLong},
			}),
		},
		{
			name:        "ordinal slot swap",
			targetAlias: alias,
			targetType: NewRecordType("derived-row", false, []Field{
				{Name: "B_1", FieldType: NullableString},
				{Name: "A_1", FieldType: NotNullLong},
			}),
		},
		{
			name:        "leaf type",
			targetAlias: alias,
			targetType: NewRecordType("derived-row", false, []Field{
				{Name: "A_1", FieldType: NotNullLong},
				{Name: "B_1", FieldType: NullableBytes},
			}),
		},
		{
			name:        "leaf nullability",
			targetAlias: alias,
			targetType: NewRecordType("derived-row", false, []Field{
				{Name: "A_1", FieldType: NullableLong},
				{Name: "B_1", FieldType: NullableString},
			}),
		},
		{
			name:        "record nullability",
			targetAlias: alias,
			targetType: NewRecordType("derived-row", true, []Field{
				{Name: "A_1", FieldType: NotNullLong},
				{Name: "B_1", FieldType: NullableString},
			}),
		},
		{
			name:        "record identity",
			targetAlias: alias,
			targetType: NewRecordType("other-row", false, []Field{
				{Name: "A_1", FieldType: NotNullLong},
				{Name: "B_1", FieldType: NullableString},
			}),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target, targetErr := NewQuantifiedObjectValue(test.targetAlias, test.targetType)
			if targetErr != nil {
				t.Fatal(targetErr)
			}
			translated, translateErr := TranslateProjectionInputNameNormalization(field, declaration, target)
			if translated != nil || translateErr == nil {
				t.Fatalf("structural-drift translation = (%T,%v), want nil,error", translated, translateErr)
			}
		})
	}

	// A malformed ordinal cannot be used to manufacture a bridge fixture: the
	// checked QOV constructor rejects it before translation is callable.
	malformedOrdinal := &RecordType{RecordName: "derived-row", Fields: []Field{{
		Name: "A", Ordinal: 1, FieldType: NotNullLong,
	}}}
	if admitted, admissionErr := NewQuantifiedObjectValue(alias, malformedOrdinal); admitted != nil || admissionErr == nil {
		t.Fatalf("malformed-ordinal QOV admission = (%T,%v), want nil,error", admitted, admissionErr)
	}
}
