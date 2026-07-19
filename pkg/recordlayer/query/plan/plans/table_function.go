package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTableFunctionPlan delegates row-stream production to an
// underlying streaming Value (e.g. RangeValue). Leaf plan (no
// children). Mirrors Java's RecordQueryTableFunctionPlan.
type RecordQueryTableFunctionPlan struct {
	PlanExprBase
	streamValue values.Value
}

func NewRecordQueryTableFunctionPlan(streamValue values.Value) *RecordQueryTableFunctionPlan {
	return &RecordQueryTableFunctionPlan{streamValue: streamValue}
}

func (p *RecordQueryTableFunctionPlan) GetStreamValue() values.Value { return p.streamValue }

func (p *RecordQueryTableFunctionPlan) GetResultType() values.Type {
	if p.streamValue == nil {
		return values.UnknownType
	}
	return p.streamValue.Type()
}

func (p *RecordQueryTableFunctionPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryTableFunctionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTableFunctionPlan)
	if !ok {
		return false
	}
	return p.streamValue == o.streamValue
}

func (p *RecordQueryTableFunctionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("tablefnplan|"))
	if p.streamValue != nil {
		h.Write([]byte(p.streamValue.Name()))
	}
	return h.Sum64()
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

// GetCorrelatedToWithoutChildren reports the correlations of this plan's
// stream value, mirroring physicalTableFunctionWrapper.
func (p *RecordQueryTableFunctionPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if v := p.GetStreamValue(); v != nil {
		return values.GetCorrelatedToOfValue(v)
	}
	return map[values.CorrelationIdentifier]struct{}{}
}
