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
	comparisonKeysOnCarrier    bool
	reverse                    bool
}

// NewRecordQueryIntersectionPlan constructs an N-way intersection.
// `comparisonKeyValues` defines the row-equality key (typically the
// primary-key columns of the result type). This compatibility constructor
// builds the historical forward, naturally-ascending contract; directional
// producers use NewRecordQueryIntersectionPlanWithOrdering.
func NewRecordQueryIntersectionPlan(inners []RecordQueryPlan, comparisonKeyValues []values.Value) (*RecordQueryIntersectionPlan, error) {
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
) (*RecordQueryIntersectionPlan, error) {
	comparisonKeyValues, ok := properties.NaturalComparisonKeyValues(comparisonKeyOrderingParts, reverse)
	if !ok {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan: comparison ordering cannot be evaluated naturally")
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
func NewRecordQueryIntersectionPlanFromQuantifiers(qs []expressions.Quantifier, comparisonKeyValues []values.Value) (*RecordQueryIntersectionPlan, error) {
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
) (*RecordQueryIntersectionPlan, error) {
	comparisonKeyValues, ok := properties.NaturalComparisonKeyValues(comparisonKeyOrderingParts, reverse)
	if !ok {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan: comparison ordering cannot be evaluated naturally")
	}
	return newRecordQueryIntersectionPlan(
		qs, comparisonKeyOrderingParts, comparisonKeyValues, reverse,
	)
}

// NewRecordQueryIntersectionPlanFromQuantifiersWithOrderingAndSource is the
// exact-carrier constructor used when the ordering proof declares one physical
// current-row phase for every comparison key. The source handle is authority:
// only top-level fields rooted at that exact QOV are moved onto the
// intersection's provided-output carrier. A same-shaped current QOV minted by
// another layout is not interchangeable and fails construction.
func NewRecordQueryIntersectionPlanFromQuantifiersWithOrderingAndSource(
	qs []expressions.Quantifier,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	reverse bool,
	comparisonKeySource values.QuantifiedObjectValue,
) (*RecordQueryIntersectionPlan, error) {
	comparisonKeyValues, ok := properties.NaturalComparisonKeyValues(comparisonKeyOrderingParts, reverse)
	if !ok {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan: comparison ordering cannot be evaluated naturally")
	}
	return newRecordQueryIntersectionPlanWithSource(
		qs, comparisonKeyOrderingParts, comparisonKeyValues, reverse, comparisonKeySource,
	)
}

func newRecordQueryIntersectionPlan(
	qs []expressions.Quantifier,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	comparisonKeyValues []values.Value,
	reverse bool,
) (*RecordQueryIntersectionPlan, error) {
	base, err := newPlanExprBaseForFirstQuantifier("RecordQueryIntersectionPlan", qs)
	if err != nil {
		return nil, err
	}
	return &RecordQueryIntersectionPlan{
		PlanExprBase:               base,
		childQs:                    append([]expressions.Quantifier(nil), qs...),
		comparisonKeyOrderingParts: append([]properties.ProvidedOrderingPart(nil), comparisonKeyOrderingParts...),
		comparisonKeyValues:        append([]values.Value(nil), comparisonKeyValues...),
		reverse:                    reverse,
	}, nil
}

func newRecordQueryIntersectionPlanWithSource(
	qs []expressions.Quantifier,
	comparisonKeyOrderingParts []properties.ProvidedOrderingPart,
	comparisonKeyValues []values.Value,
	reverse bool,
	comparisonKeySource values.QuantifiedObjectValue,
) (*RecordQueryIntersectionPlan, error) {
	base, err := newPlanExprBaseForFirstQuantifier("RecordQueryIntersectionPlan", qs)
	if err != nil {
		return nil, err
	}
	layout, err := base.ProvidedOutputLayout()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan comparison-key output layout: %w", err)
	}
	normalizedParts, err := normalizeIntersectionOrderingParts(
		comparisonKeyOrderingParts, comparisonKeySource, layout.Carrier())
	if err != nil {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan comparison key: %w", err)
	}
	normalizedValues, ok := properties.NaturalComparisonKeyValues(normalizedParts, reverse)
	if !ok || len(normalizedValues) != len(comparisonKeyValues) {
		return nil, fmt.Errorf("RecordQueryIntersectionPlan: normalized comparison ordering cannot be evaluated naturally")
	}
	return &RecordQueryIntersectionPlan{
		PlanExprBase:               base,
		childQs:                    append([]expressions.Quantifier(nil), qs...),
		comparisonKeyOrderingParts: normalizedParts,
		comparisonKeyValues:        normalizedValues,
		comparisonKeysOnCarrier:    true,
		reverse:                    reverse,
	}, nil
}

func normalizeIntersectionOrderingParts(
	parts []properties.ProvidedOrderingPart,
	source values.QuantifiedObjectValue,
	target values.QuantifiedObjectValue,
) ([]properties.ProvidedOrderingPart, error) {
	if source == nil || source.Correlation() != values.CurrentCorrelation() {
		return nil, fmt.Errorf("comparison-key source must be an exact current carrier")
	}
	normalized := make([]properties.ProvidedOrderingPart, len(parts))
	for i, part := range parts {
		field, isField := values.AsFieldValue(part.Value)
		if !isField || field.Path() == nil || field.Path().Len() != 1 {
			return nil, fmt.Errorf("part %d is not an exact top-level field", i)
		}
		root, isRoot := values.AsQuantifiedObjectValue(field.ChildValue())
		if !isRoot || root != source {
			return nil, fmt.Errorf("part %d is not rooted at the declared comparison-key source", i)
		}
		translated, err := values.TranslatePhaseRoot(part.Value, source, target)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		normalized[i] = properties.ProvidedOrderingPart{
			Value:     translated,
			SortOrder: part.SortOrder,
		}
	}
	return normalized, nil
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
func (p *RecordQueryIntersectionPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

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
func (p *RecordQueryIntersectionPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryIntersectionPlan", len(qs), len(p.childQs)); err != nil {
		return nil, err
	}
	cp := *p
	base, err := newPlanExprBaseForFirstQuantifier("RecordQueryIntersectionPlan", qs)
	if err != nil {
		return nil, err
	}
	cp.PlanExprBase = base
	cp.childQs = append([]expressions.Quantifier(nil), qs...)
	if p.comparisonKeysOnCarrier {
		oldLayout, oldLayoutErr := p.ProvidedOutputLayout()
		if oldLayoutErr != nil {
			return nil, fmt.Errorf("RecordQueryIntersectionPlan old comparison-key output layout: %w", oldLayoutErr)
		}
		newLayout, newLayoutErr := base.ProvidedOutputLayout()
		if newLayoutErr != nil {
			return nil, fmt.Errorf("RecordQueryIntersectionPlan new comparison-key output layout: %w", newLayoutErr)
		}
		normalizedParts, normalizeErr := normalizeIntersectionOrderingParts(
			p.comparisonKeyOrderingParts, oldLayout.Carrier(), newLayout.Carrier())
		if normalizeErr != nil {
			return nil, fmt.Errorf("RecordQueryIntersectionPlan relink comparison key: %w", normalizeErr)
		}
		normalizedValues, natural := properties.NaturalComparisonKeyValues(normalizedParts, p.reverse)
		if !natural {
			return nil, fmt.Errorf("RecordQueryIntersectionPlan: relinked comparison ordering cannot be evaluated naturally")
		}
		cp.comparisonKeyOrderingParts = normalizedParts
		cp.comparisonKeyValues = normalizedValues
	}
	return &cp, nil
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
	return p.PlanExprBase.GetResultValue()
}

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The intersection carries its legs as LIVE memo edges, so the relink
// is a quantifier swap: WithQuantifiers rebinds the legs and GetInners re-resolves
// through the new references (RFC-184 W2, replacing physicalIntersectionWrapper.WithChildren).
func (p *RecordQueryIntersectionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryIntersectionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
