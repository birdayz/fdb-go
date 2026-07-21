package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTableFunctionPlan delegates row-stream production to an
// underlying streaming Value (e.g. RangeValue). Leaf plan (no
// children). Mirrors Java's RecordQueryTableFunctionPlan.
type RecordQueryTableFunctionPlan struct {
	PlanExprBase
	streamValue values.Value
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this table function emits — minted once at construction, returned
	// by GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique
	// per instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalTableFunctionWrapper's fresh-per-call GetResultValue could not
	// (RFC-184 W2). nil for struct-literal test plans that bypass the constructor.
	resultValue values.Value
}

func NewRecordQueryTableFunctionPlan(streamValue values.Value) *RecordQueryTableFunctionPlan {
	return &RecordQueryTableFunctionPlan{
		streamValue: streamValue,
		resultValue: values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
	}
}

func (p *RecordQueryTableFunctionPlan) GetStreamValue() values.Value { return p.streamValue }

func (p *RecordQueryTableFunctionPlan) GetResultType() values.Type {
	if p.streamValue == nil {
		return values.UnknownType
	}
	return p.streamValue.Type()
}

func (p *RecordQueryTableFunctionPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the table-function identity: the stream Value by POINTER
// identity (ValuePtr — the hand-rolled equals used ==, NOT semantic equality).
// Drives both Equals and Hash.
func (p *RecordQueryTableFunctionPlan) structuralKey() *structuralKey {
	return newStructuralKey().ValuePtr(p.streamValue)
}

func (p *RecordQueryTableFunctionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTableFunctionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryTableFunctionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("tablefnplan|")
}

func (p *RecordQueryTableFunctionPlan) Explain() string {
	if p.streamValue != nil {
		return fmt.Sprintf("TableFunction(%s)", p.streamValue.Name())
	}
	return "TableFunction(<nil>)"
}

var (
	_ RecordQueryPlan                  = (*RecordQueryTableFunctionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryTableFunctionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryTableFunctionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryTableFunctionPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetResultValue returns the table function's STABLE per-instance result value —
// the single correlation identity a bare table function carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryTableFunctionPlan) GetResultValue() values.Value {
	if p.resultValue == nil {
		return p.PlanExprBase.GetResultValue()
	}
	return p.resultValue
}

// GetCorrelatedToWithoutChildren reports the correlations of this plan's
// stream value, mirroring physicalTableFunctionWrapper.
func (p *RecordQueryTableFunctionPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if v := p.GetStreamValue(); v != nil {
		return values.GetCorrelatedToOfValue(v)
	}
	return map[values.CorrelationIdentifier]struct{}{}
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTableFunctionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
