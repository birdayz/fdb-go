package values

import (
	"strings"
	"testing"

	"fdb.dev/gen"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// The operator tables are RelOpValue's enums in declaration order, which
// protoEnumBiMap pins to the proto enums' order: entry i is proto number i+1.
func TestRelOpOperatorTablesMatchTheProtoEnums(t *testing.T) {
	t.Parallel()
	if len(binaryRelOpOperators) != len(gen.PBinaryRelOpValue_PBinaryPhysicalOperator_name) {
		t.Fatalf("%d binary operators, proto has %d", len(binaryRelOpOperators), len(gen.PBinaryRelOpValue_PBinaryPhysicalOperator_name))
	}
	for i, op := range binaryRelOpOperators {
		if int(op.proto) != i+1 {
			t.Errorf("binary operator %d is %v", i, op.proto)
		}
	}
	if len(unaryRelOpOperators) != len(gen.PUnaryRelOpValue_PUnaryPhysicalOperator_name) {
		t.Fatalf("%d unary operators, proto has %d", len(unaryRelOpOperators), len(gen.PUnaryRelOpValue_PUnaryPhysicalOperator_name))
	}
	for i, op := range unaryRelOpOperators {
		if int(op.proto) != i+1 {
			t.Errorf("unary operator %d is %v", i, op.proto)
		}
	}
	if len(binaryRelOpBySignature) != len(binaryRelOpOperators) || len(unaryRelOpBySignature) != len(unaryRelOpOperators) {
		t.Errorf("operator signatures are not unique")
	}
}

// RelOpValue.encapsulate: the operator is typed over the operands as given,
// BOOLEAN has no ordering, a record is a complex comparand, and an ARRAY
// against NULL promotes the NULL.
func TestRelOpValueEncapsulate(t *testing.T) {
	t.Parallel()
	lng := &ConstantValue{Value: int64(1), Typ: NotNullLong}
	in := &ConstantValue{Value: int64(5), Typ: NotNullInt}
	rel, err := NewBinaryRelOpValue(RelOpGreaterThan, lng, in)
	if err != nil || rel.PhysicalOperator() != gen.PBinaryRelOpValue_GT_LI || rel.FunctionName != "gt" {
		t.Fatalf("lng > int = %+v, %v", rel, err)
	}
	if _, err := NewBinaryRelOpValue(RelOpLessThan, NewBooleanValue(true), NewBooleanValue(false)); err == nil {
		t.Errorf("TRUE < FALSE has no operator")
	}
	rec := NewRecordType("R", true, []Field{{Name: "X", FieldType: NullableLong}})
	q, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q"), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBinaryRelOpValue(RelOpEquals, q, q); err == nil || !err.(*RelOpError).Complex {
		t.Errorf("record = record: %v", err)
	}
	arr := NewArrayConstructorValue(NotNullLong, []Value{lng})
	rel, err = NewBinaryRelOpValue(RelOpEquals, arr, NewNullValue(NullType))
	if err != nil || rel.PhysicalOperator() != gen.PBinaryRelOpValue_EQ_ARRAY_ARRAY {
		t.Fatalf("array = NULL = %+v, %v", rel, err)
	}
	un, err := NewUnaryRelOpValue(RelOpIsNull, lng)
	if err != nil || un.PhysicalOperator() != gen.PUnaryRelOpValue_IS_NULL_LI || un.FunctionName != "isNull" {
		t.Fatalf("IS NULL = %+v, %v", un, err)
	}
}

// A macro body as Java writes it — `x > 5 AND NOT (y IS NULL) OR x <> y` over
// RelOpValue, AndOrValue and NotValue — reads back and rewrites unchanged.
func TestJavaRelOpMacroBodyRoundTrips(t *testing.T) {
	t.Parallel()
	qov := func(alias string) string {
		return `quantified_object_value: { alias: "` + alias + `" result_type: { primitive_type: { type_code: LONG is_nullable: true } } }`
	}
	var java gen.PUserDefinedFunction
	if err := prototext.Unmarshal([]byte(`user_defined_macro_function: {
		function_name: "M"
		arguments: { `+qov("x")+` }
		arguments: { `+qov("y")+` }
		body: { and_or_value: { function_name: "or" operator: OR
			left_child: { and_or_value: { function_name: "and" operator: AND
				left_child: { binary_rel_op_value: { operator: GT_LI super: { function_name: "gt" comparison_type: GREATER_THAN
					children: { `+qov("x")+` }
					children: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 5 } } } } } } }
				right_child: { not_value: { child: { unary_rel_op_value: { operator: IS_NULL_LI super: { function_name: "isNull" comparison_type: IS_NULL
					children: { `+qov("y")+` } } } } } } } }
			right_child: { binary_rel_op_value: { operator: NEQ_LL super: { function_name: "notEquals" comparison_type: NOT_EQUALS
				children: { `+qov("x")+` } children: { `+qov("y")+` } } } } } }
		argumentNames: "X"
		argumentNames: "Y"
		defaultArgumentValues: { isProvided: false }
		defaultArgumentValues: { isProvided: false }
	}`), &java); err != nil {
		t.Fatal(err)
	}
	m, err := MacroFunctionFromProto(java.GetUserDefinedMacroFunction())
	if err != nil {
		t.Fatal(err)
	}
	or, ok := m.Body.(*AndOrValue)
	if !ok || or.Op != AndOrOr {
		t.Fatalf("body = %#v", m.Body)
	}
	again, err := m.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&java, again) {
		t.Errorf("Java body changed on rewrite:\n%s\n%s", prototext.Format(&java), prototext.Format(again))
	}
	bad := proto.Clone(&java).(*gen.PUserDefinedFunction)
	bad.GetUserDefinedMacroFunction().GetBody().GetAndOrValue().GetRightChild().GetBinaryRelOpValue().Operator = gen.PBinaryRelOpValue_LT_LL.Enum()
	if _, err := MacroFunctionFromProto(bad.GetUserDefinedMacroFunction()); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Errorf("an operator of another comparison was accepted: %v", err)
	}
}
