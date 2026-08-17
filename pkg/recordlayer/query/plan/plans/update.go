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
// Result type: the Java-shaped two-field record {OLD: inner, NEW: target}.
type RecordQueryUpdatePlan struct {
	PlanExprBase
	innerQ           expressions.Quantifier
	targetRecordType string
	targetType       values.ExactTypeHandle
	transforms       []expressions.UpdateTransform
}

// NewRecordQueryUpdatePlan constructs the UPDATE plan.
func NewRecordQueryUpdatePlan(inner RecordQueryPlan, targetRecordType string, transforms []expressions.UpdateTransform) (*RecordQueryUpdatePlan, error) {
	return NewRecordQueryUpdatePlanFromQuantifier(QuantifierOverPlan(inner), targetRecordType, transforms)
}

// NewRecordQueryUpdatePlanFromQuantifier builds an UPDATE whose child is a LIVE
// memo quantifier (the implementation rule passes
// ForEachQuantifier(MemoizeExpression(winner))) instead of a snapshot over a
// single plan. This makes the plan its own cascades expression carrying its
// child edge directly: the memo holds it without a physical wrapper, and
// GetInner / GetQuantifiers / GetResultValue all resolve through the one live
// edge (RFC-184 W2). transforms are copied, unchanged from NewRecordQueryUpdatePlan.
func NewRecordQueryUpdatePlanFromQuantifier(innerQ expressions.Quantifier, targetRecordType string, transforms []expressions.UpdateTransform) (*RecordQueryUpdatePlan, error) {
	flowedType, err := innerQ.GetFlowedObjectType()
	if err != nil {
		return nil, err
	}
	return NewRecordQueryUpdatePlanFromQuantifierWithTargetType(innerQ, targetRecordType, flowedType, transforms)
}

// NewRecordQueryUpdatePlanFromQuantifierWithTargetType preserves the target
// schema carried by the logical UpdateExpression. The legacy convenience
// constructor derives this from the input because ordinary updates do not
// change record type; the rule path calls this form so memo admission can prove
// the exact logical and physical OLD/NEW contracts agree.
func NewRecordQueryUpdatePlanFromQuantifierWithTargetType(
	innerQ expressions.Quantifier,
	targetRecordType string,
	targetType values.Type,
	transforms []expressions.UpdateTransform,
) (*RecordQueryUpdatePlan, error) {
	oldType, err := innerQ.GetFlowedObjectType()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryUpdatePlan OLD type: %w", err)
	}
	if _, ok := oldType.(*values.RecordType); !ok {
		return nil, fmt.Errorf("RecordQueryUpdatePlan OLD type: expected record, got %v", oldType)
	}
	if _, ok := targetType.(*values.RecordType); !ok {
		return nil, fmt.Errorf("RecordQueryUpdatePlan NEW type: expected record, got %v", targetType)
	}
	exactTarget, err := values.SnapshotExactType(targetType)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryUpdatePlan NEW type: %w", err)
	}
	resultType := &values.RecordType{Fields: []values.Field{
		{Name: "OLD", Ordinal: 0, FieldType: oldType},
		{Name: "NEW", Ordinal: 1, FieldType: exactTarget.Type()},
	}}
	base, err := newPlanExprBaseForType("RecordQueryUpdatePlan", resultType)
	if err != nil {
		return nil, err
	}
	copied := make([]expressions.UpdateTransform, len(transforms))
	copy(copied, transforms)
	return &RecordQueryUpdatePlan{
		PlanExprBase:     base,
		innerQ:           innerQ,
		targetRecordType: targetRecordType,
		targetType:       exactTarget,
		transforms:       copied,
	}, nil
}

// GetInner returns the source plan, dereferenced through the quantifier.
func (p *RecordQueryUpdatePlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetResultValue returns the stable current QOV for {OLD,NEW}.
func (p *RecordQueryUpdatePlan) GetResultValue() values.Value {
	return p.PlanExprBase.GetResultValue()
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

// GetTargetType returns a defensive exact target type.
func (p *RecordQueryUpdatePlan) GetTargetType() values.Type {
	if p.targetType == nil {
		return nil
	}
	return p.targetType.Type()
}

// GetTransforms returns the per-row transform list (read-only).
func (p *RecordQueryUpdatePlan) GetTransforms() []expressions.UpdateTransform { return p.transforms }

// GetResultType returns the inner's result type.
func (p *RecordQueryUpdatePlan) GetResultType() values.Type { return p.GetResultValue().Type() }

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
	k := newStructuralKey().Str(p.targetRecordType).Type(p.GetTargetType())
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
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("updateplan|")
	p.storeStructuralHash(p, hash)
	return hash
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
func (p *RecordQueryUpdatePlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryUpdatePlan", len(qs), 1); err != nil {
		return nil, err
	}
	cp := *p
	rebuilt, err := NewRecordQueryUpdatePlanFromQuantifierWithTargetType(
		qs[0], p.targetRecordType, p.GetTargetType(), p.transforms)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = rebuilt.PlanExprBase
	cp.innerQ = rebuilt.innerQ
	cp.targetType = rebuilt.targetType
	return &cp, nil
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
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUpdatePlan) GetRecordQueryPlan() RecordQueryPlan { return p }
