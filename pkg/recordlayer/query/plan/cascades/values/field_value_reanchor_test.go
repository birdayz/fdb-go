package values

import (
	"errors"
	"testing"
)

func mustReanchorField(t testing.TB, value Value) FieldValue {
	t.Helper()
	field, ok := AsFieldValue(value)
	if !ok {
		t.Fatalf("value %T is not an exact FieldValue", value)
	}
	return field
}

func requireReanchorError(t testing.TB, field FieldValue, err error, want ResolutionErrorCode) {
	t.Helper()
	if field != nil {
		t.Fatalf("failed reanchor returned partial field %T", field)
	}
	var coded interface{ Code() ResolutionErrorCode }
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("reanchor error = %v, want code %d", err, want)
	}
}

func TestReanchorFieldValueMapsFieldAndObjectWindowsByCompletePath(t *testing.T) {
	t.Parallel()
	fixture := newLayoutFixture(t, "-reanchor")
	layout, err := NewOrdinalLayout(fixture.carrier, fixture.tiles, fixture.windows[:3])
	if err != nil {
		t.Fatal(err)
	}

	fieldB, err := ResolveFieldOrdinals(fixture.fieldSource, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	reanchoredB, err := ReanchorFieldValue(mustReanchorField(t, fieldB), layout.Carrier(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := reanchoredB.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("field-window path = %v, want [1 1]", got)
	}
	if reanchoredB.ChildValue() != layout.Carrier() {
		t.Fatal("field-window reanchor did not use the layout's exact carrier handle")
	}

	objectB, err := ResolveFieldOrdinals(fixture.objectSource, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	reanchoredObjectB, err := ReanchorFieldValue(mustReanchorField(t, objectB), layout.Carrier(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := reanchoredObjectB.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("object-window path = %v, want [1 1]", got)
	}

	row := &bindingTestRow{slots: []any{
		int64(7),
		&bindingTestRow{slots: []any{int64(8), "nested"}},
		"outer",
		int64(9),
	}}
	binder, err := NewOrdinalObjectBinder(layout, row, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &RowEvalContext{Objects: binder}
	for name, pair := range map[string][2]Value{
		"field":  {fieldB, reanchoredB},
		"object": {objectB, reanchoredObjectB},
	} {
		name, pair := name, pair
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			left, leftErr := pair[0].Evaluate(ctx)
			right, rightErr := pair[1].Evaluate(ctx)
			if leftErr != nil || rightErr != nil || left != "nested" || right != left {
				t.Fatalf("source/carrier evaluation = (%v,%v) / (%v,%v), want nested/nil twice", left, leftErr, right, rightErr)
			}
		})
	}
}

func TestReanchorFieldValueCurrentHandleRulesAndAtomicFailures(t *testing.T) {
	t.Parallel()
	fixture := newLayoutFixture(t, "-rules")
	layout := mustOrdinalLayout(t, fixture)
	target := layout.Carrier()

	targetFieldValue, err := ResolveFieldOrdinals(target, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	targetField := mustReanchorField(t, targetFieldValue)
	noOp, err := ReanchorFieldValue(targetField, target, layout)
	if err != nil || noOp != targetField {
		t.Fatalf("exact-handle no-op = (%T, %v), want original pointer and nil", noOp, err)
	}

	otherCurrent := mustLayoutCurrentQOV(t, fixture.carrierType)
	otherFieldValue, err := ResolveFieldOrdinals(otherCurrent, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	otherField := mustReanchorField(t, otherFieldValue)
	rebuilt, err := ReanchorFieldValue(otherField, target, layout)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt == otherField || rebuilt.ChildValue() != target {
		t.Fatal("equal-type distinct current root was not rebuilt onto the target handle")
	}

	wrongTarget := mustLayoutCurrentQOV(t, fixture.carrierType)
	failed, err := ReanchorFieldValue(targetField, wrongTarget, layout)
	requireReanchorError(t, failed, err, ReanchorTargetMismatch)

	missingSource := mustLayoutSourceQOV(t, "missing", fixture.fieldSourceType)
	missingValue, err := ResolveFieldOrdinals(missingSource, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = ReanchorFieldValue(mustReanchorField(t, missingValue), target, layout)
	requireReanchorError(t, failed, err, ReanchorUnmappedSource)

	var malformed *fieldValue
	failed, err = ReanchorFieldValue(malformed, target, layout)
	requireReanchorError(t, failed, err, ReanchorInvalidValue)

	failed, err = ReanchorFieldValue(targetField, target, (*ordinalLayout)(nil))
	requireReanchorError(t, failed, err, LayoutForeignValue)
}

func TestReanchorFieldValueFieldWindowPreservesNestedSuffix(t *testing.T) {
	t.Parallel()
	leaf := &RecordType{Fields: []Field{
		{Name: "X", Ordinal: 0, FieldType: NotNullLong},
		{Name: "Y", Ordinal: 1, FieldType: NullableString},
	}}
	sourceType := &RecordType{Fields: []Field{{Name: "N", Ordinal: 0, FieldType: leaf}}}
	carrierType := &RecordType{Fields: []Field{
		{Name: "PADDING", Ordinal: 0, FieldType: NotNullLong},
		{Name: "NESTED", Ordinal: 1, FieldType: leaf},
	}}
	source := mustLayoutSourceQOV(t, "nested-source", sourceType)
	carrier := mustLayoutCurrentQOV(t, carrierType)
	layout, err := NewOrdinalLayout(carrier, []OrdinalTileSpec{
		{Start: 0, Width: 1, Kind: OrdinalTileFlat},
		{Start: 1, Width: 1, Kind: OrdinalTileNested},
		{Parent: []int{1}, Start: 0, Width: 2, Kind: OrdinalTileFlat},
	}, []OrdinalWindowSpec{{Source: source, FieldPaths: [][]int{{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := ResolveFieldOrdinals(source, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	reanchored, err := ReanchorFieldValue(mustReanchorField(t, nested), carrier, layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := reanchored.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("mapped nested suffix = %v, want [1 1]", got)
	}
}

func TestReanchorValueThroughProducerUsesOwnedOrUniqueLineageOnly(t *testing.T) {
	t.Parallel()
	outerExact := NewRecordType("CUST", false, []Field{
		{Name: "CID", Ordinal: 0, FieldType: NotNullLong},
	})
	innerExact := NewRecordType("ORD", false, []Field{
		{Name: "CID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "OID", Ordinal: 1, FieldType: NotNullLong},
	})
	outer := mustLayoutSourceQOV(t, "C", outerExact)
	inner := mustLayoutSourceQOV(t, "O", innerExact)
	outerCID, err := ResolveFieldOrdinals(outer, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	innerCID, err := ResolveFieldOrdinals(inner, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	innerOID, err := ResolveFieldOrdinals(inner, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "C_CID", Value: outerCID},
		RecordConstructorField{Name: "O_CID", Value: innerCID},
		RecordConstructorField{Name: "O_OID", Value: innerOID},
	)
	output, err := NewOrdinalLayoutForCarrierType(
		producer.Type(),
		[]OrdinalTileSpec{{Start: 0, Width: 3, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The logical root kept C's owner but predates exact storage record naming.
	// Owner plus complete accessor path identifies the first producer slot.
	logicalOuter := mustLayoutSourceQOV(t, "C",
		NewRecordType("", false, []Field{{Name: "CID", Ordinal: 0, FieldType: NotNullLong}}))
	logicalCID, err := ResolveFieldOrdinals(logicalOuter, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	mappedCID, err := ReanchorValueThroughProducer(logicalCID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	mappedCIDField := mustReanchorField(t, mappedCID)
	if mappedCIDField.ChildValue() != output.Carrier() {
		t.Fatal("owner-matched CID did not use the exact producer output carrier")
	}
	if got := mappedCIDField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("owner-matched CID path = %v, want [0]", got)
	}
	if logicalCID == mappedCID || mustReanchorField(t, logicalCID).ChildValue() != logicalOuter {
		t.Fatal("producer rewrite mutated or reused the input FieldValue")
	}

	// A selected child producer publishes reserved-current, while the retaining
	// FlatMap program still names its logical C/O legs. Correlation therefore
	// cannot select the slot. The child's complete exact row type can: CUST and
	// ORD both expose CID, but only CUST is the incoming current phase.
	currentOuter := mustLayoutCurrentQOV(t, outerExact)
	currentCID, err := ResolveFieldOrdinals(currentOuter, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	mappedCurrent, err := ReanchorValueThroughProducer(currentCID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	mappedCurrentField := mustReanchorField(t, mappedCurrent)
	if mappedCurrentField.ChildValue() != output.Carrier() {
		t.Fatal("exact current child phase did not use the producer output carrier")
	}
	if got := mappedCurrentField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("exact current child path = %v, want [0]", got)
	}

	// Merely being current and exposing CID is not enough. A third exact row
	// shape matches both producer fields by accessor name and neither by owner,
	// so it must remain unchanged instead of guessing C or O.
	foreignCurrentType := NewRecordType("OTHER", false, []Field{
		{Name: "CID", Ordinal: 0, FieldType: NotNullLong},
	})
	foreignCurrent := mustLayoutCurrentQOV(t, foreignCurrentType)
	foreignCurrentCID, err := ResolveFieldOrdinals(foreignCurrent, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	unchangedCurrent, err := ReanchorValueThroughProducer(
		foreignCurrentCID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchangedCurrent != foreignCurrentCID {
		t.Fatal("unowned current row was guessed into a namesake producer slot")
	}

	// An extraction edge can replace O's owner alias. OID is still uniquely
	// identified by its complete accessor path and exact leaf type.
	physicalEdge := mustLayoutSourceQOV(t, "physical-edge", innerExact)
	edgeOID, err := ResolveFieldOrdinals(physicalEdge, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	mappedOID, err := ReanchorValueThroughProducer(edgeOID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	mappedOIDField := mustReanchorField(t, mappedOID)
	if got := mappedOIDField.Path().Ordinals(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("unique OID path = %v, want [2]", got)
	}

	// CID is present on both legs. Without an owner match there is no authority
	// to choose a slot, so the complete original value must survive unchanged.
	edgeCID, err := ResolveFieldOrdinals(physicalEdge, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	ambiguous, err := ReanchorValueThroughProducer(edgeCID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous != edgeCID {
		t.Fatal("ambiguous foreign CID was guessed into one producer leg")
	}
}

func TestReanchorValueThroughProducerPrefersExactOwnerOrdinalPath(t *testing.T) {
	t.Parallel()
	// This is the shape produced by a gathered join: one exact materialized
	// source has two same-named, same-typed ID fields at different ordinals.
	// NewRecordType rejects duplicate names for authored schemas, so the raw
	// type is deliberate; physical merged rows can and do carry this shape.
	leaf := &RecordType{Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "V", Ordinal: 1, FieldType: NullableLong},
		{Name: "ID", Ordinal: 2, FieldType: NullableLong},
		{Name: "HID", Ordinal: 3, FieldType: NullableLong},
	}}
	middle := &RecordType{Fields: []Field{{Name: "_0", Ordinal: 0, FieldType: leaf}}}
	rootType := &RecordType{Fields: []Field{{Name: "_0", Ordinal: 0, FieldType: middle}}}
	root := mustLayoutSourceQOV(t, `$m"47`, rootType)
	resolve := func(path ...int) Value {
		t.Helper()
		result, err := ResolveFieldOrdinals(root, path)
		if err != nil {
			t.Fatalf("resolve %v: %v", path, err)
		}
		return result
	}
	firstID := resolve(0, 0, 0)
	value := resolve(0, 0, 1)
	secondID := resolve(0, 0, 2)
	absentHID := resolve(0, 0, 3)
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "H_ID", Value: firstID},
		RecordConstructorField{Name: "V", Value: value},
		RecordConstructorField{Name: "S1_ID", Value: secondID},
	)
	output, err := NewOrdinalLayoutForCarrierType(
		producer.Type(),
		[]OrdinalTileSpec{{Start: 0, Width: 3, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatalf("construct producer output layout: %v", err)
	}

	for _, test := range []struct {
		name      string
		requested Value
		want      int
	}{
		{"first duplicate ID", firstID, 0},
		{"second duplicate ID", secondID, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapped, mapErr := ReanchorValueThroughProducer(
				test.requested, producer, output.Carrier())
			if mapErr != nil {
				t.Fatalf("reanchor duplicate owner field: %v", mapErr)
			}
			field := mustReanchorField(t, mapped)
			if field.ChildValue() != output.Carrier() {
				t.Fatal("duplicate owner field did not use the exact producer carrier")
			}
			if got := field.Path().Ordinals(); len(got) != 1 || got[0] != test.want {
				t.Fatalf("duplicate owner field output path = %v, want [%d]", got, test.want)
			}
		})
	}

	foreign := mustLayoutSourceQOV(t, "FOREIGN", rootType)
	foreignID, err := ResolveFieldOrdinals(foreign, []int{0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := ReanchorValueThroughProducer(foreignID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignID {
		t.Fatal("foreign same-shaped duplicate ID was guessed into one producer slot")
	}
	wrongLeaf := &RecordType{Fields: []Field{
		{Name: "ID", Ordinal: 0, FieldType: NullableLong},
		{Name: "V", Ordinal: 1, FieldType: NullableString},
		{Name: "ID", Ordinal: 2, FieldType: NullableLong},
		{Name: "HID", Ordinal: 3, FieldType: NullableLong},
	}}
	wrongMiddle := &RecordType{Fields: []Field{{Name: "_0", Ordinal: 0, FieldType: wrongLeaf}}}
	wrongRootType := &RecordType{Fields: []Field{{Name: "_0", Ordinal: 0, FieldType: wrongMiddle}}}
	wrongRoot := mustLayoutSourceQOV(t, root.Correlation().Name(), wrongRootType)
	wrongID, err := ResolveFieldOrdinals(wrongRoot, []int{0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = ReanchorValueThroughProducer(wrongID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != wrongID {
		t.Fatal("same-alias different exact row type borrowed the ordinal match tier")
	}
	unchanged, err = ReanchorValueThroughProducer(absentHID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != absentHID {
		t.Fatal("same-owner field absent from the producer was invented")
	}

	// The bridge is copy-on-write: neither source FieldValue nor its exact root
	// declaration may be altered while the output copies are built.
	if firstIDField := mustReanchorField(t, firstID); firstIDField.ChildValue() != root ||
		!ordinalPathsEqual(firstIDField.Path().Ordinals(), []int{0, 0, 0}) {
		t.Fatalf("producer rewrite mutated first source field: %s", ExplainValue(firstID))
	}
	if secondIDField := mustReanchorField(t, secondID); secondIDField.ChildValue() != root ||
		!ordinalPathsEqual(secondIDField.Path().Ordinals(), []int{0, 0, 2}) {
		t.Fatalf("producer rewrite mutated second source field: %s", ExplainValue(secondID))
	}
}

func TestReanchorValueThroughProducerMapsUniqueBareObjectSlot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		typ  Type
	}{
		{name: "scalar", typ: NotNullInt},
		{name: "record", typ: NewRecordType("ELEMENT", false, []Field{
			{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		})},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			producerRoot := mustLayoutSourceQOV(t, "VAL", tc.typ)
			producer := NewRawRecordConstructorValue(
				RecordConstructorField{Name: "VAL", Value: producerRoot})
			output, err := NewOrdinalLayoutForCarrierType(
				producer.Type(),
				[]OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
			if err != nil {
				t.Fatal(err)
			}

			// Logical and physical construction mint distinct QOV objects for the
			// same declared source. Correlation plus exact type and one producer
			// slot are the complete identity at this boundary.
			requested := mustLayoutSourceQOV(t, "VAL", tc.typ)
			if requested == producerRoot {
				t.Fatal("fixture did not mint independent logical and producer QOVs")
			}
			mapped, err := ReanchorValueThroughProducer(requested, producer, output.Carrier())
			if err != nil {
				t.Fatal(err)
			}
			mappedField := mustReanchorField(t, mapped)
			if mappedField.ChildValue() != output.Carrier() {
				t.Fatal("bare object did not map onto the exact producer carrier")
			}
			if got := mappedField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
				t.Fatalf("bare object output path = %v, want [0]", got)
			}
			if requested.Type().Code() != tc.typ.Code() {
				t.Fatal("producer rewrite mutated the source QOV")
			}

			foreign := mustLayoutSourceQOV(t, "FOREIGN", tc.typ)
			unchanged, err := ReanchorValueThroughProducer(foreign, producer, output.Carrier())
			if err != nil {
				t.Fatal(err)
			}
			if unchanged != foreign {
				t.Fatal("foreign same-typed object was claimed by the producer slot")
			}

			wrongType := mustLayoutSourceQOV(t, "VAL", NullableString)
			unchanged, err = ReanchorValueThroughProducer(wrongType, producer, output.Carrier())
			if err != nil {
				t.Fatal(err)
			}
			if unchanged != wrongType {
				t.Fatal("same-alias wrong-typed object was claimed by the producer slot")
			}
		})
	}

	currentProducer := mustLayoutCurrentQOV(t, NotNullInt)
	currentRC := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "CURRENT", Value: currentProducer})
	currentOutput, err := NewOrdinalLayoutForCarrierType(
		currentRC.Type(),
		[]OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignCurrent := mustLayoutCurrentQOV(t, NotNullInt)
	unchanged, err := ReanchorValueThroughProducer(foreignCurrent, currentRC, currentOutput.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignCurrent {
		t.Fatal("independently minted current phase was claimed by producer correlation alone")
	}

	duplicateRoot := mustLayoutSourceQOV(t, "VAL", NotNullInt)
	duplicateRC := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "VAL0", Value: duplicateRoot},
		RecordConstructorField{Name: "VAL1", Value: duplicateRoot})
	duplicateOutput, err := NewOrdinalLayoutForCarrierType(
		duplicateRC.Type(),
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := mustLayoutSourceQOV(t, "VAL", NotNullInt)
	unchanged, err = ReanchorValueThroughProducer(ambiguous, duplicateRC, duplicateOutput.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != ambiguous {
		t.Fatal("duplicated bare-object source was guessed into one producer slot")
	}
}

func TestReanchorValueThroughProducerCarriesNestedCurrentSuffixExactly(t *testing.T) {
	t.Parallel()
	legType := NewRecordType("LEG", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
		{Name: "NAME", Ordinal: 1, FieldType: NullableString},
	})
	mergedType := NewRecordType("MERGED", false, []Field{
		{Name: "_0", Ordinal: 0, FieldType: legType},
		{Name: "_1", Ordinal: 1, FieldType: legType},
	})
	merged := mustLayoutSourceQOV(t, "MERGED_EDGE", mergedType)
	retainedWhole, err := ResolveFieldOrdinals(merged, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "PADDING", Value: &ConstantValue{Value: int64(7), Typ: NotNullLong}},
		RecordConstructorField{Name: "RETAINED", Value: retainedWhole},
	)
	output, err := NewOrdinalLayoutForCarrierType(
		producer.Type(),
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	currentMerged := mustLayoutCurrentQOV(t, mergedType)
	requested, err := ResolveFieldOrdinals(currentMerged, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := ReanchorValueThroughProducer(requested, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	mappedField := mustReanchorField(t, mapped)
	if mappedField.ChildValue() != output.Carrier() {
		t.Fatal("nested current field did not move to the producer's exact output carrier")
	}
	if got := mappedField.Path().Ordinals(); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("nested current path = %v, want [1 1]", got)
	}
	if requested == mapped || mustReanchorField(t, requested).ChildValue() != currentMerged {
		t.Fatal("nested producer rewrite mutated or reused the input FieldValue")
	}

	// A namesake source is not the exact producer phase and cannot borrow the
	// reserved-current bridge.
	foreign := mustLayoutSourceQOV(t, "FOREIGN", mergedType)
	foreignField, err := ResolveFieldOrdinals(foreign, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := ReanchorValueThroughProducer(foreignField, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != foreignField {
		t.Fatal("foreign correlation crossed a nested producer prefix")
	}

	wrongType := NewRecordType("OTHER_MERGE", false, []Field{
		{Name: "_0", Ordinal: 0, FieldType: WithNullability(legType, true)},
		{Name: "_1", Ordinal: 1, FieldType: legType},
	})
	wrongCurrent := mustLayoutCurrentQOV(t, wrongType)
	wrongTypedField, err := ResolveFieldOrdinals(wrongCurrent, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = ReanchorValueThroughProducer(wrongTypedField, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != wrongTypedField {
		t.Fatal("different exact current type crossed a nested producer prefix")
	}

	wrongPath, err := ResolveFieldOrdinals(currentMerged, []int{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = ReanchorValueThroughProducer(wrongPath, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != wrongPath {
		t.Fatal("non-retained nested path crossed a different producer prefix")
	}
}

func TestReanchorOwnedValueThroughProducerRejectsForeignUniqueSlot(t *testing.T) {
	t.Parallel()
	inputType := NewRecordType("U", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
	})
	input := mustLayoutCurrentQOV(t, inputType)
	inputID, err := ResolveFieldOrdinals(input, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	producer := NewRawRecordConstructorValue(
		RecordConstructorField{Name: "A.ID", Value: inputID},
	)
	output, err := NewOrdinalLayoutForCarrierType(
		producer.Type(), []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	outerType := NewRecordType("T", false, []Field{
		{Name: "ID", Ordinal: 0, FieldType: NotNullLong},
	})
	outer := mustLayoutSourceQOV(t, "A", outerType)
	outerID, err := ResolveFieldOrdinals(outer, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	owned := map[CorrelationIdentifier]struct{}{input.Correlation(): {}}

	mapped, err := ReanchorOwnedValueThroughProducer(inputID, producer, output.Carrier(), owned)
	if err != nil {
		t.Fatal(err)
	}
	mappedField := mustReanchorField(t, mapped)
	if mappedField.ChildValue() != output.Carrier() {
		t.Fatal("owned inner field did not move to the producer carrier")
	}
	if got := mappedField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("owned inner path = %v, want [0]", got)
	}

	retained, err := ReanchorOwnedValueThroughProducer(outerID, producer, output.Carrier(), owned)
	if err != nil {
		t.Fatal(err)
	}
	if retained != outerID {
		t.Fatal("foreign outer A.ID was captured by a one-slot A.ID producer")
	}

	ordinary, err := ReanchorValueThroughProducer(outerID, producer, output.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	if ordinary == outerID {
		t.Fatal("ordinary producer bridge lost its intentional unique-slot fallback")
	}
}

func TestReanchorFieldValueFrontierBridgeRequiresPinAndExactWholeRow(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("frontier", false, []Field{
		{Name: "A", Ordinal: 0, FieldType: NotNullLong},
	})
	output, err := NewOrdinalLayoutForCarrierType(
		rowType, []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := mustLayoutSourceQOV(t, "logical-owner", rowType)
	pinnedValue, err := ResolveOrdinalSeedField(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ReanchorFieldValue(mustReanchorField(t, pinnedValue), output.Carrier(), output)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ChildValue() != output.Carrier() {
		t.Fatal("frontier-pinned same-row field did not cross onto the exact carrier")
	}

	unpinnedValue, err := ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := ReanchorFieldValue(mustReanchorField(t, unpinnedValue), output.Carrier(), output)
	requireReanchorError(t, failed, err, ReanchorUnmappedSource)

	otherSource := mustLayoutSourceQOV(t, "logical-owner",
		NewRecordType("other-frontier", false, []Field{{Name: "A", Ordinal: 0, FieldType: NullableLong}}))
	otherPinned, err := ResolveOrdinalSeedField(otherSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = ReanchorFieldValue(mustReanchorField(t, otherPinned), output.Carrier(), output)
	requireReanchorError(t, failed, err, ReanchorUnmappedSource)
}

func TestPinValueToExactFrontierPinsOnlyOwnedCurrentFields(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("aggregate-input", false, []Field{
		{Name: "A", Ordinal: 0, FieldType: NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: NotNullLong},
	})
	ownedLayout, err := NewOrdinalLayoutForCarrierType(
		rowType, []OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignLayout, err := NewOrdinalLayoutForCarrierType(
		rowType, []OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := ResolveFieldOrdinals(ownedLayout.Carrier(), []int{0})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := ResolveFieldOrdinals(foreignLayout.Carrier(), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	externalRoot := mustLayoutSourceQOV(t, "external", rowType)
	external, err := ResolveFieldOrdinals(externalRoot, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	expression := &ArithmeticValue{
		Op:   OpAdd,
		Left: owned,
		Right: &ArithmeticValue{
			Op:    OpAdd,
			Left:  foreign,
			Right: external,
		},
	}
	pinned, err := PinValueToExactFrontier(expression, ownedLayout.Carrier())
	if err != nil {
		t.Fatal(err)
	}
	pinnedExpression := pinned.(*ArithmeticValue)
	pinnedOwned := mustReanchorField(t, pinnedExpression.Left)
	if pinnedOwned.ChildValue() != ownedLayout.Carrier() || !pinnedOwned.Path().IsFrontierPinned() {
		t.Fatal("owned current field did not acquire the exact frontier contract")
	}
	nested := pinnedExpression.Right.(*ArithmeticValue)
	if nested.Left != foreign || nested.Right != external {
		t.Fatal("foreign current or named external field was rewritten")
	}
	if mustReanchorField(t, foreign).Path().IsFrontierPinned() ||
		mustReanchorField(t, external).Path().IsFrontierPinned() ||
		mustReanchorField(t, owned).Path().IsFrontierPinned() {
		t.Fatal("pinning mutated a source or foreign FieldValue")
	}

	if _, err := PinValueToExactFrontier(expression, externalRoot); err == nil {
		t.Fatal("named source was accepted as a physical frontier carrier")
	}
}
