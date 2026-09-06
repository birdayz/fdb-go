package values

import (
	"strings"
	"testing"
)

// TestWhichRecordTypesCanBeGivenADescriptor pins the STAMPING PREDICATE behind
// the array-of-record-literals outcomes, at the type level and without Docker.
//
// The end-to-end table (TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes, package
// sqldriver) measures which queries answer and which fail. It cannot run at all
// without a container, and it observes outcomes rather than the predicate that
// produces them. These three rows are that predicate, and they are what makes
// the outcomes explicable rather than merely recorded:
//
//   - A record whose FIELD NAME protobuf will not carry has no descriptor. That
//     is Java's rule (`ProtoUtils`), and it is why such a constructor evaluates
//     to a name-keyed map instead of a message.
//   - A record CONTAINING that record has no descriptor either, because its own
//     type contains the offending name. This is the containment that makes the
//     ordinary case safe: parent and child fail together and the whole value
//     degrades to maps, which ANSWERS in a weaker type.
//   - A record containing an ARRAY of a record whose fields are ANONYMOUS does
//     have one. That is the shape array unification produces when the elements
//     disagree, and it is exactly how the offending name gets erased from the
//     parent's type — leaving the parent stampable while the element
//     constructor underneath is not, which is the pair that fails a query.
//
// So the failing case needs the erasure, not just the bad name; and the bad name,
// not just the wrapper. Two earlier rounds asserted one of those halves alone and
// were refuted by measurement.
func TestWhichRecordTypesCanBeGivenADescriptor(t *testing.T) {
	t.Parallel()

	badNamed := NewRecordType("", false, []Field{{Name: "$lead", FieldType: NotNullInt}})
	anonymous := NewRecordType("", false, []Field{{Name: "", FieldType: NotNullInt}})

	for _, tc := range []struct {
		why string
		typ Type
		// The REASON the refusal must give, not just the offending name: the
		// error renders the whole type first, and that rendering already
		// contains `$lead`, so matching the name alone would be satisfied by
		// any unrelated descriptor failure for the same type.
		wantErrOn string // empty means it must SUCCEED
	}{
		{
			why:       "a field name protobuf will not carry",
			typ:       badNamed,
			wantErrOn: `field name "$lead"`,
		},
		{
			why:       "a record CONTAINING it: containment carries the refusal upward, which is what makes parent and child fail together",
			typ:       NewRecordType("", false, []Field{{Name: "CH", FieldType: badNamed}}),
			wantErrOn: `field name "$lead"`,
		},
		{
			why: "a record containing an ARRAY of an ANONYMOUS record: unification has erased the offending name, so this parent IS stampable while an element constructor underneath is not",
			typ: NewRecordType("", false, []Field{{Name: "CH", FieldType: NewArrayType(false, anonymous)}}),
		},
	} {
		// A fresh repository per row: a shared one would carry the first
		// refusal forward and make every later row fail for the wrong reason.
		_, err := NewTypeProtoRepository().MessageDescriptorFor(tc.typ)
		if tc.wantErrOn == "" {
			if err != nil {
				t.Errorf("%v was refused a descriptor (%v), want one: %s. If this shape stopped "+
					"being stampable, the failing queries in the sqldriver table stop failing for "+
					"the reason recorded there", tc.typ, err, tc.why)
			}
			continue
		}
		if err == nil {
			t.Errorf("%v was given a descriptor, want a refusal naming %q: %s. If protobuf accepts "+
				"this name now, the whole booking has closed", tc.typ, tc.wantErrOn, tc.why)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErrOn) {
			t.Errorf("%v was refused with %v, want a refusal naming %q: a different reason means "+
				"this row no longer measures the rule it names", tc.typ, err, tc.wantErrOn)
		}
	}
}

// TestUnificationErasesAFieldNameOnlyWhenTheNamesDisagree pins the ERASURE that
// decides whether an unsynthesisable field name reaches the parent's type.
//
// It is the condition two write-ups missed, and until now it was the only one
// still asserted in prose: the row above hand-builds an anonymous record and
// calls it "the shape array unification produces", which nothing checked. These
// two rows call the unifier.
//
// When the field names DISAGREE, unification cannot keep either, so the result
// is anonymous — and an offending name is erased along with the good one. The
// parent's type is then synthesisable while the element constructor underneath
// still is not, which is the pair that fails a query. When the names AGREE, the
// name survives into the result; if it is one protobuf will not carry, the
// parent cannot be stamped either, and the whole value degrades to maps and
// ANSWERS. Same bad name, opposite outcomes, decided here.
func TestUnificationErasesAFieldNameOnlyWhenTheNamesDisagree(t *testing.T) {
	t.Parallel()

	badInt := NewRecordType("", false, []Field{{Name: "$lead", FieldType: NotNullInt}})
	goodInt := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullInt}})
	badDouble := NewRecordType("", false, []Field{{Name: "$lead", FieldType: NotNullDouble}})

	erased := MaximumType(badInt, goodInt)
	if erased == nil {
		t.Fatal("unifying two one-field records produced no common type: the erasure this row " +
			"pins cannot be measured, and the failing query's precondition is gone")
	}
	if got := fieldNames(t, erased); len(got) != 1 || got[0] != "" {
		t.Fatalf("unifying {$lead:int} with {A:int} gave field names %q, want one ANONYMOUS field: "+
			"disagreeing names must be erased, and that erasure is what leaves the parent "+
			"stampable over a child that is not", got)
	}
	if _, err := NewTypeProtoRepository().MessageDescriptorFor(erased); err != nil {
		t.Fatalf("the erased target was refused a descriptor (%v), want one: if the target is not "+
			"synthesisable the parent cannot stamp, and the query would degrade rather than fail",
			err)
	}

	kept := MaximumType(badInt, badDouble)
	if kept == nil {
		t.Fatal("unifying {$lead:int} with {$lead:double} produced no common type")
	}
	if got := fieldNames(t, kept); len(got) != 1 || got[0] != "$lead" {
		t.Fatalf("unifying {$lead:int} with {$lead:double} gave field names %q, want the name KEPT: "+
			"agreeing names survive unification, which is why that query answers where the "+
			"disagreeing one fails", got)
	}
	if _, err := NewTypeProtoRepository().MessageDescriptorFor(kept); err == nil {
		t.Fatal("the target that KEPT `$lead` was given a descriptor: it must be refused, or the " +
			"parent would stamp and that query would fail instead of degrading")
	}
}

func fieldNames(t *testing.T, typ Type) []string {
	t.Helper()
	rec, ok := typ.(*RecordType)
	if !ok {
		t.Fatalf("unification produced %v, want a record type", typ)
	}
	names := make([]string, 0, len(rec.Fields))
	for _, f := range rec.Fields {
		names = append(names, f.Name)
	}
	return names
}
