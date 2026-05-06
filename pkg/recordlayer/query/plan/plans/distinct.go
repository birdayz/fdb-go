package plans

import (
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryDistinctPlan removes duplicate rows from an inner
// plan's row stream. Mirrors Java's `RecordQueryUnorderedPrimaryKeyDistinctPlan`
// (the simpler unordered-distinct shape — Java has multiple
// distinct-plan flavors: ordered / unordered / by-key / by-row).
// The seed picks unordered-by-row.
//
// Result type matches inner — distinct doesn't reshape rows.
type RecordQueryDistinctPlan struct {
	inner RecordQueryPlan
}

// NewRecordQueryDistinctPlan constructs a distinct plan over the
// given inner plan.
func NewRecordQueryDistinctPlan(inner RecordQueryPlan) *RecordQueryDistinctPlan {
	return &RecordQueryDistinctPlan{inner: inner}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryDistinctPlan) GetInner() RecordQueryPlan { return p.inner }

// GetResultType returns the inner's result type.
func (p *RecordQueryDistinctPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryDistinctPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren — distinct plans are interchangeable on
// node-info alone (no operator-specific data); compares only the
// concrete type.
func (p *RecordQueryDistinctPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	_, ok := other.(*RecordQueryDistinctPlan)
	return ok
}

// HashCodeWithoutChildren is a constant for the type discriminator.
func (p *RecordQueryDistinctPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("distinctplan"))
	return h.Sum64()
}

// Explain renders Distinct(inner).
func (p *RecordQueryDistinctPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("Distinct(%s)", innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryDistinctPlan)(nil)
