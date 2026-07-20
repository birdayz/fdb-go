package plans

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryValuesPlan is a leaf physical-plan that produces a single
// row of constant values — the physical counterpart of
// LogicalValuesExpression. Mirrors SQL's VALUES (a, b, c) at execution
// time.
type RecordQueryValuesPlan struct {
	PlanExprBase
	columns []values.Value
}

func NewRecordQueryValuesPlan(columns []values.Value) *RecordQueryValuesPlan {
	return &RecordQueryValuesPlan{columns: columns}
}

func (p *RecordQueryValuesPlan) GetColumns() []values.Value { return p.columns }

func (p *RecordQueryValuesPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryValuesPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryValuesPlan) structuralKey() *structuralKey {
	return newStructuralKey().Values(p.columns)
}

func (p *RecordQueryValuesPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryValuesPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryValuesPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("valuesplan|")
}

func (p *RecordQueryValuesPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Values(")
	for i, v := range p.columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(values.ExplainValue(v))
	}
	b.WriteByte(')')
	return b.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryValuesPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryValuesPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryValuesPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryValuesPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryValuesPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
