package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryInsertPlan is the physical INSERT plan: consumes
// rows from an inner plan and writes them to the target record
// type. Mirrors a simplified subset of Java's
// `RecordQueryInsertPlan` (which extends
// RecordQueryAbstractDataModificationPlan).
//
// Java's full surface includes per-record transforms, save-record
// behaviour flags, plan-graph rewriting hooks. The seed ports the
// minimal node-information needed by ImplementInsertRule:
//
//   - inner: the source plan producing rows to insert
//   - targetRecordType: the destination record type name
//   - targetType: the rich Type the inserted rows must conform to
//
// Result type matches inner — INSERT typically returns the inserted
// rows for cursor consumption.
//
// Execute is NOT in the seed surface — wiring to FDBRecordStore is
// a follow-up shift gated on the rule chain producing these plans.
type RecordQueryInsertPlan struct {
	PlanExprBase
	innerQ           expressions.Quantifier
	targetRecordType string
	targetType       values.Type
}

// NewRecordQueryInsertPlan constructs the INSERT plan.
func NewRecordQueryInsertPlan(inner RecordQueryPlan, targetRecordType string, targetType values.Type) *RecordQueryInsertPlan {
	if targetType == nil {
		targetType = values.UnknownType
	}
	return &RecordQueryInsertPlan{
		innerQ:           QuantifierOverPlan(inner),
		targetRecordType: targetRecordType,
		targetType:       targetType,
	}
}

// NewRecordQueryInsertPlanFromQuantifier builds an INSERT whose child is a LIVE
// memo quantifier (the implementation rule passes
// ForEachQuantifier(MemoizeExpression(winner))) instead of a snapshot over a
// single plan. This makes the plan its own cascades expression carrying its
// child edge directly: the memo holds it without a physical wrapper, and
// GetInner / GetQuantifiers / GetResultValue all resolve through the one live
// edge (RFC-184 W2).
func NewRecordQueryInsertPlanFromQuantifier(innerQ expressions.Quantifier, targetRecordType string, targetType values.Type) *RecordQueryInsertPlan {
	if targetType == nil {
		targetType = values.UnknownType
	}
	return &RecordQueryInsertPlan{
		innerQ:           innerQ,
		targetRecordType: targetRecordType,
		targetType:       targetType,
	}
}

// GetInner returns the source plan, dereferenced through the quantifier.
func (p *RecordQueryInsertPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetResultValue returns the flowed object value of the live child quantifier —
// INSERT passes its inner's rows through, so the result identity is the inner's,
// the value physicalInsertWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryInsertPlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryInsertPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryInsertPlan) GetTargetRecordType() string { return p.targetRecordType }

// GetTargetType returns the rich Type the inserted rows must
// conform to.
func (p *RecordQueryInsertPlan) GetTargetType() values.Type { return p.targetType }

// GetResultType returns the inner's result type — INSERT typically
// returns the inserted rows for cursor consumption.
func (p *RecordQueryInsertPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryInsertPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey folds the INSERT identity: the destination record-type name and
// the target Type (equals-only — the hash omits it, matching the hand-rolled
// hash which folded only targetRecordType). Drives both Equals and Hash.
func (p *RecordQueryInsertPlan) structuralKey() *structuralKey {
	return newStructuralKey().Str(p.targetRecordType).Type(p.targetType)
}

// EqualsWithoutChildren compares targetRecordType + targetType.
func (p *RecordQueryInsertPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInsertPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes class + targetRecordType.
func (p *RecordQueryInsertPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("insertplan|")
}

// Explain renders Insert(target, inner).
func (p *RecordQueryInsertPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Insert(%s, %s)", p.targetRecordType, innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryInsertPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryInsertPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryInsertPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryInsertPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). Because the plan carries its child as a single LIVE memo edge, the
// relink is exactly a quantifier swap: WithQuantifiers preserves the target and
// type, and GetInner re-resolves through the new singleton reference. This
// replaces physicalInsertWrapper.WithChildren (RFC-184 W2).
func (p *RecordQueryInsertPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryInsertPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryInsertPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
