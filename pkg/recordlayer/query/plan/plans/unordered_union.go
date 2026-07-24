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
// Go's RecordQueryUnionPlan is another concatenating, no-dedup UNION ALL
// implementation; it is not merge-sorted. Ordered behavior lives in
// RecordQueryMergeSortUnionPlan. The two concat plans currently differ in
// execution/continuation machinery and are tracked for taxonomy cleanup.
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

// NewRecordQueryUnorderedUnionPlanFromQuantifiers builds the union over the LIVE
// leg quantifiers the implement rule memoized. Each leg is a SHARED multi-member
// group whose per-ordering winner is resolved at extraction via ref.Winner()
// (planFromQuantifier) — the deferred-winner set-op case. The plan carries each
// leg edge once, with no wrapper snapshot (RFC-184 W2).
func NewRecordQueryUnorderedUnionPlanFromQuantifiers(childQs []expressions.Quantifier) *RecordQueryUnorderedUnionPlan {
	cp := make([]expressions.Quantifier, len(childQs))
	copy(cp, childQs)
	return &RecordQueryUnorderedUnionPlan{childQs: cp}
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
	// TYPE resolution, not identity: the legs are deferred-winner groups still
	// multi-member during planning; every alternative shares the row shape, so
	// any member answers without tripping the singleton guard (RFC-184 W2).
	return planTypeFromQuantifier(p.childQs[0]).GetResultType()
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
// the concatenation imposes no order, so any leg order is equivalent.
func (p *RecordQueryUnorderedUnionPlan) ChildrenAsSet() bool { return true }

// GetResultValue flows the first leg's object value (union legs are
// column-aligned, so any leg's shape stands in); falls back to a fresh
// quantified object value when there are no legs.
func (p *RecordQueryUnorderedUnionPlan) GetResultValue() values.Value {
	if len(p.childQs) == 0 {
		return p.PlanExprBase.GetResultValue()
	}
	return p.childQs[0].GetFlowedObjectValue()
}

// WithChildren rebuilds over fresh leg quantifiers — the interface plan
// extraction uses. Delegates to WithQuantifiers; the leg count must match.
func (p *RecordQueryUnorderedUnionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != len(p.childQs) {
		return nil, fmt.Errorf("RecordQueryUnorderedUnionPlan.WithChildren: expected %d legs, got %d", len(p.childQs), len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUnorderedUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
