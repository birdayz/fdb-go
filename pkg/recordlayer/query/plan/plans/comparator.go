package plans

import (
	"fmt"
	"hash/fnv"
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
type RecordQueryComparatorPlan struct {
	PlanExprBase
	children                 []RecordQueryPlan
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
	cpChildren := make([]RecordQueryPlan, len(children))
	copy(cpChildren, children)
	cpKeys := make([]values.Value, len(comparisonKeyValues))
	copy(cpKeys, comparisonKeyValues)
	return &RecordQueryComparatorPlan{
		children:                 cpChildren,
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
	if len(p.children) == 0 {
		return values.UnknownType
	}
	return p.children[0].GetResultType()
}

// GetChildren returns the child plans.
func (p *RecordQueryComparatorPlan) GetChildren() []RecordQueryPlan { return p.children }

// EqualsWithoutChildren compares comparison keys (semantic Value identity —
// see semanticValueEquals), reference index, and reverse flag. The keys join
// equality per RFC-176 §1: before P2, equality checked only the key COUNT
// while the hash folded the full keys, so two plans with equal counts but
// different keys compared equal yet hashed apart — a live plan-level
// equal⟹same-hash violation (a hash-first memo lookup misses the "equal"
// member and inserts a duplicate).
func (p *RecordQueryComparatorPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryComparatorPlan)
	if !ok {
		return false
	}
	if p.reverse != o.reverse {
		return false
	}
	if p.referencePlanIndex != o.referencePlanIndex {
		return false
	}
	if len(p.comparisonKeyValues) != len(o.comparisonKeyValues) {
		return false
	}
	for i, k := range p.comparisonKeyValues {
		if !semanticValueEquals(k, o.comparisonKeyValues[i]) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes comparison keys (semantic Value hashes),
// reference index, and reverse flag.
func (p *RecordQueryComparatorPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("comparatorplan|"))
	for _, k := range p.comparisonKeyValues {
		writeValueHash(h, k)
	}
	// Mix in referencePlanIndex as two bytes (little-endian).
	h.Write([]byte{byte(p.referencePlanIndex), byte(p.referencePlanIndex >> 8)})
	if p.reverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders Comparator(child1, child2, ..., ref=N).
func (p *RecordQueryComparatorPlan) Explain() string {
	parts := make([]string, len(p.children))
	for i, child := range p.children {
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

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryComparatorPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}
