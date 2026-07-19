package plans

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTempTableScanPlan scans a temporary table identified by
// a correlation alias. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryTempTableScanPlan`.
type RecordQueryTempTableScanPlan struct {
	PlanExprBase
	tempTableAlias values.CorrelationIdentifier
}

func NewRecordQueryTempTableScanPlan(alias values.CorrelationIdentifier) *RecordQueryTempTableScanPlan {
	return &RecordQueryTempTableScanPlan{tempTableAlias: alias}
}

func (p *RecordQueryTempTableScanPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryTempTableScanPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryTempTableScanPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableScanPlan)
	if !ok {
		return false
	}
	return p.tempTableAlias == o.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("temptablescan|"))
	h.Write([]byte(p.tempTableAlias.Name()))
	return h.Sum64()
}

func (p *RecordQueryTempTableScanPlan) Explain() string {
	return "TempTableScan(" + p.tempTableAlias.Name() + ")"
}

var (
	_ RecordQueryPlan                  = (*RecordQueryTempTableScanPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryTempTableScanPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryTempTableScanPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryTempTableScanPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}
