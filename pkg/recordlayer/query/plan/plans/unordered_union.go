package plans

import (
	"fmt"
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
//
// The legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `inners []RecordQueryPlan` slice they replace was a second storage location
// for the same edges. RFC-183 P5 step 2.
type RecordQueryUnorderedUnionPlan struct {
	PlanExprBase
	childQs []expressions.Quantifier
}

func NewRecordQueryUnorderedUnionPlan(inners []RecordQueryPlan) *RecordQueryUnorderedUnionPlan {
	return &RecordQueryUnorderedUnionPlan{childQs: QuantifiersOverPlans(inners)}
}

// GetInners returns the legs, dereferenced through the quantifiers. Order is
// preserved even though the union imposes none on its OUTPUT: it is the
// concatenation order the executor consumes the legs in.
func (p *RecordQueryUnorderedUnionPlan) GetInners() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

func (p *RecordQueryUnorderedUnionPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

func (p *RecordQueryUnorderedUnionPlan) GetChildren() []RecordQueryPlan { return p.GetInners() }

// structuralKey carries no fields — the unordered union has no
// operator-specific node-info beyond its children, so identity is the type
// discriminator alone. The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren.
func (p *RecordQueryUnorderedUnionPlan) structuralKey() *structuralKey {
	return newStructuralKey()
}

func (p *RecordQueryUnorderedUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUnorderedUnionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryUnorderedUnionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("unorderedunionplan")
}

func (p *RecordQueryUnorderedUnionPlan) Explain() string {
	inners := p.GetInners()
	parts := make([]string, len(inners))
	for i, inner := range inners {
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

// GetQuantifiers reports the real leg quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQueryUnorderedUnionPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
func (p *RecordQueryUnorderedUnionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// ChildrenAsSet reports that the legs of this set operation are commutative,
// mirroring physicalUnorderedUnionWrapper.
func (p *RecordQueryUnorderedUnionPlan) ChildrenAsSet() bool { return true }

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUnorderedUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
