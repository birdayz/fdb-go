package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryUpdatePlan is the physical UPDATE plan: applies a
// list of per-row transforms to records emitted by an inner plan.
// Mirrors a simplified subset of Java's `RecordQueryUpdatePlan`.
//
// The transforms list is the same `expressions.UpdateTransform`
// shape used by the logical UpdateExpression — Java carries them
// through to the physical plan unchanged.
//
// Result type: same as inner.
type RecordQueryUpdatePlan struct {
	PlanExprBase
	innerQ           expressions.Quantifier
	targetRecordType string
	transforms       []expressions.UpdateTransform
}

// NewRecordQueryUpdatePlan constructs the UPDATE plan.
func NewRecordQueryUpdatePlan(inner RecordQueryPlan, targetRecordType string, transforms []expressions.UpdateTransform) *RecordQueryUpdatePlan {
	copied := make([]expressions.UpdateTransform, len(transforms))
	copy(copied, transforms)
	return &RecordQueryUpdatePlan{
		innerQ:           QuantifierOverPlan(inner),
		targetRecordType: targetRecordType,
		transforms:       copied,
	}
}

// NewRecordQueryUpdatePlanFromQuantifier builds an UPDATE whose child is a LIVE
// memo quantifier (the implementation rule passes
// ForEachQuantifier(MemoizeExpression(winner))) instead of a snapshot over a
// single plan. This makes the plan its own cascades expression carrying its
// child edge directly: the memo holds it without a physical wrapper, and
// GetInner / GetQuantifiers / GetResultValue all resolve through the one live
// edge (RFC-184 W2). transforms are copied, unchanged from NewRecordQueryUpdatePlan.
func NewRecordQueryUpdatePlanFromQuantifier(innerQ expressions.Quantifier, targetRecordType string, transforms []expressions.UpdateTransform) *RecordQueryUpdatePlan {
	copied := make([]expressions.UpdateTransform, len(transforms))
	copy(copied, transforms)
	return &RecordQueryUpdatePlan{
		innerQ:           innerQ,
		targetRecordType: targetRecordType,
		transforms:       copied,
	}
}

// GetInner returns the source plan, dereferenced through the quantifier.
func (p *RecordQueryUpdatePlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetResultValue returns the flowed object value of the live child quantifier —
// UPDATE passes its inner's rows through, so the result identity is the inner's,
// the value physicalUpdateWrapper.GetResultValue supplied (RFC-184 W2).
func (p *RecordQueryUpdatePlan) GetResultValue() values.Value {
	return p.innerQ.GetFlowedObjectValue()
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryUpdatePlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryUpdatePlan) GetTargetRecordType() string { return p.targetRecordType }

// GetTransforms returns the per-row transform list (read-only).
func (p *RecordQueryUpdatePlan) GetTransforms() []expressions.UpdateTransform { return p.transforms }

// GetResultType returns the inner's result type.
func (p *RecordQueryUpdatePlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryUpdatePlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey folds targetRecordType + the transforms BY VALUE (FieldPath +
// semantic NewValue identity), per Java RecordQueryAbstractDataModificationPlan.
// equalsWithoutChildren (transformationsTrie equality). Count-only comparison
// made `SET a=1` ≡ `SET a=2` on the write path — a memo collapse that executes
// the WRONG update. Transforms are canonicalised sorted by FieldPath at
// construction (UpdateExpression), so pairwise comparison is order-stable.
// Java's targetType/coercionTrie/computationValue have no Go counterpart yet;
// they join identity when they land.
func (p *RecordQueryUpdatePlan) structuralKey() *structuralKey {
	k := newStructuralKey().Str(p.targetRecordType)
	for _, tr := range p.transforms {
		k.Str(tr.FieldPath).Value(tr.NewValue)
	}
	return k
}

func (p *RecordQueryUpdatePlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUpdatePlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes class + targetRecordType + per-transform
// FieldPath and NewValue (semantic hash), pairing with the by-value equality
// above so equal⟹same-hash holds.
func (p *RecordQueryUpdatePlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("updateplan|")
}

// Explain renders Update(target, [N transforms], inner).
func (p *RecordQueryUpdatePlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	return fmt.Sprintf("Update(%s, [%d transforms], %s)", p.targetRecordType, len(p.transforms), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryUpdatePlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryUpdatePlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryUpdatePlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryUpdatePlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
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
// transforms, and GetInner re-resolves through the new singleton reference. This
// replaces physicalUpdateWrapper.WithChildren (RFC-184 W2).
func (p *RecordQueryUpdatePlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryUpdatePlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUpdatePlan) GetRecordQueryPlan() RecordQueryPlan { return p }
