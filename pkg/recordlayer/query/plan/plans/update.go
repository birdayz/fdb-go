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
	inner            RecordQueryPlan
	targetRecordType string
	transforms       []expressions.UpdateTransform
}

// NewRecordQueryUpdatePlan constructs the UPDATE plan.
func NewRecordQueryUpdatePlan(inner RecordQueryPlan, targetRecordType string, transforms []expressions.UpdateTransform) *RecordQueryUpdatePlan {
	copied := make([]expressions.UpdateTransform, len(transforms))
	copy(copied, transforms)
	return &RecordQueryUpdatePlan{
		inner:            inner,
		targetRecordType: targetRecordType,
		transforms:       copied,
	}
}

// GetInner returns the source plan.
func (p *RecordQueryUpdatePlan) GetInner() RecordQueryPlan { return p.inner }

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryUpdatePlan) GetTargetRecordType() string { return p.targetRecordType }

// GetTransforms returns the per-row transform list (read-only).
func (p *RecordQueryUpdatePlan) GetTransforms() []expressions.UpdateTransform { return p.transforms }

// GetResultType returns the inner's result type.
func (p *RecordQueryUpdatePlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryUpdatePlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares targetRecordType + transform count.
// (Per-transform structural comparison is gated on a UpdateTransform
// equality method which the seed doesn't expose; count match is the
// best the seed can do without reaching into the transform shape.)
func (p *RecordQueryUpdatePlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUpdatePlan)
	if !ok {
		return false
	}
	if p.targetRecordType != o.targetRecordType {
		return false
	}
	return len(p.transforms) == len(o.transforms)
}

// HashCodeWithoutChildren mixes class + targetRecordType + transform count.
func (p *RecordQueryUpdatePlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("updateplan|"))
	h.Write([]byte(p.targetRecordType))
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(uint64(len(p.transforms)) >> (8 * (7 - i)))
	}
	h.Write(b[:])
	return h.Sum64()
}

// Explain renders Update(target, [N transforms], inner).
func (p *RecordQueryUpdatePlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Update(%s, [%d transforms], %s)", p.targetRecordType, len(p.transforms), innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryUpdatePlan)(nil)
