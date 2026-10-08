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

// A literal's type is NOT NULL, as Java's LiteralValue.ofScalar types it,
// whatever nullability the carrier's declared type has.
func TestLiteralSerializationNullability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v        *ConstantValue
		nullable bool
	}{
		{&ConstantValue{Value: int64(0), Typ: NullableLong}, false},
		{&ConstantValue{Value: int64(2), Typ: NullableInt}, false},
		{&ConstantValue{Value: "a", Typ: NullableString}, false},
		{&ConstantValue{Value: int64(0), Typ: NotNullLong}, false},
	} {
		p, err := NewSerializationContext().ValueToProto(tc.v)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.GetLiteralValue().GetResultType().GetPrimitiveType().GetIsNullable(); got != tc.nullable {
			t.Errorf("literal %v of %v serializes is_nullable=%v, want %v", tc.v.Value, tc.v.Typ, got, tc.nullable)
		}
	}
}

// Boolean literals persist as Java's LiteralValue and read back as Go's
// BooleanValue; LIKE persists as Java's PLikeOperatorValue over a
// PPatternForLikeValue.
func TestBooleanAndLikeMacroBodiesSerialize(t *testing.T) {
	t.Parallel()
	s, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("s"), NullableString)
	if err != nil {
		t.Fatal(err)
	}
	like := NewLikeOperatorValue(s, NewPatternForLikeValue(&ConstantValue{Value: "a%", Typ: NotNullString}, NewNullValue(NullType)))
	for _, tc := range []struct {
		body Value
		want string
	}{
		{NewBooleanValue(true), `literal_value:{result_type:{primitive_type:{type_code:BOOLEANis_nullable:false}}value:{primitive_object:{bool_value:true}}}`},
		{NewBooleanValue(false), `bool_value:false`},
		{&BooleanValue{}, `primitive_type:{type_code:BOOLEANis_nullable:true}`},
		{like, `like_operator_value:{src_child:{quantified_object_value:{alias:"s"`},
		{like, `pattern_child:{pattern_for_like_value:{pattern_child:{literal_value:`},
	} {
		m := &MacroFunction{
			Name: "F", Params: []QuantifiedObjectValue{s}, ParamTypes: []Type{NullableString},
			ParamNames: []string{"S"}, Defaults: []Value{nil}, Body: tc.body,
		}
		p, err := m.ToProto()
		if err != nil {
			t.Fatalf("%T: %v", tc.body, err)
		}
		if compact := strings.Join(strings.Fields(prototext.Format(p)), ""); !strings.Contains(compact, tc.want) {
			t.Errorf("serialized %T lacks %s:\n%s", tc.body, tc.want, compact)
		}
		back, err := MacroFunctionFromProto(p.GetUserDefinedMacroFunction())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tc.body.(*BooleanValue); ok {
			if got, ok := back.Body.(*BooleanValue); !ok || !got.Type().Equals(tc.body.Type()) {
				t.Errorf("boolean literal read back as %#v", back.Body)
			}
		}
		again, err := back.ToProto()
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(p, again) {
			t.Errorf("round trip changed the macro:\n%s\n%s", prototext.Format(p), prototext.Format(again))
		}
	}
}

// A macro over an enum, as Java writes it (Type.Enum.toProto): the members
// with their storage names, the name and a differing storage name all read
// back and rewrite unchanged; a struct with an enum field serializes too.
func TestEnumMacroTypesRoundTrip(t *testing.T) {
	t.Parallel()
	var java gen.PUserDefinedFunction
	if err := prototext.Unmarshal([]byte(`user_defined_macro_function: {
		function_name: "M"
		arguments: { quantified_object_value: { alias: "e" result_type: { enum_type: {
			is_nullable: true
			enum_values: { name: "HAPPY" number: 0 }
			enum_values: { name: "a.b" number: 1 storage_name: "a__1b" }
			name: "MOOD" storage_name: "MOOD__"
		} } } }
		argumentNames: "E"
		defaultArgumentValues: { isProvided: false }
		body: { quantified_object_value: { alias: "e" result_type: { enum_type: {
			is_nullable: true
			enum_values: { name: "HAPPY" number: 0 }
			enum_values: { name: "a.b" number: 1 storage_name: "a__1b" }
			name: "MOOD" storage_name: "MOOD__"
		} } } }
	}`), &java); err != nil {
		t.Fatal(err)
	}
	m, err := MacroFunctionFromProto(java.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	enum, ok := m.ParamTypes[0].(*EnumType)
	if !ok || enum.StorageName != "MOOD__" || enum.Values[1].StorageName != "a__1b" {
		t.Fatalf("parameter type = %#v", m.ParamTypes[0])
	}
	again, err := m.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&java, again) {
		t.Errorf("Java enum macro changed on rewrite:\n%s\n%s", prototext.Format(&java), prototext.Format(again))
	}

	// A member that states no storage name has the derived one (EnumValue.from).
	derived, err := NewSerializationContext().TypeToProto(NewEnumType("E", true, []EnumValue{{Name: "CASH$", Number: 40}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := derived.GetEnumType().GetEnumValues()[0].GetStorageName(); got != "CASH__1" {
		t.Errorf("CASH$ storage name = %q, want CASH__1", got)
	}

	st := NewRecordType("ST", true, []Field{{Name: "M", FieldType: enum}, {Name: "N", FieldType: NullableLong, Ordinal: 1}})
	param, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("x"), st)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ResolveFieldOrdinals(param, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	goMacro := &MacroFunction{
		Name: "GM", Params: []QuantifiedObjectValue{param}, ParamTypes: []Type{st},
		ParamNames: []string{"X"}, Defaults: []Value{nil}, Body: body,
	}
	p, err := goMacro.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	back, err := MacroFunctionFromProto(p.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	if !back.Body.Type().Equals(enum) {
		t.Errorf("body type = %v, want %v", back.Body.Type(), enum)
	}
	if again, err := back.ToProto(); err != nil || !proto.Equal(p, again) {
		t.Errorf("round trip changed the macro (%v):\n%s\n%s", err, prototext.Format(p), prototext.Format(again))
	}
}

// Record types differing only in an enum field's members are different types
// (Type.Enum.equals), so the second is written in full, not by reference.
func TestEnumFieldsDistinguishRecordReferences(t *testing.T) {
	t.Parallel()
	c := NewSerializationContext()
	rec := func(members ...string) *RecordType {
		vals := make([]EnumValue, len(members))
		for i, m := range members {
			vals[i] = EnumValue{Name: m, Number: int32(i)}
		}
		return NewRecordType("ST", true, []Field{{Name: "M", FieldType: NewEnumType("MOOD", true, vals)}})
	}
	if _, err := c.TypeToProto(rec("HAPPY")); err != nil {
		t.Fatal(err)
	}
	p, err := c.TypeToProto(rec("HAPPY", "SAD"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.GetRecordType().GetFields()) != 1 {
		t.Fatalf("a different enum was written as a reference: %v", p)
	}
}

// A descriptor-derived record keeps its protobuf field numbers and storage
// names (Type.Record.Field.toProto), in the parameter type and in the body's
// field path, though the body's quantified object flows an exact type that
// has neither.
func TestRecordFieldNumbersAndStorageNamesSerialize(t *testing.T) {
	t.Parallel()
	inner := NewRecordType("I", true, []Field{{Name: "z", FieldType: NullableLong, Index: 7}})
	st := NewRecordType("T", true, []Field{
		{Name: "a.b", FieldType: NullableString, Index: 10, StorageName: "a__2b"},
		{Name: "b", FieldType: inner, Index: 20},
	})
	param, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("x"), st)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ResolveFieldOrdinals(param, []int{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	m := &MacroFunction{
		Name: "F", Params: []QuantifiedObjectValue{param}, ParamTypes: []Type{st},
		ParamNames: []string{"X"}, Defaults: []Value{nil}, Body: body,
	}
	p, err := m.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	compact := strings.Join(strings.Fields(prototext.Format(p)), "")
	for _, want := range []string{
		`field_name:"a.b"field_index:10field_storage_name:"a__2b"`,
		`field_name:"b"field_index:20`,
		`field_name:"z"field_index:7`,
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("serialized macro lacks %s:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "field_index:1}") || strings.Contains(compact, "field_index:2}") {
		t.Errorf("a field was renumbered by position:\n%s", compact)
	}
	back, err := MacroFunctionFromProto(p.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	if f := back.ParamTypes[0].(*RecordType).Fields[0]; f.Index != 10 || (f.StorageName != "a__2b" && DerivedStorageName(f.Name) != "a__2b") {
		t.Errorf("read back field %+v", f)
	}
	again, err := back.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(p, again) {
		t.Errorf("round trip changed the macro:\n%s\n%s", prototext.Format(p), prototext.Format(again))
	}
}
