package expressions

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// InsertExpression represents an INSERT INTO <recordType> SELECT ...
// It carries the target record-type name + the inner Quantifier
// producing the rows to insert.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.InsertExpression`.
// Java's full implementation tracks a Type.Record `targetType` derived
// from the SchemaTemplate at construction time; the seed accepts the
// targetType but doesn't validate it (validation is upstream — the
// SQL planner is expected to type-check before constructing this
// node).
type InsertExpression struct {
	inner            Quantifier
	targetRecordType string
	targetType       values.Type
}

// NewInsertExpression builds an INSERT for `targetRecordType` over
// `inner`. `targetType` is the row Type the planner expects; pass
// values.UnknownType if it isn't yet resolved.
func NewInsertExpression(inner Quantifier, targetRecordType string, targetType values.Type) *InsertExpression {
	if targetType == nil {
		targetType = values.UnknownType
	}
	return &InsertExpression{
		inner:            inner,
		targetRecordType: targetRecordType,
		targetType:       targetType,
	}
}

// GetInner returns the inner Quantifier.
func (e *InsertExpression) GetInner() Quantifier { return e.inner }

// GetTargetRecordType returns the target record-type name.
func (e *InsertExpression) GetTargetRecordType() string { return e.targetRecordType }

// GetTargetType returns the target row Type.
func (e *InsertExpression) GetTargetType() values.Type { return e.targetType }

// GetResultValue is the inner's flowed object value — INSERT
// passes-through the rows it inserted (so callers can chain a
// projection counting them, etc.).
func (e *InsertExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *InsertExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is false — INSERT has one input.
func (e *InsertExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (e *InsertExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (e *InsertExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares targetRecordType + targetType.
func (e *InsertExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*InsertExpression)
	if !ok {
		return false
	}
	if e.targetRecordType != o.targetRecordType {
		return false
	}
	return typeEquals(e.targetType, o.targetType)
}

// HashCodeWithoutChildren mixes a class-discriminating constant with
// the target record-type name.
func (e *InsertExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("insert|"))
	h.Write([]byte(e.targetRecordType))
	return h.Sum64()
}

func (e *InsertExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &InsertExpression{
		inner:            quantifiers[0],
		targetRecordType: e.targetRecordType,
		targetType:       e.targetType,
	}
}

var _ RelationalExpression = (*InsertExpression)(nil)

// typeEquals is a pragma-shim for Type equality. Each Type subtype in
// values/ has its own Equals(Type) method but the Type interface
// itself doesn't declare it. Until the interface gains Equals (B0
// follow-on), this shim type-switches.
func typeEquals(a, b values.Type) bool {
	switch ta := a.(type) {
	case *values.PrimitiveType:
		return ta.Equals(b)
	case *values.RecordType:
		return ta.Equals(b)
	case *values.ArrayType:
		return ta.Equals(b)
	case *values.EnumType:
		return ta.Equals(b)
	case *values.RelationType:
		return ta.Equals(b)
	default:
		return a == b // singleton fallback (UnknownType, NullType, NoneType, AnyType)
	}
}
