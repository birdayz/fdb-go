package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryUnorderedPrimaryKeyDistinctPlan removes duplicate rows by
// means of a hash set of primary keys already seen. Unlike
// RecordQueryDistinctPlan (which deduplicates by full row), this plan
// deduplicates by primary key only — two rows with the same PK but
// different projected columns collapse to one.
//
// Mirrors Java's RecordQueryUnorderedPrimaryKeyDistinctPlan. This is a
// single-child plan: it wraps an inner plan and filters its output
// stream.
//
// This is a STRUCTURE-ONLY port — no execution logic. The hash-set
// dedup belongs in the execution layer.
type RecordQueryUnorderedPrimaryKeyDistinctPlan struct {
	inner RecordQueryPlan
}

// NewRecordQueryUnorderedPrimaryKeyDistinctPlan constructs a PK-based
// distinct plan over the given inner plan.
func NewRecordQueryUnorderedPrimaryKeyDistinctPlan(inner RecordQueryPlan) *RecordQueryUnorderedPrimaryKeyDistinctPlan {
	return &RecordQueryUnorderedPrimaryKeyDistinctPlan{inner: inner}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetInner() RecordQueryPlan { return p.inner }

// IsReverse delegates to the inner plan.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) IsReverse() bool {
	if c, ok := p.inner.(interface{ IsReverse() bool }); ok {
		return c.IsReverse()
	}
	return false
}

// GetResultType returns the inner plan's result type — PK-distinct
// doesn't reshape rows.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren — PK-distinct plans have no node-specific data
// beyond the concrete type. Mirrors Java where equalsWithoutChildren
// only checks `getClass() == otherExpression.getClass()`.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	_, ok := other.(*RecordQueryUnorderedPrimaryKeyDistinctPlan)
	return ok
}

// HashCodeWithoutChildren is a constant for the type discriminator.
// Mirrors Java's BASE_HASH("Record-Query-Unordered-Primary-Key-Distinct-Plan").
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("unorderedprimarykeyDistinctplan"))
	return h.Sum64()
}

// Explain renders UnorderedPrimaryKeyDistinct(inner).
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) Explain() string {
	innerLabel := "<nil>"
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	return fmt.Sprintf("UnorderedPrimaryKeyDistinct(%s)", innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil)
