package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
//
// The legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `inners []RecordQueryPlan` slice they replace was a second storage location
// for the same edges. RFC-183 P5 step 2.
type RecordQueryUnionPlan struct {
	PlanExprBase
	childQs []expressions.Quantifier
}

// NewRecordQueryUnionPlan constructs a UNION ALL over the given
// inner plans.
func NewRecordQueryUnionPlan(inners []RecordQueryPlan) *RecordQueryUnionPlan {
	return &RecordQueryUnionPlan{childQs: QuantifiersOverPlans(inners)}
}

// NewRecordQueryUnionPlanFromQuantifiers builds the union directly over the
// LIVE leg quantifiers the implement rule already memoized, rather than
// snapshotting bare plans. The plan is then its own cascades expression
// carrying the leg edges once — no wrapper storing a second copy (RFC-184 W2).
func NewRecordQueryUnionPlanFromQuantifiers(childQs []expressions.Quantifier) *RecordQueryUnionPlan {
	cp := make([]expressions.Quantifier, len(childQs))
	copy(cp, childQs)
	return &RecordQueryUnionPlan{childQs: cp}
}

// GetInners returns the union's inner plans, dereferenced through the
// quantifiers and in leg order.
func (p *RecordQueryUnionPlan) GetInners() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// GetResultType returns the first inner's result type, or
// UnknownType if there are no inners.
func (p *RecordQueryUnionPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

// GetChildren returns the inner plans.
func (p *RecordQueryUnionPlan) GetChildren() []RecordQueryPlan { return p.GetInners() }

// structuralKey carries no fields — union has no operator-specific node-info
// beyond its children, so identity is the type discriminator alone. The same
// key drives both EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryUnionPlan) structuralKey() *structuralKey {
	return newStructuralKey()
}

// EqualsWithoutChildren is a constant-discriminated equality —
// union has no operator-specific node-info beyond its children.
func (p *RecordQueryUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryUnionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren is a constant for the type discriminator.
func (p *RecordQueryUnionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("unionplan")
}

// Explain renders Union(inner1, inner2, ...).
func (p *RecordQueryUnionPlan) Explain() string {
	inners := p.GetInners()
	parts := make([]string, len(inners))
	for i, inner := range inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	return fmt.Sprintf("Union(%s)", strings.Join(parts, ", "))
}

var (
	_ RecordQueryPlan                  = (*RecordQueryUnionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryUnionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryUnionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real leg quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQueryUnionPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
func (p *RecordQueryUnionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// ChildrenAsSet reports that the legs of this set operation are commutative —
// UNION children are bag-equivalent regardless of order.
func (p *RecordQueryUnionPlan) ChildrenAsSet() bool { return true }

// GetResultValue flows the first leg's object value (union legs are
// column-aligned by construction, so any leg's row shape stands in). Falls back
// to a fresh quantified object value when there are no legs.
func (p *RecordQueryUnionPlan) GetResultValue() values.Value {
	if len(p.childQs) == 0 {
		return p.PlanExprBase.GetResultValue()
	}
	return p.childQs[0].GetFlowedObjectValue()
}

// WithChildren rebuilds over fresh leg quantifiers — the optional interface
// plan extraction uses to preserve the strict-singleton invariant. Delegates to
// WithQuantifiers; the leg count must match.
func (p *RecordQueryUnionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != len(p.childQs) {
		return nil, fmt.Errorf("RecordQueryUnionPlan.WithChildren: expected %d legs, got %d", len(p.childQs), len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
