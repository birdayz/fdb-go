package values_test

import (
	"embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

//go:embed values.go field_value.go
var rfc232FieldValueAPISources embed.FS

type rfc232FieldBinder map[values.CorrelationIdentifier]any

func (b rfc232FieldBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	value, ok := b[id]
	return value, ok
}

type rfc232OrdinalRow []any

func (r rfc232OrdinalRow) Get(ordinal int) (any, bool) {
	if ordinal < 0 || ordinal >= len(r) {
		return nil, false
	}
	return r[ordinal], true
}

type hostileFieldChild struct{}

func (*hostileFieldChild) Children() []values.Value { panic("hostile Children invoked") }
func (*hostileFieldChild) Type() values.Type        { panic("hostile Type invoked") }
func (*hostileFieldChild) Name() string             { panic("hostile Name invoked") }
func (*hostileFieldChild) Evaluate(any) (any, error) {
	panic("hostile Evaluate invoked")
}

type embeddedFieldValueView struct {
	values.FieldValue
}

func TestRFC232FieldValuePublicSurfaceStaysSealed(t *testing.T) {
	t.Parallel()

	bannedFunctions := map[string]struct{}{
		"NewFieldPathOfSingle": {}, "NewFieldPathOfSingleInDomain": {},
		"NewFieldValue": {}, "NewFlatFieldValue": {},
		"NewFieldValueWithResolvedOrdinal": {}, "NewFieldValueWithResolvedOrdinalInDomain": {},
		"NewFieldValueWithPinnedOrdinalInDomain":             {},
		"NewCorrelatedFieldValueWithResolvedOrdinal":         {},
		"NewCorrelatedFieldValueWithResolvedOrdinalInDomain": {},
		"NewOrdinalFieldValue":                               {}, "NewFieldValueOfOrdinal": {},
		"NewFusedFieldValueOfNestedOrdinal": {}, "FuseNestedSuffix": {},
	}
	foundFieldValueInterface := false
	for _, sourceName := range []string{"values.go", "field_value.go"} {
		source, err := rfc232FieldValueAPISources.ReadFile(sourceName)
		if err != nil {
			t.Fatalf("read embedded %s: %v", sourceName, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), sourceName, source, 0)
		if err != nil {
			t.Fatalf("parse embedded %s: %v", sourceName, err)
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if _, banned := bannedFunctions[declaration.Name.Name]; banned {
					t.Errorf("legacy partial-state constructor %s is exported", declaration.Name.Name)
				}
			case *ast.GenDecl:
				for _, specification := range declaration.Specs {
					typeSpec, ok := specification.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch typeSpec.Name.Name {
					case "FieldValue":
						_, foundFieldValueInterface = typeSpec.Type.(*ast.InterfaceType)
					case "FieldPath", "ResolvedAccessor":
						t.Errorf("mutable concrete type %s remains exported", typeSpec.Name.Name)
					}
				}
			}
		}
	}
	if !foundFieldValueInterface {
		t.Fatal("FieldValue is not an exported sealed read interface")
	}
}

func mustFieldRequest(t testing.TB, request values.FieldRequest, err error) values.FieldRequest {
	t.Helper()
	if err != nil {
		t.Fatalf("construct FieldRequest: %v", err)
	}
	return request
}

func byName(t testing.TB, name string) values.FieldRequest {
	t.Helper()
	request, err := values.FieldByName(name)
	return mustFieldRequest(t, request, err)
}

func byOrdinal(t testing.TB, ordinal int) values.FieldRequest {
	t.Helper()
	request, err := values.FieldByOrdinal(ordinal)
	return mustFieldRequest(t, request, err)
}

func byNameAndOrdinal(t testing.TB, name string, ordinal int) values.FieldRequest {
	t.Helper()
	request, err := values.FieldByNameAndOrdinal(name, ordinal)
	return mustFieldRequest(t, request, err)
}

func mustResolvedField(
	t testing.TB,
	child values.Value,
	requests ...values.FieldRequest,
) values.FieldValue {
	t.Helper()
	resolved, err := values.ResolveFieldAccess(child, requests)
	if err != nil {
		t.Fatalf("ResolveFieldAccess: %v", err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("ResolveFieldAccess returned %T, want admitted FieldValue", resolved)
	}
	return field
}

func requireFieldErrorCode(
	t testing.TB,
	value values.Value,
	err error,
	want values.ResolutionErrorCode,
) {
	t.Helper()
	if value != nil {
		t.Fatalf("failed resolution returned partial Value %T", value)
	}
	var coded codedResolutionError
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("error = %v, want code %d", err, want)
	}
}

func exactFieldFixture(nullableRoot, nullableNested, nullableLeaf bool) *values.RecordType {
	nested := &values.RecordType{
		RecordName: "Nested",
		Nullable:   nullableNested,
		Fields: []values.Field{{
			Name:      "LEAF",
			Ordinal:   0,
			FieldType: values.NewPrimitiveType(values.TypeCodeLong, nullableLeaf),
		}},
	}
	return &values.RecordType{
		RecordName: "Root",
		Nullable:   nullableRoot,
		Fields: []values.Field{{
			Name:      "NESTED",
			Ordinal:   0,
			FieldType: nested,
		}},
	}
}

func TestRFC232FieldValueConstructionRejectsEveryPartialState(t *testing.T) {
	t.Parallel()

	validRoot := &values.RecordType{Fields: []values.Field{{
		Name: "A", Ordinal: 0, FieldType: values.NotNullLong,
	}}}
	validQOV := mustRFC232QOV(t, values.NamedCorrelationIdentifier("valid"), validRoot)
	scalarQOV := mustRFC232QOV(t, values.NamedCorrelationIdentifier("scalar"), values.NotNullLong)
	duplicateQOV := mustRFC232QOV(t, values.NamedCorrelationIdentifier("duplicate"), &values.RecordType{
		Fields: []values.Field{
			{Name: "D", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "D", Ordinal: 1, FieldType: values.NotNullString},
		},
	})

	negative := &struct {
		value values.FieldRequest
		err   error
	}{}
	negative.value, negative.err = values.FieldByOrdinal(-1)
	if negative.value != nil {
		t.Fatalf("negative ordinal returned a request %T", negative.value)
	}
	var negativeCode codedResolutionError
	if !errors.As(negative.err, &negativeCode) || negativeCode.Code() != values.FieldNegativeOrdinal {
		t.Fatalf("FieldByOrdinal(-1) error = %v, want FieldNegativeOrdinal", negative.err)
	}

	cases := []struct {
		name     string
		child    values.Value
		requests []values.FieldRequest
		want     values.ResolutionErrorCode
	}{
		{"nil child", nil, []values.FieldRequest{byOrdinal(t, 0)}, values.FieldNilChild},
		{"empty path", validQOV, nil, values.FieldEmptyPath},
		{"hostile child", &hostileFieldChild{}, []values.FieldRequest{byOrdinal(t, 0)}, values.FieldUnsupportedChild},
		{"nonrecord child", scalarQOV, []values.FieldRequest{byOrdinal(t, 0)}, values.FieldNonRecord},
		{"out of range", validQOV, []values.FieldRequest{byOrdinal(t, 1)}, values.FieldOutOfRange},
		{"unknown name", validQOV, []values.FieldRequest{byName(t, "MISSING")}, values.FieldUnknownName},
		{"ambiguous name", duplicateQOV, []values.FieldRequest{byName(t, "D")}, values.FieldAmbiguousName},
		{"name ordinal mismatch", validQOV, []values.FieldRequest{byNameAndOrdinal(t, "A", 1)}, values.FieldNameOrdinalMismatch},
		{"scalar intermediate", validQOV, []values.FieldRequest{byOrdinal(t, 0), byOrdinal(t, 0)}, values.FieldNonRecord},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := values.ResolveFieldAccess(testCase.child, testCase.requests)
			requireFieldErrorCode(t, resolved, err, testCase.want)
		})
	}
}

func TestRFC232FieldValueRejectsAnyRecordErasedLayout(t *testing.T) {
	t.Parallel()

	qov := mustRFC232QOV(
		t,
		values.NamedCorrelationIdentifier("erased-record"),
		values.NewAnyRecordType(false),
	)
	requests := []struct {
		name    string
		request values.FieldRequest
	}{
		{name: "ordinal", request: byOrdinal(t, 0)},
		{name: "semantic name", request: byName(t, "ID")},
	}
	for _, testCase := range requests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := values.ResolveFieldAccess(qov, []values.FieldRequest{testCase.request})
			requireFieldErrorCode(t, resolved, err, values.TypeErased)

			optional, found, optionalErr := values.TryResolveFieldAccess(qov, []values.FieldRequest{testCase.request})
			if optional != nil || found {
				t.Fatalf("TryResolveFieldAccess(AnyRecord) = (%T, %t), want (nil, false)", optional, found)
			}
			var coded codedResolutionError
			if !errors.As(optionalErr, &coded) || coded.Code() != values.TypeErased {
				t.Fatalf("TryResolveFieldAccess(AnyRecord) error = %v, want TypeErased", optionalErr)
			}
		})
	}
}

func TestRFC232FieldValueRejectsDescentThroughNestedAnyRecord(t *testing.T) {
	t.Parallel()

	rootType := values.NewRecordType("Root", false, []values.Field{
		{Name: "ERASED", FieldType: values.NewAnyRecordType(false)},
	})
	qov := mustRFC232QOV(t, values.NamedCorrelationIdentifier("nested-erased-record"), rootType)
	resolved, err := values.ResolveFieldAccess(qov, []values.FieldRequest{
		byOrdinal(t, 0),
		byOrdinal(t, 0),
	})
	requireFieldErrorCode(t, resolved, err, values.TypeErased)

	leaf := mustResolvedField(t, qov, byOrdinal(t, 0))
	if !leaf.ResultType().Equals(values.NewAnyRecordType(false)) {
		t.Fatalf("final AnyRecord field result = %v, want exact AnyRecord(false)", leaf.ResultType())
	}
}

func TestRFC232FieldValueFullPathCanonicalizesAndFuses(t *testing.T) {
	t.Parallel()

	rootType := exactFieldFixture(false, false, false)
	root := mustRFC232QOV(t, values.NamedCorrelationIdentifier("q"), rootType)
	direct := mustResolvedField(t, root, byOrdinal(t, 0), byOrdinal(t, 0))
	inner := mustResolvedField(t, root, byOrdinal(t, 0))
	fused := mustResolvedField(t, inner, byOrdinal(t, 0))

	if _, nested := values.AsFieldValue(fused.ChildValue()); nested {
		t.Fatal("canonical FieldValue retained a FieldValue child")
	}
	if _, qov := values.AsQuantifiedObjectValue(fused.ChildValue()); !qov {
		t.Fatalf("canonical child = %T, want exact QOV", fused.ChildValue())
	}
	if got := fused.Path().Ordinals(); !reflect.DeepEqual(got, []int{0, 0}) {
		t.Fatalf("fused ordinals = %v, want [0 0]", got)
	}
	wantDomain := values.OrdinalDomainOfType(rootType)
	if got := fused.Path().RootDomain(); got != wantDomain || !got.IsKnown() {
		t.Fatalf("fused root domain = %v, want captured typed-root domain %v", got, wantDomain)
	}
	if fused.Path().IsFrontierPinned() {
		t.Fatal("newly resolved semantic FieldValue unexpectedly pinned its frontier")
	}
	if !values.ValuesStructurallyEqual(direct, fused) {
		t.Fatal("direct and chained construction did not canonicalize to one identity")
	}
	if values.SemanticHashCode(direct) != values.SemanticHashCode(fused) {
		t.Fatal("equal direct and fused FieldValues have different hashes")
	}
}

func TestRFC232FieldValueCollapsesExactExistsRecordConstructorSlot(t *testing.T) {
	t.Parallel()

	exists, err := values.NewExistsValue(
		values.NamedCorrelationIdentifier("exists-child"),
		values.NewRecordType("exists_row", false, []values.Field{{
			Name: "ID", Ordinal: 0, FieldType: values.NotNullLong,
		}}),
	)
	if err != nil {
		t.Fatalf("NewExistsValue: %v", err)
	}
	constructor := values.NewRecordConstructorValue(values.RecordConstructorField{
		Name: "PRESENT", Value: exists,
	})
	resolved, err := values.ResolveFieldOrdinals(constructor, []int{0})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals: %v", err)
	}
	if resolved != exists {
		t.Fatalf("record-constructor collapse returned %T, want the exact *ExistsValue slot", resolved)
	}
	if !resolved.Type().Equals(values.NotNullBoolean) {
		t.Fatalf("collapsed EXISTS type = %v, want %v", resolved.Type(), values.NotNullBoolean)
	}
}

func TestRFC232OrdinalSeedFieldIsExactPinnedAndRequiredForWindows(t *testing.T) {
	t.Parallel()

	leftType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "V", Ordinal: 1, FieldType: values.NullableString},
	}}
	rightType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	leftAlias := values.NamedCorrelationIdentifier("left-seed")
	rightAlias := values.NamedCorrelationIdentifier("right-seed")
	left := mustRFC232QOV(t, leftAlias, leftType)
	right := mustRFC232QOV(t, rightAlias, rightType)

	seedField := func(t testing.TB, child values.Value, ordinal int) values.Value {
		t.Helper()
		resolved, err := values.ResolveOrdinalSeedField(child, ordinal)
		if err != nil {
			t.Fatalf("ResolveOrdinalSeedField(%d): %v", ordinal, err)
		}
		return resolved
	}
	leftID := seedField(t, left, 0)
	leftV := seedField(t, left, 1)
	rightID := seedField(t, right, 0)
	leftView, ok := values.AsFieldValue(leftID)
	if !ok || leftView.Path() == nil {
		t.Fatalf("seed field = %T, want admitted exact FieldValue", leftID)
	}
	if !leftView.Path().IsFrontierPinned() {
		t.Fatal("ordinal seed factory did not pin its exact physical frontier")
	}
	wantDomain := values.OrdinalDomainOfType(leftType)
	if got := leftView.Path().RootDomain(); got != wantDomain || !got.IsKnown() {
		t.Fatalf("seed domain = %v, want exact captured root domain %v", got, wantDomain)
	}
	if got := leftView.Path().Ordinals(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("seed path = %v, want [0]", got)
	}
	nestedType := &values.RecordType{Fields: []values.Field{{
		Name: "N", Ordinal: 0, FieldType: &values.RecordType{Fields: []values.Field{{
			Name: "X", Ordinal: 0, FieldType: values.NotNullLong,
		}}},
	}}}
	nested := mustRFC232QOV(t, values.NamedCorrelationIdentifier("nested-seed"), nestedType)
	suffix, err := values.FieldByName("X")
	if err != nil {
		t.Fatalf("FieldByName: %v", err)
	}
	fusedSeed, err := values.ResolveOrdinalSeedAccess(nested, 0, []values.FieldRequest{suffix})
	if err != nil {
		t.Fatalf("ResolveOrdinalSeedAccess: %v", err)
	}
	fusedView, ok := values.AsFieldValue(fusedSeed)
	if !ok {
		t.Fatalf("fused seed = %T, want exact FieldValue", fusedSeed)
	}
	if !fusedView.Path().IsFrontierPinned() ||
		!reflect.DeepEqual(fusedView.Path().Ordinals(), []int{0, 0}) {
		t.Fatalf("fused seed path=%v, want pinned [0,0]", fusedView.Path())
	}

	seed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: leftID},
		values.RecordConstructorField{Name: "V", Value: leftV},
		values.RecordConstructorField{Name: "ID", Value: rightID},
	)
	windows, merged := values.OrdinalSeedLegWindows(seed)
	if windows == nil || merged == nil {
		t.Fatal("exact purpose-built two-leg seed was not recognized")
	}
	if got := windows[leftAlias]; got.Offset != 0 || got.Typ == nil || len(got.Typ.Fields) != 2 {
		t.Fatalf("left seed window = %+v, want flat [0,2)", got)
	}
	if got := windows[rightAlias]; got.Offset != 2 || got.Typ == nil || len(got.Typ.Fields) != 1 {
		t.Fatalf("right seed window = %+v, want flat [2,3)", got)
	}

	ordinary, err := values.ResolveFieldOrdinals(left, []int{0})
	if err != nil {
		t.Fatalf("ordinary semantic field: %v", err)
	}
	ordinaryView, ok := values.AsFieldValue(ordinary)
	if !ok || ordinaryView.Path().IsFrontierPinned() {
		t.Fatal("ordinary semantic resolution unexpectedly acquired a seed frontier")
	}
	mutant := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: ordinary},
		values.RecordConstructorField{Name: "V", Value: leftV},
		values.RecordConstructorField{Name: "ID", Value: rightID},
	)
	if got, typ := values.OrdinalSeedLegWindows(mutant); got != nil || typ != nil {
		t.Fatalf("un-pinned semantic-field mutant was accepted: windows=%v type=%v", got, typ)
	}

	for _, testCase := range []struct {
		name  string
		child values.Value
		ord   int
		code  values.ResolutionErrorCode
	}{
		{name: "nil child", child: nil, ord: 0, code: values.FieldUnsupportedChild},
		{name: "already accessed child", child: ordinary, ord: 0, code: values.FieldUnsupportedChild},
		{name: "hostile child", child: &hostileFieldChild{}, ord: 0, code: values.FieldUnsupportedChild},
		{name: "out of range", child: left, ord: 2, code: values.FieldOutOfRange},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := values.ResolveOrdinalSeedField(testCase.child, testCase.ord)
			requireFieldErrorCode(t, resolved, err, testCase.code)
		})
	}
}

func TestRFC232FieldValueIdentityIncludesRootAndIsAliasInvariant(t *testing.T) {
	t.Parallel()

	rootType := &values.RecordType{RecordName: "R", Fields: []values.Field{{
		Name: "A", Ordinal: 0, FieldType: values.NotNullLong,
	}}}
	leftAlias := values.NamedCorrelationIdentifier("left")
	rightAlias := values.NamedCorrelationIdentifier("right")
	left := mustResolvedField(t, mustRFC232QOV(t, leftAlias, rootType), byOrdinal(t, 0))
	right := mustResolvedField(t, mustRFC232QOV(t, rightAlias, rootType), byOrdinal(t, 0))
	aliases := mustRFC232AliasMap(t, []values.AliasPair{{Source: leftAlias, Target: rightAlias}})

	if values.ValuesStructurallyEqual(left, right) {
		t.Fatal("different raw child correlations compared equal")
	}
	if !values.SemanticEqualsUnderAliasMap(left, right, aliases) {
		t.Fatal("alias-mapped equal FieldValues did not compare equal")
	}
	if values.SemanticHashCode(left) != values.SemanticHashCode(right) {
		t.Fatal("alias-mapped equal FieldValues have different hashes")
	}

	// The root must differ in SHAPE, not merely in RecordName: a record's name
	// is provenance and Java's Type.Record.equals ignores it, so a rename is not
	// a different exact root and would make both assertions below vacuous.
	differentRoot := mustResolvedField(t, mustRFC232QOV(t, leftAlias, &values.RecordType{
		RecordName: "Other",
		Fields:     []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NullableLong}},
	}), byOrdinal(t, 0))
	if values.EqualsWithoutChildren(left, differentRoot) {
		t.Fatal("same ordinal path over different exact roots compared equal")
	}
	if values.SemanticHashCode(left) == values.SemanticHashCode(differentRoot) {
		t.Fatal("FieldValue hash omitted the exact root type")
	}
}

func TestRFC232FieldValueDefensiveViewsAndExactNullability(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name                                       string
		rootNullable, nestedNullable, leafNullable bool
		wantNullable                               bool
	}{
		{"all nonnull", false, false, false, false},
		{"nullable root", true, false, false, true},
		{"nullable intermediate", false, true, false, true},
		{"nullable leaf", false, false, true, true},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rootType := exactFieldFixture(testCase.rootNullable, testCase.nestedNullable, testCase.leafNullable)
			field := mustResolvedField(t,
				mustRFC232QOV(t, values.NamedCorrelationIdentifier(testCase.name), rootType),
				byOrdinal(t, 0), byOrdinal(t, 0),
			)
			if got := field.ResultType().IsNullable(); got != testCase.wantNullable {
				t.Fatalf("ResultType().IsNullable() = %t, want %t", got, testCase.wantNullable)
			}
			if !field.Type().Equals(field.ResultType()) {
				t.Fatalf("Type() = %v, ResultType() = %v", field.Type(), field.ResultType())
			}

			ordinals := field.Path().Ordinals()
			ordinals[0] = 99
			if got := field.Path().Ordinals(); !reflect.DeepEqual(got, []int{0, 0}) {
				t.Fatalf("mutating Ordinals() changed path: %v", got)
			}
			accessor, ok := field.Path().Accessor(0)
			if !ok {
				t.Fatal("Accessor(0) absent")
			}
			// These two used to MUTATE the graphs the accessors returned, to prove
			// the exact snapshot behind them was unmoved. That was safe only
			// because FieldType() and ResultType() still thaw a private graph —
			// an accident of which accessors RFC-234 converted, not a property the
			// test established. The document's own follow-on (sharing
			// physicalFlowedRecordType's path) is what would have armed it, and its
			// first firing would then have read as a new finding rather than as the
			// untested branch it had been all along.
			//
			// What the test is actually about — the snapshot does not drift across
			// reads — is asserted directly, without writing to a graph this
			// function does not own.
			if got := accessor.FieldType().(*values.RecordType).Fields[0].Name; got != "LEAF" {
				t.Fatalf("accessor FieldType() = %q, want LEAF", got)
			}
			again, _ := field.Path().Accessor(0)
			if got := again.FieldType().(*values.RecordType).Fields[0].Name; got != "LEAF" {
				t.Fatalf("a second accessor read disagrees with the first: %q", got)
			}
			wantResult := &values.PrimitiveType{
				TypeCode: values.TypeCodeLong,
				Nullable: testCase.wantNullable,
			}
			firstResult, secondResult := field.ResultType(), field.ResultType()
			if !firstResult.Equals(wantResult) || !secondResult.Equals(wantResult) {
				t.Fatalf("ResultType() = %v then %v, want %v both times",
					firstResult, secondResult, wantResult)
			}
		})
	}
}

func TestRFC232FieldValueSnapshotsCallerTypeGraph(t *testing.T) {
	t.Parallel()

	// Built as a literal, not via the constructor: this test MUTATES it below to
	// prove the snapshot is isolated, and a graph from a call carries the callee's
	// provenance rather than this function's.
	leaf := &values.PrimitiveType{TypeCode: values.TypeCodeLong}
	supplied := &values.RecordType{RecordName: "Original", Fields: []values.Field{{
		Name: "A", Ordinal: 0, FieldType: leaf,
	}}}
	field := mustResolvedField(
		t,
		mustRFC232QOV(t, values.NamedCorrelationIdentifier("snapshot"), supplied),
		byOrdinal(t, 0),
	)
	beforeHash := values.SemanticHashCode(field)
	beforeDomain := field.Path().RootDomain()

	supplied.RecordName = "MUTATED"
	supplied.Fields[0].Name = "B"
	leaf.TypeCode = values.TypeCodeString
	leaf.Nullable = true

	if got := field.DisplayName(); got != "A" {
		t.Fatalf("caller type mutation changed display metadata: %q", got)
	}
	if got := field.ResultType(); !got.Equals(values.NotNullLong) {
		t.Fatalf("caller type mutation changed exact result type: %v", got)
	}
	if got := values.SemanticHashCode(field); got != beforeHash {
		t.Fatalf("caller type mutation changed identity hash: %d -> %d", beforeHash, got)
	}
	if got := field.Path().RootDomain(); got != beforeDomain || !got.IsKnown() {
		t.Fatalf("caller type mutation changed captured root domain: %v -> %v", beforeDomain, got)
	}
}

func TestRFC232FieldValueRecognizerRejectsEmbeddedViews(t *testing.T) {
	t.Parallel()

	root := mustRFC232QOV(t, values.NamedCorrelationIdentifier("q"), &values.RecordType{
		Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NotNullLong}},
	})
	field := mustResolvedField(t, root, byOrdinal(t, 0))
	if recognized, ok := values.AsFieldValue(field); !ok || recognized == nil {
		t.Fatal("values-owned FieldValue was not recognized")
	}
	foreign := &embeddedFieldValueView{FieldValue: field}
	if recognized, ok := values.AsFieldValue(foreign); ok || recognized != nil {
		t.Fatalf("embedded foreign FieldValue was admitted: (%T, %t)", recognized, ok)
	}
	var nilValue values.Value
	if recognized, ok := values.AsFieldValue(nilValue); ok || recognized != nil {
		t.Fatalf("nil Value was admitted: (%T, %t)", recognized, ok)
	}
}

func TestRFC232RecordConstructorCollapseIsOrdinalAndAtomic(t *testing.T) {
	t.Parallel()

	left := &values.ConstantValue{Value: int64(11), Typ: values.NotNullLong}
	right := &values.ConstantValue{Value: "right", Typ: values.NotNullString}
	record := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "D", Value: left},
		values.RecordConstructorField{Name: "D", Value: right},
	)
	resolved, err := values.ResolveFieldAccess(record, []values.FieldRequest{byOrdinal(t, 1)})
	if err != nil {
		t.Fatalf("ResolveFieldAccess(record, 1): %v", err)
	}
	if resolved != right {
		t.Fatalf("record-constructor collapse returned %T/%v, want selected child pointer", resolved, resolved)
	}
	ambiguous, err := values.ResolveFieldAccess(record, []values.FieldRequest{byName(t, "D")})
	requireFieldErrorCode(t, ambiguous, err, values.FieldAmbiguousName)

	nestedLeaf := &values.ConstantValue{Value: int64(37), Typ: values.NotNullLong}
	nested := values.NewRawRecordConstructorValue(values.RecordConstructorField{Name: "X", Value: nestedLeaf})
	outer := values.NewRawRecordConstructorValue(values.RecordConstructorField{Name: "N", Value: nested})
	collapsed, err := values.ResolveFieldAccess(outer, []values.FieldRequest{byOrdinal(t, 0), byOrdinal(t, 0)})
	if err != nil {
		t.Fatalf("nested record-constructor collapse: %v", err)
	}
	if collapsed != nestedLeaf {
		t.Fatalf("nested collapse returned %T, want leaf child pointer", collapsed)
	}
}

func TestRFC232FieldValueEvaluatesFullOrdinalPath(t *testing.T) {
	t.Parallel()

	root := mustRFC232QOV(t, values.NamedCorrelationIdentifier("q"), exactFieldFixture(false, false, false))
	field := mustResolvedField(t, root, byOrdinal(t, 0), byOrdinal(t, 0))
	row := rfc232OrdinalRow{rfc232OrdinalRow{int64(91)}}
	got, err := field.Evaluate(row)
	if err != nil || got != int64(91) {
		t.Fatalf("Evaluate(full path) = (%v, %v), want (91, nil)", got, err)
	}

	got, err = field.Evaluate(rfc232OrdinalRow{nil})
	if err != nil || got != nil {
		t.Fatalf("Evaluate(nil intermediate) = (%v, %v), want SQL NULL", got, err)
	}

	got, err = field.Evaluate(rfc232OrdinalRow{})
	if got != nil {
		t.Fatalf("short row returned partial value %v", got)
	}
	var coded codedResolutionError
	if !errors.As(err, &coded) || coded.Code() != values.LayoutRuntimeShape {
		t.Fatalf("short row error = %v, want LayoutRuntimeShape", err)
	}

	got, err = field.Evaluate(rfc232OrdinalRow{int64(4)})
	if err != nil || got != nil {
		t.Fatalf("nonnil non-record intermediate = (%v, %v), want Java NULL", got, err)
	}
}

func TestRFC232FieldValueProtoEvaluationUsesDeclarationOrdinalsAndCanonicalConversion(t *testing.T) {
	t.Parallel()

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("rfc232_field.proto"),
		Package: proto.String("rfc232"),
		Syntax:  proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("E"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("ZERO"), Number: proto.Int32(0)},
				{Name: proto.String("ONE"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("RuntimeRow"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("physically_second"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
				{Name: proto.String("physically_first"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), DefaultValue: proto.String("41")},
				{Name: proto.String("tags"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("enum_value"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".rfc232.E")},
			},
		}},
	}
	descriptorFile, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	descriptor := descriptorFile.Messages().ByName("RuntimeRow")
	message := dynamicpb.NewMessage(descriptor)
	fields := descriptor.Fields()
	message.Set(fields.Get(0), protoreflect.ValueOfInt64(73))
	message.Mutable(fields.Get(2)).List().Append(protoreflect.ValueOfString("a"))
	message.Mutable(fields.Get(2)).List().Append(protoreflect.ValueOfString("b"))
	message.Set(fields.Get(3), protoreflect.ValueOfEnum(1))

	logicalType := &values.RecordType{RecordName: "Logical", Fields: []values.Field{
		{Name: "MISLEADING_FIRST", Ordinal: 0, FieldType: values.FieldTypeForProtoField(fields.Get(0))},
		{Name: "MISLEADING_SECOND", Ordinal: 1, FieldType: values.FieldTypeForProtoField(fields.Get(1))},
		{Name: "MISLEADING_TAGS", Ordinal: 2, FieldType: values.FieldTypeForProtoField(fields.Get(2))},
		{Name: "MISLEADING_ENUM", Ordinal: 3, FieldType: values.FieldTypeForProtoField(fields.Get(3))},
	}}
	alias := values.NamedCorrelationIdentifier("proto")
	qov := mustRFC232QOV(t, alias, logicalType)
	binder := rfc232FieldBinder{alias: message}

	cases := []struct {
		ordinal int
		want    any
	}{
		{0, int64(73)},
		{1, int64(41)},
		{2, []any{"a", "b"}},
		{3, int64(1)},
	}
	for _, testCase := range cases {
		field := mustResolvedField(t, qov, byOrdinal(t, testCase.ordinal))
		got, evalErr := field.Evaluate(binder)
		if evalErr != nil {
			t.Fatalf("Evaluate ordinal %d: %v", testCase.ordinal, evalErr)
		}
		if !reflect.DeepEqual(got, testCase.want) {
			t.Fatalf("Evaluate ordinal %d = %#v (%T), want %#v (%T)", testCase.ordinal, got, got, testCase.want, testCase.want)
		}
	}
}

func TestRFC232FieldValueProtoDescriptorReorderIsRejectedBeforeRead(t *testing.T) {
	t.Parallel()

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("rfc232_reorder.proto"),
		Package: proto.String("rfc232"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Reordered"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("runtime_string"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("runtime_long"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			},
		}},
	}
	descriptorFile, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	descriptor := descriptorFile.Messages().ByName("Reordered")
	message := dynamicpb.NewMessage(descriptor)
	message.Set(descriptor.Fields().Get(0), protoreflect.ValueOfString("wrong-slot"))
	message.Set(descriptor.Fields().Get(1), protoreflect.ValueOfInt64(9))

	logicalType := &values.RecordType{Fields: []values.Field{
		{Name: "LOGICAL_LONG", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "LOGICAL_STRING", Ordinal: 1, FieldType: values.NotNullString},
	}}
	alias := values.NamedCorrelationIdentifier("reordered")
	field := mustResolvedField(t, mustRFC232QOV(t, alias, logicalType), byOrdinal(t, 0))
	got, err := field.Evaluate(rfc232FieldBinder{alias: message})
	if got != nil {
		t.Fatalf("descriptor mismatch returned wrong-slot value %#v", got)
	}
	var coded codedResolutionError
	if !errors.As(err, &coded) || coded.Code() != values.LayoutRuntimeShape {
		t.Fatalf("descriptor mismatch error = %v, want LayoutRuntimeShape", err)
	}
}

func TestRFC232NestedAnyRecordLeafAcceptsAnyProtoMessageShape(t *testing.T) {
	t.Parallel()

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("rfc232_any_record_leaf.proto"),
		Package: proto.String("rfc232"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Nested"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("x"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
				},
			},
			{
				Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("nested"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".rfc232.Nested")},
				},
			},
		},
	}
	descriptorFile, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	nestedDescriptor := descriptorFile.Messages().ByName("Nested")
	nested := dynamicpb.NewMessage(nestedDescriptor)
	nested.Set(nestedDescriptor.Fields().Get(0), protoreflect.ValueOfInt64(17))
	outerDescriptor := descriptorFile.Messages().ByName("Outer")
	outer := dynamicpb.NewMessage(outerDescriptor)
	outer.Set(outerDescriptor.Fields().Get(0), protoreflect.ValueOfMessage(nested))

	rootType := values.NewRecordType("Root", false, []values.Field{
		{Name: "NESTED", FieldType: values.NewAnyRecordType(false)},
	})
	alias := values.NamedCorrelationIdentifier("any-record-proto")
	field := mustResolvedField(t, mustRFC232QOV(t, alias, rootType), byOrdinal(t, 0))
	got, evalErr := field.Evaluate(rfc232FieldBinder{alias: outer})
	if evalErr != nil {
		t.Fatalf("Evaluate(AnyRecord leaf): %v", evalErr)
	}
	message, ok := got.(proto.Message)
	if !ok || message.ProtoReflect().Descriptor().FullName() != nestedDescriptor.FullName() {
		t.Fatalf("Evaluate(AnyRecord leaf) = %T, want the nested proto message", got)
	}
}

func TestRFC232TryResolveFieldAccessDeclinesOnlyDocumentedMisses(t *testing.T) {
	t.Parallel()

	root := mustRFC232QOV(t, values.NamedCorrelationIdentifier("q"), &values.RecordType{
		Fields: []values.Field{{Name: "A", Ordinal: 0, FieldType: values.NotNullLong}},
	})
	resolved, applicable, err := values.TryResolveFieldAccess(root, []values.FieldRequest{byName(t, "MISSING")})
	if err != nil || applicable || resolved != nil {
		t.Fatalf("unknown optional field = (%T, %t, %v), want clean decline", resolved, applicable, err)
	}
	resolved, applicable, err = values.TryResolveFieldAccess(nil, []values.FieldRequest{byOrdinal(t, 0)})
	if resolved != nil || applicable {
		t.Fatalf("nil child returned (%T, %t), want no partial result", resolved, applicable)
	}
	var coded codedResolutionError
	if !errors.As(err, &coded) || coded.Code() != values.FieldNilChild {
		t.Fatalf("nil child error = %v, want fatal FieldNilChild", err)
	}
}

// A NAME+ORDINAL REQUEST OVER A ROW WITH TWO SAME-NAMED FIELDS RESOLVES.
//
// The two request kinds ask different questions and only one of them can be
// ambiguous. FieldByName has the name as its sole authority, so a duplicate
// leaves it with nothing to choose by — AMBIGUOUS is the right answer and stays
// the right answer. FieldByNameAndOrdinal states both, so the ORDINAL selects
// and the name VERIFIES; a duplicate is exactly the situation that kind exists
// for, and "the D at slot 1" names one field.
//
// Answering AMBIGUOUS there is not a cosmetic error-code question. It is what
// made a streaming aggregate unable to state its own provided ordering: the
// canonical group-key output name is the same for every same-leaf key, so
// `GROUP BY ot.k, it.k` emits [K, K, COUNT(*)], HintOrdering's name+ordinal
// request came back ambiguous, the ordering went UNKNOWN, and the ORDER BY the
// aggregate already satisfies stopped matching — a second InMemorySort above
// the aggregate, with the rows still correct.
func TestRFC232FieldByNameAndOrdinalSelectsByOrdinalAcrossDuplicateNames(t *testing.T) {
	t.Parallel()

	duplicate := mustRFC232QOV(t, values.NamedCorrelationIdentifier("dup"), &values.RecordType{
		Fields: []values.Field{
			{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "K", Ordinal: 1, FieldType: values.NotNullString},
			{Name: "C", Ordinal: 2, FieldType: values.NotNullLong},
		},
	})

	// The CONTROL, first: name alone over the same row is still ambiguous. Both
	// arms below are meaningless if this ever stops being true, because the
	// whole claim is that the two kinds diverge here.
	ambiguous, err := values.ResolveFieldAccess(duplicate, []values.FieldRequest{byName(t, "K")})
	requireFieldErrorCode(t, ambiguous, err, values.FieldAmbiguousName)

	for _, tc := range []struct {
		ordinal  int
		wantType values.Type
	}{
		{0, values.NotNullLong},
		{1, values.NotNullString},
	} {
		field := mustResolvedField(t, duplicate, byNameAndOrdinal(t, "K", tc.ordinal))
		got := field.Path().Ordinals()
		if len(got) != 1 || got[0] != tc.ordinal {
			t.Fatalf("byNameAndOrdinal(K, %d) resolved ordinals %v, want [%d] — the ORDINAL "+
				"is the selector on this request kind", tc.ordinal, got, tc.ordinal)
		}
		if field.ResultType().Code() != tc.wantType.Code() {
			t.Fatalf("byNameAndOrdinal(K, %d) resolved type %v, want %v — the ordinal picked "+
				"the wrong one of the two same-named slots",
				tc.ordinal, field.ResultType(), tc.wantType)
		}
	}

	// The name still VERIFIES: a slot the ordinal reaches whose name is
	// something else is a disagreement between the two stated authorities, and
	// it must not silently resolve on the ordinal alone.
	mismatch, err := values.ResolveFieldAccess(duplicate, []values.FieldRequest{byNameAndOrdinal(t, "K", 2)})
	requireFieldErrorCode(t, mismatch, err, values.FieldNameOrdinalMismatch)
}
