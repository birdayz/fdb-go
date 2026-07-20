package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDeletePlan is the physical DELETE plan: deletes
// records emitted by an inner plan. Mirrors a simplified subset of
// Java's `RecordQueryDeletePlan`.
//
// Java's full surface includes pre-delete + post-delete hooks,
// versionstamp generation, plan-graph rewriting. The seed ports
// the minimal node-information needed by ImplementDeleteRule:
//
//   - inner: the source plan emitting rows to delete (typically a
//     filter/scan that selects the target rows)
//   - targetRecordType: the destination record type name
type RecordQueryDeletePlan struct {
	PlanExprBase
	innerQ           expressions.Quantifier
	targetRecordType string
}

// NewRecordQueryDeletePlan constructs the DELETE plan.
func NewRecordQueryDeletePlan(inner RecordQueryPlan, targetRecordType string) *RecordQueryDeletePlan {
	return &RecordQueryDeletePlan{
		innerQ:           QuantifierOverPlan(inner),
		targetRecordType: targetRecordType,
	}
}

// GetInner returns the source plan, dereferenced through the quantifier.
func (p *RecordQueryDeletePlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryDeletePlan) GetTargetRecordType() string { return p.targetRecordType }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryDeletePlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetResultType returns the inner's result type.
func (p *RecordQueryDeletePlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryDeletePlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares targetRecordType.
func (p *RecordQueryDeletePlan) structuralKey() *structuralKey {
	return newStructuralKey().Str(p.targetRecordType)
}

func (p *RecordQueryDeletePlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDeletePlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes class + targetRecordType.
func (p *RecordQueryDeletePlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("deleteplan|")
}

// Explain renders Delete(target, inner).
func (p *RecordQueryDeletePlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Delete(%s, %s)", p.targetRecordType, innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryDeletePlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryDeletePlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryDeletePlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryDeletePlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryDeletePlan) GetRecordQueryPlan() RecordQueryPlan { return p }
