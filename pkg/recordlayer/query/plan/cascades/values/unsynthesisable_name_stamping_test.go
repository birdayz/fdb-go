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
		why       string
		typ       Type
		wantErrOn string // substring the refusal must name; empty means it must succeed
	}{
		{
			why:       "a field name protobuf will not carry",
			typ:       badNamed,
			wantErrOn: "$lead",
		},
		{
			why:       "a record CONTAINING it: containment carries the refusal upward, which is what makes parent and child fail together",
			typ:       NewRecordType("", false, []Field{{Name: "CH", FieldType: badNamed}}),
			wantErrOn: "$lead",
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
