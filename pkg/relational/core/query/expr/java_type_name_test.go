package expr

// The one message this engine reproduces VERBATIM from Java carries a type
// name, and Go's TypeCode does not spell every type the way Java's
// DataType.Code does.
//
// "IN list with column reference must be of array type, but got: %s" is quoted
// from ExpressionVisitor.java:641-643 so a reader comparing the two engines
// does not have to translate. That is only true if the NAME matches too, and
// most codes coincide — which is exactly what makes the two that do not
// dangerous. The end-to-end test for this message uses a BIGINT column, where
// Go and Java both say LONG, so a verbatim claim tested only there is true of
// the test and false of the claim.
//
// Both mismatching arms are driven here rather than through SQL, because a
// STRUCT column needs a type declaration and an INTEGER one needs its own
// fixture, and neither would make the mapping any more certain than calling it
// does. The e2e side already proves the mapping is WIRED into the message
// (in_list_array_column_fdb_test.go asserts the sentence for a non-array
// column); this proves it is RIGHT.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestJavaTypeCodeName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   values.Type
		want string
	}{
		// The two that DIVERGE. Go spells these INT and RECORD; Java's
		// DataType.Code spells them INTEGER and STRUCT.
		{"INT is INTEGER to Java", values.NullableInt, "INTEGER"},
		{"RECORD is STRUCT to Java", values.NewRecordType("R", false, nil), "STRUCT"},

		// The ones that coincide, kept so a future edit that "simplified" the
		// mapping into a rename of everything would fail here rather than
		// silently changing messages Java already agrees with.
		{"LONG coincides", values.NullableLong, "LONG"},
		{"STRING coincides", values.NullableString, "STRING"},
		{"DOUBLE coincides", values.NullableDouble, "DOUBLE"},
		{"BOOLEAN coincides", values.NullableBoolean, "BOOLEAN"},

		// A nil type has no name to report and must not panic: the caller
		// reaches this with whatever the resolver produced, and an untyped
		// column reference is a real shape.
		{"nil is UNKNOWN", nil, "UNKNOWN"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := javaTypeCodeName(c.in); got != c.want {
				t.Errorf("javaTypeCodeName = %q, want %q — the message this feeds is quoted "+
					"verbatim from Java, so a name Java spells differently makes the quote "+
					"false for that type", got, c.want)
			}
		})
	}
}

// TestIncompatibleInListElement drives the gate that keeps a UUID probe from
// being compared against STRING array elements.
//
// The gate exists because an ARRAY COLUMN is ONE value: PromoteValue is scalar,
// so unlike an explicit value list there is no per-item wrapper to convert the
// elements with. Left ungated, cmpAny declines the [16]byte-versus-string pair
// rather than erroring, and the row is silently dropped while NOT IN admits it.
//
// Every arm matters in a different direction, so each is named: refusing too
// little is a silent wrong answer, refusing too much rejects working queries.
func TestIncompatibleInListElement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		probe, elem values.Type
		wantRefusal bool
		why         string
	}{
		{
			name: "UUID probe against STRING elements", probe: values.NullableUuid,
			elem: values.NullableString, wantRefusal: true,
			why: "needs per-element conversion this path cannot perform",
		},
		{
			name: "STRING probe against UUID elements", probe: values.NullableString,
			elem: values.NullableUuid, wantRefusal: true,
			why: "the same conversion, in the other direction",
		},
		{
			name: "UUID probe against UUID elements", probe: values.NullableUuid,
			elem: values.NullableUuid, wantRefusal: false,
			why: "no conversion needed — refusing this would reject a working query",
		},
		{
			name: "STRING probe against STRING elements", probe: values.NullableString,
			elem: values.NullableString, wantRefusal: false,
		},
		{
			name: "LONG probe against INT elements", probe: values.NullableLong,
			elem: values.NullableInt, wantRefusal: false,
			why: "cmpAny promotes across numeric widths at evaluation, so no gate is needed",
		},
		{
			name: "LONG probe against STRING elements", probe: values.NullableLong,
			elem: values.NullableString, wantRefusal: true,
			why: "the ordinary type-compatibility gate: these do not unify at all",
		},
		// UNKNOWN is permitted on either side, matching every other gate in this
		// resolver: an untyped side may turn out compatible, and refusing it
		// would reject working queries to catch a hypothetical one.
		{
			name: "UNKNOWN probe is permitted", probe: values.UnknownType,
			elem: values.NullableString, wantRefusal: false,
		},
		{
			name: "UNKNOWN element is permitted", probe: values.NullableUuid,
			elem: values.UnknownType, wantRefusal: false,
		},
		{name: "nil probe is permitted", probe: nil, elem: values.NullableString, wantRefusal: false},
		{name: "nil element is permitted", probe: values.NullableUuid, elem: nil, wantRefusal: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := incompatibleInListElement(c.probe, c.elem)
			if got != c.wantRefusal {
				verb := "refused"
				if !c.wantRefusal {
					verb = "permitted"
				}
				t.Errorf("incompatibleInListElement = %v, want %v — this pair must be %s: %s",
					got, c.wantRefusal, verb, c.why)
			}
		})
	}
}
