package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PlanSelector selects one child plan index from a list at runtime.
// Mirrors Java's PlanSelector interface.
type PlanSelector interface {
	// SelectPlan picks the index of the plan to execute.
	SelectPlan(plans []RecordQueryPlan) int
	// Equals reports equality with another PlanSelector.
	Equals(other PlanSelector) bool
	// String returns a human-readable label.
	String() string
}

// RelativeProbabilityPlanSelector selects a child plan based on
// relative probabilities. Mirrors Java's inner
// RelativeProbabilityPlanSelector class.
type RelativeProbabilityPlanSelector struct {
	probabilities []int
}

// NewRelativeProbabilityPlanSelector constructs the selector.
// The sum of probabilities must be 100.
func NewRelativeProbabilityPlanSelector(probabilities []int) *RelativeProbabilityPlanSelector {
	cp := make([]int, len(probabilities))
	copy(cp, probabilities)
	return &RelativeProbabilityPlanSelector{probabilities: cp}
}

// SelectPlan picks a plan index based on the probabilities.
// (Structural port only; the random-weighted selection logic belongs
// in the execution layer.)
func (s *RelativeProbabilityPlanSelector) SelectPlan(_ []RecordQueryPlan) int {
	return 0 // placeholder — execution logic is out of scope
}

// Equals compares probability lists.
func (s *RelativeProbabilityPlanSelector) Equals(other PlanSelector) bool {
	o, ok := other.(*RelativeProbabilityPlanSelector)
	if !ok {
		return false
	}
	if len(s.probabilities) != len(o.probabilities) {
		return false
	}
	for i := range s.probabilities {
		if s.probabilities[i] != o.probabilities[i] {
			return false
		}
	}
	return true
}

// String renders the probability list.
func (s *RelativeProbabilityPlanSelector) String() string {
	parts := make([]string, len(s.probabilities))
	for i, p := range s.probabilities {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return fmt.Sprintf("RelativeProb(%s)", strings.Join(parts, ", "))
}

// GetProbabilities returns the probability list.
func (s *RelativeProbabilityPlanSelector) GetProbabilities() []int { return s.probabilities }

// RecordQuerySelectorPlan selects one of its children to be executed
// at runtime. The selector determines which child plan to use via
// a PlanSelector policy. Mirrors Java's RecordQuerySelectorPlan.
//
// The children are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `children []RecordQueryPlan` slice they replace was a second storage
// location for the same edges. RFC-183 P5 step 2. Child ORDER is load-bearing:
// PlanSelector returns an index into it.
type RecordQuerySelectorPlan struct {
	PlanExprBase
	childQs      []expressions.Quantifier
	planSelector PlanSelector
	reverse      bool
}

// NewRecordQuerySelectorPlan constructs a selector plan.
// Panics if children is empty.
func NewRecordQuerySelectorPlan(
	children []RecordQueryPlan,
	planSelector PlanSelector,
	reverse bool,
) *RecordQuerySelectorPlan {
	if len(children) == 0 {
		panic("selector plan should have at least one plan")
	}
	return &RecordQuerySelectorPlan{
		childQs:      QuantifiersOverPlans(children),
		planSelector: planSelector,
		reverse:      reverse,
	}
}

// NewRecordQuerySelectorPlanWithProbabilities constructs a selector
// plan using relative probabilities. Panics if the list lengths differ
// or children is empty.
func NewRecordQuerySelectorPlanWithProbabilities(
	children []RecordQueryPlan,
	probabilities []int,
	reverse bool,
) *RecordQuerySelectorPlan {
	if len(children) != len(probabilities) {
		panic("number of plans and number of relative probabilities should be the same")
	}
	return NewRecordQuerySelectorPlan(
		children,
		NewRelativeProbabilityPlanSelector(probabilities),
		reverse,
	)
}

// GetPlanSelector returns the plan selector.
func (p *RecordQuerySelectorPlan) GetPlanSelector() PlanSelector { return p.planSelector }

// IsReverse reports the scan direction.
func (p *RecordQuerySelectorPlan) IsReverse() bool { return p.reverse }

// GetResultType returns the first child's result type, or UnknownType
// if there are no children.
func (p *RecordQuerySelectorPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

// GetChildren returns the child plans, dereferenced through the quantifiers
// and in the order PlanSelector's index refers to.
func (p *RecordQuerySelectorPlan) GetChildren() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// structuralKey folds the selector identity: reverse flag and the PlanSelector
// via its own .Equals() (Equatable), hashed by its stable .String() label — the
// exact primitives the hand-rolled equals/hash used. Drives both Equals and Hash.
func (p *RecordQuerySelectorPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Bool(p.reverse).
		Equatable(p.planSelector, func(other any) bool {
			o, ok := other.(PlanSelector)
			return ok && p.planSelector.Equals(o)
		}, []byte(p.planSelector.String()))
}

// EqualsWithoutChildren compares reverse flag and plan selector.
func (p *RecordQuerySelectorPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQuerySelectorPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes reverse flag and plan selector label.
func (p *RecordQuerySelectorPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("selectorplan|")
}

// Explain renders Selector(child1, child2, ..., selector).
func (p *RecordQuerySelectorPlan) Explain() string {
	children := p.GetChildren()
	parts := make([]string, len(children))
	for i, child := range children {
		if child == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = child.Explain()
		}
	}
	return fmt.Sprintf("Selector(%s, %s)",
		strings.Join(parts, ", "), p.planSelector.String())
}

var (
	_ RecordQueryPlan                  = (*RecordQuerySelectorPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQuerySelectorPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQuerySelectorPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real child quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQuerySelectorPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given child quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
//
// The arity check keeps the PlanSelector's index meaningful: a probability
// list is sized to the child count at construction, so only a same-length
// replacement is admissible.
func (p *RecordQuerySelectorPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQuerySelectorPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
