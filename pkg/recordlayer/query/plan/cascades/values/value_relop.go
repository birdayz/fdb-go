// Portions derived from FoundationDB Record Layer (RelOpValue.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/gen"
)

// RelOpComparison is the Comparisons.Type of a RelOpValue.
type RelOpComparison int

const (
	relOpEquals RelOpComparison = iota + 1
	relOpNotEquals
	relOpLessThan
	relOpLessThanOrEquals
	relOpGreaterThan
	relOpGreaterThanOrEquals
	relOpIsDistinctFrom
	relOpNotDistinctFrom
	relOpIsNull
	relOpNotNull
)

// The comparisons a RelOpValue carries.
const (
	RelOpEquals              = relOpEquals
	RelOpNotEquals           = relOpNotEquals
	RelOpLessThan            = relOpLessThan
	RelOpLessThanOrEquals    = relOpLessThanOrEquals
	RelOpGreaterThan         = relOpGreaterThan
	RelOpGreaterThanOrEquals = relOpGreaterThanOrEquals
	RelOpIsDistinctFrom      = relOpIsDistinctFrom
	RelOpNotDistinctFrom     = relOpNotDistinctFrom
	RelOpIsNull              = relOpIsNull
	RelOpNotNull             = relOpNotNull
)

var relOpComparisonProto = map[RelOpComparison]gen.PComparison_PComparisonType{
	relOpEquals: gen.PComparison_EQUALS, relOpNotEquals: gen.PComparison_NOT_EQUALS,
	relOpLessThan: gen.PComparison_LESS_THAN, relOpLessThanOrEquals: gen.PComparison_LESS_THAN_OR_EQUALS,
	relOpGreaterThan: gen.PComparison_GREATER_THAN, relOpGreaterThanOrEquals: gen.PComparison_GREATER_THAN_OR_EQUALS,
	relOpIsDistinctFrom: gen.PComparison_IS_DISTINCT_FROM, relOpNotDistinctFrom: gen.PComparison_NOT_DISTINCT_FROM,
	relOpIsNull: gen.PComparison_IS_NULL, relOpNotNull: gen.PComparison_NOT_NULL,
}

// relOpFunctionNames are the BuiltInFunction names the SQL operators resolve
// to (SqlFunctionCatalogImpl), which PRelOpValue.function_name records.
var relOpFunctionNames = map[RelOpComparison]string{
	relOpEquals: "equals", relOpNotEquals: "notEquals", relOpLessThan: "lt", relOpLessThanOrEquals: "lte",
	relOpGreaterThan: "gt", relOpGreaterThanOrEquals: "gte", relOpIsDistinctFrom: "isDistinctFrom",
	relOpNotDistinctFrom: "notDistinctFrom", relOpIsNull: "isNull", relOpNotNull: "notNull",
}

type relOpEvalKind int

const (
	relOpEvalNull relOpEvalKind = iota
	relOpEvalTrue
	relOpEvalFalse
	relOpEvalCompare
)

type binaryRelOpOperator struct {
	proto       gen.PBinaryRelOpValue_PBinaryPhysicalOperator
	comparison  RelOpComparison
	left, right TypeCode
	eval        relOpEvalKind
}

type unaryRelOpOperator struct {
	proto      gen.PUnaryRelOpValue_PUnaryPhysicalOperator
	comparison RelOpComparison
	arg        TypeCode
}

type binaryRelOpSignature struct {
	comparison  RelOpComparison
	left, right TypeCode
}

type unaryRelOpSignature struct {
	comparison RelOpComparison
	arg        TypeCode
}

var (
	binaryRelOpBySignature = map[binaryRelOpSignature]int{}
	binaryRelOpByProto     = map[gen.PBinaryRelOpValue_PBinaryPhysicalOperator]int{}
	unaryRelOpBySignature  = map[unaryRelOpSignature]int{}
	unaryRelOpByProto      = map[gen.PUnaryRelOpValue_PUnaryPhysicalOperator]int{}
)

func init() {
	for i, op := range binaryRelOpOperators {
		binaryRelOpBySignature[binaryRelOpSignature{op.comparison, op.left, op.right}] = i
		binaryRelOpByProto[op.proto] = i
	}
	for i, op := range unaryRelOpOperators {
		unaryRelOpBySignature[unaryRelOpSignature{op.comparison, op.arg}] = i
		unaryRelOpByProto[op.proto] = i
	}
}

// RelOpError is the SemanticException RelOpValue.encapsulate raises.
type RelOpError struct {
	// Complex is COMPARAND_TO_COMPARISON_IS_OF_COMPLEX_TYPE; otherwise
	// COMPARISON_OF_INCOMPATIBLE_TYPES.
	Complex bool
	Detail  string
}

func (e *RelOpError) Error() string {
	if e.Complex {
		return "comparand to comparison is of complex type: " + e.Detail
	}
	return "comparison of incompatible types: " + e.Detail
}

// relOpEvaluator evaluates a comparison over two runtime values exactly as
// the predicate layer evaluates the same comparison; the predicates package
// installs it (values cannot import predicates).
var relOpEvaluator func(comparison RelOpComparison, left, right any) (any, error)

// SetRelOpEvaluator installs the comparison evaluation RelOpValues use.
func SetRelOpEvaluator(f func(comparison RelOpComparison, left, right any) (any, error)) {
	relOpEvaluator = f
}

// isSupportedRelOpOperandType is RelOpValue.isSupportedOperandType.
func isSupportedRelOpOperandType(t Type) bool {
	if t == nil {
		return false
	}
	switch t.Code() {
	case TypeCodeEnum, TypeCodeUuid, TypeCodeArray, TypeCodeNone:
		return true
	}
	return t.Code().IsPrimitive() || t.Code() == TypeCodeUnknown || t.Code() == TypeCodeNull
}

func relOpCode(t Type) TypeCode {
	if t == nil {
		return TypeCodeUnknown
	}
	return t.Code()
}

// BinaryRelOpValue is Java's RelOpValue.BinaryRelOpValue: a binary
// comparison as a nullable boolean value, typed by its physical operator.
type BinaryRelOpValue struct {
	FunctionName string
	Comparison   RelOpComparison
	Left, Right  Value
	operator     int
}

// UnaryRelOpValue is Java's RelOpValue.UnaryRelOpValue: IS [NOT] NULL as a
// boolean value.
type UnaryRelOpValue struct {
	FunctionName string
	Comparison   RelOpComparison
	Child        Value
	operator     int
}

// NewBinaryRelOpValue is RelOpValue.encapsulate for two arguments
// (RelOpValue.java:340-382).
func NewBinaryRelOpValue(comparison RelOpComparison, left, right Value) (*BinaryRelOpValue, error) {
	lt, rt := left.Type(), right.Type()
	if !isSupportedRelOpOperandType(lt) || !isSupportedRelOpOperandType(rt) {
		return nil, &RelOpError{Complex: true, Detail: fmt.Sprintf("%v, %v", lt, rt)}
	}
	isArray := relOpCode(lt) == TypeCodeArray || relOpCode(rt) == TypeCodeArray
	if isArray && (relOpCode(lt) == TypeCodeNone || relOpCode(rt) == TypeCodeNone ||
		relOpCode(lt) == TypeCodeNull || relOpCode(rt) == TypeCodeNull) {
		maximum := MaximumType(lt, rt)
		if maximum == nil {
			return nil, &RelOpError{Detail: fmt.Sprintf("left type: %v, right type: %v", lt, rt)}
		}
		if !lt.Equals(maximum) {
			left = NewPromoteValue(left, maximum)
		}
		if !rt.Equals(maximum) {
			right = NewPromoteValue(right, maximum)
		}
		lt, rt = left.Type(), right.Type()
	}
	if isArray && !WithNullability(lt, false).Equals(WithNullability(rt, false)) {
		return nil, &RelOpError{Detail: fmt.Sprintf("%v, %v", lt, rt)}
	}
	op, ok := binaryRelOpBySignature[binaryRelOpSignature{comparison, relOpCode(lt), relOpCode(rt)}]
	if !ok {
		return nil, &RelOpError{Detail: fmt.Sprintf("%v, %v", lt, rt)}
	}
	return &BinaryRelOpValue{FunctionName: relOpFunctionNames[comparison], Comparison: comparison, Left: left, Right: right, operator: op}, nil
}

// NewUnaryRelOpValue is RelOpValue.encapsulate for one argument
// (RelOpValue.java:328-339).
func NewUnaryRelOpValue(comparison RelOpComparison, child Value) (*UnaryRelOpValue, error) {
	t := child.Type()
	if !isSupportedRelOpOperandType(t) {
		return nil, &RelOpError{Complex: true, Detail: fmt.Sprint(t)}
	}
	op, ok := unaryRelOpBySignature[unaryRelOpSignature{comparison, relOpCode(t)}]
	if !ok {
		return nil, &RelOpError{Detail: fmt.Sprint(t)}
	}
	return &UnaryRelOpValue{FunctionName: relOpFunctionNames[comparison], Comparison: comparison, Child: child, operator: op}, nil
}

func (v *BinaryRelOpValue) Children() []Value { return []Value{v.Left, v.Right} }
func (v *BinaryRelOpValue) Name() string      { return v.FunctionName }

// Type is BooleanValue.getResultType: nullable BOOLEAN.
func (*BinaryRelOpValue) Type() Type { return NullableBoolean }

// Evaluate is BinaryPhysicalOperator.eval: a NULL operand makes the result
// NULL except for the null-safe comparisons; otherwise the operator decides.
func (v *BinaryRelOpValue) Evaluate(evalCtx any) (any, error) {
	l, err := v.Left.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	r, err := v.Right.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if l == nil || r == nil {
		switch v.Comparison {
		case relOpIsDistinctFrom:
			return l != nil || r != nil, nil
		case relOpNotDistinctFrom:
			return l == nil && r == nil, nil
		}
		return nil, nil
	}
	op := binaryRelOpOperators[v.operator]
	switch op.eval {
	case relOpEvalNull:
		return nil, nil
	case relOpEvalTrue:
		return true, nil
	case relOpEvalFalse:
		return false, nil
	}
	// ENUM/UUID against STRING promotes the string (stringToEnumValue,
	// stringToUuidValue).
	if op.left == TypeCodeString && op.right != TypeCodeString {
		if l, err = relOpStringOperand(l, v.Right.Type()); err != nil {
			return nil, err
		}
	} else if op.right == TypeCodeString && op.left != TypeCodeString {
		if r, err = relOpStringOperand(r, v.Left.Type()); err != nil {
			return nil, err
		}
	}
	if relOpEvaluator == nil {
		return nil, fmt.Errorf("RelOpValue: no comparison evaluator installed")
	}
	return relOpEvaluator(v.Comparison, l, r)
}

// relOpStringOperand is the string side of an ENUM/UUID-against-STRING
// operator read as the other side's type.
func relOpStringOperand(s any, other Type) (any, error) {
	str, ok := s.(string)
	if !ok {
		return s, nil
	}
	switch t := other.(type) {
	case *EnumType:
		return stringToEnumValue(t, str)
	}
	if other != nil && other.Code() == TypeCodeUuid {
		u, ok := ParseJavaUUID(str)
		if !ok {
			return nil, &InvalidUUIDValueError{Value: str}
		}
		return u, nil
	}
	return s, nil
}

// PhysicalOperator is the operator's proto enum.
func (v *BinaryRelOpValue) PhysicalOperator() gen.PBinaryRelOpValue_PBinaryPhysicalOperator {
	return binaryRelOpOperators[v.operator].proto
}

func (v *BinaryRelOpValue) WithChildrenValue(newChildren []Value) Value {
	cp := *v
	cp.Left, cp.Right = newChildren[0], newChildren[1]
	return &cp
}

func (v *BinaryRelOpValue) EqualsWithoutChildrenValue(other Value) bool {
	o, ok := other.(*BinaryRelOpValue)
	return ok && o.Comparison == v.Comparison && o.operator == v.operator
}

func (v *BinaryRelOpValue) SemanticHashDiscriminator() uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "binrelop:%d:%d", v.Comparison, v.operator)
	return h.Sum64()
}

func (v *UnaryRelOpValue) Children() []Value { return []Value{v.Child} }
func (v *UnaryRelOpValue) Name() string      { return v.FunctionName }

// Type is BooleanValue.getResultType: nullable BOOLEAN.
func (*UnaryRelOpValue) Type() Type { return NullableBoolean }

// Evaluate is UnaryPhysicalOperator.eval: Objects.isNull / Objects.nonNull.
func (v *UnaryRelOpValue) Evaluate(evalCtx any) (any, error) {
	c, err := v.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if v.Comparison == relOpIsNull {
		return c == nil, nil
	}
	return c != nil, nil
}

// PhysicalOperator is the operator's proto enum.
func (v *UnaryRelOpValue) PhysicalOperator() gen.PUnaryRelOpValue_PUnaryPhysicalOperator {
	return unaryRelOpOperators[v.operator].proto
}

func (v *UnaryRelOpValue) WithChildrenValue(newChildren []Value) Value {
	cp := *v
	cp.Child = newChildren[0]
	return &cp
}

func (v *UnaryRelOpValue) EqualsWithoutChildrenValue(other Value) bool {
	o, ok := other.(*UnaryRelOpValue)
	return ok && o.Comparison == v.Comparison && o.operator == v.operator
}

func (v *UnaryRelOpValue) SemanticHashDiscriminator() uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "unrelop:%d:%d", v.Comparison, v.operator)
	return h.Sum64()
}

var (
	_ SelfWithChildren          = (*BinaryRelOpValue)(nil)
	_ SelfEqualsWithoutChildren = (*BinaryRelOpValue)(nil)
	_ SelfSemanticHash          = (*BinaryRelOpValue)(nil)
	_ SelfWithChildren          = (*UnaryRelOpValue)(nil)
	_ SelfEqualsWithoutChildren = (*UnaryRelOpValue)(nil)
	_ SelfSemanticHash          = (*UnaryRelOpValue)(nil)
)
