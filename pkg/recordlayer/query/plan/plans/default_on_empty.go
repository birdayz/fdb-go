package plans

import (
	"fmt"

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

// NewRecordQueryDefaultOnEmptyPlanFromQuantifier builds a default-on-empty whose
// child is a LIVE memo quantifier (the implementation rule passes the live
// currentQuant) instead of a snapshot over a single plan. This makes the plan its
// own cascades expression carrying its child edge directly: the memo holds it
// without a physical wrapper, and GetInner / GetQuantifiers / GetResultValue all
// resolve through the one live edge (RFC-184 W2).
func NewRecordQueryDefaultOnEmptyPlanFromQuantifier(innerQ expressions.Quantifier, defaultValue values.Value) *RecordQueryDefaultOnEmptyPlan {
	return &RecordQueryDefaultOnEmptyPlan{innerQ: innerQ, defaultValue: defaultValue}
}

func (p *RecordQueryDefaultOnEmptyPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetInnerQuantifier returns the live child quantifier — the single memo edge the
// default-on-empty ranges over. derivationsForDefaultOnEmpty reads its alias to
// translate the default value's correlation; since RFC-184 W2 the memo holds the
// bare plan (no physicalDefaultOnEmptyWrapper whose innerQuant field it used to
// read), this exposes the same edge.
func (p *RecordQueryDefaultOnEmptyPlan) GetInnerQuantifier() expressions.Quantifier {
	return p.innerQ
}

// GetResultValue returns the flowed object value of the live child quantifier —
// a default-on-empty passes its input's rows through (with an empty→default row),
// so its row identity IS the inner's. This is the identity
// physicalDefaultOnEmptyWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryDefaultOnEmptyPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
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
func (p *RecordQueryDefaultOnEmptyPlan) structuralKey() *structuralKey {
	return newStructuralKey().Value(p.defaultValue)
}

func (p *RecordQueryDefaultOnEmptyPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDefaultOnEmptyPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryDefaultOnEmptyPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("defaultonemptyplan|")
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the default-on-empty carries its child as a single LIVE memo
// edge, the relink is exactly a quantifier swap: WithQuantifiers preserves the
// default value, and GetInner re-resolves through the new singleton reference. This
// replaces physicalDefaultOnEmptyWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field forced a constructor rebuild gated on isLeafReplaceable — a
// single live child edge needs neither.
func (p *RecordQueryDefaultOnEmptyPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryDefaultOnEmptyPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDefaultOnEmptyPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
