package values

import (
	"math"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/vectorcodec"

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

// `a[1]` over an array parameter serializes as Java's PSubscriptValue (index,
// source) and reads back typed as the nullable element.
func TestSubscriptMacroProtoRoundTrip(t *testing.T) {
	t.Parallel()
	st1 := NewRecordType("ST1", true, []Field{{Name: "Y", FieldType: NullableLong}})
	arr := &ArrayType{ElementType: st1, Nullable: true}
	param, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("c1"), arr)
	if err != nil {
		t.Fatal(err)
	}
	m := &MacroFunction{
		Name: "FIRST", Params: []QuantifiedObjectValue{param}, ParamTypes: []Type{arr},
		ParamNames: []string{"A"}, Defaults: []Value{nil},
		Body: NewSubscriptValue(param, &ConstantValue{Value: int64(1), Typ: NotNullLong}, st1),
	}
	p, err := m.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	back, err := MacroFunctionFromProto(p.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	sub, ok := back.Body.(*SubscriptValue)
	if !ok || !sub.Type().Equals(st1) {
		t.Fatalf("body = %#v, want a subscript typed %v", back.Body, st1)
	}
	again, err := back.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(p, again) {
		t.Errorf("round trip changed the macro:\n%s\n%s", prototext.Format(p), prototext.Format(again))
	}
}

func TestLiteralSerializationJavaCarriers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		value    any
		typ      Type
		expected *gen.Value
	}{
		{"int", int64(7), NotNullInt, &gen.Value{IntValue: proto.Int32(7)}},
		{"float", float64(1.25), NotNullFloat, &gen.Value{FloatValue: proto.Float32(1.25)}},
		{"long", int64(7), NotNullLong, &gen.Value{LongValue: proto.Int64(7)}},
		{"native int", int(7), NotNullInt, &gen.Value{IntValue: proto.Int32(7)}},
		{"narrow long carrier", int32(7), NotNullLong, &gen.Value{LongValue: proto.Int64(7)}},
		{"narrow double carrier", float32(1.25), NotNullDouble, &gen.Value{DoubleValue: proto.Float64(1.25)}},
		{"rounded float", float64(1.23456789), NotNullFloat, &gen.Value{FloatValue: proto.Float32(float32(1.23456789))}},
		{"double", float64(1.25), NotNullDouble, &gen.Value{DoubleValue: proto.Float64(1.25)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wire, err := NewSerializationContext().ValueToProto(&ConstantValue{Value: tc.value, Typ: tc.typ})
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(wire.GetLiteralValue().GetValue().GetPrimitiveObject(), tc.expected) {
				t.Fatalf("wrong Java carrier: %v", wire)
			}
		})
	}
	for _, v := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1} {
		if _, err := NewSerializationContext().ValueToProto(&ConstantValue{Value: v, Typ: NotNullInt}); err == nil {
			t.Fatalf("serialized out-of-range INT %d", v)
		}
	}
}

func TestVectorLiteralAndNullPromotionSerialization(t *testing.T) {
	t.Parallel()
	typ := NewVectorType(true, 32, 2)
	wire, err := NewSerializationContext().ValueToProto(&ConstantValue{Value: vectorcodec.SerializeAs(vectorcodec.TypeSingle, []float64{1, 2}), Typ: typ})
	if err != nil {
		t.Fatal(err)
	}
	literal := wire.GetLiteralValue()
	if !proto.Equal(literal.GetResultType(), literal.GetValue().GetType()) {
		t.Errorf("Java needs the nested comparable-object VECTOR type: %v", literal)
	}
	promotion, err := NewPromoteValueChecked(NewNullValue(NullType), typ)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewSerializationContext().ValueToProto(promotion)
	if err != nil {
		t.Fatal(err)
	}
	if op := p.GetPromoteValue().GetPromotionTrie().GetValue().GetPrimitiveCoercionBiFunction().GetOperator(); op.String() != "NULL_TO_VECTOR" {
		t.Fatalf("operator=%s", op)
	}
}
