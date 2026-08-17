package values

import (
	"bytes"
	"testing"
)

// A record/enum NAME is PROVENANCE, not identity — Java's Type.Record.equals and
// Type.Enum.equals compare (typeCode, isNullable, fields/enumValues) and never
// the name, and Go's exact channel must not be stricter than the Type equality
// it exists to make checkable.
//
// Each arm below drives ONE attribute, so the pin says which dimension it
// watches rather than asserting a single blob: the name must NOT separate two
// otherwise-identical types, and every attribute that IS identity must still
// separate them. Without the negative arms the whole file would pass against a
// comparison that had degenerated to "always true".

func recordOf(name string, nullable bool, fields ...Field) *RecordType {
	return &RecordType{RecordName: name, Nullable: nullable, Fields: fields}
}

func fieldOf(name string, ordinal int, typ Type) Field {
	return Field{Name: name, Ordinal: ordinal, FieldType: typ}
}

func TestRecordTypeEqualsIgnoresRecordName(t *testing.T) {
	t.Parallel()
	base := recordOf("EMP", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))
	renamed := recordOf("DEPT", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))
	anonymous := recordOf("", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))

	for _, other := range []*RecordType{renamed, anonymous} {
		if !base.Equals(other) {
			t.Fatalf("RecordName must not participate in equality: %s != %s",
				DescribeType(base), DescribeType(other))
		}
	}

	// Every attribute that IS identity still separates. A mutation per line, so
	// a degenerate always-true Equals cannot pass this test.
	differing := map[string]*RecordType{
		"record nullability": recordOf("EMP", true, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong)),
		"field name":         recordOf("EMP", false, fieldOf("ID", 0, NullableLong), fieldOf("EID", 1, NullableLong)),
		"field type":         recordOf("EMP", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableInt)),
		"field nullability":  recordOf("EMP", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NotNullLong)),
		"arity":              recordOf("EMP", false, fieldOf("ID", 0, NullableLong)),
	}
	for what, other := range differing {
		if base.Equals(other) {
			t.Fatalf("%s must separate two record types: %s == %s",
				what, DescribeType(base), DescribeType(other))
		}
	}
}

func TestEnumTypeEqualsIgnoresEnumName(t *testing.T) {
	t.Parallel()
	vals := []EnumValue{{Name: "RED", Number: 0}, {Name: "GREEN", Number: 1}}
	base := &EnumType{EnumName: "COLOUR", Values: vals}
	renamed := &EnumType{EnumName: "SHADE", Values: vals}
	if !base.Equals(renamed) {
		t.Fatalf("EnumName must not participate in equality: %v != %v", base, renamed)
	}
	if base.Equals(&EnumType{EnumName: "COLOUR", Nullable: true, Values: vals}) {
		t.Fatal("enum nullability must separate two enum types")
	}
	if base.Equals(&EnumType{EnumName: "COLOUR", Values: vals[:1]}) {
		t.Fatal("enum value list must separate two enum types")
	}
}

// The exact channel's canonical bytes are the identity used at memo admission
// (memo.member.resultType) and by every plan-side exact comparison. They must
// agree with Type.Equals: a member rejected there for carrying a differently
// NAMED row shape is a correct alternative refused, and that is how one
// legitimate join orientation stopped competing on cost at all.
func TestExactCanonicalIgnoresRecordAndEnumName(t *testing.T) {
	t.Parallel()
	canonical := func(t *testing.T, typ Type) []byte {
		t.Helper()
		handle, err := SnapshotExactType(typ)
		if err != nil {
			t.Fatalf("snapshot %s: %v", DescribeType(typ), err)
		}
		return handle.CanonicalBytes()
	}
	named := canonical(t, recordOf("EMP", false, fieldOf("ID", 0, NullableLong)))
	renamed := canonical(t, recordOf("DEPT", false, fieldOf("ID", 0, NullableLong)))
	if !bytes.Equal(named, renamed) {
		t.Fatal("record name must not enter the exact canonical identity")
	}
	nested := canonical(t, recordOf("OUT", false,
		fieldOf("D", 0, recordOf("DEEP", true, fieldOf("DK", 0, NullableLong)))))
	nestedRenamed := canonical(t, recordOf("OTHER", false,
		fieldOf("D", 0, recordOf("ELSEWHERE", true, fieldOf("DK", 0, NullableLong)))))
	if !bytes.Equal(nested, nestedRenamed) {
		t.Fatal("a NESTED record name must not enter the exact canonical identity either")
	}
	// The identity is still an identity: a nested nullability flip separates.
	nestedNotNull := canonical(t, recordOf("OUT", false,
		fieldOf("D", 0, recordOf("DEEP", false, fieldOf("DK", 0, NullableLong)))))
	if bytes.Equal(nested, nestedNotNull) {
		t.Fatal("nested record nullability must separate two exact identities")
	}
	enumNamed := canonical(t, &EnumType{EnumName: "COLOUR", Values: []EnumValue{{Name: "RED", Number: 0}}})
	enumRenamed := canonical(t, &EnumType{EnumName: "SHADE", Values: []EnumValue{{Name: "RED", Number: 0}}})
	if !bytes.Equal(enumNamed, enumRenamed) {
		t.Fatal("enum name must not enter the exact canonical identity")
	}
}

// QuantifiedRowShapesAgree and its exact-handle twin decide every runtime QOV
// LOOKUP. Both arms are driven here — the executor binders reach the first, the
// values-owned binders the second — because an arm that only the corpus happens
// to reach ships untested, and these two are reached by different channels.
func TestRowShapeAgreementIgnoresOnlyTheTopLevelNullableBit(t *testing.T) {
	t.Parallel()
	own := recordOf("EMP", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))
	nullExtended := recordOf("EMP", true, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))

	agree := func(t *testing.T, left, right Type) bool {
		t.Helper()
		typeLevel := QuantifiedRowShapesAgree(left, right)
		lh, lerr := SnapshotExactType(left)
		rh, rerr := SnapshotExactType(right)
		if lerr != nil || rerr != nil {
			t.Fatalf("snapshot: %v / %v", lerr, rerr)
		}
		exactLevel := exactRowShapesAgree(lh.(*exactType), rh.(*exactType))
		if typeLevel != exactLevel {
			t.Fatalf("the two agreement arms disagree on (%s, %s): Type=%v exact=%v",
				DescribeType(left), DescribeType(right), typeLevel, exactLevel)
		}
		return typeLevel
	}

	if !agree(t, own, nullExtended) || !agree(t, nullExtended, own) {
		t.Fatal("the TOP-LEVEL nullable bit states that the row may be ABSENT, which the binding carries structurally; it cannot separate two lookups of one leg")
	}
	if !agree(t, own, own) {
		t.Fatal("a shape must agree with itself")
	}
	if !agree(t, own, recordOf("OTHERNAME", false, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableLong))) {
		t.Fatal("the record name must not separate two lookups of one leg")
	}

	// Everything else about the shape still separates — including a NESTED
	// nullability flip, which is the one that would otherwise slip through a
	// comparison written as \"ignore nullability\".
	for what, other := range map[string]Type{
		"field name": recordOf("EMP", true, fieldOf("ID", 0, NullableLong), fieldOf("EID", 1, NullableLong)),
		"arity":      recordOf("EMP", true, fieldOf("ID", 0, NullableLong)),
		"field type": recordOf("EMP", true, fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NullableInt)),
		"field nullability": recordOf("EMP", true,
			fieldOf("ID", 0, NullableLong), fieldOf("DID", 1, NotNullLong)),
		"nested record nullability": recordOf("EMP", true,
			fieldOf("ID", 0, NullableLong),
			fieldOf("DID", 1, recordOf("DEEP", true, fieldOf("DK", 0, NullableLong)))),
	} {
		if agree(t, own, other) {
			t.Fatalf("%s must separate two row shapes: %s vs %s",
				what, DescribeType(own), DescribeType(other))
		}
	}

	if QuantifiedRowShapesAgree(nil, own) || QuantifiedRowShapesAgree(own, nil) {
		t.Fatal("a nil side can never agree — an absent declaration is not a match")
	}
}

// The conflict rendering must show every attribute equality compares, or two
// provably-unequal types print identically and the message reads as a bug in
// the diagnostic. It cost one investigation a full lap when the record name and
// the field ordinals were both invisible.
func TestDescribeTypeRendersWhatEqualityCompares(t *testing.T) {
	t.Parallel()
	got := DescribeType(recordOf("EMP", true,
		fieldOf("ID", 0, NullableLong),
		fieldOf("D", 1, recordOf("DEEP", true, fieldOf("DK", 0, NotNullLong)))))
	want := "RECORD@EMP(ID:LONG?,D:RECORD@DEEP(DK:LONG)?)?"
	if got != want {
		t.Fatalf("DescribeType = %q, want %q", got, want)
	}
	if plain := DescribeType(recordOf("", false, fieldOf("ID", 0, NullableLong))); plain != "RECORD(ID:LONG?)" {
		t.Fatalf("an unnamed record must render without the @ marker, got %q", plain)
	}
}
