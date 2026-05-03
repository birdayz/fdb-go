package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalUnionExpression represents the bag-union (UNION ALL) of its N
// children. Java's class is marked `ChildrenAsSet` — the planner is
// free to permute children freely and a permutation-aware equality
// check is required to keep semantic-equality correct under reordering.
// The seed's positional SemanticEquals does NOT enumerate
// permutations; equality of two unions over the same children but in
// different orders returns false. The full alias-permutation matcher
// lands alongside MatchableSortExpression / B2 follow-on.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalUnionExpression`.
// Java's GetResultValue is a `RecordQuerySetPlan.mergeValues(children)`
// reduction that picks a unified row-shape Value across children. We
// approximate by returning the first child's flowed object value —
// good enough for seed shape inspection. Real merge lands when the
// physical executor (Track C) needs to materialise the union and
// row-shape compatibility actually has to be enforced.
type LogicalUnionExpression struct {
	quantifiers []Quantifier
}

// NewLogicalUnionExpression builds a union of N children. Children
// list is copied. Empty children list is allowed but pathological —
// callers should use a no-op scan instead.
func NewLogicalUnionExpression(quantifiers []Quantifier) *LogicalUnionExpression {
	copied := make([]Quantifier, len(quantifiers))
	copy(copied, quantifiers)
	return &LogicalUnionExpression{quantifiers: copied}
}

// GetResultValue approximates Java's mergeValues — see type doc.
func (e *LogicalUnionExpression) GetResultValue() values.Value {
	if len(e.quantifiers) == 0 {
		return values.NewNullValue(values.UnknownType)
	}
	return e.quantifiers[0].GetFlowedObjectValue()
}

// GetQuantifiers returns the children. Read-only.
func (e *LogicalUnionExpression) GetQuantifiers() []Quantifier { return e.quantifiers }

// CanCorrelate is false — by SQL semantics, evaluating one branch of
// a UNION cannot bind values seen by another. Java's behaviour
// matches. This is the discriminating flag versus Select-shaped
// expressions.
func (e *LogicalUnionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is true — Java marks LogicalUnion as
// ChildrenAsSet, since UNION is commutative.
func (e *LogicalUnionExpression) ChildrenAsSet() bool { return true }

// GetCorrelatedToWithoutChildren returns the empty set (Java behaviour).
func (e *LogicalUnionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren is true iff `other` is a LogicalUnion. Union
// carries no node-information of its own — the children's equivalence
// is the entire content. Java's behaviour matches.
func (e *LogicalUnionExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	_, ok := other.(*LogicalUnionExpression)
	return ok
}

// HashCodeWithoutChildren is a class-discriminating constant.
func (e *LogicalUnionExpression) HashCodeWithoutChildren() uint64 { return 37 }

func (e *LogicalUnionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	copied := make([]Quantifier, len(quantifiers))
	copy(copied, quantifiers)
	return &LogicalUnionExpression{
		quantifiers: copied,
	}
}

var _ RelationalExpression = (*LogicalUnionExpression)(nil)
