package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TableFunctionExpression is a table-function expression that
// delegates row-stream production to an underlying streaming Value
// (typically RangeValue, but any Value semantically capable of
// producing a row stream qualifies). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.TableFunctionExpression`.
//
// Conceptually: SQL TABLE(range(0, 100)). The streaming Value
// produces rows; the TableFunctionExpression wraps it as a
// RelationalExpression so the rest of the planner can reason about
// it (apply Filter, Distinct, Sort, etc., over the produced rows).
//
// Distinction from ExplodeExpression:
//
//   - ExplodeExpression: takes a Value typed as ARRAY and produces
//     a row-per-element stream. Sugar for SQL UNNEST(array).
//   - TableFunctionExpression: takes any streaming-capable Value
//     (RangeValue, future StreamingAggregateValue, etc.) and
//     delegates row production to it. Sugar for SQL
//     TABLE(streaming_func()).
//
// Both are leaf-shaped (no Quantifier children); both forward
// correlations from their wrapped Value.
//
// Java's TableFunctionExpression takes a `StreamingValue`
// (interface marker). The Go port accepts `Value` directly —
// caller is expected to pass a streaming-capable Value (RangeValue
// today). The seed defers the type-check to caller; mismatched
// callers surface as a degenerate result type.
//
// Result type: a QueriedValue typed at the streaming Value's
// result type (Java does the same).
type TableFunctionExpression struct {
	streamValue values.Value
}

// NewTableFunctionExpression builds a TableFunction over the given
// streaming Value.
func NewTableFunctionExpression(stream values.Value) *TableFunctionExpression {
	return &TableFunctionExpression{streamValue: stream}
}

// GetValue returns the wrapped streaming Value.
func (e *TableFunctionExpression) GetValue() values.Value {
	return e.streamValue
}

// GetResultValue returns a QueriedValue typed at the streaming
// Value's result type.
func (e *TableFunctionExpression) GetResultValue() values.Value {
	if e.streamValue == nil {
		return values.NewQueriedValue(nil, values.UnknownType)
	}
	return values.NewQueriedValue(nil, e.streamValue.Type())
}

// GetQuantifiers returns the empty slice — leaf-shaped.
func (*TableFunctionExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{}
}

// CanCorrelate is false — the streaming Value's correlations are
// surfaced via GetCorrelatedToWithoutChildren, but TableFunction
// itself doesn't introduce a new correlation scope.
func (*TableFunctionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — no children.
func (*TableFunctionExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the streaming Value's
// correlation set. Mirrors Java's `value.getCorrelatedTo()`.
func (e *TableFunctionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if e.streamValue == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return values.GetCorrelatedToOfValue(e.streamValue)
}

// EqualsWithoutChildren is true iff `other` is a
// TableFunctionExpression AND its streamValue is pointer-equal to
// ours.
//
// The seed conservatively requires pointer equality. A previous
// version used a Name() fallback which produces false positives
// for Values whose Name() returns a class-discriminating constant
// (e.g. *FieldValue returns "field" for all instances). Same fix
// as ExplodeExpression — when SemanticEquals over Values is
// ported as a free function, this can broaden.
func (e *TableFunctionExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*TableFunctionExpression)
	if !ok {
		return false
	}
	return e.streamValue == o.streamValue
}

// HashCodeWithoutChildren mixes class discriminator + the streaming
// Value's name.
func (e *TableFunctionExpression) HashCodeWithoutChildren() uint64 {
	const classDisc uint64 = 0xAB1EF
	if e.streamValue == nil {
		return classDisc
	}
	var h uint64 = classDisc
	for _, b := range []byte(e.streamValue.Name()) {
		h = h*31 + uint64(b)
	}
	return h
}

func (e *TableFunctionExpression) WithQuantifiers(_ []Quantifier) RelationalExpression {
	return e
}

var _ RelationalExpression = (*TableFunctionExpression)(nil)
