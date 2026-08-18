package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTempTableInsertPlan inserts the output of an inner plan
// into a temporary table identified by a correlation alias. The owning
// flag controls whether this plan owns the temp table lifecycle.
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryTempTableInsertPlan`.
type RecordQueryTempTableInsertPlan struct {
	PlanExprBase
	innerQ         expressions.Quantifier
	tempTableAlias values.CorrelationIdentifier
	owning         bool
}

func NewRecordQueryTempTableInsertPlan(
	inner RecordQueryPlan,
	alias values.CorrelationIdentifier,
	owning bool,
) (*RecordQueryTempTableInsertPlan, error) {
	return NewRecordQueryTempTableInsertPlanFromQuantifier(QuantifierOverPlan(inner), alias, owning)
}

// NewRecordQueryTempTableInsertPlanFromQuantifier builds an insert whose child is
// a LIVE memo quantifier.
func NewRecordQueryTempTableInsertPlanFromQuantifier(
	innerQ expressions.Quantifier,
	alias values.CorrelationIdentifier,
	owning bool,
) (*RecordQueryTempTableInsertPlan, error) {
	base, err := newPlanExprBaseForQuantifier("RecordQueryTempTableInsertPlan", innerQ)
	if err != nil {
		return nil, err
	}
	return &RecordQueryTempTableInsertPlan{
		PlanExprBase:   base,
		innerQ:         innerQ,
		tempTableAlias: alias,
		owning:         owning,
	}, nil
}

func (p *RecordQueryTempTableInsertPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryTempTableInsertPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

func (p *RecordQueryTempTableInsertPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableInsertPlan) IsOwning() bool { return p.owning }

func (p *RecordQueryTempTableInsertPlan) GetResultType() values.Type {
	return p.GetResultValue().Type()
}

func (p *RecordQueryTempTableInsertPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey lists the fields that distinguish this insert in the memo: the
// temp-table alias and the owning flag. Children are excluded; the same key
// drives both EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryTempTableInsertPlan) structuralKey() *structuralKey {
	return newStructuralKey().Alias(p.tempTableAlias).Bool(p.owning)
}

func (p *RecordQueryTempTableInsertPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableInsertPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryTempTableInsertPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("temptableinsert|")
	p.storeStructuralHash(p, hash)
	return hash
}

func (p *RecordQueryTempTableInsertPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return "TempTableInsert(" + p.tempTableAlias.Name() + ", " + innerLabel + ")"
}

var (
	_ RecordQueryPlan                  = (*RecordQueryTempTableInsertPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryTempTableInsertPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryTempTableInsertPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryTempTableInsertPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryTempTableInsertPlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	cp.innerQ = qs[0]
	base, err := newPlanExprBaseForQuantifier("RecordQueryTempTableInsertPlan", qs[0])
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	return &cp, nil
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the insert carries its child as a single LIVE memo edge,
// the relink is exactly a quantifier swap: WithQuantifiers preserves the temp-table
// alias and owning flag, and GetInner re-resolves through the new singleton
// reference. This replaces physicalTempTableInsertWrapper.WithChildren (RFC-184 W2),
// whose separate snapshot plan field forced a constructor rebuild.
func (p *RecordQueryTempTableInsertPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryTempTableInsertPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTempTableInsertPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
