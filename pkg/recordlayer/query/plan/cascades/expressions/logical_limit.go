package expressions

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalLimitExpression caps the row count to Limit after skipping
// Offset rows from the inner stream. Mirrors Java's "fetch" expression
// (LIMIT/OFFSET in SQL).
//
// Negative Limit means no cap (pure offset); zero Offset means no skip.
type LogicalLimitExpression struct {
	limit  int64
	offset int64
	inner  Quantifier
}

func NewLogicalLimitExpression(limit, offset int64, inner Quantifier) *LogicalLimitExpression {
	return &LogicalLimitExpression{
		limit:  limit,
		offset: offset,
		inner:  inner,
	}
}

func (e *LogicalLimitExpression) GetLimit() int64      { return e.limit }
func (e *LogicalLimitExpression) GetOffset() int64     { return e.offset }
func (e *LogicalLimitExpression) GetInner() Quantifier { return e.inner }
func (e *LogicalLimitExpression) CanCorrelate() bool   { return false }
func (e *LogicalLimitExpression) ChildrenAsSet() bool  { return false }

func (e *LogicalLimitExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

func (e *LogicalLimitExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

func (e *LogicalLimitExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (e *LogicalLimitExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*LogicalLimitExpression)
	if !ok {
		return false
	}
	return e.limit == o.limit && e.offset == o.offset
}

func (e *LogicalLimitExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("limit|"))
	writeInt64(h, e.limit)
	writeInt64(h, e.offset)
	return h.Sum64()
}

func writeInt64(h interface{ Write([]byte) (int, error) }, v int64) {
	b := [8]byte{
		byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56),
	}
	h.Write(b[:])
}

func (e *LogicalLimitExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalLimitExpression{
		inner:  quantifiers[0],
		limit:  e.limit,
		offset: e.offset,
	}
}

var _ RelationalExpression = (*LogicalLimitExpression)(nil)
