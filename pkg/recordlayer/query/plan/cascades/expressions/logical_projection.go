package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalProjectionExpression represents the projection list of a SELECT.
// The set of Values it captures determines what columns flow upward;
// rows themselves are passed through from the inner Quantifier.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalProjectionExpression`.
//
// Note: Java's GetResultValue returns the inner's flowed object value,
// NOT a RecordConstructor over the projection list. The projection list
// is captured separately and consumed by upstream rules (the projection
// is implemented physically by a RecordQueryMapPlan that pulls the
// projected values out of the inner). We preserve that contract here.
type LogicalProjectionExpression struct {
	projectedValues []values.Value
	aliases         []string
	inner           Quantifier
}

// NewLogicalProjectionExpression constructs a projection over `inner`
// emitting the given Value list. The list is copied defensively.
func NewLogicalProjectionExpression(projectedValues []values.Value, inner Quantifier) *LogicalProjectionExpression {
	copied := make([]values.Value, len(projectedValues))
	copy(copied, projectedValues)
	return &LogicalProjectionExpression{
		projectedValues: copied,
		inner:           inner,
	}
}

// NewLogicalProjectionExpressionWithAliases includes output column aliases.
func NewLogicalProjectionExpressionWithAliases(projectedValues []values.Value, aliases []string, inner Quantifier) *LogicalProjectionExpression {
	copied := make([]values.Value, len(projectedValues))
	copy(copied, projectedValues)
	return &LogicalProjectionExpression{
		projectedValues: copied,
		aliases:         aliases,
		inner:           inner,
	}
}

// GetProjectedValues returns the projection list. Read-only.
func (e *LogicalProjectionExpression) GetProjectedValues() []values.Value {
	return e.projectedValues
}

// GetAliases returns the output column aliases (parallel to projectedValues).
func (e *LogicalProjectionExpression) GetAliases() []string {
	return e.aliases
}

// GetInner returns the inner Quantifier.
func (e *LogicalProjectionExpression) GetInner() Quantifier { return e.inner }

// GetResultValue passes the inner's flowed object value through —
// matches Java's contract.
func (e *LogicalProjectionExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalProjectionExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false for a projection — single child.
func (e *LogicalProjectionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalProjectionExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren is the union of correlation sets
// across the projection list.
func (e *LogicalProjectionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, v := range e.projectedValues {
		for k := range values.GetCorrelatedToOfValue(v) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares two projections by projection-list
// equality. Two Values are considered equal if their ExplainValue
// renderings match — bridges the gap until Value gains a real
// SemanticEquals (which is the Track B2 follow-on).
func (e *LogicalProjectionExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalProjectionExpression)
	if !ok {
		return false
	}
	_ = aliases
	if len(e.projectedValues) != len(o.projectedValues) {
		return false
	}
	for i := range e.projectedValues {
		if values.ExplainValue(e.projectedValues[i]) != values.ExplainValue(o.projectedValues[i]) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the projection list via Explain text.
func (e *LogicalProjectionExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	for _, v := range e.projectedValues {
		h.Write([]byte(values.ExplainValue(v)))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *LogicalProjectionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalProjectionExpression{
		inner:           quantifiers[0],
		projectedValues: e.projectedValues,
		aliases:         e.aliases,
	}
}

var _ RelationalExpression = (*LogicalProjectionExpression)(nil)
