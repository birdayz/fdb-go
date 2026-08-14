package values

import (
	"errors"
	"testing"
)

type layoutCodedError interface {
	Code() ResolutionErrorCode
}

type layoutFixture struct {
	carrierType     *RecordType
	nestedType      *RecordType
	fieldSourceType *RecordType
	carrier         *quantifiedObjectValue
	fieldSource     *quantifiedObjectValue
	objectSource    *quantifiedObjectValue
	scalarSource    *quantifiedObjectValue
	nullableSource  *quantifiedObjectValue
	tiles           []OrdinalTileSpec
	windows         []OrdinalWindowSpec
}

func newLayoutFixture(t testing.TB, suffix string) *layoutFixture {
	t.Helper()
	nested := &RecordType{
		RecordName: "Nested",
		Fields: []Field{
			{Name: "INNER_LONG", Ordinal: 0, FieldType: NotNullLong},
			{Name: "INNER_STRING", Ordinal: 1, FieldType: NewPrimitiveType(TypeCodeString, true)},
		},
	}
	carrierType := &RecordType{
		RecordName: "Carrier",
		Fields: []Field{
			{Name: "OUTER_LONG", Ordinal: 0, FieldType: NotNullLong},
			{Name: "NESTED", Ordinal: 1, FieldType: nested},
			{Name: "OUTER_STRING", Ordinal: 2, FieldType: NewPrimitiveType(TypeCodeString, true)},
			{Name: "NULLABLE_LONG", Ordinal: 3, FieldType: NullableLong},
		},
	}
	fieldSourceType := &RecordType{
		RecordName: "SourceFields",
		Fields: []Field{
			{Name: "A", Ordinal: 0, FieldType: NotNullLong},
			{Name: "B", Ordinal: 1, FieldType: NewPrimitiveType(TypeCodeString, true)},
		},
	}
	fixture := &layoutFixture{
		carrierType:     carrierType,
		nestedType:      nested,
		fieldSourceType: fieldSourceType,
		carrier:         mustLayoutCurrentQOV(t, carrierType),
		fieldSource:     mustLayoutSourceQOV(t, "fields"+suffix, fieldSourceType),
		objectSource:    mustLayoutSourceQOV(t, "object"+suffix, nested),
		scalarSource:    mustLayoutSourceQOV(t, "scalar"+suffix, NotNullLong),
		nullableSource:  mustLayoutSourceQOV(t, "nullable"+suffix, NullableLong),
		tiles: []OrdinalTileSpec{
			{Start: 0, Width: 1, Kind: OrdinalTileFlat},
			{Start: 1, Width: 1, Kind: OrdinalTileNested},
			{Start: 2, Width: 2, Kind: OrdinalTileFlat},
			{Parent: []int{1}, Start: 0, Width: 2, Kind: OrdinalTileFlat},
		},
	}
	fixture.windows = []OrdinalWindowSpec{
		{Source: fixture.fieldSource, FieldPaths: [][]int{{0}, {1, 1}}},
		{Source: fixture.objectSource, ObjectPath: []int{1}},
		{Source: fixture.scalarSource, ObjectPath: []int{1, 0}},
		{Source: fixture.nullableSource, ObjectPath: []int{3}, NullSupplying: true},
	}
	return fixture
}

func mustLayoutCurrentQOV(t testing.TB, typ Type) *quantifiedObjectValue {
	t.Helper()
	handle, err := SnapshotExactType(typ)
	if err != nil {
		t.Fatalf("SnapshotExactType(%v): %v", typ, err)
	}
	return &quantifiedObjectValue{correlation: CurrentCorrelation(), flowed: handle.(*exactType)}
}

func mustLayoutSourceQOV(t testing.TB, name string, typ Type) *quantifiedObjectValue {
	t.Helper()
	qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(name), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q, %v): %v", name, typ, err)
	}
	return qov.(*quantifiedObjectValue)
}

func mustOrdinalLayout(t testing.TB, fixture *layoutFixture) OrdinalLayout {
	t.Helper()
	layout, err := NewOrdinalLayout(fixture.carrier, fixture.tiles, fixture.windows)
	if err != nil {
		t.Fatalf("NewOrdinalLayout: %v", err)
	}
	return layout
}

func requireLayoutError(t testing.TB, layout OrdinalLayout, err error, want ResolutionErrorCode) {
	t.Helper()
	if layout != nil {
		t.Fatalf("failure returned partial layout %T", layout)
	}
	var coded layoutCodedError
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("error = %v, want code %d", err, want)
	}
}

func requireLayoutPurposeError(t testing.TB, provided bool, err error, want ResolutionErrorCode) {
	t.Helper()
	if provided {
		t.Fatal("failed LayoutProvides returned true")
	}
	var coded layoutCodedError
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("error = %v, want code %d", err, want)
	}
}

func cloneLayoutTiles(input []OrdinalTileSpec) []OrdinalTileSpec {
	result := make([]OrdinalTileSpec, len(input))
	for i := range input {
		result[i] = input[i]
		result[i].Parent = append([]int(nil), input[i].Parent...)
	}
	return result
}

func cloneLayoutWindows(input []OrdinalWindowSpec) []OrdinalWindowSpec {
	result := make([]OrdinalWindowSpec, len(input))
	for i := range input {
		result[i] = input[i]
		if input[i].ObjectPath != nil {
			result[i].ObjectPath = append([]int(nil), input[i].ObjectPath...)
		}
		if input[i].FieldPaths != nil {
			result[i].FieldPaths = make([][]int, len(input[i].FieldPaths))
			for j := range input[i].FieldPaths {
				result[i].FieldPaths[j] = append([]int(nil), input[i].FieldPaths[j]...)
			}
		}
	}
	return result
}

func TestOrdinalLayoutRecordAndScalarFactories(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "")
	layout := mustOrdinalLayout(t, fixture)
	if layout.CarrierKind() != OrdinalCarrierRecord {
		t.Fatalf("CarrierKind = %d, want record", layout.CarrierKind())
	}
	if layout.Carrier() != fixture.carrier {
		t.Fatal("Carrier did not return the admitted exact QOV")
	}
	if !layout.RawEqual(layout) || !layout.EqualUnderAliases(layout, EmptyAliasMap()) {
		t.Fatal("layout is not equal to itself")
	}
	for _, source := range []*quantifiedObjectValue{
		fixture.fieldSource,
		fixture.objectSource,
		fixture.scalarSource,
		fixture.nullableSource,
	} {
		provided, err := LayoutProvides(layout, source)
		if err != nil || !provided {
			t.Fatalf("LayoutProvides(%q) = (%t, %v), want (true, nil)", source.correlation, provided, err)
		}
	}

	zeroRecord, err := NewOrdinalLayout(
		mustLayoutCurrentQOV(t, &RecordType{RecordName: "Unit"}),
		nil,
		nil,
	)
	if err != nil || zeroRecord.CarrierKind() != OrdinalCarrierRecord {
		t.Fatalf("zero-field record layout = (%T, %v)", zeroRecord, err)
	}

	flatRecord, err := NewOrdinalLayout(
		fixture.carrier,
		[]OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}},
		nil,
	)
	if err != nil || flatRecord == nil {
		t.Fatalf("flat record-valued column was inferred as nested: (%T, %v)", flatRecord, err)
	}

	emptyNestedType := &RecordType{
		RecordName: "HasUnit",
		Fields:     []Field{{Name: "UNIT", Ordinal: 0, FieldType: &RecordType{RecordName: "Unit"}}},
	}
	emptyNested, err := NewOrdinalLayout(
		mustLayoutCurrentQOV(t, emptyNestedType),
		[]OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileNested}},
		nil,
	)
	if err != nil || emptyNested == nil {
		t.Fatalf("empty nested record layout = (%T, %v)", emptyNested, err)
	}

	scalarCarrier := mustLayoutCurrentQOV(t, NotNullLong)
	scalar, err := NewScalarOrdinalLayout(scalarCarrier)
	if err != nil {
		t.Fatalf("NewScalarOrdinalLayout: %v", err)
	}
	if scalar.CarrierKind() != OrdinalCarrierScalar || scalar.Carrier() != scalarCarrier {
		t.Fatalf("scalar carrier view = (%d, %T)", scalar.CarrierKind(), scalar.Carrier())
	}
	provided, err := LayoutProvides(scalar, fixture.scalarSource)
	requireLayoutPurposeError(t, provided, err, LayoutSourceNotProvided)

	recordFromScalar, err := NewOrdinalLayout(scalarCarrier, nil, nil)
	requireLayoutError(t, recordFromScalar, err, LayoutNonRecordCarrier)
	scalarFromRecord, err := NewScalarOrdinalLayout(fixture.carrier)
	requireLayoutError(t, scalarFromRecord, err, LayoutNonRecordCarrier)
	erasedRecord, err := NewOrdinalLayout(mustLayoutCurrentQOV(t, NewAnyRecordType(true)), nil, nil)
	requireLayoutError(t, erasedRecord, err, LayoutNonRecordCarrier)

	namedRecord := mustLayoutSourceQOV(t, "not_current_record", fixture.carrierType)
	namedLayout, err := NewOrdinalLayout(namedRecord, fixture.tiles, fixture.windows)
	requireLayoutError(t, namedLayout, err, CorrelationKindMismatch)
	namedScalar := mustLayoutSourceQOV(t, "not_current_scalar", NotNullLong)
	namedScalarLayout, err := NewScalarOrdinalLayout(namedScalar)
	requireLayoutError(t, namedScalarLayout, err, CorrelationKindMismatch)
}

func TestIsCanonicalCurrentOnlyOrdinalLayout(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "")
	windowed := mustOrdinalLayout(t, fixture)
	canonical, err := IsCanonicalCurrentOnlyOrdinalLayout(windowed)
	if err != nil || canonical {
		t.Fatalf("windowed layout = (%t, %v), want (false, nil)", canonical, err)
	}

	currentOnly, err := NewOrdinalLayout(fixture.carrier, fixture.tiles, nil)
	if err != nil {
		t.Fatalf("NewOrdinalLayout(current-only): %v", err)
	}
	canonical, err = IsCanonicalCurrentOnlyOrdinalLayout(currentOnly)
	if err != nil || !canonical {
		t.Fatalf("current-only record layout = (%t, %v), want (true, nil)", canonical, err)
	}

	scalar, err := NewScalarOrdinalLayout(mustLayoutCurrentQOV(t, NotNullLong))
	if err != nil {
		t.Fatalf("NewScalarOrdinalLayout: %v", err)
	}
	canonical, err = IsCanonicalCurrentOnlyOrdinalLayout(scalar)
	if err != nil || !canonical {
		t.Fatalf("current-only scalar layout = (%t, %v), want (true, nil)", canonical, err)
	}

	foreign := &embeddedOrdinalLayout{OrdinalLayout: currentOnly}
	canonical, err = IsCanonicalCurrentOnlyOrdinalLayout(foreign)
	requireLayoutPurposeError(t, canonical, err, LayoutForeignValue)
	var typedNil *ordinalLayout
	canonical, err = IsCanonicalCurrentOnlyOrdinalLayout(typedNil)
	requireLayoutPurposeError(t, canonical, err, LayoutForeignValue)
}

func TestOrdinalLayoutAnyRecordIsOpaqueButExact(t *testing.T) {
	t.Parallel()

	anyRecord := NewAnyRecordType(true)
	arrayOfAnyRecord := &ArrayType{Nullable: true, ElementType: NewAnyRecordType(false)}
	carrierType := &RecordType{
		RecordName: "OpaqueCarrier",
		Fields: []Field{
			{Name: "MESSAGE", Ordinal: 0, FieldType: anyRecord},
			{Name: "REPEATED_MESSAGE", Ordinal: 1, FieldType: arrayOfAnyRecord},
		},
	}
	carrier := mustLayoutCurrentQOV(t, carrierType)
	messageSource := mustLayoutSourceQOV(t, "message", anyRecord)
	repeatedSource := mustLayoutSourceQOV(t, "repeated", arrayOfAnyRecord)
	flatTiles := []OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}}
	layout, err := NewOrdinalLayout(
		carrier,
		flatTiles,
		[]OrdinalWindowSpec{
			{Source: messageSource, ObjectPath: []int{0}},
			{Source: repeatedSource, ObjectPath: []int{1}},
		},
	)
	if err != nil || layout == nil {
		t.Fatalf("opaque exact leaves were rejected: (%T, %v)", layout, err)
	}
	for _, source := range []*quantifiedObjectValue{messageSource, repeatedSource} {
		provided, providesErr := LayoutProvides(layout, source)
		if providesErr != nil || !provided {
			t.Fatalf("LayoutProvides(%q) = (%t, %v)", source.correlation, provided, providesErr)
		}
	}

	nestedTiles := []OrdinalTileSpec{
		{Start: 0, Width: 1, Kind: OrdinalTileNested},
		{Start: 1, Width: 1, Kind: OrdinalTileFlat},
	}
	nested, err := NewOrdinalLayout(carrier, nestedTiles, nil)
	requireLayoutError(t, nested, err, LayoutInvalidTile)

	descent, err := NewOrdinalLayout(
		carrier,
		flatTiles,
		[]OrdinalWindowSpec{{Source: mustLayoutSourceQOV(t, "leaf", NullableLong), ObjectPath: []int{0, 0}}},
	)
	requireLayoutError(t, descent, err, LayoutInvalidPath)

	fieldExpanded, err := NewOrdinalLayout(
		carrier,
		flatTiles,
		[]OrdinalWindowSpec{{Source: messageSource, FieldPaths: [][]int{{0}}}},
	)
	requireLayoutError(t, fieldExpanded, err, LayoutInvalidWindow)
}

func TestOrdinalLayoutFactoryRejectsEverySingleFieldMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*layoutFixture)
		want   ResolutionErrorCode
	}{
		{
			name: "invalid tile kind",
			mutate: func(f *layoutFixture) {
				f.tiles[0].Kind = OrdinalTileInvalid
			},
			want: LayoutInvalidTile,
		},
		{
			name: "negative tile start",
			mutate: func(f *layoutFixture) {
				f.tiles[0].Start = -1
			},
			want: LayoutInvalidTile,
		},
		{
			name: "zero tile width",
			mutate: func(f *layoutFixture) {
				f.tiles[0].Width = 0
			},
			want: LayoutInvalidTile,
		},
		{
			name: "out of bounds tile",
			mutate: func(f *layoutFixture) {
				f.tiles[2].Width = 3
			},
			want: LayoutInvalidTile,
		},
		{
			name: "nested tile width",
			mutate: func(f *layoutFixture) {
				f.tiles[1].Width = 2
			},
			want: LayoutInvalidTile,
		},
		{
			name: "nested tile on scalar",
			mutate: func(f *layoutFixture) {
				f.tiles[0].Kind = OrdinalTileNested
			},
			want: LayoutInvalidTile,
		},
		{
			name: "negative parent path",
			mutate: func(f *layoutFixture) {
				f.tiles[3].Parent = []int{-1}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "parent descends through scalar",
			mutate: func(f *layoutFixture) {
				f.tiles[3].Parent = []int{0}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "tile gap",
			mutate: func(f *layoutFixture) {
				f.tiles[2].Start = 3
				f.tiles[2].Width = 1
			},
			want: LayoutTileGap,
		},
		{
			name: "tile overlap",
			mutate: func(f *layoutFixture) {
				f.tiles[2].Start = 1
				f.tiles[2].Width = 3
			},
			want: LayoutTileOverlap,
		},
		{
			name: "missing nested partition",
			mutate: func(f *layoutFixture) {
				f.tiles = f.tiles[:3]
			},
			want: LayoutTileGap,
		},
		{
			name: "undeclared nested parent",
			mutate: func(f *layoutFixture) {
				f.tiles[1].Kind = OrdinalTileFlat
			},
			want: LayoutInvalidTile,
		},
		{
			name: "negative window path",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{-1}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "out of range window path",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{99}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "window path descends through scalar",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{0, 0}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "empty object path",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "both window modes",
			mutate: func(f *layoutFixture) {
				f.windows[0].ObjectPath = []int{1}
			},
			want: LayoutInvalidWindow,
		},
		{
			name: "neither window mode",
			mutate: func(f *layoutFixture) {
				f.windows[0].FieldPaths = nil
			},
			want: LayoutInvalidWindow,
		},
		{
			name: "field path count",
			mutate: func(f *layoutFixture) {
				f.windows[0].FieldPaths = f.windows[0].FieldPaths[:1]
			},
			want: LayoutInvalidWindow,
		},
		{
			name: "empty field path",
			mutate: func(f *layoutFixture) {
				f.windows[0].FieldPaths[1] = []int{}
			},
			want: LayoutInvalidPath,
		},
		{
			name: "duplicate field address",
			mutate: func(f *layoutFixture) {
				f.windows[0].FieldPaths[1] = []int{0}
			},
			want: LayoutInvalidWindow,
		},
		{
			name: "scalar in field mode",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = nil
				f.windows[2].FieldPaths = [][]int{{1, 0}}
			},
			want: LayoutInvalidWindow,
		},
		{
			name: "duplicate exact source",
			mutate: func(f *layoutFixture) {
				f.windows = append(f.windows, OrdinalWindowSpec{Source: f.fieldSource, ObjectPath: []int{1}})
			},
			want: LayoutDuplicateSource,
		},
		{
			name: "same correlation different type",
			mutate: func(f *layoutFixture) {
				conflict := mustLayoutSourceQOV(t, "fields", f.nestedType)
				f.windows = append(f.windows, OrdinalWindowSpec{Source: conflict, ObjectPath: []int{1}})
			},
			want: CorrelationTypeConflict,
		},
		{
			name: "type mismatch",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{2}
			},
			want: LayoutTypeMismatch,
		},
		{
			name: "nullability mismatch",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{3}
			},
			want: LayoutNullabilityMismatch,
		},
		{
			name: "nonnull null supplying source",
			mutate: func(f *layoutFixture) {
				f.windows[2].NullSupplying = true
			},
			want: LayoutNullabilityMismatch,
		},
		{
			name: "current as source",
			mutate: func(f *layoutFixture) {
				f.windows[2].Source = mustLayoutCurrentQOV(t, NotNullLong)
			},
			want: CorrelationKindMismatch,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newLayoutFixture(t, "")
			fixture.tiles = cloneLayoutTiles(fixture.tiles)
			fixture.windows = cloneLayoutWindows(fixture.windows)
			testCase.mutate(fixture)
			layout, err := NewOrdinalLayout(fixture.carrier, fixture.tiles, fixture.windows)
			requireLayoutError(t, layout, err, testCase.want)
		})
	}
}

func TestOrdinalLayoutWindowNullabilityUsesInheritedOR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		carrier *RecordType
		path    []int
	}{
		{
			name: "nullable root",
			carrier: &RecordType{
				RecordName: "NullableRoot",
				Nullable:   true,
				Fields:     []Field{{Name: "VALUE", Ordinal: 0, FieldType: NotNullLong}},
			},
			path: []int{0},
		},
		{
			name: "nullable intermediate",
			carrier: &RecordType{
				RecordName: "Root",
				Fields: []Field{{
					Name:    "NESTED",
					Ordinal: 0,
					FieldType: &RecordType{
						RecordName: "NullableNested",
						Nullable:   true,
						Fields:     []Field{{Name: "VALUE", Ordinal: 0, FieldType: NotNullLong}},
					},
				}},
			},
			path: []int{0, 0},
		},
		{
			name: "nullable leaf",
			carrier: &RecordType{
				RecordName: "Root",
				Fields:     []Field{{Name: "VALUE", Ordinal: 0, FieldType: NullableLong}},
			},
			path: []int{0},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			width := len(testCase.carrier.Fields)
			tiles := []OrdinalTileSpec{{Start: 0, Width: width, Kind: OrdinalTileFlat}}
			nullableSource := mustLayoutSourceQOV(t, "nullable", NullableLong)
			layout, err := NewOrdinalLayout(
				mustLayoutCurrentQOV(t, testCase.carrier),
				tiles,
				[]OrdinalWindowSpec{{Source: nullableSource, ObjectPath: testCase.path}},
			)
			if err != nil || layout == nil {
				t.Fatalf("nullable source did not match inherited nullability: (%T, %v)", layout, err)
			}
			nonnullSource := mustLayoutSourceQOV(t, "nonnull", NotNullLong)
			mismatch, err := NewOrdinalLayout(
				mustLayoutCurrentQOV(t, testCase.carrier),
				tiles,
				[]OrdinalWindowSpec{{Source: nonnullSource, ObjectPath: testCase.path}},
			)
			requireLayoutError(t, mismatch, err, LayoutNullabilityMismatch)
		})
	}
}

func TestOrdinalLayoutSnapshotsEveryMutableInputAndGetter(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "")
	layout := mustOrdinalLayout(t, fixture)
	independent := mustOrdinalLayout(t, newLayoutFixture(t, ""))
	wantHash := layout.AliasFreeHash()

	fixture.tiles[0].Parent = []int{99}
	fixture.tiles[0].Start = 99
	fixture.tiles[3].Parent[0] = 99
	fixture.windows[0].FieldPaths[0][0] = 99
	fixture.windows[1].ObjectPath[0] = 99
	fixture.windows[3].NullSupplying = false
	fixture.tiles[1] = OrdinalTileSpec{}
	fixture.windows[2] = OrdinalWindowSpec{}
	fixture.carrierType.RecordName = "MUTATED"
	fixture.carrierType.Fields[0] = Field{Name: "MUTATED", Ordinal: 0, FieldType: NewPrimitiveType(TypeCodeBytes, true)}
	fixture.nestedType.Fields = nil
	fixture.fieldSourceType.Fields = nil

	getterType := layout.Carrier().FlowedType().(*RecordType)
	getterType.RecordName = "GETTER_MUTATED"
	getterType.Fields[0] = Field{Name: "GETTER_MUTATED", Ordinal: 0, FieldType: NullableInt}
	secondGetter := layout.Carrier().FlowedType().(*RecordType)
	if secondGetter.RecordName != "Carrier" || secondGetter.Fields[0].Name != "OUTER_LONG" {
		t.Fatalf("carrier FlowedType getter leaked mutation: %#v", secondGetter)
	}

	if !layout.RawEqual(independent) || !independent.RawEqual(layout) {
		t.Fatal("input or getter mutation changed snapshotted layout equality")
	}
	if layout.AliasFreeHash() != wantHash || independent.AliasFreeHash() != wantHash {
		t.Fatal("input or getter mutation changed cached alias-free hash")
	}
	for _, source := range []*quantifiedObjectValue{
		mustLayoutSourceQOV(t, "fields", (&RecordType{
			RecordName: "SourceFields",
			Fields: []Field{
				{Name: "A", Ordinal: 0, FieldType: NotNullLong},
				{Name: "B", Ordinal: 1, FieldType: NewPrimitiveType(TypeCodeString, true)},
			},
		})),
		mustLayoutSourceQOV(t, "scalar", NotNullLong),
	} {
		provided, err := LayoutProvides(layout, source)
		if err != nil || !provided {
			t.Fatalf("snapshotted LayoutProvides(%q) = (%t, %v)", source.correlation, provided, err)
		}
	}
}

func TestOrdinalLayoutIdentityAndAliasFreeHashLaws(t *testing.T) {
	t.Parallel()

	leftFixture := newLayoutFixture(t, "_left")
	left := mustOrdinalLayout(t, leftFixture)

	orderedFixture := newLayoutFixture(t, "_left")
	orderedFixture.tiles[0], orderedFixture.tiles[3] = orderedFixture.tiles[3], orderedFixture.tiles[0]
	orderedFixture.windows[0], orderedFixture.windows[3] = orderedFixture.windows[3], orderedFixture.windows[0]
	ordered := mustOrdinalLayout(t, orderedFixture)
	if !left.RawEqual(ordered) || !ordered.RawEqual(left) {
		t.Fatal("tile/window input ordering changed raw layout identity")
	}
	if left.AliasFreeHash() != ordered.AliasFreeHash() {
		t.Fatal("raw-equal layouts have different hashes")
	}

	rightFixture := newLayoutFixture(t, "_right")
	right := mustOrdinalLayout(t, rightFixture)
	if left.RawEqual(right) || right.RawEqual(left) {
		t.Fatal("renamed source layouts compared raw-equal")
	}
	pairs := []AliasPair{{Source: CurrentCorrelation(), Target: CurrentCorrelation()}}
	for i := range leftFixture.windows {
		pairs = append(pairs, AliasPair{
			Source: leftFixture.windows[i].Source.Correlation(),
			Target: rightFixture.windows[i].Source.Correlation(),
		})
	}
	aliases, err := NewAliasMap(pairs)
	if err != nil {
		t.Fatalf("NewAliasMap: %v", err)
	}
	if !left.EqualUnderAliases(right, aliases) {
		t.Fatal("consistent source alpha-renaming was not equal")
	}
	if left.AliasFreeHash() != right.AliasFreeHash() {
		t.Fatal("alias-equal layouts have different alias-free hashes")
	}
	inversePairs := make([]AliasPair, len(pairs))
	for i := range pairs {
		inversePairs[i] = AliasPair{Source: pairs[i].Target, Target: pairs[i].Source}
	}
	inverseAliases, err := NewAliasMap(inversePairs)
	if err != nil {
		t.Fatalf("NewAliasMap(inverse): %v", err)
	}
	if !right.EqualUnderAliases(left, inverseAliases) {
		t.Fatal("inverse consistent source alpha-renaming was not equal")
	}
	if !left.EqualUnderAliases(ordered, nil) {
		t.Fatal("nil AliasMap did not act as the identity map")
	}

	mutations := []struct {
		name   string
		mutate func(*layoutFixture)
	}{
		{
			name: "physical tile shape",
			mutate: func(f *layoutFixture) {
				f.tiles = []OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}}
			},
		},
		{
			name: "object path",
			mutate: func(f *layoutFixture) {
				f.windows[2].ObjectPath = []int{0}
			},
		},
		{
			name: "field paths",
			mutate: func(f *layoutFixture) {
				f.windows[0].FieldPaths = [][]int{{1, 0}, {2}}
			},
		},
		{
			name: "object versus fields mode",
			mutate: func(f *layoutFixture) {
				f.windows[1].ObjectPath = nil
				f.windows[1].FieldPaths = [][]int{{1, 0}, {1, 1}}
			},
		},
		{
			name: "null supplying bit",
			mutate: func(f *layoutFixture) {
				f.windows[3].NullSupplying = false
			},
		},
		{
			name: "source exact type",
			mutate: func(f *layoutFixture) {
				changedType := &RecordType{
					RecordName: "DifferentSourceName",
					Fields: []Field{
						{Name: "A", Ordinal: 0, FieldType: NotNullLong},
						{Name: "B", Ordinal: 1, FieldType: NewPrimitiveType(TypeCodeString, true)},
					},
				}
				f.fieldSource = mustLayoutSourceQOV(t, "fields_left", changedType)
				f.windows[0].Source = f.fieldSource
			},
		},
		{
			name: "removed source window",
			mutate: func(f *layoutFixture) {
				f.windows = f.windows[:3]
			},
		},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			fixture := newLayoutFixture(t, "_left")
			fixture.tiles = cloneLayoutTiles(fixture.tiles)
			fixture.windows = cloneLayoutWindows(fixture.windows)
			mutation.mutate(fixture)
			changed := mustOrdinalLayout(t, fixture)
			if left.RawEqual(changed) || changed.RawEqual(left) {
				t.Fatal("non-alias layout mutation compared raw-equal")
			}
			if left.EqualUnderAliases(changed, EmptyAliasMap()) {
				t.Fatal("non-alias layout mutation compared alias-equal under identity map")
			}
		})
	}

	differentCarrierType := &RecordType{
		RecordName: "DifferentCarrier",
		Fields: []Field{
			{Name: "A", Ordinal: 0, FieldType: NotNullLong},
			{Name: "B", Ordinal: 1, FieldType: NotNullLong},
		},
	}
	differentCarrier, err := NewOrdinalLayout(
		mustLayoutCurrentQOV(t, differentCarrierType),
		[]OrdinalTileSpec{{Start: 0, Width: 2, Kind: OrdinalTileFlat}},
		nil,
	)
	if err != nil {
		t.Fatalf("different carrier layout: %v", err)
	}
	if left.RawEqual(differentCarrier) {
		t.Fatal("different exact carrier type compared raw-equal")
	}
	scalar, err := NewScalarOrdinalLayout(mustLayoutCurrentQOV(t, NotNullLong))
	if err != nil {
		t.Fatalf("scalar layout: %v", err)
	}
	if left.RawEqual(scalar) {
		t.Fatal("record and scalar carrier kinds compared raw-equal")
	}
}

type embeddedLayoutQOV struct {
	QuantifiedObjectValue
}

func (*embeddedLayoutQOV) Correlation() CorrelationIdentifier {
	panic("foreign QOV method invoked")
}

func (*embeddedLayoutQOV) FlowedType() Type {
	panic("foreign QOV method invoked")
}

type embeddedOrdinalLayout struct {
	OrdinalLayout
}

func (*embeddedOrdinalLayout) Carrier() QuantifiedObjectValue {
	panic("foreign layout method invoked")
}

func (*embeddedOrdinalLayout) CarrierKind() OrdinalCarrierKind {
	panic("foreign layout method invoked")
}

type embeddedLayoutAliasMap struct {
	AliasMap
}

func (*embeddedLayoutAliasMap) Target(CorrelationIdentifier) (CorrelationIdentifier, bool) {
	panic("foreign AliasMap method invoked")
}

func (*embeddedLayoutAliasMap) Source(CorrelationIdentifier) (CorrelationIdentifier, bool) {
	panic("foreign AliasMap method invoked")
}

func TestOrdinalLayoutPurposeAPIsExactRecognizeBeforeMethodUse(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "")
	layout := mustOrdinalLayout(t, fixture)

	foreignQOV := &embeddedLayoutQOV{QuantifiedObjectValue: fixture.scalarSource}
	foreignCarrier, err := NewOrdinalLayout(foreignQOV, fixture.tiles, fixture.windows)
	requireLayoutError(t, foreignCarrier, err, CorrelationForeignValue)
	foreignWindows := cloneLayoutWindows(fixture.windows)
	foreignWindows[2].Source = foreignQOV
	foreignSourceLayout, err := NewOrdinalLayout(fixture.carrier, fixture.tiles, foreignWindows)
	requireLayoutError(t, foreignSourceLayout, err, CorrelationForeignValue)

	var nilQOV *quantifiedObjectValue
	typedNilCarrier, err := NewOrdinalLayout(nilQOV, fixture.tiles, fixture.windows)
	requireLayoutError(t, typedNilCarrier, err, CorrelationForeignValue)

	foreignLayout := &embeddedOrdinalLayout{OrdinalLayout: layout}
	provided, err := LayoutProvides(foreignLayout, fixture.scalarSource)
	requireLayoutPurposeError(t, provided, err, LayoutForeignValue)
	var nilLayout *ordinalLayout
	provided, err = LayoutProvides(nilLayout, fixture.scalarSource)
	requireLayoutPurposeError(t, provided, err, LayoutForeignValue)
	provided, err = LayoutProvides(layout, foreignQOV)
	requireLayoutPurposeError(t, provided, err, CorrelationForeignValue)
	provided, err = LayoutProvides(layout, nilQOV)
	requireLayoutPurposeError(t, provided, err, CorrelationForeignValue)
	provided, err = LayoutProvides(layout, mustLayoutCurrentQOV(t, NotNullLong))
	requireLayoutPurposeError(t, provided, err, CorrelationKindMismatch)

	missing := mustLayoutSourceQOV(t, "missing", NotNullLong)
	provided, err = LayoutProvides(layout, missing)
	requireLayoutPurposeError(t, provided, err, LayoutSourceNotProvided)
	conflicting := mustLayoutSourceQOV(t, "scalar", NullableLong)
	provided, err = LayoutProvides(layout, conflicting)
	requireLayoutPurposeError(t, provided, err, CorrelationTypeConflict)
	independent := mustLayoutSourceQOV(t, "scalar", NotNullLong)
	provided, err = LayoutProvides(layout, independent)
	if err != nil || !provided {
		t.Fatalf("independent exact source was not provided: (%t, %v)", provided, err)
	}

	if layout.RawEqual(foreignLayout) || layout.RawEqual(nilLayout) {
		t.Fatal("RawEqual admitted a foreign or typed-nil layout")
	}
	foreignAliases := &embeddedLayoutAliasMap{AliasMap: EmptyAliasMap()}
	if layout.EqualUnderAliases(layout, foreignAliases) {
		t.Fatal("EqualUnderAliases admitted a foreign alias map")
	}
}

func FuzzOrdinalLayoutEqualImpliesEqualHash(f *testing.F) {
	f.Add(uint8(0), uint8(0))
	f.Add(uint8(1), uint8(1))
	f.Add(uint8(2), uint8(3))
	f.Fuzz(func(t *testing.T, leftBits, rightBits uint8) {
		build := func(bits uint8, suffix string) OrdinalLayout {
			fixture := newLayoutFixture(t, suffix)
			if bits&1 != 0 {
				fixture.windows[2].ObjectPath = []int{0}
			}
			if bits&2 != 0 {
				fixture.tiles = []OrdinalTileSpec{{Start: 0, Width: 4, Kind: OrdinalTileFlat}}
			}
			if bits&4 != 0 {
				fixture.windows[3].NullSupplying = false
			}
			if bits&8 != 0 {
				fixture.windows[0], fixture.windows[3] = fixture.windows[3], fixture.windows[0]
				fixture.tiles[0], fixture.tiles[len(fixture.tiles)-1] = fixture.tiles[len(fixture.tiles)-1], fixture.tiles[0]
			}
			return mustOrdinalLayout(t, fixture)
		}

		left := build(leftBits, "")
		right := build(rightBits, "")
		if left.RawEqual(right) && left.AliasFreeHash() != right.AliasFreeHash() {
			t.Fatal("raw-equal fuzz layouts have different hashes")
		}

		alphaLeftFixture := newLayoutFixture(t, "_alpha_left")
		alphaRightFixture := newLayoutFixture(t, "_alpha_right")
		if leftBits&1 != 0 {
			alphaLeftFixture.windows[2].ObjectPath = []int{0}
			alphaRightFixture.windows[2].ObjectPath = []int{0}
		}
		alphaLeft := mustOrdinalLayout(t, alphaLeftFixture)
		alphaRight := mustOrdinalLayout(t, alphaRightFixture)
		pairs := []AliasPair{{Source: CurrentCorrelation(), Target: CurrentCorrelation()}}
		for i := range alphaLeftFixture.windows {
			pairs = append(pairs, AliasPair{
				Source: alphaLeftFixture.windows[i].Source.Correlation(),
				Target: alphaRightFixture.windows[i].Source.Correlation(),
			})
		}
		aliases, err := NewAliasMap(pairs)
		if err != nil {
			t.Fatalf("NewAliasMap: %v", err)
		}
		if !alphaLeft.EqualUnderAliases(alphaRight, aliases) {
			t.Fatal("alias-renamed fuzz layouts are not equal")
		}
		if alphaLeft.AliasFreeHash() != alphaRight.AliasFreeHash() {
			t.Fatal("alias-equal fuzz layouts have different hashes")
		}
	})
}
