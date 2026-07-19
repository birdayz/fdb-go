package plans

import (
	"hash/fnv"

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
) *RecordQueryTempTableInsertPlan {
	return &RecordQueryTempTableInsertPlan{
		innerQ:         QuantifierOverPlan(inner),
		tempTableAlias: alias,
		owning:         owning,
	}
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

func (p *RecordQueryTempTableInsertPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryTempTableInsertPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

func (p *RecordQueryTempTableInsertPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableInsertPlan)
	if !ok {
		return false
	}
	return p.tempTableAlias == o.tempTableAlias && p.owning == o.owning
}

func (p *RecordQueryTempTableInsertPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptableinsert|"))
	h.Write([]byte(p.tempTableAlias.Name()))
	if p.owning {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
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
func (p *RecordQueryTempTableInsertPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTempTableInsertPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
