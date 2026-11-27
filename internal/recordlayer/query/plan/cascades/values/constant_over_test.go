package values

import "testing"

// TestIsConstantOver pins Java's MatchConstantValueRule test: a value is
// constant over an operator when it reads only aliases the operator does not
// bind, and holds no aggregate, record constructor or queried value.
func TestIsConstantOver(t *testing.T) {
	t.Parallel()
	row := NewRecordType("", false, []Field{{Name: "X", FieldType: NullableLong, Ordinal: 0}})
	field := func(alias string) Value {
		qov, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier(alias), row)
		if err != nil {
			t.Fatal(err)
		}
		request, err := FieldByName("X")
		if err != nil {
			t.Fatal(err)
		}
		v, err := ResolveFieldAccess(qov, []FieldRequest{request})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	local := map[CorrelationIdentifier]struct{}{NamedCorrelationIdentifier("inner"): {}}
	current, err := newCurrentQOVForLayout(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		v    Value
		want bool
	}{
		{"literal", &ConstantValue{Value: int64(1), Typ: NotNullLong}, true},
		{"outer row", field("outer"), true},
		{"bound row", field("inner"), false},
		{"input carrier", current, false},
		{"aggregate over the outer row", NewAggregateValue(AggSum, field("outer")), false},
		{"record over the outer row", NewRecordConstructorValue(RecordConstructorField{Name: "X", Value: field("outer")}), false},
		{"queried value", NewQueriedValue([]string{"T"}, row), false},
	} {
		if got := IsConstantOver(tc.v, local); got != tc.want {
			t.Errorf("IsConstantOver(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
