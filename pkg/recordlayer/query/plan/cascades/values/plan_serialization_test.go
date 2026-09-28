package values

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// A macro over a nested struct serializes each record type once and by
// reference afterwards, and reads back to the same bytes.
func TestMacroFunctionProtoRoundTrip(t *testing.T) {
	t.Parallel()
	inner := NewRecordType("ST1", true, []Field{{Name: "Y", FieldType: NullableLong}, {Name: "Z", FieldType: NullableLong, Ordinal: 1}})
	outer := NewRecordType("ST3", true, []Field{{Name: "V", FieldType: inner}})
	param, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("c1"), outer)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ResolveFieldOrdinals(param, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	num, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("c2"), NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	m := &MacroFunction{
		Name: "Z", Params: []QuantifiedObjectValue{param, num}, ParamTypes: []Type{outer, NullableLong},
		ParamNames: []string{"X", "N"},
		Defaults:   []Value{nil, NewPromoteValue(&ConstantValue{Value: int32(2), Typ: NullableInt}, NullableLong)},
		Body:       body,
	}
	p, err := m.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	text := prototext.Format(p)
	compact := strings.Join(strings.Fields(text), "")
	for _, want := range []string{"reference_id:0", "name:\"ST3\"", "reference_id:1", "operator:INT_TO_LONG", "isProvided:true"} {
		if !strings.Contains(compact, strings.ReplaceAll(want, " ", "")) {
			t.Errorf("serialized macro lacks %s:\n%s", want, text)
		}
	}
	back, err := MacroFunctionFromProto(p.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	again, err := back.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(p, again) {
		t.Errorf("round trip changed the macro:\n%s\n%s", text, prototext.Format(again))
	}
}
