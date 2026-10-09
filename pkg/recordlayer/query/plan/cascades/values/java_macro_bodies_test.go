package values

import (
	"testing"

	"fdb.dev/gen"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// Macro bodies exactly as the 4.14.2.0 target stores them (captured from its
// metadata by the WSHBooleanMacroConformance spec) read back and rewrite
// byte-identical: InOpValue over a promoted array (ArrayCoercionBiFunction
// trie), PickValue over a ConditionSelectorValue ending in the packed
// PTautologicalValue, and a NULL-typed literal escape.
func TestJavaMacroBodiesRoundTrip(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"IN (1, 2, 3) over BIGINT: the array promoted INT to LONG": `in_op_value: { probe_value: { quantified_object_value: { alias: "c000000000000000000000000000000000000" result_type: { primitive_type: { type_code: LONG is_nullable: true } } } } in_array_value: { promote_value: { in_value: { light_array_constructor_value: { super: { children: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 1 } } } } children: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 2 } } } } children: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 3 } } } } element_type: { primitive_type: { type_code: INT is_nullable: false } } } } } promote_to_type: { array_type: { is_nullable: false element_type: { primitive_type: { type_code: LONG is_nullable: true } } } } promotion_trie: { children_map_is_null: false child_pair: { index: -1 child_coercion_trie_node: { children_map_is_null: true value: { primitive_coercion_bi_function: { operator: INT_TO_LONG } } } } value: { array_coercion_bi_function: { from_array_type: { array_type: { is_nullable: false element_type: { primitive_type: { type_code: INT is_nullable: false } } } } to_array_type: { array_type: { is_nullable: false element_type: { primitive_type: { type_code: LONG is_nullable: true } } } } elements_trie: { children_map_is_null: true value: { primitive_coercion_bi_function: { operator: INT_TO_LONG } } } } } } } } }`,
		"CASE WHEN a > 1 THEN 'x' ELSE 'y' END":                    `pick_value: { selector_value: { condition_selector_value: { implications: { binary_rel_op_value: { super: { function_name: "gt" comparison_type: GREATER_THAN children: { quantified_object_value: { alias: "c000000000000000000000000000000000000" result_type: { primitive_type: { type_code: LONG is_nullable: true } } } } children: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 1 } } } } } operator: GT_LI } } implications: { additional_values: { type_url: "c.a.fdb.types/com.apple.foundationdb.record.PTautologicalValue" } } } } alternative_values: { literal_value: { result_type: { primitive_type: { type_code: STRING is_nullable: false } } value: { primitive_object: { string_value: "x" } } } } alternative_values: { literal_value: { result_type: { primitive_type: { type_code: STRING is_nullable: false } } value: { primitive_object: { string_value: "y" } } } } result_type: { primitive_type: { type_code: STRING is_nullable: true } } }`,
		"LIKE 'a%' without ESCAPE":                                 `like_operator_value: { src_child: { quantified_object_value: { alias: "c000000000000000000000000000000000000" result_type: { primitive_type: { type_code: STRING is_nullable: true } } } } pattern_child: { pattern_for_like_value: { pattern_child: { literal_value: { result_type: { primitive_type: { type_code: STRING is_nullable: false } } value: { primitive_object: { string_value: "a%" } } } } escape_child: { literal_value: { result_type: { null_type: {} } value: { primitive_object: {} } } } } } }`,
		"CASE WHEN b THEN a ELSE 5 END":                            `pick_value: { selector_value: { condition_selector_value: { implications: { quantified_object_value: { alias: "c000000000000000000000000000000000001" result_type: { primitive_type: { type_code: BOOLEAN is_nullable: true } } } } implications: { additional_values: { type_url: "c.a.fdb.types/com.apple.foundationdb.record.PTautologicalValue" } } } } alternative_values: { quantified_object_value: { alias: "c000000000000000000000000000000000000" result_type: { primitive_type: { type_code: LONG is_nullable: true } } } } alternative_values: { promote_value: { in_value: { literal_value: { result_type: { primitive_type: { type_code: INT is_nullable: false } } value: { primitive_object: { int_value: 5 } } } } promote_to_type: { primitive_type: { type_code: LONG is_nullable: true } } promotion_trie: { children_map_is_null: true value: { primitive_coercion_bi_function: { operator: INT_TO_LONG } } } } } result_type: { primitive_type: { type_code: LONG is_nullable: true } } }`,
	} {
		var java gen.PValue
		if err := prototext.Unmarshal([]byte(text), &java); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v, err := NewSerializationContext().ValueFromProto(&java)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, err := NewSerializationContext().ValueToProto(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&java)
		got, _ := proto.MarshalOptions{Deterministic: true}.Marshal(again)
		if string(want) != string(got) {
			t.Errorf("%s changed on rewrite:\n%s\n%s", name, prototext.Format(&java), prototext.Format(again))
		}
	}
}

// PromoteValue.inject: a NULL of no type is retyped, a non-NULL literal is
// promoted even when only its nullability differs (LiteralValue
// .canResultInType).
func TestInjectPromotion(t *testing.T) {
	t.Parallel()
	lit := &ConstantValue{Value: int64(5), Typ: NotNullInt}
	if p, ok := InjectPromotion(lit, NullableInt).(*PromoteValue); !ok || !p.Type().Equals(NullableInt) {
		t.Errorf("literal to nullable = %#v", p)
	}
	if v := InjectPromotion(lit, NotNullInt); v != lit {
		t.Errorf("same type was not kept")
	}
	if n, ok := InjectPromotion(NewNullValue(NullType), NullableLong).(*NullValue); !ok || !n.Type().Equals(NullableLong) {
		t.Errorf("untyped NULL to LONG = %#v", n)
	}
}
