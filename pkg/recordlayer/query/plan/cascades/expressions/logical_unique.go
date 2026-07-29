package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// Single inner Quantifier; no comparison keys (the PK comparison is implicit
// based on the inner's record type). The required bit is a Go-side planning
// mode: ordinary Unique may be absorbed when its exact physical input is
// already PK-distinct, while required Unique must materialize a physical
// PK-distinct operator. It therefore participates in memo identity.
type LogicalUniqueExpression struct {
	inner    Quantifier
	required bool
}

// NewLogicalUniqueExpression builds an ordinary, absorbable Unique over inner.
func NewLogicalUniqueExpression(inner Quantifier) *LogicalUniqueExpression {
	return &LogicalUniqueExpression{inner: inner}
}

// NewRequiredLogicalUniqueExpression builds a Unique whose physical
// PK-distinct operator must be retained even when the input is already
// distinct. This mode is dormant until a cardinality-safe E-to-ForEach
// mapping explicitly requests it.
func NewRequiredLogicalUniqueExpression(inner Quantifier) *LogicalUniqueExpression {
	return &LogicalUniqueExpression{
		inner:    inner,
		required: true,
	}
}

// GetInner returns the inner Quantifier.
func (e *LogicalUniqueExpression) GetInner() Quantifier { return e.inner }

// IsRequired reports whether implementation must retain a physical
// PK-distinct operator instead of absorbing this logical Unique.
func (e *LogicalUniqueExpression) IsRequired() bool { return e.required }

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

// EqualsWithoutChildren is true iff other is a LogicalUnique in the same
// implementation mode. Required and ordinary Unique expressions are not
// interchangeable in the memo.
func (e *LogicalUniqueExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*LogicalUniqueExpression)
	return ok && e.required == o.required
}

// HashCodeWithoutChildren preserves Java's class-discriminating constant for
// ordinary Unique and mixes the Go-side required mode into the hash.
func (e *LogicalUniqueExpression) HashCodeWithoutChildren() uint64 {
	if !e.required {
		return 251
	}
	return 251*31 + 1
}

func (e *LogicalUniqueExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	cp := *e
	cp.inner = quantifiers[0]
	return &cp
}

var _ RelationalExpression = (*LogicalUniqueExpression)(nil)
