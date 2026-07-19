package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
//
// The legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQuerySetPlan`'s `List<Quantifier.Physical> quantifiers`). The raw
// `inners []RecordQueryPlan` slice they replace was a second storage location
// for the same edges. RFC-183 P5 step 2.
type RecordQueryIntersectionPlan struct {
	PlanExprBase
	childQs             []expressions.Quantifier
	comparisonKeyValues []values.Value
}

// NewRecordQueryIntersectionPlan constructs an N-way intersection.
// `comparisonKeyValues` defines the row-equality key (typically the
// primary-key columns of the result type).
func NewRecordQueryIntersectionPlan(inners []RecordQueryPlan, comparisonKeyValues []values.Value) *RecordQueryIntersectionPlan {
	cpKeys := make([]values.Value, len(comparisonKeyValues))
	copy(cpKeys, comparisonKeyValues)
	return &RecordQueryIntersectionPlan{
		childQs:             QuantifiersOverPlans(inners),
		comparisonKeyValues: cpKeys,
	}
}

// GetInners returns the intersection's inner plans, dereferenced through the
// quantifiers and in leg order.
func (p *RecordQueryIntersectionPlan) GetInners() []RecordQueryPlan {
	return plansFromQuantifiers(p.childQs)
}

// GetComparisonKeyValues returns the row-equality key list (read-only).
func (p *RecordQueryIntersectionPlan) GetComparisonKeyValues() []values.Value {
	return p.comparisonKeyValues
}

// GetResultType returns the first inner's result type, or
// UnknownType if there are no inners.
func (p *RecordQueryIntersectionPlan) GetResultType() values.Type {
	if len(p.childQs) == 0 {
		return values.UnknownType
	}
	return planFromQuantifier(p.childQs[0]).GetResultType()
}

// GetChildren returns the inner plans.
func (p *RecordQueryIntersectionPlan) GetChildren() []RecordQueryPlan { return p.GetInners() }

// EqualsWithoutChildren matches IntersectionPlan + comparison-key list by
// semantic Value identity, per Java RecordQueryIntersectionPlan.
// equalsWithoutChildren (comparisonKeyFunction.equals). Java also compares
// `reverse`; Go's intersection has no reverse field because the implement
// rules only emit forward intersections — when reverse intersections land,
// the field joins identity here and in the hash.
func (p *RecordQueryIntersectionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIntersectionPlan)
	if !ok {
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

// HashCodeWithoutChildren folds the type discriminator + the comparison-key
// Values (semantic hashes — see writeValueHash), pairing with the semantic
// key equality above so equal⟹same-hash holds.
func (p *RecordQueryIntersectionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("intersectionplan"))
	for _, k := range p.comparisonKeyValues {
		writeValueHash(h, k)
	}
	return h.Sum64()
}

// Explain renders Intersection(inner1, inner2, ...).
func (p *RecordQueryIntersectionPlan) Explain() string {
	inners := p.GetInners()
	parts := make([]string, len(inners))
	for i, inner := range inners {
		if inner == nil {
			parts[i] = "<nil>"
		} else {
			parts[i] = inner.Explain()
		}
	}
	return fmt.Sprintf("Intersection(%s)", strings.Join(parts, ", "))
}

var (
	_ RecordQueryPlan                  = (*RecordQueryIntersectionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryIntersectionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryIntersectionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetQuantifiers reports the real leg quantifiers, overriding PlanExprBase's
// none.
func (p *RecordQueryIntersectionPlan) GetQuantifiers() []expressions.Quantifier {
	if len(p.childQs) == 0 {
		return nil
	}
	return p.childQs
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers —
// Java's copy-on-write withChildrenReferences. The receiver is never mutated,
// which is what keeps a memoized plan safe to share; the incoming slice is
// copied so the caller cannot alias the copy's storage either.
func (p *RecordQueryIntersectionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != len(p.childQs) {
		return p
	}
	cp := *p
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	return &cp
}

// ChildrenAsSet reports that the legs of this set operation are commutative,
// mirroring physicalIntersectionWrapper.
func (p *RecordQueryIntersectionPlan) ChildrenAsSet() bool { return true }
