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
	innerQ       expressions.Quantifier
	defaultValue values.Value
}

func NewRecordQueryDefaultOnEmptyPlan(inner RecordQueryPlan, defaultValue values.Value) *RecordQueryDefaultOnEmptyPlan {
	return &RecordQueryDefaultOnEmptyPlan{innerQ: QuantifierOverPlan(inner), defaultValue: defaultValue}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

func (p *RecordQueryDefaultOnEmptyPlan) GetDefaultValue() values.Value { return p.defaultValue }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryDefaultOnEmptyPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetResultType() values.Type {
	if inner := p.GetInner(); inner != nil {
		return inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryDefaultOnEmptyPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
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
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return "DefaultOnEmpty(" + innerLabel + ")"
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

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryDefaultOnEmptyPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDefaultOnEmptyPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
