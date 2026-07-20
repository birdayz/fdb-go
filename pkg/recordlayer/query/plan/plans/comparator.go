package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryComparatorPlan is a multi-child plan that executes all
// child plans and compares their results using a comparison key.
// Results from child plans are assumed to all be in a compatible sort
// order. Mirrors Java's RecordQueryComparatorPlan.
//
// Fields:
//   - children: the list of sub-plans whose results are compared.
//   - comparisonKeyValues: values by which the results are compared
//     (equivalent to Java's KeyExpression comparisonKey).
//   - referencePlanIndex: the index of the "reference plan" (source of
//     truth) among the sub-plans.
//   - reverse: whether the children produce results in reverse order.
//   - abortOnComparisonFailure: whether to abort execution when a
//     comparison mismatch is detected (used in testing).
//
// The children are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `children []RecordQueryPlan` slice they replace was a second storage
// location for the same edges. RFC-183 P5 step 2. Child ORDER is doubly
// load-bearing here: referencePlanIndex indexes into it.
type RecordQueryComparatorPlan struct {
	PlanExprBase
	childQs                  []expressions.Quantifier
	comparisonKeyValues      []values.Value
	referencePlanIndex       int
	reverse                  bool
	abortOnComparisonFailure bool
}

// NewRecordQueryComparatorPlan constructs a comparator plan.
// Panics if children is empty or referencePlanIndex is out of range.
func NewRecordQueryComparatorPlan(
	children []RecordQueryPlan,
	comparisonKeyValues []values.Value,
	referencePlanIndex int,
	reverse bool,
	abortOnComparisonFailure bool,
) *RecordQueryComparatorPlan {
	if len(children) == 0 {
		panic("comparator plan should have at least one plan")
	}
	if referencePlanIndex < 0 || referencePlanIndex >= len(children) {
		panic("reference plan index should be within the range of sub plans")
	}
	cpKeys := make([]values.Value, len(comparisonKeyValues))
	copy(cpKeys, comparisonKeyValues)
	return &RecordQueryComparatorPlan{
		childQs:                  QuantifiersOverPlans(children),
		comparisonKeyValues:      cpKeys,
		referencePlanIndex:       referencePlanIndex,
		reverse:                  reverse,
		abortOnComparisonFailure: abortOnComparisonFailure,
	}
}

// GetComparisonKeyValues returns the comparison key values.
func (p *RecordQueryComparatorPlan) GetComparisonKeyValues() []values.Value {
	return p.comparisonKeyValues
}

// GetReferencePlanIndex returns the index of the reference plan.
func (p *RecordQueryComparatorPlan) GetReferencePlanIndex() int { return p.referencePlanIndex }

// IsReverse reports the scan direction.
func (p *RecordQueryComparatorPlan) IsReverse() bool { return p.reverse }

// AbortOnComparisonFailure reports whether mismatches abort execution.
func (p *RecordQueryComparatorPlan) AbortOnComparisonFailure() bool {
	return p.abortOnComparisonFailure
}

// GetResultType returns the first child's result type, or UnknownType
// if there are no children.
func (p *RecordQueryComparatorPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

// GetChildren returns the child plans, dereferenced through the quantifiers
// and in the order referencePlanIndex indexes into.
func (p *RecordQueryComparatorPlan) GetChildren() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// EqualsWithoutChildren compares comparison keys (semantic Value identity —
// see semanticValueEquals), reference index, and reverse flag. The keys join
// equality per RFC-176 §1: before P2, equality checked only the key COUNT
// while the hash folded the full keys, so two plans with equal counts but
// different keys compared equal yet hashed apart — a live plan-level
// equal⟹same-hash violation (a hash-first memo lookup misses the "equal"
// member and inserts a duplicate).
// structuralKey lists the fields that distinguish this comparator in the memo:
// reverse flag, reference-plan index, and the comparison-key Values. Children
// are excluded. abortOnComparisonFailure is intentionally NOT included — it is
// an execution/debug flag (abort vs continue on a detected mismatch), not a
// plan-shape discriminator: two comparators differing only in it produce the
// same rows, so collapsing them in the memo is correct. The hand-rolled
// equals/hash it replaces excluded it for the same reason; this refactor
// preserves that memo identity exactly.
func (p *RecordQueryComparatorPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Bool(p.reverse).
		Int(p.referencePlanIndex).
		Values(p.comparisonKeyValues)
}

func (p *RecordQueryComparatorPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryComparatorPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes comparison keys (semantic Value hashes),
// reference index, and reverse flag.
func (p *RecordQueryComparatorPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("comparatorplan|")
}

// Explain renders Comparator(child1, child2, ..., ref=N).
func (p *RecordQueryComparatorPlan) Explain() string {
	children := p.GetChildren()
	parts := make([]string, len(children))
	for i, child := range children {
		if child == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = child.Explain()
		}
	}
	dir := "ASC"
	if p.reverse {
		dir = "DESC"
	}
	return fmt.Sprintf("Comparator(%s, ref=%d, keys=[%d], %s)",
		strings.Join(parts, ", "), p.referencePlanIndex,
		len(p.comparisonKeyValues), dir)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryComparatorPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryComparatorPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryComparatorPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real child quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQueryComparatorPlan) GetQuantifiers() []expressions.Quantifier {
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
// The arity check is what keeps referencePlanIndex in range: it was validated
// against the child count at construction, so a same-length replacement cannot
// invalidate it.
func (p *RecordQueryComparatorPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryComparatorPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
