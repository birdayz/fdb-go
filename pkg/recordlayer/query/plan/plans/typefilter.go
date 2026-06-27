package plans

import (
	"fmt"
	"hash/fnv"

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
	recordTypes []string
	inner       RecordQueryPlan
}

// NewRecordQueryTypeFilterPlan constructs a type-filter over the
// given record-type set + inner plan.
func NewRecordQueryTypeFilterPlan(recordTypes []string, inner RecordQueryPlan) *RecordQueryTypeFilterPlan {
	return &RecordQueryTypeFilterPlan{
		recordTypes: dedupSortedStrings(recordTypes),
		inner:       inner,
	}
}

// GetRecordTypes returns the canonical record-type-name list.
func (p *RecordQueryTypeFilterPlan) GetRecordTypes() []string { return p.recordTypes }

// GetInner returns the wrapped inner plan.
func (p *RecordQueryTypeFilterPlan) GetInner() RecordQueryPlan { return p.inner }

// GetResultType returns the inner's result type.
func (p *RecordQueryTypeFilterPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryTypeFilterPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares record-type sets.
func (p *RecordQueryTypeFilterPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTypeFilterPlan)
	if !ok {
		return false
	}
	if len(p.recordTypes) != len(o.recordTypes) {
		return false
	}
	for i := range p.recordTypes {
		if p.recordTypes[i] != o.recordTypes[i] {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes class + record-type set.
func (p *RecordQueryTypeFilterPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("typefilterplan|"))
	for _, name := range p.recordTypes {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders TypeFilter([T1, T2], inner).
func (p *RecordQueryTypeFilterPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("TypeFilter(%v, %s)", p.recordTypes, innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryTypeFilterPlan)(nil)
