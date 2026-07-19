package plans

import (
	"fmt"
	"hash/fnv"

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

// GetInner returns the source plan, dereferenced through the quantifier.
func (p *RecordQueryUpdatePlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

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

// EqualsWithoutChildren compares targetRecordType + the transforms BY VALUE
// (FieldPath + semantic NewValue identity), per Java RecordQueryAbstract-
// DataModificationPlan.equalsWithoutChildren (transformationsTrie equality).
// Count-only comparison made `SET a=1` ≡ `SET a=2` on the write path — a
// memo collapse that executes the WRONG update. Transforms are canonicalised
// sorted by FieldPath at construction (UpdateExpression), so pairwise
// comparison is order-stable. Java's targetType/coercionTrie/
// computationValue have no Go counterpart yet; they join identity when
// they land.
func (p *RecordQueryUpdatePlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUpdatePlan)
	if !ok {
		return false
	}
	if p.targetRecordType != o.targetRecordType {
		return false
	}
	if len(p.transforms) != len(o.transforms) {
		return false
	}
	for i, tr := range p.transforms {
		if tr.FieldPath != o.transforms[i].FieldPath {
			return false
		}
		if !semanticValueEquals(tr.NewValue, o.transforms[i].NewValue) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes class + targetRecordType + per-transform
// FieldPath and NewValue (semantic hash), pairing with the by-value equality
// above so equal⟹same-hash holds.
func (p *RecordQueryUpdatePlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("updateplan|"))
	h.Write([]byte(p.targetRecordType))
	h.Write([]byte{0})
	for _, tr := range p.transforms {
		h.Write([]byte(tr.FieldPath))
		h.Write([]byte{0})
		writeValueHash(h, tr.NewValue)
	}
	return h.Sum64()
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

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUpdatePlan) GetRecordQueryPlan() RecordQueryPlan { return p }
