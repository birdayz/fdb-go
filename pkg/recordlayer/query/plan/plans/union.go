package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryUnionPlan emits the rows of all input plans
// concatenated. Mirrors Java's `RecordQueryUnionPlan` (the simple
// UNION ALL variant).
//
// Java has multiple union-plan flavors keyed on dedup vs no-dedup
// and on key-expression vs values comparison. The seed ports the
// simplest: UNION ALL with no dedup.
//
// Result type matches the first inner's result type. All inners
// must produce row-compatible streams (the planner's responsibility).
type RecordQueryUnionPlan struct {
	inners []RecordQueryPlan
}

// NewRecordQueryUnionPlan constructs a UNION ALL over the given
// inner plans.
func NewRecordQueryUnionPlan(inners []RecordQueryPlan) *RecordQueryUnionPlan {
	copied := make([]RecordQueryPlan, len(inners))
	copy(copied, inners)
	return &RecordQueryUnionPlan{inners: copied}
}

// GetInners returns the union's inner plans (read-only).
func (p *RecordQueryUnionPlan) GetInners() []RecordQueryPlan { return p.inners }

// GetResultType returns the first inner's result type, or
// UnknownType if there are no inners.
func (p *RecordQueryUnionPlan) GetResultType() values.Type {
	if len(p.inners) == 0 {
		return values.UnknownType
	}
	return p.inners[0].GetResultType()
}

// GetChildren returns the inner plans.
func (p *RecordQueryUnionPlan) GetChildren() []RecordQueryPlan { return p.inners }

// EqualsWithoutChildren is a constant-discriminated equality —
// union has no operator-specific node-info beyond its children.
func (p *RecordQueryUnionPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	_, ok := other.(*RecordQueryUnionPlan)
	return ok
}

// HashCodeWithoutChildren is a constant for the type discriminator.
func (p *RecordQueryUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("unionplan"))
	return h.Sum64()
}

// Explain renders Union(inner1, inner2, ...).
func (p *RecordQueryUnionPlan) Explain() string {
	parts := make([]string, len(p.inners))
	for i, inner := range p.inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	return fmt.Sprintf("Union(%s)", strings.Join(parts, ", "))
}

var _ RecordQueryPlan = (*RecordQueryUnionPlan)(nil)
