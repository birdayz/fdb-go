package plans

import (
	"fmt"
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
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the single row this plan emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalValuesWrapper's fresh-per-call GetResultValue could not (RFC-184 W2).
	// nil for struct-literal test plans that bypass the constructor —
	// GetResultValue falls back to PlanExprBase's fresh QOV there.
	resultValue values.Value
}

func NewRecordQueryValuesPlan(columns []values.Value) (*RecordQueryValuesPlan, error) {
	rowValue, err := values.ProjectionResultValue(columns, nil)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryValuesPlan result row: %w", err)
	}
	base, err := newPlanExprBaseForType("RecordQueryValuesPlan", rowValue.Type())
	if err != nil {
		return nil, err
	}
	return &RecordQueryValuesPlan{
		PlanExprBase: base,
		columns:      append([]values.Value(nil), columns...),
		resultValue:  base.resultValue,
	}, nil
}

func (p *RecordQueryValuesPlan) GetColumns() []values.Value { return p.columns }

// GetResultValue returns the values plan's STABLE per-instance result value —
// the single correlation identity a bare values plan carries as its own memo
// expression (RFC-184 W2). Falls back to PlanExprBase (a fresh QOV per call) for
// struct-literal test plans that bypass the constructor (resultValue is nil).
func (p *RecordQueryValuesPlan) GetResultValue() values.Value {
	return p.resultValue
}

func (p *RecordQueryValuesPlan) GetResultType() values.Type {
	if qov, ok := values.AsQuantifiedObjectValue(p.resultValue); ok {
		return qov.FlowedType()
	}
	return nil
}

func (p *RecordQueryValuesPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryValuesPlan) structuralKey() *structuralKey {
	return newStructuralKey().Values(p.columns)
}

func (p *RecordQueryValuesPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryValuesPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryValuesPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("valuesplan|")
	p.storeStructuralHash(p, hash)
	return hash
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
func (p *RecordQueryValuesPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryValuesPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryValuesPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
