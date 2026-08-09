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
		properties.PropRichOrdering:    computeWrapperRichOrdering(w),
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
	case *plans.RecordQueryCoveringIndexPlan:
		// Java DistinctRecordsProperty.visitCoveringIndexPlan delegates straight
		// to visitIndexPlan on the held index plan. The inner is a FIELD, not a
		// child quantifier (RFC-220 criterion C1), so child traversal never
		// reaches it and this arm must fold it explicitly — without it every
		// value-index access path reports NOT distinct and distinctness-dependent
		// rules silently stop firing.
		if cp, ok := plan.(*plans.RecordQueryCoveringIndexPlan); ok {
			return cp.ProducesDistinctRecords()
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
		// Java StoredRecordProperty.visitCoveringIndexPlan returns true, the
		// same constant as visitIndexPlan. RFC-220 criterion C1 makes the inner
		// a field, so the covering type needs its own arm rather than inheriting
		// one through child traversal.
		*plans.RecordQueryCoveringIndexPlan,
		*plans.RecordQueryVectorIndexPlan,
		*plans.RecordQueryDistinctPlan,
		// DIVERGENCE, deliberate and NOT a simplification — Java's
		// StoredRecordProperty.visitFetchFromPartialRecordPlan returns true
		// (:306-307), so a fetch belongs in the unconditional-true arm above:
		// it turns an index entry into the stored record, and delegating to
		// its child (a covering scan flowing PARTIAL records) answers false
		// and calls the fetch's whole purpose non-stored. Go answers false via
		// the default arm.
		//
		// Correcting it in isolation REGRESSES SELECT plans, measured over the
		// explaindiff corpus: six ordered InUnions collapse into
		// InMemorySort(Fetch(InJoin(...))) — e.g. `SELECT * FROM tbl WHERE a IN
		// (10,20,30) ORDER BY a, id, k`. So it is BLOCKED on the propagation fix
		// below, not on doubt about which answer is right. Java's is.
		//
		// StoredRecord is a PARTITIONING dimension (expression_partition.go
		// toPartitionsFromMap), so this arm makes a fetch share a partition with
		// the index and filter members it is an alternative to — partitions of
		// the form [PredicatesFilter, Fetch, IndexPlan] where the corpus
		// previously had singletons. That is correct and desirable.
		//
		// THE BLOCKER IS ONE MISSING LINE OF PROPAGATION, in a third file.
		// MemoizeFinalExpressionsFromOther (implementation_rule.go:124-151)
		// mints a fresh Reference and copies the source's plan properties, but
		// registers NO constraint entry for it. OptimizeGroupTask's per-ordering
		// retention then looks the constraint up keyed on that NEW reference
		// (unified_tasks.go:663-666), finds nothing, and resolves the group by
		// COST ALONE — so the ordered member that would have been retained is
		// pruned as a loser. A RequestedOrdering pushed onto the SOURCE
		// reference does not survive the copy.
		//
		// DO NOT ADD A PushConstraint CALL TO ImplementInUnionRule. Java's
		// ImplementInUnionRule DECLARES REQUESTED_ORDERING as a constraint it
		// CONSUMES (ImplementInUnionRule.java:82) and calls pushConstraint zero
		// times. The push for this shape lives in a separate rule,
		// PushRequestedOrderingThroughInLikeSelectRule.java:99-104, which pushes
		// the requested orderings VERBATIM onto the inner quantifier's
		// reference — not derived from the IN bindings. Go already has that rule
		// and already registers it (default_rules.go:261). There is no missing
		// push to write: the ordering arrives correctly and is then dropped by
		// the copy.
		//
		// Java needs no such lookup for an independent reason: after rollUpTo
		// every member of the partition it copies is ordering-HOMOGENEOUS by
		// construction, so whichever winner the memo resolves satisfies the
		// contract. Go has two independent ways to be correct here and currently
		// neither is reliable — the roll-up equivalence was lossy
		// (PropRichOrdering and richOrderingsEqual close that half) and the
		// constraint does not survive the copy.
		//
		// This arm changes NO DML plan in the corpus (measured: golden with and
		// without it differ in zero Update/Delete lines), so the DML rules'
		// StoredRecord filter is unaffected either way.
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
	case *plans.RecordQueryCoveringIndexPlan:
		// Java PrimaryKeyProperty.visitCoveringIndexPlan delegates to
		// visitIndexPlan on the held index plan. RFC-220 criterion C1 makes the
		// inner a field, so without this arm the common primary key vanishes
		// above every value-index access path and ordered set operators lose the
		// key they dedup and merge on.
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
	return properties.NewRichOrdering(bm, o.Keys, properties.NotDistinct())
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

// computeCardinalities computes the Cardinalities property for a physical plan
// wrapper. Matches Java's CardinalitiesVisitor per-plan-type logic for all plan
// types Go supports.
//
// Since RFC-195 this is a THIN ADAPTER: the per-operator derivation lives on the
// plans themselves (plans.CardinalityProver), and all that remains here is
// resolving each child edge to an interval — which is genuinely this layer's
// job, since only cascades knows about References, property maps and winners.
// The signature is kept for the property-map consumers.
//
// The derivation moved because it lived at the TOP of the layering
// (expressions <- properties <- plans <- cascades) while switching exclusively
// on `plans.*` types, which put it out of reach of the `properties` cost walk
// that the memo's winner selection actually ranks with. That unreachability is
// why the cost model could contradict it six ways.
func computeCardinalities(w physicalPlanExpression, plan plans.RecordQueryPlan) properties.Cardinalities {
	prover, ok := plan.(properties.CardinalityProver)
	if !ok {
		return properties.UnknownCardinalities()
	}
	return prover.ProvenCardinalities(cardinalityChildrenForPlan(w, plan))
}

// cardinalityChildrenForPlan resolves plan's child edges to intervals,
// preserving the per-arm resolver choice the derivation had before it moved to
// the plans layer.
//
// Transparent (1:1) wrappers and the InUnion use the OrInner resolver: the
// data-access path exposes a composite EITHER as a wrapper with a single
// SNAPSHOT-quantifier child whose Reference carries no populated property map,
// OR as an adapter reporting no child quantifier at all, and in both forms a
// plain Reference read reports unknown so a point lookup never receives its
// proven AtMostOne. Every other operator reads its children straight off the
// wrapper's quantifiers.
func cardinalityChildrenForPlan(w physicalPlanExpression, plan plans.RecordQueryPlan) []properties.Cardinalities {
	// A LEAF proves its bound from its own shape alone and never consults a
	// child, so it must stay answerable with no wrapper at all — callers wanting
	// a plan-local proof legitimately pass a nil one, and the per-arm switch
	// this adapter replaced never touched the wrapper on those arms. Resolving
	// edges unconditionally would make a leaf's proof depend on a wrapper it
	// does not use.
	if w == nil {
		return nil
	}
	if usesOrInnerChildResolver(plan) {
		return []properties.Cardinalities{cardinalitiesFromChildRefOrInner(w, plan)}
	}
	return cardinalitiesFromChildRefs(w)
}

// usesOrInnerChildResolver reports whether plan's child edge must be resolved
// through the OrInner fallback rather than straight off the wrapper's
// quantifiers.
//
// Split out as a named predicate so a test can ENUMERATE this taxonomy instead
// of re-typing it. A second hand-maintained list of plan types is exactly the
// drift channel RFC-195 exists to close, one level down: a transparent wrapper
// added later would silently take the plain resolver and lose its child's
// proven bound wherever the data-access path exposes the composite without a
// populated property map.
func usesOrInnerChildResolver(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryTempTableInsertPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryInUnionPlan:
		return true
	}
	return false
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
