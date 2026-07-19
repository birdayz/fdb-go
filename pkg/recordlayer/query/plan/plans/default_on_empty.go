package plans

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDefaultOnEmptyPlan returns the inner plan's rows if any
// exist, or a single row with the default value if the inner is empty.
// Mirrors Java's RecordQueryDefaultOnEmptyPlan.
type RecordQueryDefaultOnEmptyPlan struct {
	PlanExprBase
	inner        RecordQueryPlan
	defaultValue values.Value
}

func NewRecordQueryDefaultOnEmptyPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryDefaultOnEmptyPlan {
	return &RecordQueryDefaultOnEmptyPlan{inner: inner, defaultValue: defaultValue}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetInner() RecordQueryPlan     { return p.inner }
func (p *RecordQueryDefaultOnEmptyPlan) GetDefaultValue() values.Value { return p.defaultValue }

func (p *RecordQueryDefaultOnEmptyPlan) GetResultType() values.Type {
	if p.inner != nil {
		return p.inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryDefaultOnEmptyPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the default value by semantic Value identity
// (RFC-176 P2 — see semanticValueEquals).
func (p *RecordQueryDefaultOnEmptyPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDefaultOnEmptyPlan)
	if !ok {
		return false
	}
	return semanticValueEquals(p.defaultValue, o.defaultValue)
}

func (p *RecordQueryDefaultOnEmptyPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("defaultonemptyplan|"))
	writeValueHash(h, p.defaultValue)
	return h.Sum64()
}

func (p *RecordQueryDefaultOnEmptyPlan) Explain() string {
	inner := "<nil>"
	if p.inner != nil {
		inner = p.inner.Explain()
	}
	return "DefaultOnEmpty(" + inner + ")"
}

var (
	_ RecordQueryPlan                  = (*RecordQueryDefaultOnEmptyPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryDefaultOnEmptyPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryDefaultOnEmptyPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryDefaultOnEmptyPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}
