package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryTypeFilterPlan filters an inner plan's row stream to
// only those records of one of the specified record types. Mirrors
// Java's `RecordQueryTypeFilterPlan`.
//
// Uses the record-type discriminator (the implicit int64 ID FDB
// records carry) to filter without inspecting the row payload.
//
// Result type: same as inner (filter doesn't reshape rows).
type RecordQueryTypeFilterPlan struct {
	PlanExprBase
	recordTypes []string
	innerQ      expressions.Quantifier
}

// NewRecordQueryTypeFilterPlan constructs a type-filter over the
// given record-type set + inner plan.
func NewRecordQueryTypeFilterPlan(recordTypes []string, inner RecordQueryPlan) *RecordQueryTypeFilterPlan {
	return &RecordQueryTypeFilterPlan{
		recordTypes: dedupSortedStrings(recordTypes),
		innerQ:      QuantifierOverPlan(inner),
	}
}

// GetRecordTypes returns the canonical record-type-name list.
func (p *RecordQueryTypeFilterPlan) GetRecordTypes() []string { return p.recordTypes }

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryTypeFilterPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryTypeFilterPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the inner's result type.
func (p *RecordQueryTypeFilterPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryTypeFilterPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey identifies a type filter by its ORDERED record-type set.
func (p *RecordQueryTypeFilterPlan) structuralKey() *structuralKey {
	return newStructuralKey().Strs(p.recordTypes)
}

// EqualsWithoutChildren compares record-type sets.
func (p *RecordQueryTypeFilterPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTypeFilterPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes class + record-type set.
func (p *RecordQueryTypeFilterPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("typefilterplan|")
}

// Explain renders TypeFilter([T1, T2], inner).
func (p *RecordQueryTypeFilterPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("TypeFilter(%v, %s)", p.recordTypes, innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryTypeFilterPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryTypeFilterPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryTypeFilterPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryTypeFilterPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTypeFilterPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
