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
	flowedType     values.Type
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this temp-table scan emits — minted once at construction, returned
	// by GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique
	// per instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalTempTableScanWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor.
	resultValue values.Value
}

func NewRecordQueryTempTableScanPlan(alias values.CorrelationIdentifier, flowedType values.Type) (*RecordQueryTempTableScanPlan, error) {
	base, err := newPlanExprBaseForType("RecordQueryTempTableScanPlan", flowedType)
	if err != nil {
		return nil, err
	}
	return &RecordQueryTempTableScanPlan{
		PlanExprBase:   base,
		tempTableAlias: alias,
		flowedType:     base.resultValue.Type(),
		resultValue:    base.resultValue,
	}, nil
}

func (p *RecordQueryTempTableScanPlan) GetTempTableAlias() values.CorrelationIdentifier {
	return p.tempTableAlias
}

func (p *RecordQueryTempTableScanPlan) GetResultType() values.Type { return p.flowedType }

// GetResultValue returns the temp-table scan's STABLE per-instance result value —
// the single correlation identity a bare temp-table scan carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryTempTableScanPlan) GetResultValue() values.Value {
	return p.resultValue
}

func (p *RecordQueryTempTableScanPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey lists the fields that distinguish this scan in the memo: the
// temp-table alias and the row it flows. The same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren.
//
// The alias alone was the key back when the constructor derived the flowed row
// from nothing else, so two scans of one alias could not differ. Once the
// constructor started taking an exact flowedType, that stopped holding: two
// scans sharing an alias but flowing different rows compared and hashed equal,
// so MemoizeExpression interned the second into the first and handed back a
// reference whose result QOV carries the OTHER expression's type.
func (p *RecordQueryTempTableScanPlan) structuralKey() *structuralKey {
	return newStructuralKey().Alias(p.tempTableAlias).Type(p.flowedType)
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
func (p *RecordQueryTempTableScanPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryTempTableScanPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTempTableScanPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
