package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// DeleteExpression represents a DELETE FROM <recordType> WHERE ... It
// carries the target record-type name + the inner Quantifier
// producing the rows to delete.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.DeleteExpression`.
type DeleteExpression struct {
	inner            Quantifier
	targetRecordType string
}

// NewDeleteExpression builds a DELETE for `targetRecordType` over
// `inner`.
func NewDeleteExpression(inner Quantifier, targetRecordType string) *DeleteExpression {
	return &DeleteExpression{
		inner:            inner,
		targetRecordType: targetRecordType,
	}
}

// GetInner returns the inner Quantifier.
func (e *DeleteExpression) GetInner() Quantifier { return e.inner }

// GetTargetRecordType returns the target record-type name.
func (e *DeleteExpression) GetTargetRecordType() string { return e.targetRecordType }

// GetResultValue is the inner's flowed object value — DELETE
// passes-through the rows it deleted (lets callers chain a count or
// projection over them).
func (e *DeleteExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *DeleteExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is false.
func (e *DeleteExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (e *DeleteExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set.
func (e *DeleteExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares targetRecordType.
func (e *DeleteExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*DeleteExpression)
	if !ok {
		return false
	}
	return e.targetRecordType == o.targetRecordType
}

// HashCodeWithoutChildren mixes a class-discriminating constant with
// the target record-type name.
func (e *DeleteExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("delete|"))
	h.Write([]byte(e.targetRecordType))
	return h.Sum64()
}

func (e *DeleteExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &DeleteExpression{
		inner:            quantifiers[0],
		targetRecordType: e.targetRecordType,
	}
}

var _ RelationalExpression = (*DeleteExpression)(nil)
