package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalUniqueExpression filters its inner stream to keep only the
// rows that are unique by primary key. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalUniqueExpression`.
//
// Distinction from LogicalDistinctExpression:
//
//   - LogicalDistinct: full-row deduplication (drop two rows iff
//     they're equal across ALL columns).
//   - LogicalUnique: primary-key-based deduplication (drop two rows
//     iff their PK columns match — non-PK columns may differ; the
//     surviving row's non-PK columns are implementation-defined).
//
// Used by Java's planner to express "ensure no two rows have the same
// PK" — typically inserted by uniqueness rules to guarantee a join
// path doesn't produce duplicate primary keys (e.g. when a join's
// right side is range-scanned with possible re-emission).
//
// Carries no node-information beyond its identity (matches Java's
// `getClass() == other.getClass()` equality check). Single inner
// Quantifier; no comparison keys (the PK comparison is implicit
// based on the inner's record type).
type LogicalUniqueExpression struct {
	inner Quantifier
}

// NewLogicalUniqueExpression builds a Unique over inner.
func NewLogicalUniqueExpression(inner Quantifier) *LogicalUniqueExpression {
	return &LogicalUniqueExpression{inner: inner}
}

// GetInner returns the inner Quantifier.
func (e *LogicalUniqueExpression) GetInner() Quantifier { return e.inner }

// GetResultValue returns the inner's flowed object value — Unique
// doesn't reshape rows, only filters.
func (e *LogicalUniqueExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalUniqueExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is false — single child.
func (e *LogicalUniqueExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalUniqueExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set (Java
// behaviour: Unique has no correlations of its own).
func (e *LogicalUniqueExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren is true iff `other` is a LogicalUnique.
// Unique carries no node-information of its own (matches Java's
// `getClass() == other.getClass()`).
func (e *LogicalUniqueExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	_, ok := other.(*LogicalUniqueExpression)
	return ok
}

// HashCodeWithoutChildren is a class-discriminating constant.
// Mirrors Java's `return 251`.
func (e *LogicalUniqueExpression) HashCodeWithoutChildren() uint64 { return 251 }

func (e *LogicalUniqueExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalUniqueExpression{
		inner: quantifiers[0],
	}
}

var _ RelationalExpression = (*LogicalUniqueExpression)(nil)
