package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalDistinctExpression de-duplicates the rows flowing through its
// inner Quantifier. It carries no node-information — two
// LogicalDistinctExpressions are EqualsWithoutChildren iff they're the
// same class. (Java's behaviour is identical.)
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalDistinctExpression`.
type LogicalDistinctExpression struct {
	inner       Quantifier
	resultValue values.QuantifiedObjectValue
}

// NewLogicalDistinctExpression builds a Distinct over inner.
func NewLogicalDistinctExpression(inner Quantifier) (*LogicalDistinctExpression, error) {
	resultValue, err := requireFlowedResult("LogicalDistinctExpression", inner)
	if err != nil {
		return nil, err
	}
	return &LogicalDistinctExpression{inner: inner, resultValue: resultValue}, nil
}

// GetInner returns the inner Quantifier.
func (e *LogicalDistinctExpression) GetInner() Quantifier { return e.inner }

// GetResultValue is the inner's flowed object value (Distinct doesn't
// reshape rows).
func (e *LogicalDistinctExpression) GetResultValue() values.Value {
	return e.resultValue
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalDistinctExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is false — single child.
func (e *LogicalDistinctExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalDistinctExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set (Java behaviour).
func (e *LogicalDistinctExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren is true iff `other` is a LogicalDistinct.
// Distinct carries no node-information of its own.
func (e *LogicalDistinctExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	_, ok := other.(*LogicalDistinctExpression)
	return ok
}

// HashCodeWithoutChildren is a class-discriminating constant. Mirrors
// Java's `return 31`.
func (e *LogicalDistinctExpression) HashCodeWithoutChildren() uint64 { return 31 }

func (e *LogicalDistinctExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("LogicalDistinctExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	return NewLogicalDistinctExpression(quantifiers[0])
}

var _ RelationalExpression = (*LogicalDistinctExpression)(nil)
