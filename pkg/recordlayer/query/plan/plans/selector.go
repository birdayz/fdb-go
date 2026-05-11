package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
type RecordQuerySelectorPlan struct {
	children     []RecordQueryPlan
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
	cpChildren := make([]RecordQueryPlan, len(children))
	copy(cpChildren, children)
	return &RecordQuerySelectorPlan{
		children:     cpChildren,
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
	if len(p.children) == 0 {
		return values.UnknownType
	}
	return p.children[0].GetResultType()
}

// GetChildren returns the child plans.
func (p *RecordQuerySelectorPlan) GetChildren() []RecordQueryPlan { return p.children }

// EqualsWithoutChildren compares reverse flag and plan selector.
func (p *RecordQuerySelectorPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQuerySelectorPlan)
	if !ok {
		return false
	}
	return p.reverse == o.reverse && p.planSelector.Equals(o.planSelector)
}

// HashCodeWithoutChildren mixes reverse flag and plan selector label.
func (p *RecordQuerySelectorPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("selectorplan|"))
	if p.reverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write([]byte(p.planSelector.String()))
	return h.Sum64()
}

// Explain renders Selector(child1, child2, ..., selector).
func (p *RecordQuerySelectorPlan) Explain() string {
	parts := make([]string, len(p.children))
	for i, child := range p.children {
		if child == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = child.Explain()
		}
	}
	return fmt.Sprintf("Selector(%s, %s)",
		strings.Join(parts, ", "), p.planSelector.String())
}

var _ RecordQueryPlan = (*RecordQuerySelectorPlan)(nil)
