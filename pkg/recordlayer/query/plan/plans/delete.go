package plans

import (
	"fmt"
	"hash/fnv"

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
	inner            RecordQueryPlan
	targetRecordType string
}

// NewRecordQueryDeletePlan constructs the DELETE plan.
func NewRecordQueryDeletePlan(inner RecordQueryPlan, targetRecordType string) *RecordQueryDeletePlan {
	return &RecordQueryDeletePlan{
		inner:            inner,
		targetRecordType: targetRecordType,
	}
}

// GetInner returns the source plan.
func (p *RecordQueryDeletePlan) GetInner() RecordQueryPlan { return p.inner }

// GetTargetRecordType returns the destination record-type name.
func (p *RecordQueryDeletePlan) GetTargetRecordType() string { return p.targetRecordType }

// GetResultType returns the inner's result type.
func (p *RecordQueryDeletePlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryDeletePlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares targetRecordType.
func (p *RecordQueryDeletePlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryDeletePlan)
	if !ok {
		return false
	}
	return p.targetRecordType == o.targetRecordType
}

// HashCodeWithoutChildren mixes class + targetRecordType.
func (p *RecordQueryDeletePlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("deleteplan|"))
	h.Write([]byte(p.targetRecordType))
	return h.Sum64()
}

// Explain renders Delete(target, inner).
func (p *RecordQueryDeletePlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Delete(%s, %s)", p.targetRecordType, innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryDeletePlan)(nil)
