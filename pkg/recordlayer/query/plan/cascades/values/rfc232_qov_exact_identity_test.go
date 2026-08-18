package values_test

import (
	"bytes"
	"embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type embeddedQOVView struct {
	values.QuantifiedObjectValue
}

type codedResolutionError interface {
	Code() values.ResolutionErrorCode
}

// correlationSource embeds the declaration site of the correlation API so the
// assignability claim below is checked from source. A runtime equality test
// alone cannot distinguish a package variable from a function declaration.
//
//go:embed correlation.go
var correlationSource embed.FS

func mustRFC232QOV(
	t testing.TB,
	correlation values.CorrelationIdentifier,
	flowed values.Type,
) values.Value {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(correlation, flowed)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q, %v): %v", correlation, flowed, err)
	}
	return qov
}

func mustRFC232AliasMap(t testing.TB, pairs []values.AliasPair) values.AliasMap {
	t.Helper()
	aliases, err := values.NewAliasMap(pairs)
	if err != nil {
		t.Fatalf("NewAliasMap(%v): %v", pairs, err)
	}
	return aliases
}

func TestRFC232QOVExactTypeParticipatesInEqualityAndHash(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("q")
	notNull := mustRFC232QOV(t, alias, values.NotNullLong)
	notNullTwin := mustRFC232QOV(t, alias, values.NotNullLong)
	nullable := mustRFC232QOV(t, alias, values.NullableLong)
	emptyAliases := mustRFC232AliasMap(t, nil)

	// Positive control: independently snapshotted equal exact types remain one
	// identity. This prevents a blanket "all QOVs differ" implementation from
	// satisfying the negative assertions below.
	if !values.EqualsWithoutChildren(notNull, notNullTwin) {
		t.Fatal("same correlation and exact type are not raw-equal")
	}
	if !values.SemanticEqualsUnderAliasMap(notNull, notNullTwin, emptyAliases) {
		t.Fatal("same correlation and exact type are not semantically equal")
	}
	if values.SemanticHashCode(notNull) != values.SemanticHashCode(notNullTwin) {
		t.Fatal("equal QOVs have different semantic hashes")
	}

	if values.EqualsWithoutChildren(notNull, nullable) ||
		values.EqualsWithoutChildren(nullable, notNull) {
		t.Fatal("same correlation with different exact nullability compared raw-equal")
	}
	if values.SemanticEqualsUnderAliasMap(notNull, nullable, emptyAliases) ||
		values.SemanticEqualsUnderAliasMap(nullable, notNull, emptyAliases) {
		t.Fatal("same correlation with different exact nullability compared semantically equal")
	}
	if values.SemanticHashCode(notNull) == values.SemanticHashCode(nullable) {
		t.Fatal("QOV semantic hash omitted the exact type/nullability discriminator")
	}
}

func TestRFC232QOVCorrelationIsAliasMappedAndHashInvariant(t *testing.T) {
	t.Parallel()

	leftAlias := values.NamedCorrelationIdentifier("left")
	rightAlias := values.NamedCorrelationIdentifier("right")
	left := mustRFC232QOV(t, leftAlias, values.NotNullLong)
	right := mustRFC232QOV(t, rightAlias, values.NotNullLong)
	emptyAliases := mustRFC232AliasMap(t, nil)
	aliases := mustRFC232AliasMap(t, []values.AliasPair{{
		Source: leftAlias,
		Target: rightAlias,
	}})

	if values.EqualsWithoutChildren(left, right) {
		t.Fatal("different correlations compared raw-equal")
	}
	if values.SemanticEqualsUnderAliasMap(left, right, emptyAliases) {
		t.Fatal("different unmapped correlations compared semantically equal")
	}
	if !values.SemanticEqualsUnderAliasMap(left, right, aliases) {
		t.Fatal("alias-mapped correlations with the same exact type did not compare equal")
	}
	if values.SemanticHashCode(left) != values.SemanticHashCode(right) {
		t.Fatal("QOV semantic hash included correlation bytes and broke alpha-renaming")
	}
}

func TestRFC232QOVTypePreservesSuppliedNullability(t *testing.T) {
	t.Parallel()

	notNull := mustRFC232QOV(
		t,
		values.NamedCorrelationIdentifier("not_null"),
		values.NotNullLong,
	)
	if got := notNull.Type(); got.IsNullable() || !got.Equals(values.NotNullLong) {
		t.Fatalf("QOV.Type() = %v, want exact LONG NOT NULL without blanket widening", got)
	}

	nullable := mustRFC232QOV(
		t,
		values.NamedCorrelationIdentifier("nullable"),
		values.NullableLong,
	)
	if got := nullable.Type(); !got.IsNullable() || !got.Equals(values.NullableLong) {
		t.Fatalf("QOV.Type() = %v, want exact LONG NULL", got)
	}
}

// TestRFC232QOVSnapshotsAndSharesItsThawedType asserts two DIFFERENT things, and
// only one of them changed when Type() stopped copying.
//
// The snapshot half — a caller mutating the graph it supplied cannot reach the
// QOV — is unchanged and matters MORE under sharing, not less: it is what keeps
// a caller's later edit out of a graph the whole process now reads.
//
// The freshness half inverted. It used to require that mutating one Type()
// result leaves the next unaffected; the guard's direction is now the opposite,
// because the alarm moved. What is dangerous is no longer "the graph is shared"
// but "a Type was mutated at all", and that is watched by the immutability gate
// rather than here. So this asserts the SAME POINTER: a reintroduced defensive
// copy is a performance regression of 128.9M objects per planner sweep, and
// without this assertion nothing would notice it.
//
// The mutation that used to live here is deleted rather than moved. Under
// sharing it wrote through to an INTERNED handle, and interning keys on shape,
// so it corrupted two unrelated tests that happened to snapshot the same record
// — order-dependently, under t.Parallel(). That is what the gate exists to
// prevent, and a test may not be the one place it is allowed.
func TestRFC232QOVSnapshotsAndSharesItsThawedType(t *testing.T) {
	t.Parallel()

	// Built as a literal, not via the constructor: this test MUTATES it below.
	fieldType := &values.PrimitiveType{TypeCode: values.TypeCodeLong}
	supplied := &values.RecordType{
		RecordName: "R",
		Nullable:   false,
		Fields: []values.Field{{
			Name:      "ID",
			FieldType: fieldType,
			Ordinal:   0,
		}},
	}
	qov := mustRFC232QOV(t, values.NamedCorrelationIdentifier("q"), supplied)
	beforeHash := values.SemanticHashCode(qov)

	// Mutating the caller-owned graph after construction cannot mutate QOV
	// identity or the type it reports.
	supplied.RecordName = "MUTATED"
	supplied.Nullable = true
	supplied.Fields[0].Name = "CHANGED"
	fieldType.TypeCode = values.TypeCodeString
	fieldType.Nullable = true

	first, ok := qov.Type().(*values.RecordType)
	if !ok {
		t.Fatalf("QOV.Type() = %T, want *RecordType", qov.Type())
	}
	if first.RecordName != "R" || first.Nullable || len(first.Fields) != 1 ||
		first.Fields[0].Name != "ID" || !first.Fields[0].FieldType.Equals(values.NotNullLong) {
		t.Fatalf("caller mutation reached exact QOV snapshot: %#v", first)
	}

	// Type() returns the SHARED graph: the same pointer, every call. Asserted
	// because a reintroduced defensive copy passes every correctness test in this
	// file and costs 128.9M objects per planner sweep.
	second := qov.Type().(*values.RecordType)
	if first != second {
		t.Fatalf("QOV.Type() returned two different graphs (%p, %p); the defensive "+
			"copy is back. It was removed deliberately — see RFC-234 — and nothing "+
			"else in the suite observes its return", first, second)
	}
	// The identity is unmoved by any of the above. The hash is computed from the
	// canonical bytes, which CanonicalBytes still copies defensively precisely
	// because they ARE the identity rather than a derivation of it.
	if afterHash := values.SemanticHashCode(qov); afterHash != beforeHash {
		t.Fatalf("QOV hash changed after external type mutation: %d -> %d", beforeHash, afterHash)
	}
}

func TestRFC232AnyRecordIsExactImmutableAndDistinctFromEmptyRecord(t *testing.T) {
	t.Parallel()

	notNull := values.NewAnyRecordType(false)
	notNullTwin := values.NewAnyRecordType(false)
	nullable := values.NewAnyRecordType(true)
	emptyRecord := values.NewRecordType("", false, nil)

	if notNull.Code() != values.TypeCodeRecord || notNull.IsNullable() ||
		!values.IsRecord(notNull) || values.IsUnresolved(notNull) {
		t.Fatalf("AnyRecord(false) has the wrong Type contract: %v", notNull)
	}
	if !notNull.Equals(notNullTwin) || !notNullTwin.Equals(notNull) {
		t.Fatal("independently constructed AnyRecord(false) values are not equal")
	}
	if notNull.Equals(nullable) || nullable.Equals(notNull) {
		t.Fatal("AnyRecord equality omitted exact nullability")
	}
	if notNull.Equals(emptyRecord) || emptyRecord.Equals(notNull) {
		t.Fatal("erased AnyRecord compared equal to concrete RECORD<>")
	}
	if _, concrete := notNull.(*values.RecordType); concrete {
		t.Fatal("AnyRecord was exposed as a mutable concrete RecordType")
	}

	widened := values.WithNullability(notNull, true)
	if !widened.Equals(nullable) || !widened.IsNullable() {
		t.Fatalf("WithNullability(AnyRecord(false), true) = %v, want AnyRecord(true)", widened)
	}
	if notNull.IsNullable() {
		t.Fatal("WithNullability mutated the original immutable AnyRecord")
	}
	if maximum := values.MaximumType(notNull, nullable); maximum == nil || !maximum.Equals(nullable) {
		t.Fatalf("MaximumType(AnyRecord(false), AnyRecord(true)) = %v, want AnyRecord(true)", maximum)
	}
	if maximum := values.MaximumType(notNull, emptyRecord); maximum != nil {
		t.Fatalf("MaximumType(AnyRecord, RECORD<>) = %v, want no invented concrete shape", maximum)
	}

	snapshot := func(t testing.TB, typ values.Type) values.ExactTypeHandle {
		t.Helper()
		handle, err := values.SnapshotExactType(typ)
		if err != nil {
			t.Fatalf("SnapshotExactType(%v): %v", typ, err)
		}
		return handle
	}
	notNullHandle := snapshot(t, notNull)
	twinHandle := snapshot(t, notNullTwin)
	nullableHandle := snapshot(t, nullable)
	emptyHandle := snapshot(t, emptyRecord)
	if !bytes.Equal(notNullHandle.CanonicalBytes(), twinHandle.CanonicalBytes()) {
		t.Fatal("equal AnyRecord values produced different canonical identities")
	}
	if bytes.Equal(notNullHandle.CanonicalBytes(), nullableHandle.CanonicalBytes()) {
		t.Fatal("AnyRecord canonical identity omitted nullability")
	}
	if bytes.Equal(notNullHandle.CanonicalBytes(), emptyHandle.CanonicalBytes()) {
		t.Fatal("AnyRecord canonical identity collapsed into concrete RECORD<>")
	}
	if thawed := notNullHandle.Type(); !notNull.Equals(thawed) || thawed.Equals(emptyRecord) {
		t.Fatalf("exact AnyRecord thaw = %v, want erased AnyRecord(false)", thawed)
	}
	arrayHandle := snapshot(t, values.NewArrayType(false, notNull))
	array, ok := arrayHandle.Type().(*values.ArrayType)
	if !ok || array.ElementType == nil || !array.ElementType.Equals(notNull) {
		t.Fatalf("exact ARRAY<AnyRecord> thaw = %v, want ARRAY<AnyRecord(false)>", arrayHandle.Type())
	}
	relationHandle, err := values.ExactRelationOf(notNull)
	if err != nil {
		t.Fatalf("ExactRelationOf(AnyRecord): %v", err)
	}
	innerHandle, ok := relationHandle.RelationInner()
	if !ok || !innerHandle.Type().Equals(notNull) {
		t.Fatalf("RELATION<AnyRecord> inner = (%v, %t), want AnyRecord(false)", innerHandle, ok)
	}

	canonicalBefore := notNullHandle.CanonicalBytes()
	mutatedCopy := notNullHandle.CanonicalBytes()
	mutatedCopy[0] ^= 0xff
	if !bytes.Equal(notNullHandle.CanonicalBytes(), canonicalBefore) {
		t.Fatal("mutating CanonicalBytes result reached the immutable exact handle")
	}

	alias := values.NamedCorrelationIdentifier("any-record")
	anyQOV := mustRFC232QOV(t, alias, notNull)
	anyTwinQOV := mustRFC232QOV(t, alias, notNullTwin)
	nullableQOV := mustRFC232QOV(t, alias, nullable)
	emptyQOV := mustRFC232QOV(t, alias, emptyRecord)
	if !values.EqualsWithoutChildren(anyQOV, anyTwinQOV) ||
		values.SemanticHashCode(anyQOV) != values.SemanticHashCode(anyTwinQOV) {
		t.Fatal("equal exact AnyRecord QOVs disagree on equality or hash")
	}
	if values.EqualsWithoutChildren(anyQOV, nullableQOV) ||
		values.SemanticHashCode(anyQOV) == values.SemanticHashCode(nullableQOV) {
		t.Fatal("AnyRecord QOV identity/hash omitted nullability")
	}
	if values.EqualsWithoutChildren(anyQOV, emptyQOV) ||
		values.SemanticHashCode(anyQOV) == values.SemanticHashCode(emptyQOV) {
		t.Fatal("AnyRecord QOV identity/hash collapsed into concrete RECORD<>")
	}

	baseInvoked := false
	accessors, accessorErr := values.PrimitiveAccessorsForType(notNull, func() values.Value {
		baseInvoked = true
		return anyQOV
	})
	var incompatible *values.IncompatibleOrderingTypeError
	if accessors != nil || !errors.As(accessorErr, &incompatible) || baseInvoked {
		t.Fatalf("PrimitiveAccessorsForType(AnyRecord) = (%v, %v, baseInvoked=%t), want erased-layout rejection", accessors, accessorErr, baseInvoked)
	}
}

func TestRFC232AnyRecordDoesNotAdmitPlaceholderTypes(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		typ  values.Type
	}{
		{name: "unknown", typ: values.UnknownType},
		{name: "any", typ: values.AnyType},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			handle, err := values.SnapshotExactType(testCase.typ)
			if handle != nil {
				t.Fatalf("SnapshotExactType(%v) returned a partial handle", testCase.typ)
			}
			var coded codedResolutionError
			if !errors.As(err, &coded) || coded.Code() != values.TypeUnresolved {
				t.Fatalf("SnapshotExactType(%v) error = %v, want TypeUnresolved", testCase.typ, err)
			}
		})
	}
}

func TestRFC232QOVConstructorRejectsInvalidInputsWithoutPartialValue(t *testing.T) {
	t.Parallel()

	var typedNil *values.RecordType
	cases := []struct {
		name        string
		correlation values.CorrelationIdentifier
		flowed      values.Type
		want        values.ResolutionErrorCode
	}{
		{"zero correlation", values.CorrelationIdentifier{}, values.NotNullLong, values.CorrelationZero},
		{"reserved current", values.CurrentCorrelation(), values.NotNullLong, values.CorrelationKindMismatch},
		{"nil type", values.NamedCorrelationIdentifier("nil"), nil, values.TypeNil},
		{"typed nil type", values.NamedCorrelationIdentifier("typed_nil"), typedNil, values.TypeTypedNil},
		{"unresolved type", values.NamedCorrelationIdentifier("unknown"), values.UnknownType, values.TypeUnresolved},
		{"universal any type", values.NamedCorrelationIdentifier("any"), values.AnyType, values.TypeUnresolved},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			qov, err := values.NewQuantifiedObjectValue(testCase.correlation, testCase.flowed)
			if qov != nil {
				t.Fatalf("invalid input returned partial QOV %T", qov)
			}
			var coded codedResolutionError
			if !errors.As(err, &coded) || coded.Code() != testCase.want {
				t.Fatalf("error = %v, want code %d", err, testCase.want)
			}
		})
	}
}

func TestRFC232QOVRecognizerRejectsEmbeddedViews(t *testing.T) {
	t.Parallel()

	qovValue := mustRFC232QOV(
		t,
		values.NamedCorrelationIdentifier("q"),
		values.NotNullLong,
	)
	qov, ok := values.AsQuantifiedObjectValue(qovValue)
	if !ok || qov == nil {
		t.Fatalf("values-owned QOV was not recognized: (%T, %t)", qov, ok)
	}

	foreign := &embeddedQOVView{QuantifiedObjectValue: qov}
	if recognized, ok := values.AsQuantifiedObjectValue(foreign); ok || recognized != nil {
		t.Fatalf("embedded foreign QOV view was admitted: (%T, %t)", recognized, ok)
	}
	var nilValue values.Value
	if recognized, ok := values.AsQuantifiedObjectValue(nilValue); ok || recognized != nil {
		t.Fatalf("nil Value was admitted as QOV: (%T, %t)", recognized, ok)
	}
}

func TestRFC232AliasMapRejectsKindAndBijectionViolations(t *testing.T) {
	t.Parallel()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	cases := []struct {
		name  string
		pairs []values.AliasPair
		want  values.ResolutionErrorCode
	}{
		{
			"current to named",
			[]values.AliasPair{{Source: values.CurrentCorrelation(), Target: a}},
			values.CorrelationKindMismatch,
		},
		{
			"duplicate source",
			[]values.AliasPair{{Source: a, Target: b}, {Source: a, Target: c}},
			values.CorrelationTypeConflict,
		},
		{
			"duplicate target",
			[]values.AliasPair{{Source: a, Target: c}, {Source: b, Target: c}},
			values.CorrelationTypeConflict,
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			aliases, err := values.NewAliasMap(testCase.pairs)
			if aliases != nil {
				t.Fatalf("invalid pairs returned partial AliasMap %T", aliases)
			}
			var coded codedResolutionError
			if !errors.As(err, &coded) || coded.Code() != testCase.want {
				t.Fatalf("error = %v, want code %d", err, testCase.want)
			}
		})
	}

	empty := values.EmptyAliasMap()
	if _, ok := empty.Target(a); ok {
		t.Fatal("EmptyAliasMap unexpectedly mapped a correlation")
	}
}

func TestRFC232CurrentCorrelationCannotBeForgedByName(t *testing.T) {
	t.Parallel()

	current := values.CurrentCorrelation()
	if current != values.CurrentCorrelation() {
		t.Fatal("CurrentCorrelation() did not return the stable tagged current identifier")
	}
	namedForgery := values.NamedCorrelationIdentifier(current.Name())
	if namedForgery == current || values.SameLeg(namedForgery, current) {
		t.Fatalf("named correlation %q forged the reserved current correlation kind", namedForgery)
	}
}

func TestRFC232CurrentCorrelationIsAFunctionNotAssignableState(t *testing.T) {
	t.Parallel()

	source, err := correlationSource.ReadFile("correlation.go")
	if err != nil {
		t.Fatalf("read embedded correlation.go: %v", err)
	}
	parsed, err := parser.ParseFile(
		token.NewFileSet(),
		"correlation.go",
		source,
		parser.SkipObjectResolution,
	)
	if err != nil {
		t.Fatalf("parse correlation.go: %v", err)
	}

	foundFunction := false
	for _, declaration := range parsed.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv == nil && declaration.Name.Name == "CurrentCorrelation" {
				foundFunction = true
			}
		case *ast.GenDecl:
			if declaration.Tok != token.VAR && declaration.Tok != token.CONST {
				continue
			}
			for _, specification := range declaration.Specs {
				valueSpecification, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range valueSpecification.Names {
					if name.Name == "CurrentAlias" || name.Name == "CurrentCorrelation" {
						t.Fatalf("%s remains assignable package state; reserved current must be exposed only by CurrentCorrelation()", name.Name)
					}
				}
			}
		}
	}
	if !foundFunction {
		t.Fatal("correlation.go has no package-level CurrentCorrelation function")
	}
}
