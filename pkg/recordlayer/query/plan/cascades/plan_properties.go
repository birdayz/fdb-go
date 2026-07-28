package cascades

import (
	"math"

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
		// Java DistinctRecordsProperty.visitIndexPlan: distinct iff the match
		// candidate did NOT create duplicates (empty candidate → false). NOT the
		// UNIQUE flag — a non-unique SCALAR index does not create duplicates and
		// so produces distinct records; only a fan-out index does not. The
		// candidate's createsDuplicates signal is stamped onto the plan at
		// build time (WithDistinctRecordsSignal).
		if ip, ok := plan.(*plans.RecordQueryIndexPlan); ok {
			return ip.ProducesDistinctRecords()
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
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryInsertPlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryUpdatePlan,
		*plans.RecordQueryTempTableInsertPlan,
		// A fetch is 1:1 (one record per index entry) — Java treats it as
		// transparent in DistinctRecordsProperty. Without this arm the M4
		// distinct fact is hidden above the common Fetch(IndexScan).
		*plans.RecordQueryFetchFromPartialRecordPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryFirstOrDefaultPlan:
		return true
	case *plans.RecordQueryDefaultOnEmptyPlan,
		*plans.RecordQueryInJoinPlan:
		return distinctRecordsFromChildRef(w)
	case *plans.RecordQueryMergeSortUnionPlan:
		// Java's RecordQueryUnionOnValuesPlan/UnionOnKeyExpressionPlan (this
		// plan's counterpart) has no non-dedup mode at all — UnionCursor
		// always dedups, so DistinctRecordsProperty.visitUnionOnValuesPlan /
		// visitUnionOnKeyExpressionPlan return true unconditionally. Go
		// additionally supports removeDuplicates=false as an ordered UNION
		// ALL (see merge_sort_union.go's doc comment); that mode does NOT
		// remove duplicates, so the property must track the flag instead of
		// assuming Java's always-true answer.
		mp, ok := plan.(*plans.RecordQueryMergeSortUnionPlan)
		return ok && mp.RemovesDuplicates()
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan,
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
	mw, ok := w.(*plans.RecordQueryMapPlan)
	if !ok {
		return false
	}
	rv := mw.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		return false
	}
	if qov.Correlation == mw.GetInnerQuantifier().GetAlias() {
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
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		return storedRecordFromChildren(plan.GetChildren())
	case *plans.RecordQueryFlatMapPlan:
		if p, ok := plan.(*plans.RecordQueryFlatMapPlan); ok &&
			p.InheritOuterRecordProperties() {
			return computeStoredRecord(p.GetOuter())
		}
		return false
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
		// RFC-189 B3 (re-port of the reverted M5): the plan carries the index's
		// common primary key translated to STRUCTURE-encoding Values (record-type
		// -key prefixes, nesting), stamped from the match candidate. Java's
		// PrimaryKeyProperty.visitIndexPlan does the same via
		// ScalarTranslationVisitor. Structural identity means Field("ID") never
		// equates Concat(RecordTypeKey(), Field("ID")) — the by-name conflation
		// that made M5 unsafe (ImplementDistinctUnionRule dropping rows). nil when
		// the candidate/def supplied no structural PK → the property abstains
		// (no dedup), the safe under-report.
		if pk := p.GetCommonPrimaryKeyValues(); pk != nil {
			return pk
		}
		return nil
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan,
		*plans.RecordQueryInJoinPlan,
		*plans.RecordQueryInUnionPlan,
		*plans.RecordQueryFirstOrDefaultPlan,
		*plans.RecordQueryDeletePlan,
		// A fetch is 1:1 — Java's PrimaryKeyProperty passes it through to its
		// single child, so the M5 index common-PK survives above the fetch.
		*plans.RecordQueryFetchFromPartialRecordPlan:
		return pkFromChildren(plan.GetChildren())
	case *plans.RecordQueryFlatMapPlan:
		if p.InheritOuterRecordProperties() {
			return computePrimaryKey(p.GetOuter())
		}
		return nil
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
			if !values.ValuesStructurallyEqual(pk[i], firstPK[i]) {
				return nil
			}
		}
	}
	return firstPK
}

func computeWrapperOrdering(w physicalPlanExpression) properties.Ordering {
	if rich, isJoin := computeJoinRichOrdering(w); isJoin {
		return plainOrderingFromRich(rich)
	}
	if hinter, ok := w.(properties.OrderingHinter); ok {
		return hinter.HintOrdering()
	}
	return properties.Ordering{}
}

func computeWrapperRichOrdering(w physicalPlanExpression) *properties.RichOrdering {
	if rich, isJoin := computeJoinRichOrdering(w); isJoin {
		return rich
	}
	if rh, ok := w.(properties.RichOrderingHinter); ok {
		return rh.HintRichOrdering()
	}
	o := computeWrapperOrdering(w)
	if !o.IsKnown || len(o.Keys) == 0 {
		return properties.EmptyOrdering()
	}
	bm := make(map[values.Value][]properties.OrderingBinding, len(o.Keys))
	for i, k := range o.Keys {
		// Map (descending, nulls-first) -> the matching ProvidedSortOrder,
		// including the counterflow variants. NullsFirstAt defaults to the
		// natural placement, so natural-order producers (index scans, which
		// leave NullsFirst empty) yield plain ASCENDING/DESCENDING unchanged;
		// only a counterflow in-memory sort yields a counterflow value, which
		// keeps a parent sort from eliding against it incorrectly.
		desc := o.DescendingAt(i)
		nullsFirst := o.NullsFirstAt(i)
		var dir properties.ProvidedSortOrder
		switch {
		case !desc && nullsFirst:
			dir = properties.ProvidedSortOrderAscending // ASC_NULLS_FIRST
		case !desc && !nullsFirst:
			dir = properties.ProvidedSortOrderAscendingNullsLast // ASC_NULLS_LAST (counterflow)
		case desc && !nullsFirst:
			dir = properties.ProvidedSortOrderDescending // DESC_NULLS_LAST
		default: // desc && nullsFirst
			dir = properties.ProvidedSortOrderDescendingNullsFirst // DESC_NULLS_FIRST (counterflow)
		}
		bm[k] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	return properties.NewRichOrdering(bm, o.Keys, false)
}

// computeJoinRichOrdering ports Java OrderingProperty.visitFlatMapPlan for
// Go's correlated FlatMap implementation. An ordered claim is made only
// when both child edges are exact final singletons. That qualification is
// essential in Go: a normal join ranges over shared memo groups and extraction
// is free to relink those edges to their cheaper winners. The ordering-aware
// NLJ variants freeze both selected children in private final references before
// they reach this property.
//
// Java's cases are:
//   - outer max cardinality == 1: the result follows the inner;
//   - otherwise, a non-distinct outer determines the whole result ordering;
//   - a distinct outer permits the inner ordering to be appended.
func computeJoinRichOrdering(w physicalPlanExpression) (*properties.RichOrdering, bool) {
	if w == nil {
		return nil, false
	}
	plan := w.GetRecordQueryPlan()
	if plan == nil {
		return nil, false
	}

	var flatMap *plans.RecordQueryFlatMapPlan
	switch p := plan.(type) {
	case *plans.RecordQueryFlatMapPlan:
		flatMap = p
	default:
		return nil, false
	}
	result := flatMap.GetResultValue()

	quantifiers := plan.GetQuantifiers()
	if len(quantifiers) != 2 || result == nil {
		return properties.EmptyOrdering(), true
	}
	outerAlias := quantifiers[0].GetAlias()
	innerAlias := quantifiers[1].GetAlias()
	outerExpr, ok := exactFinalPhysicalMember(quantifiers[0].GetRangesOver())
	if !ok {
		return properties.EmptyOrdering(), true
	}
	innerExpr, ok := exactFinalPhysicalMember(quantifiers[1].GetRangesOver())
	if !ok {
		return properties.EmptyOrdering(), true
	}

	outerOrdering := computeWrapperRichOrdering(outerExpr)
	innerOrdering := computeWrapperRichOrdering(innerExpr)
	if outerOrdering == nil || innerOrdering == nil {
		return properties.EmptyOrdering(), true
	}
	outerOrdering = outerOrdering.PullUpThroughValue(
		flatMapOrderingResultForChild(flatMap, outerAlias, true), outerAlias)
	innerOrdering = innerOrdering.PullUpThroughValue(result, innerAlias)
	if outerOrdering == nil || innerOrdering == nil {
		return properties.EmptyOrdering(), true
	}

	outerCardinalities := computeCardinalities(outerExpr, outerExpr.GetRecordQueryPlan())
	outerMax := outerCardinalities.GetMaxCardinality()
	if !outerMax.IsUnknown() && outerMax.Value() == 1 {
		return innerOrdering, true
	}
	if !outerOrdering.IsDistinct() {
		return outerOrdering, true
	}
	return properties.ConcatOrderings(outerOrdering, innerOrdering), true
}

// flatMapOrderingResultForChild returns the semantic result-value lens used to
// translate an individual child's ordering. Projected EXISTS FlatMaps mark
// inheritOuterRecordProperties and can carry the outer projection's field
// accesses in source-local form (ID#0) after SQL lowering. That representation
// has no correlation with which to distinguish the two FlatMap children, even
// though the inherit flag is explicit authority that those ordinary fields
// come from the outer row.
//
// For ordering translation only, root source-local FieldValues in that result
// constructor on the outer alias. The executable result value is untouched.
// This lets the normal pull-up/push-down and safe root-bridge checks prove
// ID#0 <-> T1.ID#0 while constants and the existential boolean remain
// unowned. Ordinary joins (inherit=false) keep the original, conservative
// multi-child ambiguity behavior.
func flatMapOrderingResultForChild(
	flatMap *plans.RecordQueryFlatMapPlan,
	childAlias values.CorrelationIdentifier,
	outer bool,
) values.Value {
	if flatMap == nil {
		return nil
	}
	result := flatMap.GetResultValue()
	if !outer || !flatMap.InheritOuterRecordProperties() {
		return result
	}
	rc, ok := result.(*values.RecordConstructorValue)
	if !ok {
		return result
	}
	fields := make([]values.RecordConstructorField, len(rc.Fields))
	copy(fields, rc.Fields)
	changed := false
	for i := range fields {
		fieldValue, ok := fields[i].Value.(*values.FieldValue)
		if !ok || fieldValue == nil || fieldValue.Child != nil ||
			len(values.GetCorrelatedToOfValue(fieldValue)) != 0 {
			continue
		}
		qualified := *fieldValue
		qualified.Child = values.NewQuantifiedObjectValue(childAlias)
		fields[i].Value = &qualified
		changed = true
	}
	if !changed {
		return result
	}
	return values.NewRawRecordConstructorValue(fields...)
}

// exactFinalPhysicalMember resolves the private-reference shape used by an
// ordering-aware join leg. A shared or exploratory group is deliberately
// rejected even when it currently has one apparent winner: it can still grow
// or be relinked, invalidating a sort-elision proof made against that member.
func exactFinalPhysicalMember(ref *expressions.Reference) (physicalPlanExpression, bool) {
	if ref == nil || len(ref.Members()) != 0 {
		return nil, false
	}
	finals := ref.FinalMembers()
	if len(finals) != 1 {
		return nil, false
	}
	ph, ok := finals[0].(physicalPlanExpression)
	return ph, ok && ph.GetRecordQueryPlan() != nil
}

// plainOrderingFromRich is the partition-key projection of a rich ordering.
// Fixed keys do not consume a sort position; directional bindings retain both
// direction and counterflow NULL placement. Sort satisfaction itself still
// uses the full RichOrdering.
func plainOrderingFromRich(rich *properties.RichOrdering) properties.Ordering {
	if rich == nil {
		return properties.Ordering{}
	}
	var (
		keys       []values.Value
		descending []bool
		nullsFirst []bool
	)
	for _, key := range rich.GetKeys() {
		sortOrder := properties.SortOrderOf(rich.GetBindingMap()[key])
		if !sortOrder.IsDirectional() {
			continue
		}
		keys = append(keys, key)
		desc := sortOrder.IsAnyDescending()
		descending = append(descending, desc)
		nullsFirst = append(nullsFirst,
			sortOrder == properties.ProvidedSortOrderAscending ||
				sortOrder == properties.ProvidedSortOrderDescendingNullsFirst)
	}
	if len(keys) == 0 {
		return properties.Ordering{}
	}
	return properties.Ordering{
		IsKnown:    true,
		Keys:       keys,
		Descending: descending,
		NullsFirst: nullsFirst,
	}
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
	case *plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryTempTableInsertPlan,
		// A fetch is 1:1 — Java's CardinalitiesProperty passes it through, so a
		// bounded index access under a Fetch keeps its proven max cardinality
		// (feeds the M3 whole-plan guard and criterion #2).
		*plans.RecordQueryFetchFromPartialRecordPlan:
		return cardinalitiesFromChildRefOrInner(w, plan)

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
		// A full-PK-equality primary scan is a point lookup (max 1), exactly as
		// Java's CardinalitiesProperty.visitRecordQueryScanPlan bounds a scan
		// whose equality-bound values cover the whole primary key. This feeds
		// the M3 whole-plan-cardinality outer guard (RFC-188) so a
		// bounded-primary-scan-vs-unbounded comparison is no longer reported
		// unknown (over-abstaining criterion #2). A range / partial / prefix
		// bind stays unknown. scanProvableMaxCard consults the PK arity stamped
		// on the scan and proves full coverage, not merely that every comparison
		// in the (possibly partial) prefix is an equality.
		if _, known := scanProvableMaxCard(p); known {
			return properties.AtMostOne()
		}
		return properties.UnknownMaxCardinality()

	case *plans.RecordQueryIndexPlan:
		// The index scan is its own physical expression now (RFC-184 W2) — its
		// UNIQUE flag and column list live on the plan.
		if p.IsUnique() {
			comps := p.GetScanComparisons()
			allEquality := true
			for _, cr := range comps {
				if !cr.IsEquality() {
					allEquality = false
					break
				}
				// A zero-valued FLOAT/DOUBLE equality does NOT pin a single
				// key, so it cannot support an at-most-one PROOF. The executor
				// widens it across both signed zeros (-0.0 and +0.0 are
				// IEEE-equal but pack to distinct adjacent keys), and a UNIQUE
				// index legitimately holds BOTH — uniqueness is enforced on the
				// raw packed prefix, so the two are different entries.
				//
				// Measured: with a unique index on a DOUBLE column holding both
				// zeros, `WHERE v = 0` returns TWO rows. Claiming AtMostOne
				// there is not a loose estimate, it is a false proof, and every
				// consumer of it — DISTINCT elision, single-row shortcuts, the
				// cost model — inherits the falsehood.
				if isZeroFloatEqualityRange(cr) {
					allEquality = false
					break
				}
			}
			if allEquality && len(comps) == len(p.GetColumnNames()) {
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

	// --- Full-row distinct: current model conservatively preserves child bounds ---
	case *plans.RecordQueryDistinctPlan:
		return cardinalitiesFromChildRef(w)

	// --- Primary-key distinct: max is unchanged; any known non-empty input
	// produces at least one unique key. ---
	case *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		child := cardinalitiesFromChildRef(w)
		minCard := child.GetMinCardinality()
		if !minCard.IsUnknown() && minCard.Value() > 0 {
			minCard = properties.OfCardinality(1)
		}
		return properties.Cardinalities{
			Min: minCard,
			Max: child.GetMaxCardinality(),
		}

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

	// --- InUnion: child × Cartesian product of literal source sizes ---
	case *plans.RecordQueryInUnionPlan:
		fanout, known := p.LiteralFanout()
		if !known {
			return properties.UnknownMaxCardinality()
		}
		if fanout == 0 {
			// Go precision extension: a known-empty dimension makes the
			// executor return an empty cursor even when another dimension is
			// runtime-unknown. Java's generic unknown multiplication keeps
			// that mixed maximum unknown; exact zero is stronger but sound.
			return properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(0),
			}
		}
		child := cardinalitiesFromChildRefOrInner(w, plan)
		return properties.Cardinalities{
			Min: multiplyCardinalityBound(fanout, child.GetMinCardinality()),
			Max: multiplyCardinalityBound(fanout, child.GetMaxCardinality()),
		}

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

	// --- Nested loop joins: outer × inner ---
	case *plans.RecordQueryNestedLoopJoinPlan,
		*plans.RecordQueryFlatMapPlan:
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

// multiplyCardinalityBound multiplies an exact non-negative factor by one
// cardinality bound without allowing int64 wraparound to reach OfCardinality.
// An unrepresentable product conservatively becomes unknown.
func multiplyCardinalityBound(factor int64, bound properties.Cardinality) properties.Cardinality {
	if bound.IsUnknown() {
		return properties.UnknownCardinality()
	}
	value := bound.Value()
	if factor == 0 || value == 0 {
		return properties.OfCardinality(0)
	}
	if factor > math.MaxInt64/value {
		return properties.UnknownCardinality()
	}
	return properties.OfCardinality(factor * value)
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

// cardinalitiesFromChildRefOrInner is cardinalitiesFromChildRef for a transparent
// (1:1) wrapper, with a fallback to the concrete embedded child. The data-access
// path exposes a composite (e.g. TypeFilter(Scan) over a full-PK-equality point
// scan) EITHER as a wrapper with a single SNAPSHOT-quantifier child whose
// Reference carries no populated property map, OR as a scanPlanExpression adapter
// that reports NO child quantifier at all — in both forms cardinalitiesForRef
// reports unknown and a point lookup never receives its proven AtMostOne (the
// booked M3-followup).
//
// Two hazards shape the fallback:
//   - A POPULATED property map that weakens to unknown is a LEGITIMATE unknown,
//     not a missing property (a group with both bounded and unbounded final
//     members correctly weakens to the least-constraining bound). Never
//     second-guess it — trust the ref when its property map is populated.
//   - The concrete child is reached through planFromQuantifier, which PANICS on a
//     reference with ≥2 DISTINCT plan-typed final members and no winner. Resolve
//     the child edge only when it is unambiguous (childPlanIfUnambiguous).
//
// Under those guards computing the embedded child is exact for a 1:1 wrapper and
// can only TIGHTEN the bound (the M3 guard's safe direction).
func cardinalitiesFromChildRefOrInner(w physicalPlanExpression, plan plans.RecordQueryPlan) properties.Cardinalities {
	// Resolve the child edge off the PLAN, not the physical wrapper: a
	// scanPlanExpression adapter exposes NO wrapper quantifier while the wrapped
	// plan has a live child, so w.GetQuantifiers() would miss both the child's
	// populated property map and its concrete plan.
	qs := plan.GetQuantifiers()
	if len(qs) != 1 {
		return properties.UnknownCardinalities()
	}
	childRef := qs[0].GetRangesOver()
	if childRef == nil {
		return properties.UnknownCardinalities()
	}
	if pm := GetRefPlanPropertiesMap(childRef); pm != nil && len(pm.All()) > 0 {
		// Populated — trust the (possibly weakened) group cardinality as-is. A
		// mixed bounded/unbounded group legitimately weakens to unknown and must
		// not be second-guessed by following one member's bound.
		return cardinalitiesForRef(childRef)
	}
	// No populated property map (a snapshot quantifier, or the scanPlanExpression
	// adapter). Recover the bound from the single concrete child, resolved
	// unambiguously.
	if childPlan, ok := childPlanIfUnambiguous(childRef); ok {
		if ph, ok := childPlan.(physicalPlanExpression); ok {
			return computeCardinalities(ph, childPlan)
		}
	}
	return properties.UnknownCardinalities()
}

// childPlanIfUnambiguous returns the single concrete plan a child reference
// resolves to, and true, only when that resolution is UNAMBIGUOUS. It follows
// planFromQuantifier's resolution ORDER — a group winner first; else exactly one
// distinct plan-typed FINAL member (≥2 there is the onlyPlanFromFinalMembers
// panic case → ambiguous); else, with NO plan among the final members, a lone
// distinct plan-typed general member (the InitialOf(plan) exploratory shape
// MemoizeExpression can leave during planning) — but is deliberately STRICTER
// than planFromQuantifier at the last step: where firstPlanFromMembers would pick
// an arbitrary first among ≥2 competing general members, this declines (ok=false),
// because following an arbitrary member's cardinality would be unsound and
// UnknownCardinalities is the M3 guard's safe direction. It therefore resolves
// the same plan whenever it resolves, and never resolves a shape planFromQuantifier
// panics on.
func childPlanIfUnambiguous(ref *expressions.Reference) (plans.RecordQueryPlan, bool) {
	if ref == nil {
		return nil, false
	}
	// A group winner is what planFromQuantifier returns first — unambiguous.
	if wn := ref.Winner(); wn != nil {
		if ph, ok := wn.(physicalPlanExpression); ok {
			if p := ph.GetRecordQueryPlan(); p != nil {
				return p, true
			}
		}
	}
	// Final members: exactly one distinct plan resolves; ≥2 is the panic case.
	if p, n := distinctPlanMember(ref.FinalMembers()); n == 1 {
		return p, true
	} else if n >= 2 {
		return nil, false
	}
	// No plan among final members — resolve a lone exploratory member.
	if p, n := distinctPlanMember(ref.Members()); n == 1 {
		return p, true
	}
	return nil, false
}

// distinctPlanMember returns the sole distinct plan-typed member and a count
// capped at 2 (0 = none, 1 = exactly one, 2 = two or more distinct plans). The
// distinct dedup by pointer identity mirrors onlyPlanFromFinalMembers.
func distinctPlanMember(members []expressions.RelationalExpression) (plans.RecordQueryPlan, int) {
	var only plans.RecordQueryPlan
	n := 0
	for _, m := range members {
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		p := ph.GetRecordQueryPlan()
		if p == nil || p == only {
			continue
		}
		if only != nil {
			return only, 2 // ≥2 distinct plans
		}
		only = p
		n = 1
	}
	return only, n
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
