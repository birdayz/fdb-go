package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryUnorderedUnionPlan emits the rows of all input plans
// concatenated without any ordering guarantee. Mirrors Java's
// RecordQueryUnorderedUnionPlan.
//
// Distinct from RecordQueryUnionPlan which does merge-sorted output.
// This plan simply concatenates children in implementation order.
type RecordQueryUnorderedUnionPlan struct {
	PlanExprBase
	inners []RecordQueryPlan
}

func NewRecordQueryUnorderedUnionPlan(inners []RecordQueryPlan) *RecordQueryUnorderedUnionPlan {
	copied := make([]RecordQueryPlan, len(inners))
	copy(copied, inners)
	return &RecordQueryUnorderedUnionPlan{inners: copied}
}

func (p *RecordQueryUnorderedUnionPlan) GetInners() []RecordQueryPlan { return p.inners }

func (p *RecordQueryUnorderedUnionPlan) GetResultType() values.Type {
	if len(p.inners) == 0 {
		return values.UnknownType
	}
	return p.inners[0].GetResultType()
}

func (p *RecordQueryUnorderedUnionPlan) GetChildren() []RecordQueryPlan { return p.inners }

func (p *RecordQueryUnorderedUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	_, ok := other.(*RecordQueryUnorderedUnionPlan)
	return ok
}

func (p *RecordQueryUnorderedUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("unorderedunionplan"))
	return h.Sum64()
}

func (p *RecordQueryUnorderedUnionPlan) Explain() string {
	parts := make([]string, len(p.inners))
	for i, inner := range p.inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	return fmt.Sprintf("UnorderedUnion(%s)", strings.Join(parts, ", "))
}

var (
	_ RecordQueryPlan                  = (*RecordQueryUnorderedUnionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryUnorderedUnionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryUnorderedUnionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryUnorderedUnionPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// ChildrenAsSet reports that the legs of this set operation are commutative,
// mirroring physicalUnorderedUnionWrapper.
func (p *RecordQueryUnorderedUnionPlan) ChildrenAsSet() bool { return true }
