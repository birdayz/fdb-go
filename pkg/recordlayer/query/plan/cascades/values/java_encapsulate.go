package values

import "fmt"

// InjectPromotion is Java's PromoteValue.inject (PromoteValue.java:441-451):
// v unchanged when it already has the type; retyped when it can result in it
// (canResultInType: an untyped NULL or a CAST, to a nullable target);
// otherwise a PromoteValue. A non-NULL literal is promoted, never retyped
// (LiteralValue.canResultInType holds only for a nullable literal type).
func InjectPromotion(v Value, target Type) Value {
	t := v.Type()
	if t != nil && t.Equals(target) {
		return v
	}
	if target.IsNullable() {
		switch vv := v.(type) {
		case *NullValue:
			if IsUnresolved(t) {
				return NewNullValue(target)
			}
		case *CastValue:
			if vv.Target != nil && WithNullability(vv.Target, true).Equals(target) {
				return &CastValue{Child: vv.Child, Target: target}
			}
		}
	}
	return NewPromoteValue(v, target)
}

// EncapsulationError is the SemanticException INCOMPATIBLE_TYPE an
// encapsulate raises.
type EncapsulationError struct{ Detail string }

func (e *EncapsulationError) Error() string { return "incompatible type: " + e.Detail }

// NewLightArrayConstructorValue is the `__internal_array` built-in,
// AbstractArrayConstructorValue.encapsulateInternal: the element type is the
// maximum of the items' types, and an item whose type differs other than in
// nullability is promoted to it (AbstractArrayConstructorValue.java:144-178).
func NewLightArrayConstructorValue(items []Value) (*ArrayConstructorValue, error) {
	if len(items) == 0 {
		return NewArrayConstructorValue(NullType, nil), nil
	}
	var elem Type
	for _, item := range items {
		if elem == nil {
			elem = item.Type()
			continue
		}
		if elem = MaximumType(elem, item.Type()); elem == nil {
			return nil, &EncapsulationError{Detail: "array elements have no common type"}
		}
	}
	promoted := make([]Value, len(items))
	for i, item := range items {
		if WithNullability(item.Type(), true).Equals(WithNullability(elem, true)) {
			promoted[i] = item
		} else {
			promoted[i] = InjectPromotion(item, elem)
		}
	}
	return NewArrayConstructorValue(elem, promoted), nil
}

// NewJavaInOpValue is InOpValue.InFn.encapsulateInternal
// (InOpValue.java:230-268): the operand or the array is promoted to the
// maximum of the operand and element types.
func NewJavaInOpValue(operand, array Value) (*InOpValue, error) {
	at, ok := array.Type().(*ArrayType)
	if !ok || at.ElementType == nil {
		if ot, isArray := operand.Type().(*ArrayType); isArray && ot.ElementType != nil {
			operand, array, at = array, operand, ot
		} else {
			return nil, &EncapsulationError{Detail: fmt.Sprintf("IN over %v", array.Type())}
		}
	}
	operandType, elemType := operand.Type(), at.ElementType
	if !IsUnresolved(elemType) && operandType.Code() != elemType.Code() {
		maximum := MaximumType(operandType, elemType)
		if maximum == nil {
			return nil, &EncapsulationError{Detail: fmt.Sprintf("%v IN %v", operandType, at)}
		}
		if !operandType.Equals(maximum) {
			return &InOpValue{Probe: InjectPromotion(operand, maximum), List: array}, nil
		}
		return &InOpValue{Probe: operand, List: InjectPromotion(array, NewArrayType(false, maximum))}, nil
	}
	if probe, isRecord := operandType.(*RecordType); isRecord {
		elem, ok := elemType.(*RecordType)
		if !ok || len(elem.Fields) != len(probe.Fields) {
			return nil, &EncapsulationError{Detail: fmt.Sprintf("%v IN %v", operandType, at)}
		}
		for i, f := range elem.Fields {
			pf := probe.Fields[i].FieldType
			if !pf.Code().IsPrimitive() || !f.FieldType.Code().IsPrimitive() || pf.Code() != f.FieldType.Code() {
				return nil, &EncapsulationError{Detail: fmt.Sprintf("%v IN %v", operandType, at)}
			}
		}
	}
	return &InOpValue{Probe: operand, List: array}, nil
}

// NewJavaPickValue is PickValue.PickValueFn.encapsulate
// (PickValue.java:235-256): the alternatives are promoted to their maximum
// type, which made nullable is the result type.
func NewJavaPickValue(selector Value, alternatives []Value) (*PickValue, error) {
	if len(alternatives) == 0 {
		return nil, &EncapsulationError{Detail: "pick without alternatives"}
	}
	maximum := alternatives[0].Type()
	for _, alt := range alternatives[1:] {
		if maximum = MaximumType(maximum, alt.Type()); maximum == nil {
			return nil, &EncapsulationError{Detail: "CASE branches have no common type"}
		}
	}
	promoted := make([]Value, len(alternatives))
	for i, alt := range alternatives {
		promoted[i] = InjectPromotion(alt, maximum)
	}
	return NewPickValue(selector, promoted, WithNullability(maximum, true)), nil
}

// TautologicalValue is the relational layer's TautologicalValue: the
// always-true implication a CASE's ELSE selects with.
type TautologicalValue struct{}

func (TautologicalValue) Children() []Value                 { return []Value{} }
func (TautologicalValue) Name() string                      { return "tautology" }
func (TautologicalValue) Type() Type                        { return NullableBoolean }
func (TautologicalValue) Evaluate(any) (any, error)         { return true, nil }
func (v TautologicalValue) WithChildrenValue([]Value) Value { return v }
func (TautologicalValue) EqualsWithoutChildrenValue(other Value) bool {
	_, ok := other.(TautologicalValue)
	return ok
}
func (TautologicalValue) SemanticHashDiscriminator() uint64 { return 0x7a07 }

// tautologicalValueTypeURL is the Any type URL TautologicalValue.toValueProto
// packs PTautologicalValue under (PlanSerialization.protoObjectToAny).
const tautologicalValueTypeURL = "c.a.fdb.types/com.apple.foundationdb.record.PTautologicalValue"
