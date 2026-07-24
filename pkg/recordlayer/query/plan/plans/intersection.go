package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryIntersectionPlan emits the bag-intersection of its
// inner plans — rows that appear in EVERY inner stream, compared by
// the comparison-key columns. Mirrors Java's
// `RecordQueryIntersectionPlan`.
//
// Go's physical form is an ordered N-way intersection. Semantic,
// directional comparison-key parts determine the merge direction, while
// executable comparison values determine row equality. Unsupported
// ordered-bytes key shapes fail closed at construction time.
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
	childQs                    []expressions.Quantifier
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart
	comparisonKeyValues        []values.Value
	reverse                    bool
}

// NewRecordQueryIntersectionPlan constructs an N-way intersection.
// `comparisonKeyValues` defines the row-equality key (typically the
// primary-key columns of the result type). This compatibility constructor
// builds the historical forward, naturally-ascending contract; directional
// producers use NewRecordQueryIntersectionPlanWithOrdering.
func NewRecordQueryIntersectionPlan(inners []RecordQueryPlan, comparisonKeyValues []values.Value) *RecordQueryIntersectionPlan {
	parts := ascendingIntersectionParts(comparisonKeyValues)
	return newRecordQueryIntersectionPlan(
		QuantifiersOverPlans(inners), parts, comparisonKeyValues, false,
	)
}

// NewRecordQueryIntersectionPlanWithOrdering constructs an intersection from
// its semantic comparison ordering. The executable comparison Values are
// derived from those parts so the two contracts cannot disagree. A nil result
// means at least one part requires ToOrderedBytesValue evaluation, which Go
// does not support yet; callers must treat that as an optimization miss.
func NewRecordQueryIntersectionPlanWithOrdering(
	inners []RecordQueryPlan,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	reverse bool,
) *RecordQueryIntersectionPlan {
	comparisonKeyValues, ok := properties.NaturalComparisonKeyValues(comparisonKeyOrderingParts, reverse)
	if !ok {
		return nil
	}
	return newRecordQueryIntersectionPlan(
		QuantifiersOverPlans(inners), comparisonKeyOrderingParts, comparisonKeyValues, reverse,
	)
}

// NewRecordQueryIntersectionPlanFromQuantifiers builds an N-way intersection
// whose legs are LIVE memo quantifiers (the implementation / data-access rules
// pass ForEachQuantifiers over the freshly-memoized leg winners) instead of
// snapshots over plans. This makes the intersection its own cascades expression
// carrying its leg edges directly — the memo holds it without a physical wrapper
// (RFC-184 W2). comparisonKeyValues carries over verbatim.
func NewRecordQueryIntersectionPlanFromQuantifiers(qs []expressions.Quantifier, comparisonKeyValues []values.Value) *RecordQueryIntersectionPlan {
	parts := ascendingIntersectionParts(comparisonKeyValues)
	return newRecordQueryIntersectionPlan(qs, parts, comparisonKeyValues, false)
}

// NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering is the live-memo
// counterpart of NewRecordQueryIntersectionPlanWithOrdering. A nil result is
// the same fail-closed ordered-byte optimization miss.
func NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(
	qs []expressions.Quantifier,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	reverse bool,
) *RecordQueryIntersectionPlan {
	comparisonKeyValues, ok := properties.NaturalComparisonKeyValues(comparisonKeyOrderingParts, reverse)
	if !ok {
		return nil
	}
	return newRecordQueryIntersectionPlan(
		qs, comparisonKeyOrderingParts, comparisonKeyValues, reverse,
	)
}

func newRecordQueryIntersectionPlan(
	qs []expressions.Quantifier,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	comparisonKeyValues []values.Value,
	reverse bool,
) *RecordQueryIntersectionPlan {
	return &RecordQueryIntersectionPlan{
		childQs:                    append([]expressions.Quantifier(nil), qs...),
		comparisonKeyOrderingParts: append([]properties.ProvidedOrderingPart(nil), comparisonKeyOrderingParts...),
		comparisonKeyValues:        append([]values.Value(nil), comparisonKeyValues...),
		reverse:                    reverse,
	}
}

func ascendingIntersectionParts(comparisonKeyValues []values.Value) []properties.ProvidedOrderingPart {
	parts := make([]properties.ProvidedOrderingPart, len(comparisonKeyValues))
	for i, value := range comparisonKeyValues {
		parts[i] = properties.ProvidedOrderingPart{
			Value:     value,
			SortOrder: properties.ProvidedSortOrderAscending,
		}
	}
	return parts
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

// GetComparisonKeyOrderingParts returns the semantic output ordering used to
// derive the executable comparison key (read-only).
func (p *RecordQueryIntersectionPlan) GetComparisonKeyOrderingParts() []properties.ProvidedOrderingPart {
	return p.comparisonKeyOrderingParts
}

// IsReverse reports whether the merge compares descending physical keys.
func (p *RecordQueryIntersectionPlan) IsReverse() bool { return p.reverse }

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

// structuralKey lists the fields that distinguish this intersection in the
// memo: the comparison-key Value list (semantic Value identity, per Java
// RecordQueryIntersectionPlan.equalsWithoutChildren — comparisonKeyFunction
// plus reverse). Children and the transient semantic ordering parts are
// excluded, matching Java RecordQueryIntersectionOnValuesPlan: physical
// comparison Values + reverse define executable identity.
func (p *RecordQueryIntersectionPlan) structuralKey() *structuralKey {
	return newStructuralKey().Values(p.comparisonKeyValues).Bool(p.reverse)
}

func (p *RecordQueryIntersectionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIntersectionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren folds the type discriminator + the comparison-key
// Values (semantic hashes — see writeValueHash), pairing with the semantic
// key equality above so equal⟹same-hash holds.
func (p *RecordQueryIntersectionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("intersectionplan")
}

// Explain renders Intersection(inner1, inner2, ...), appending REVERSE only
// for descending plans so the existing forward corpus remains byte-identical.
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
	explain := fmt.Sprintf("Intersection(%s)", strings.Join(parts, ", "))
	if p.reverse {
		explain += " REVERSE"
	}
	return explain
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

// ChildrenAsSet reports that the legs of this set operation are commutative.
func (p *RecordQueryIntersectionPlan) ChildrenAsSet() bool { return true }

// IsIntersection implements properties.IntersectionExpression — the marker
// ComparisonsProperty.EvaluateComparisons keys on to intersect (not union) its
// children's comparison sets. Adopted from the retired physicalIntersectionWrapper
// (RFC-184 W2) so the memo member the property walks still reports it.
func (p *RecordQueryIntersectionPlan) IsIntersection() {}

// GetResultValue returns the first leg's flowed object value — intersection
// emits rows compatible with all legs. Adopted from the retired
// physicalIntersectionWrapper (RFC-184 W2); an empty intersection falls back to
// PlanExprBase's fresh stand-in.
func (p *RecordQueryIntersectionPlan) GetResultValue() values.Value {
	if len(p.childQs) == 0 {
		return p.PlanExprBase.GetResultValue()
	}
	return p.childQs[0].GetFlowedObjectValue()
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The intersection carries its legs as LIVE memo edges, so the relink
// is a quantifier swap: WithQuantifiers rebinds the legs and GetInners re-resolves
// through the new references (RFC-184 W2, replacing physicalIntersectionWrapper.WithChildren).
func (p *RecordQueryIntersectionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryIntersectionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
