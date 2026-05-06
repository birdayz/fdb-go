package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalIntersectionExpression represents the bag-intersection of its
// N children. Same children-as-set semantics as LogicalUnion — see
// that file for the permutation-equality caveat.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalIntersectionExpression`.
// Java's intersection requires a `comparisonKeyValues` argument that
// pins how rows are compared for set-intersection semantics. We
// capture it as a slice of Values; the actual evaluation semantics
// (which physical operator implements the comparison) lands when
// Track C / B5 Batch B+ needs it.
type LogicalIntersectionExpression struct {
	quantifiers         []Quantifier
	comparisonKeyValues []values.Value
}

// NewLogicalIntersectionExpression builds an N-way intersection.
// `comparisonKeyValues` defines the row-equality key (typically the
// primary-key columns of the result type). Both lists are copied.
func NewLogicalIntersectionExpression(quantifiers []Quantifier, comparisonKeyValues []values.Value) *LogicalIntersectionExpression {
	copiedQ := make([]Quantifier, len(quantifiers))
	copy(copiedQ, quantifiers)
	copiedK := make([]values.Value, len(comparisonKeyValues))
	copy(copiedK, comparisonKeyValues)
	return &LogicalIntersectionExpression{
		quantifiers:         copiedQ,
		comparisonKeyValues: copiedK,
	}
}

// GetComparisonKeyValues returns the row-equality key list. Read-only.
func (e *LogicalIntersectionExpression) GetComparisonKeyValues() []values.Value {
	return e.comparisonKeyValues
}

// GetResultValue approximates by returning the first child's flowed
// object — same caveat as LogicalUnion.
func (e *LogicalIntersectionExpression) GetResultValue() values.Value {
	if len(e.quantifiers) == 0 {
		return values.NewNullValue(values.UnknownType)
	}
	return e.quantifiers[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the children.
func (e *LogicalIntersectionExpression) GetQuantifiers() []Quantifier { return e.quantifiers }

// CanCorrelate is false — same reasoning as Union.
func (e *LogicalIntersectionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is true — INTERSECTION is commutative.
func (e *LogicalIntersectionExpression) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the union of correlation
// sets across the comparison-key Values. The keys are typically
// FieldValue references that carry the alias of the operator their
// row stream comes from.
func (e *LogicalIntersectionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, v := range e.comparisonKeyValues {
		for k := range values.GetCorrelatedToOfValue(v) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares classes AND comparison-key lists.
// Two intersections with different comparison keys are not equal even
// over the same children.
func (e *LogicalIntersectionExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*LogicalIntersectionExpression)
	if !ok {
		return false
	}
	if len(e.comparisonKeyValues) != len(o.comparisonKeyValues) {
		return false
	}
	for i := range e.comparisonKeyValues {
		if values.ExplainValue(e.comparisonKeyValues[i]) != values.ExplainValue(o.comparisonKeyValues[i]) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes a class-discriminating constant with
// the comparison-key Explain text.
func (e *LogicalIntersectionExpression) HashCodeWithoutChildren() uint64 {
	const seed uint64 = 41
	h := seed
	for _, v := range e.comparisonKeyValues {
		s := values.ExplainValue(v)
		for i := 0; i < len(s); i++ {
			h = h*31 + uint64(s[i])
		}
		h = h*31 + 0xff
	}
	return h
}

func (e *LogicalIntersectionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	copied := make([]Quantifier, len(quantifiers))
	copy(copied, quantifiers)
	return &LogicalIntersectionExpression{
		quantifiers:         copied,
		comparisonKeyValues: e.comparisonKeyValues,
	}
}

var _ RelationalExpression = (*LogicalIntersectionExpression)(nil)
