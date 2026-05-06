package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryIntersectionPlan emits the bag-intersection of its
// inner plans — rows that appear in EVERY inner stream, compared by
// the comparison-key columns. Mirrors Java's
// `RecordQueryIntersectionPlan`.
//
// Java has multiple intersection-plan flavors (ordered, unordered,
// primary-key-based, value-based). The seed ports the simplest:
// generic N-way intersection over a comparison-key column list.
// Specialised flavors land when their consumers do.
//
// All inners must produce row-compatible streams (planner's
// responsibility); the comparison-key columns are matched against
// each row to determine intersection membership.
type RecordQueryIntersectionPlan struct {
	inners              []RecordQueryPlan
	comparisonKeyValues []values.Value
}

// NewRecordQueryIntersectionPlan constructs an N-way intersection.
// `comparisonKeyValues` defines the row-equality key (typically the
// primary-key columns of the result type).
func NewRecordQueryIntersectionPlan(inners []RecordQueryPlan, comparisonKeyValues []values.Value) *RecordQueryIntersectionPlan {
	cpInners := make([]RecordQueryPlan, len(inners))
	copy(cpInners, inners)
	cpKeys := make([]values.Value, len(comparisonKeyValues))
	copy(cpKeys, comparisonKeyValues)
	return &RecordQueryIntersectionPlan{
		inners:              cpInners,
		comparisonKeyValues: cpKeys,
	}
}

// GetInners returns the intersection's inner plans (read-only).
func (p *RecordQueryIntersectionPlan) GetInners() []RecordQueryPlan { return p.inners }

// GetComparisonKeyValues returns the row-equality key list (read-only).
func (p *RecordQueryIntersectionPlan) GetComparisonKeyValues() []values.Value {
	return p.comparisonKeyValues
}

// GetResultType returns the first inner's result type, or
// UnknownType if there are no inners.
func (p *RecordQueryIntersectionPlan) GetResultType() values.Type {
	if len(p.inners) == 0 {
		return values.UnknownType
	}
	return p.inners[0].GetResultType()
}

// GetChildren returns the inner plans.
func (p *RecordQueryIntersectionPlan) GetChildren() []RecordQueryPlan { return p.inners }

// EqualsWithoutChildren matches IntersectionPlan + same-shape
// comparison-key list (length only — Value-level equality lives at
// the Value layer's SemanticEquals).
func (p *RecordQueryIntersectionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIntersectionPlan)
	if !ok {
		return false
	}
	return len(p.comparisonKeyValues) == len(o.comparisonKeyValues)
}

// HashCodeWithoutChildren hashes the type discriminator + the
// comparison-key column count.
func (p *RecordQueryIntersectionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("intersectionplan"))
	for range p.comparisonKeyValues {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders Intersection(inner1, inner2, ...).
func (p *RecordQueryIntersectionPlan) Explain() string {
	parts := make([]string, len(p.inners))
	for i, inner := range p.inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	return fmt.Sprintf("Intersection(%s)", strings.Join(parts, ", "))
}

var _ RecordQueryPlan = (*RecordQueryIntersectionPlan)(nil)
