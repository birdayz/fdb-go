package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTempTableScanPlan scans a temporary table identified by
// a correlation alias. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryTempTableScanPlan`.
type RecordQueryTempTableScanPlan struct {
	PlanExprBase
	tempTableAlias values.CorrelationIdentifier
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this temp-table scan emits — minted once at construction, returned
	// by GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique
	// per instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalTempTableScanWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor.
	resultValue values.Value
}

func NewRecordQueryTempTableScanPlan(alias values.CorrelationIdentifier) *RecordQueryTempTableScanPlan {
	return &RecordQueryTempTableScanPlan{
		tempTableAlias: alias,
		resultValue:    values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
	}
}

func (p *RecordQueryTempTableScanPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) GetResultType() values.Type { return values.UnknownType }

// GetResultValue returns the temp-table scan's STABLE per-instance result value —
// the single correlation identity a bare temp-table scan carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryTempTableScanPlan) GetResultValue() values.Value {
	if p.resultValue == nil {
		return p.PlanExprBase.GetResultValue()
	}
	return p.resultValue
}

func (p *RecordQueryTempTableScanPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey lists the field that distinguishes this scan in the memo: the
// temp-table alias. The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren.
func (p *RecordQueryTempTableScanPlan) structuralKey() *structuralKey {
	return newStructuralKey().Alias(p.tempTableAlias)
}

func (p *RecordQueryTempTableScanPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTempTableScanPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryTempTableScanPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("temptablescan|")
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

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTempTableScanPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
