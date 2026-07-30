package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalIntersectionExpression represents the bag-intersection of its
// N children. Same children-as-set semantics as LogicalUnion — see
// that file for the permutation-equality caveat.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalIntersectionExpression`.
// Java's intersection requires a `comparisonKeyValues` argument that
// pins how rows are compared for set-intersection semantics. We
// capture it as a slice of Values; it carries through unchanged into
// the physical RecordQueryIntersectionPlan (ImplementIntersectionRule),
// which implements the row-equality comparison.
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

// GetResultValue returns the first non-existential child's flowed object value.
// Same source as LogicalUnion and the same disposition: Java's
// LogicalIntersectionExpression.java:75 is `mergeValues(quantifiers)`, whose
// result TYPE is the first non-existential quantifier's flowed object type
// (RecordQuerySetPlan.java:252-261). Child 0's row is Java's stated row, so this
// site stays TYPED — see LogicalUnionExpression.GetResultValue for why the
// mergeValues citation does not mean what it looks like it means, and for the
// DerivedValue difference that is real. The shared selection, including why the
// existential filter is worth carrying while it cannot fire, is
// setOperationResultValue.
func (e *LogicalIntersectionExpression) GetResultValue() values.Value {
	return setOperationResultValue(e.quantifiers)
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
func (e *LogicalIntersectionExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalIntersectionExpression)
	if !ok {
		return false
	}
	if len(e.comparisonKeyValues) != len(o.comparisonKeyValues) {
		return false
	}
	// Alias-aware comparison-key equality (RFC-040 040.2). Inert under the
	// memo's empty-alias path until PR-A.
	vm := aliases.ToValuesAliasMap()
	for i := range e.comparisonKeyValues {
		if !values.SemanticEqualsUnderAliasMap(e.comparisonKeyValues[i], o.comparisonKeyValues[i], vm) {
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
		// Alias-invariant (RFC-040 040.2), consistent with the alias-aware
		// EqualsWithoutChildren above.
		h = h*31 + values.SemanticHashCode(v)
		h = h*31 + 0xff
	}
	return h
}

func (e *LogicalIntersectionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	copied := make([]Quantifier, len(quantifiers))
	copy(copied, quantifiers)
	cp := *e
	cp.quantifiers = copied
	return &cp
}

var _ RelationalExpression = (*LogicalIntersectionExpression)(nil)
