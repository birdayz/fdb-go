package expressions

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// GetResultValue is the inner's flowed object value — DELETE passes through the
// rows it deleted (lets callers chain a count or projection over them).
//
// It stays TYPED, and it is the DML site that does. Java's
// DeleteExpression.java:62 is literally `inner.getFlowedObjectValue()`, which in
// Java is `QuantifiedObjectValue.of(alias, getFlowedObjectType())`
// (Quantifier.java:801-803) — typed. So this is not a passthrough Go invented and
// then typed by accident; it is Java's own answer, and a DELETE really does flow
// the rows it deleted.
//
// Its siblings do not, and the difference is easy to lose. INSERT flows the TARGET
// row (`new QueriedValue(targetType)`, InsertExpression.java:71) and UPDATE flows
// an OLD/NEW pair (UpdateExpression.java:84, :209-213). Both Go sites therefore
// state no type until they can state the right one; this one states the inner's
// because that is the right one. Pinned by
// TestDeleteResultValueStatesTheInnerRow so the three are not "made consistent"
// in either direction.
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
	cp := *e
	cp.inner = quantifiers[0]
	return &cp
}

var _ RelationalExpression = (*DeleteExpression)(nil)
