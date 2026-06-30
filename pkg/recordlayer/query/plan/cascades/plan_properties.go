package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PlanPropertiesMap stores computed property values for each physical-plan
// wrapper expression in a Reference's final members.
type PlanPropertiesMap struct {
	props map[expressions.RelationalExpression]properties.PropertyMap
	order []expressions.RelationalExpression // insertion order
}

// NewPlanPropertiesMap creates a new empty properties map.
func NewPlanPropertiesMap() *PlanPropertiesMap {
	return &PlanPropertiesMap{
		props: make(map[expressions.RelationalExpression]properties.PropertyMap),
	}
}

// Add computes and stores properties for the given physical wrapper.
func (m *PlanPropertiesMap) Add(w physicalPlanExpression) {
	if _, exists := m.props[w]; !exists {
		m.order = append(m.order, w)
	}
	m.props[w] = computeWrapperProperties(w)
}

// GetProperties returns the computed properties for a wrapper expression.
func (m *PlanPropertiesMap) GetProperties(expr expressions.RelationalExpression) properties.PropertyMap {
	return m.props[expr]
}

// Expressions returns all wrapper expressions in insertion order.
func (m *PlanPropertiesMap) Expressions() []expressions.RelationalExpression {
	return m.order
}

// All returns the full underlying map. Callers that need deterministic
// iteration should use Expressions() and GetProperties() instead.
func (m *PlanPropertiesMap) All() map[expressions.RelationalExpression]properties.PropertyMap {
	return m.props
}

func computeWrapperProperties(w physicalPlanExpression) properties.PropertyMap {
	plan := w.GetRecordQueryPlan()
	return properties.PropertyMap{
		properties.PropDistinctRecords: computeDistinctRecords(w, plan),
		properties.PropStoredRecord:    computeStoredRecord(plan),
		properties.PropPrimaryKey:      computePrimaryKey(plan),
		properties.PropOrdering:        computeWrapperOrdering(w),
		properties.PropCardinalities:   computeCardinalities(w, plan),
		properties.PropDerivations:     ComputeDerivations(w),
	}
}

func computeDistinctRecords(w physicalPlanExpression, plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryScanPlan:
		return true
	case *plans.RecordQueryIndexPlan:
		if iw, ok := w.(*physicalIndexScanWrapper); ok {
			return iw.unique
		}
		return false
	case *plans.RecordQueryProjectionPlan:
		// A SQL-level projection reshapes the output (selects specific
		// columns); two different underlying records can project to the
		// same value tuple, so record-level distinctness is NOT preserved.
		return false
	case *plans.RecordQueryMapPlan:
		return computeDistinctRecordsForMap(w)
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan,
		*plans.RecordQueryTempTableInsertPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryFirstOrDefaultPlan:
		return true
	case *plans.RecordQueryDefaultOnEmptyPlan,
		*plans.RecordQueryInJoinPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryMultiIntersectionOnValuesPlan,
		*plans.RecordQueryInUnionPlan:
		return true
	case *plans.RecordQueryUnorderedUnionPlan,
		// RecordQueryUnionPlan is Go's NO-DEDUP UNION ALL variant (union.go:
		// "UNION ALL with no dedup"; executeUnion concatenates branches). It
		// does NOT remove duplicates, so it must NOT report distinct records —
		// otherwise ImplementDistinctFinalRule treats its partition as already
		// distinct and elides an enclosing SELECT DISTINCT, returning dups.
		*plans.RecordQueryUnionPlan:
		return false
	default:
		return false
	}
}

func distinctRecordsFromChildRef(w physicalPlanExpression) bool {
	qs := w.GetQuantifiers()
	if len(qs) != 1 {
		return false
	}
	ref := qs[0].GetRangesOver()
	return distinctRecordsForRef(ref)
}

func distinctRecordsForRef(ref *expressions.Reference) bool {
	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		return false
	}
	for _, props := range pm.All() {
		if !props.GetBool(properties.PropDistinctRecords) {
			return false
		}
	}
	return len(pm.All()) > 0
}

// computeDistinctRecordsForMap checks whether a RecordQueryMapPlan is an
// identity mapping (result value is a QuantifiedObjectValue whose
// correlation matches the inner quantifier alias). Identity maps
// transparently propagate child distinctness; non-identity maps reshape
// the output and distinctness is not preserved — matching Java's
// DistinctRecordsProperty.evaluateAtExpression for RecordQueryMapPlan.
func computeDistinctRecordsForMap(w physicalPlanExpression) bool {
	mw, ok := w.(*physicalMapWrapper)
	if !ok {
		return false
	}
	rv := mw.plan.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		return false
	}
	if qov.Correlation == mw.innerQuant.GetAlias() {
		return distinctRecordsFromChildRef(w)
	}
	return false
}

func computeStoredRecord(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryScanPlan,
		*plans.RecordQueryIndexPlan,
		*plans.RecordQueryVectorIndexPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan:
		return true
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryDefaultOnEmptyPlan:
		return false
	case *plans.RecordQueryInJoinPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryInUnionPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryMultiIntersectionOnValuesPlan,
		*plans.RecordQueryUnorderedUnionPlan,
		*plans.RecordQueryRecursiveDfsJoinPlan,
		*plans.RecordQueryRecursiveLevelUnionPlan:
		return storedRecordAllChildren(plan.GetChildren())
	default:
		return false
	}
}

func storedRecordFromChildren(children []plans.RecordQueryPlan) bool {
	if len(children) != 1 {
		return false
	}
	return computeStoredRecord(children[0])
}

func storedRecordAllChildren(children []plans.RecordQueryPlan) bool {
	for _, c := range children {
		if !computeStoredRecord(c) {
			return false
		}
	}
	return len(children) > 0
}

func computePrimaryKey(plan plans.RecordQueryPlan) any {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		if pk := p.GetPrimaryKeyValues(); pk != nil {
			return pk
		}
		return nil
	case *plans.RecordQueryIndexPlan:
		return nil
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryInJoinPlan,
		*plans.RecordQueryInUnionPlan,
		*plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryDeletePlan:
		return pkFromChildren(plan.GetChildren())
	case *plans.RecordQueryUnionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryMultiIntersectionOnValuesPlan,
		*plans.RecordQueryUnorderedUnionPlan:
		return commonPKFromChildren(plan.GetChildren())
	default:
		return nil
	}
}

func pkFromChildren(children []plans.RecordQueryPlan) any {
	if len(children) != 1 {
		return nil
	}
	return computePrimaryKey(children[0])
}

func commonPKFromChildren(children []plans.RecordQueryPlan) any {
	if len(children) == 0 {
		return nil
	}
	first := computePrimaryKey(children[0])
	if first == nil {
		return nil
	}
	firstPK := first.([]values.Value)
	for _, c := range children[1:] {
		childPK := computePrimaryKey(c)
		if childPK == nil {
			return nil
		}
		pk := childPK.([]values.Value)
		if len(pk) != len(firstPK) {
			return nil
		}
		for i := range pk {
			if !valuesEqual(pk[i], firstPK[i]) {
				return nil
			}
		}
	}
	return firstPK
}

func computeWrapperOrdering(w physicalPlanExpression) properties.Ordering {
	if hinter, ok := w.(properties.OrderingHinter); ok {
		return hinter.HintOrdering()
	}
	return properties.Ordering{}
}

// RichOrderingHinter is the optional interface a wrapper implements
// to provide full ordering info with bindings. Falls back to
// converting from HintOrdering if not implemented.
type RichOrderingHinter interface {
	HintRichOrdering() *RichOrdering
}

func computeWrapperRichOrdering(w physicalPlanExpression) *RichOrdering {
	if rh, ok := w.(RichOrderingHinter); ok {
		return rh.HintRichOrdering()
	}
	o := computeWrapperOrdering(w)
	if !o.IsKnown || len(o.Keys) == 0 {
		return EmptyOrdering()
	}
	bm := make(map[values.Value][]OrderingBinding, len(o.Keys))
	for i, k := range o.Keys {
		dir := ProvidedSortOrderAscending
		if i < len(o.Descending) && o.Descending[i] {
			dir = ProvidedSortOrderDescending
		}
		bm[k] = []OrderingBinding{SortedBinding(dir)}
	}
	return NewRichOrdering(bm, o.Keys, false)
}

// computeRefPlanProperties computes and stores plan properties for all
// final-member physical plans in the given Reference. Called during the
// PLANNING phase after ImplementationRules have fired on ref.
func computeRefPlanProperties(ref *expressions.Reference) {
	members := ref.FinalMembers()
	if len(members) == 0 {
		members = ref.AllMembers()
	}
	pm := NewPlanPropertiesMap()
	for _, m := range members {
		if ph, ok := m.(physicalPlanExpression); ok {
			pm.Add(ph)
		}
	}
	ref.SetPlanProperties(pm)
}

// GetRefPlanPropertiesMap retrieves the PlanPropertiesMap from a Reference,
// or nil if not yet computed.
func GetRefPlanPropertiesMap(ref *expressions.Reference) *PlanPropertiesMap {
	if ref == nil {
		return nil
	}
	pm, _ := ref.GetPlanProperties().(*PlanPropertiesMap)
	return pm
}

// computeCardinalities computes the Cardinalities property for a
// physical plan wrapper. Matches Java's CardinalitiesVisitor per-plan-
// type logic for all plan types Go supports.
func computeCardinalities(w physicalPlanExpression, plan plans.RecordQueryPlan) properties.Cardinalities {
	switch p := plan.(type) {

	// --- Exactly one row ---
	case *plans.RecordQueryFirstOrDefaultPlan:
		return properties.ExactlyOne()

	// --- Transparent: same cardinality as child ---
	case *plans.RecordQuerySortPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryTempTableInsertPlan:
		return cardinalitiesFromChildRef(w)

	// --- DefaultOnEmpty: floor at 1 ---
	case *plans.RecordQueryDefaultOnEmptyPlan:
		child := cardinalitiesFromChildRef(w)
		return child.Floor(1)

	// --- Filters: min drops to 0, max stays ---
	case *plans.RecordQueryFilterPlan:
		child := cardinalitiesFromChildRef(w)
		return properties.Cardinalities{
			Min: properties.OfCardinality(0),
			Max: child.GetMaxCardinality(),
		}
	case *plans.RecordQueryPredicatesFilterPlan:
		child := cardinalitiesFromChildRef(w)
		return properties.Cardinalities{
			Min: properties.OfCardinality(0),
			Max: child.GetMaxCardinality(),
		}

	// --- Scans ---
	case *plans.RecordQueryScanPlan:
		return properties.UnknownMaxCardinality()

	case *plans.RecordQueryIndexPlan:
		if iw, ok := w.(*physicalIndexScanWrapper); ok && iw.unique {
			comps := p.GetScanComparisons()
			allEquality := true
			for _, cr := range comps {
				if !cr.IsEquality() {
					allEquality = false
					break
				}
			}
			if allEquality && len(comps) == len(iw.columnNames) {
				return properties.AtMostOne()
			}
		}
		return properties.UnknownMaxCardinality()

	// --- Set operations: union ---
	case *plans.RecordQueryUnionPlan:
		return properties.UnionCardinalities(cardinalitiesFromChildRefs(w))
	case *plans.RecordQueryMergeSortUnionPlan:
		return properties.UnionCardinalities(cardinalitiesFromChildRefs(w))
	case *plans.RecordQueryUnorderedUnionPlan:
		return properties.UnionCardinalities(cardinalitiesFromChildRefs(w))

	// --- Set operations: intersection ---
	case *plans.RecordQueryIntersectionPlan:
		return properties.IntersectCardinalities(cardinalitiesFromChildRefs(w))
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		return properties.IntersectCardinalities(cardinalitiesFromChildRefs(w))

	// --- Distinct: same as child (distinct doesn't change bounds) ---
	case *plans.RecordQueryDistinctPlan:
		return cardinalitiesFromChildRef(w)

	// --- Limit: cap max at limit value ---
	case *plans.RecordQueryLimitPlan:
		child := cardinalitiesFromChildRef(w)
		limit := p.GetLimit()
		if limit == 0 {
			// LIMIT 0 produces exactly zero rows — not "no cap". Folding it into
			// the negative ("no limit") arm would mis-cost any parent over a
			// LIMIT-0 subtree as the full child cardinality.
			return properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(0),
			}
		}
		if limit < 0 {
			// Negative limit = no cap (OFFSET-only stream). A RUNTIME-Value limit
			// (GetLimitValue()!=nil) is also stored as limit=-1 and DELIBERATELY
			// lands here: its real cap is only known at execution, so reading it as
			// conservative no-cap (over-estimating cardinality) is the sound choice —
			// over-estimation never enables an unsound rewrite (matching the explicit
			// HintCost conservative choice). Do not "fix" this into reducing to -1.
			return child
		}
		maxCard := child.GetMaxCardinality()
		if !maxCard.IsUnknown() && maxCard.Value() <= limit {
			return child
		}
		// Cap max at limit; min is min(child.min, limit).
		newMin := child.GetMinCardinality()
		if !newMin.IsUnknown() && newMin.Value() > limit {
			newMin = properties.OfCardinality(limit)
		}
		return properties.Cardinalities{
			Min: newMin,
			Max: properties.OfCardinality(limit),
		}

	// --- InJoin: child × in-list-size ---
	case *plans.RecordQueryInJoinPlan:
		child := cardinalitiesFromChildRef(w)
		inVals := p.GetInValues()
		if len(inVals) == 0 {
			// Unknown in-list size.
			return properties.UnknownMaxCardinality()
		}
		inSize := properties.OfCardinality(int64(len(inVals)))
		return properties.Cardinalities{
			Min: inSize.Times(child.GetMinCardinality()),
			Max: inSize.Times(child.GetMaxCardinality()),
		}

	// --- InUnion: child × unknown (binding sizes not available) ---
	case *plans.RecordQueryInUnionPlan:
		return properties.UnknownMaxCardinality()

	// --- DML: same as child ---
	case *plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan:
		return cardinalitiesFromChildRef(w)

	// --- Streaming aggregation ---
	case *plans.RecordQueryStreamingAggregationPlan:
		if len(p.GetGroupingKeys()) == 0 {
			// No grouping — single output row.
			return properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(1),
			}
		}
		return properties.UnknownMaxCardinality()

	// --- InMemorySort (Go-only): transparent ---
	case *plans.RecordQueryInMemorySortPlan:
		return cardinalitiesFromChildRef(w)

	// --- Nested loop join: outer × inner ---
	case *plans.RecordQueryNestedLoopJoinPlan:
		children := cardinalitiesFromChildRefs(w)
		if len(children) < 2 {
			return properties.UnknownCardinalities()
		}
		return children[0].Times(children[1])

	// --- Recursive plans ---
	case *plans.RecordQueryRecursiveDfsJoinPlan:
		return properties.UnknownMaxCardinality()
	case *plans.RecordQueryRecursiveLevelUnionPlan:
		children := cardinalitiesFromChildRefs(w)
		if len(children) == 0 {
			return properties.UnknownMaxCardinality()
		}
		return properties.Cardinalities{
			Min: children[0].GetMinCardinality(),
			Max: properties.UnknownCardinality(),
		}

	// --- Leaf plans ---
	case *plans.RecordQueryValuesPlan:
		return properties.ExactlyOne()
	case *plans.RecordQueryExplodePlan:
		return properties.UnknownMaxCardinality()
	case *plans.RecordQueryTableFunctionPlan:
		return properties.UnknownMaxCardinality()
	case *plans.RecordQueryTempTableScanPlan:
		return properties.UnknownMaxCardinality()
	case *plans.RecordQueryAggregateIndexPlan:
		return properties.UnknownMaxCardinality()

	default:
		return properties.UnknownCardinalities()
	}
}

// cardinalitiesFromChildRef returns the Cardinalities from the first
// (only) child Reference's plan properties. For single-inner wrappers.
func cardinalitiesFromChildRef(w physicalPlanExpression) properties.Cardinalities {
	qs := w.GetQuantifiers()
	if len(qs) != 1 {
		return properties.UnknownCardinalities()
	}
	return cardinalitiesForRef(qs[0].GetRangesOver())
}

// cardinalitiesFromChildRefs returns Cardinalities for each child
// Reference. For multi-child wrappers (union, intersection, join).
func cardinalitiesFromChildRefs(w physicalPlanExpression) []properties.Cardinalities {
	qs := w.GetQuantifiers()
	result := make([]properties.Cardinalities, len(qs))
	for i, q := range qs {
		result[i] = cardinalitiesForRef(q.GetRangesOver())
	}
	return result
}

// cardinalitiesForRef returns the Cardinalities from a Reference's
// plan properties. If the Reference has plan properties, it returns
// the weakened (least constraining) cardinality across all members.
// Falls back to UnknownCardinalities if no properties are available.
func cardinalitiesForRef(ref *expressions.Reference) properties.Cardinalities {
	pm := GetRefPlanPropertiesMap(ref)
	if pm == nil {
		return properties.UnknownCardinalities()
	}
	all := pm.All()
	if len(all) == 0 {
		return properties.UnknownCardinalities()
	}
	items := make([]properties.Cardinalities, 0, len(all))
	for _, props := range all {
		items = append(items, props.GetCardinalities())
	}
	// Weaken across all members — take the least constraining bounds.
	return properties.WeakenCardinalities(items)
}
