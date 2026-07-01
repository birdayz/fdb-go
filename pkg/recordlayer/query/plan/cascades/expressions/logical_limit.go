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
//
// limitValue is an OPTIONAL runtime row cap (RFC-156 parameterized vector rank
// limit): when non-nil the physical plan evaluates it at execution against the
// bound parameters and `limit` is the no-cap sentinel (-1). The limit-rewriting
// rules (no-op elimination, merge, push-through, remove-range-one) decline on a
// non-nil limitValue rather than mishandling the sentinel.
type LogicalLimitExpression struct {
	limit      int64
	offset     int64
	inner      Quantifier
	limitValue values.Value
}

func NewLogicalLimitExpression(limit, offset int64, inner Quantifier) *LogicalLimitExpression {
	return &LogicalLimitExpression{
		limit:  limit,
		offset: offset,
		inner:  inner,
	}
}

// NewRuntimeLogicalLimitExpression builds a LIMIT whose row cap is a runtime
// Value (evaluated at execution). The static limit is the no-cap sentinel (-1).
func NewRuntimeLogicalLimitExpression(limitValue values.Value, offset int64, inner Quantifier) *LogicalLimitExpression {
	return &LogicalLimitExpression{
		limit:      -1,
		offset:     offset,
		inner:      inner,
		limitValue: limitValue,
	}
}

func (e *LogicalLimitExpression) GetLimit() int64             { return e.limit }
func (e *LogicalLimitExpression) GetOffset() int64            { return e.offset }
func (e *LogicalLimitExpression) GetInner() Quantifier        { return e.inner }
func (e *LogicalLimitExpression) GetLimitValue() values.Value { return e.limitValue }
func (e *LogicalLimitExpression) CanCorrelate() bool          { return false }
func (e *LogicalLimitExpression) ChildrenAsSet() bool         { return false }

func (e *LogicalLimitExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

func (e *LogicalLimitExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

func (e *LogicalLimitExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	// A static LIMIT correlates to nothing; a runtime cap correlates to whatever
	// its Value references (a query parameter references nothing — but be precise
	// so correlation tracking stays sound if a correlated cap is ever used).
	if e.limitValue == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return values.GetCorrelatedToOfValue(e.limitValue)
}

func (e *LogicalLimitExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*LogicalLimitExpression)
	if !ok {
		return false
	}
	if (e.limitValue == nil) != (o.limitValue == nil) {
		return false
	}
	if e.limitValue != nil && !values.ValuesStructurallyEqual(e.limitValue, o.limitValue) {
		return false
	}
	return e.limit == o.limit && e.offset == o.offset
}

func (e *LogicalLimitExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("limit|"))
	writeInt64(h, e.limit)
	writeInt64(h, e.offset)
	if e.limitValue != nil {
		writeInt64(h, int64(values.SemanticHashCode(e.limitValue)))
	}
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
		inner:      quantifiers[0],
		limit:      e.limit,
		offset:     e.offset,
		limitValue: e.limitValue,
	}
}

var _ RelationalExpression = (*LogicalLimitExpression)(nil)
