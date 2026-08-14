package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctUnionRule implements Distinct(Union(legs...)) as a
// merge-sorted union plan. It matches LogicalDistinctExpression over
// LogicalUnionExpression, finds compatible orderings across all union
// legs, and creates a RecordQueryMergeSortUnionPlan with deduplication.
//
// Ports Java's ImplementDistinctUnionRule. The full algorithm:
//  1. Get requested orderings from planner constraints
//  2. For each cross-product combo of per-leg plan partitions:
//     a. Verify common primary key across all legs
//     b. Extract ordering from each leg's partition
//     c. Incrementally merge orderings (bail on incompatibility)
//     d. Verify PK values are covered by merged ordering
//     e. Enumerate comparison keys satisfying the requested ordering
//     f. Create MergeSortUnionPlan with comparison keys
type ImplementDistinctUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctUnionRule() *ImplementDistinctUnionRule {
	return &ImplementDistinctUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("implement_distinct"),
	}
}

func (r *ImplementDistinctUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctUnionRule) OnMatch(call *ImplementationRuleCall) {
	distinct := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)

	distinctQs := distinct.GetQuantifiers()
	if len(distinctQs) != 1 {
		return
	}
	unionRef := distinctQs[0].GetRangesOver()
	if unionRef == nil {
		return
	}

	var unionExpr *expressions.LogicalUnionExpression
	for _, m := range unionRef.AllMembers() {
		if u, ok := m.(*expressions.LogicalUnionExpression); ok {
			unionExpr = u
			break
		}
	}
	if unionExpr == nil {
		return
	}

	unionQs := unionExpr.GetQuantifiers()
	if len(unionQs) < 2 {
		return
	}

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	}

	legPartitions := make([][]*PlanPartition, len(unionQs))
	for i, q := range unionQs {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		partitions := ToPlanPartitions(ref)
		var filtered []*PlanPartition
		for _, p := range partitions {
			if p.IsStoredRecord() && p.HasPrimaryKey() {
				filtered = append(filtered, p)
			}
		}
		allExcept := AllAttributesExcept(properties.PropDistinctRecords)
		rolled := RollUpPlanPartitions(filtered, allExcept...)
		if len(rolled) == 0 {
			return
		}
		legPartitions[i] = rolled
	}

	for _, requestedOrdering := range requestedOrderings {
		for _, q := range unionQs {
			if ref := q.GetRangesOver(); ref != nil {
				call.PushConstraint(ref, []*properties.RequestedOrdering{requestedOrdering})
			}
		}

		iter := NewCrossProductIterator(legPartitions)
		type mergeEntry struct {
			merged  *properties.RichOrdering
			current *properties.RichOrdering
		}
		var merge []mergeEntry

		for iter.HasNext() {
			combo := iter.Next()

			pkValues := getCommonPK(combo)
			if pkValues == nil {
				continue
			}

			orderings := make([]*properties.RichOrdering, len(combo))
			for i, partition := range combo {
				exprs := partition.GetExpressions()
				var ro *properties.RichOrdering
				for _, expr := range exprs {
					if ph, ok := expr.(physicalPlanExpression); ok {
						ro = computeWrapperRichOrdering(ph)
						break
					}
				}
				if ro == nil {
					o := partition.GetOrdering()
					bm := make(map[values.Value][]properties.OrderingBinding)
					for _, k := range o.Keys {
						bm[k] = []properties.OrderingBinding{properties.SortedBinding(properties.ProvidedSortOrderAscending)}
					}
					ro = properties.NewRichOrdering(bm, o.Keys, properties.NotDistinct())
				}
				orderings[i] = ro
			}
			orderings = removeCommonEqualityBoundParts(orderings)

			for i := 0; i < len(merge); i++ {
				if !richOrderingEquals(orderings[i], merge[i].current) {
					merge = merge[:i]
					break
				}
			}

			for len(merge) < len(orderings) {
				if len(merge) == 0 {
					merge = append(merge, mergeEntry{
						merged:  properties.CreateUnionOrdering(orderings[0]),
						current: orderings[0],
					})
				} else {
					lastMerged := merge[len(merge)-1].merged
					merged := properties.MergeOrderings(lastMerged, orderings[len(merge)])
					if !isPrimaryKeyCompatibleWithOrdering(pkValues, merged) {
						iter.Skip(len(merge))
						break
					}
					merge = append(merge, mergeEntry{
						merged:  merged,
						current: orderings[len(merge)],
					})
				}
			}

			if len(merge) == len(orderings) {
				mergedOrdering := merge[len(merge)-1].merged
				r.yieldFromMergedOrdering(call, combo, mergedOrdering, requestedOrdering)
			}
		}
	}
}

func (r *ImplementDistinctUnionRule) yieldFromMergedOrdering(
	call *ImplementationRuleCall,
	combo []*PlanPartition,
	mergedOrdering *properties.RichOrdering,
	requestedOrdering *properties.RequestedOrdering,
) {
	if len(combo) < 2 {
		return
	}
	commonPrimaryKey := getCommonPK(combo)
	if len(commonPrimaryKey) == 0 {
		return
	}

	satisfyingKeys := mergedOrdering.EnumerateSatisfyingComparisonKeyValues(requestedOrdering)
	for _, comparisonKeyValues := range satisfyingKeys {
		comparisonParts := mergedOrdering.DirectionalOrderingParts(
			comparisonKeyValues, requestedOrdering, properties.ProvidedSortOrderFixed)
		isReverse := ResolveComparisonDirection(comparisonParts)
		comparisonParts = AdjustFixedBindings(comparisonParts, isReverse)

		// The comparison keys ARE the per-leg ordering contract of the
		// merge-front dedup: a leg whose EXECUTED order diverges from them
		// mis-merges (duplicates through UNION distinct). The old shape
		// trusted a partition-level first-member ordering ESTIMATE (a
		// delegator's group hint, untethered to its baked child) and baked
		// EVERY partition member as a plan child (a 2-leg union executing a
		// 3+-way merge). Now: per leg, the cheapest member that
		// STRUCTURALLY satisfies the comparison-key requirement
		// (memberSatisfiesOrdering resolves delegators through their
		// source groups), spine-PINNED (pinOrderedSpine — executable-plan
		// verified) and baked as the ONE child over a FinalOf singleton.
		// An unpinnable leg skips this comparison-key candidate — the
		// in-memory-sort alternative still competes.
		legReqParts := make([]properties.RequestedOrderingPart, len(comparisonParts))
		for i, p := range comparisonParts {
			so := properties.RequestedSortOrderAny
			if p.SortOrder != properties.ProvidedSortOrderFixed {
				so = p.SortOrder.ToRequestedSortOrder()
			}
			legReqParts[i] = properties.RequestedOrderingPart{Value: p.Value, SortOrder: so}
		}
		legReq := properties.NewRequestedOrdering(legReqParts, properties.DistinctnessPreserveDistinctness, false)

		tieBrokenLess := lessWithHashTieBreak(call.CostModel())
		var childPlans []plans.RecordQueryPlan
		var newQuantifiers []expressions.Quantifier
		commonRecordType := ""
		haveCommonRecordType := false
		ok := true
		for _, partition := range combo {
			var best expressions.RelationalExpression
			for _, pe := range partition.GetExpressions() {
				if !memberSatisfiesOrdering(pe, legReq) {
					continue
				}
				if best == nil || tieBrokenLess(pe, best) {
					best = pe
				}
			}
			if best == nil {
				ok = false
				break
			}
			pinned := pinOrderedSpine(best, legReq, call.CostModel())
			if pinned == nil {
				ok = false
				break
			}
			pp, isPhys := pinned.(physicalPlanExpression)
			if !isPhys {
				ok = false
				break
			}
			childPlan := pp.GetRecordQueryPlan()
			recordType, identityOK := mergeDistinctStoredRecordIdentity(
				childPlan, commonPrimaryKey,
			)
			if !identityOK ||
				!mergeDistinctLegProducesDistinctRecords(childPlan) ||
				haveCommonRecordType && recordType != commonRecordType {
				// The comparison key is a PHYSICAL record key. It is a valid
				// SQL UNION-DISTINCT key only while every leg emits the same full
				// stored record for that key. Row-shaping projections/maps and
				// different record types can emit different SQL rows with the same
				// propagated PK. Each leg must also be internally distinct: a merge
				// cursor holds only one head per leg, so it cannot consume two
				// consecutive equal rows from that same leg together.
				ok = false
				break
			}
			commonRecordType = recordType
			haveCommonRecordType = true
			childPlans = append(childPlans, childPlan)
			newQuantifiers = append(newQuantifiers,
				expressions.NewPhysicalQuantifier(expressions.FinalOf(pinned)))
		}
		if !ok {
			continue
		}

		// One merge direction, raw tuple-encoded comparison Values: every part
		// must agree with the resolved direction or the merge front compares one
		// key the wrong way round. Fail closed, as the intersection plans do.
		//
		// UNREACHABLE TODAY, and kept as a fence rather than because a corpus
		// query needs it: the union merge does not carry equality-bound keys
		// into the merged ordering (measured — see
		// TestDistinctUnionMergedOrderingCarriesNoEqualityBoundKeys), so every
		// part here takes a leg's own uniform scan direction and AdjustFixedBindings
		// normalizes the rest. Over the 2475-query corpus this declines nothing,
		// while its in-union twin declines twice. It stays because the two rules
		// share one algebra and the day the union path starts carrying such a key
		// is the day a silent mis-merge would appear.
		comparisonKeys, natural := properties.NaturalComparisonKeyValues(comparisonParts, isReverse)
		if !natural {
			continue
		}
		comparisonKeys = bakeMergeComparisonKeys(comparisonKeys, requestedOrdering, childPlans[0].GetResultType())
		if comparisonKeys == nil {
			// An unresolved free-suffix tiebreak cannot be discarded: the merge
			// front also uses this tuple for DISTINCT, so a shortened/empty key
			// would collapse unrelated rows that tie on the requested prefix.
			// Mirror ImplementInUnionRule and decline this physical candidate;
			// the sort-based UNION DISTINCT alternative remains available.
			continue
		}

		// The merge carries its leg edges directly — one live quantifier per
		// pinned winner, no separate physical wrapper (RFC-184 W2).
		mergePlan, err := plans.NewRecordQueryMergeSortUnionPlanFromQuantifiers(
			newQuantifiers, comparisonKeys, isReverse, true)
		if err != nil {
			call.Fail(err)
			return
		}
		call.YieldFinalExpression(mergePlan)
	}
}

const maxMergeDistinctIdentityDepth = 64

// mergeDistinctStoredRecordIdentity proves the fact DistinctUnion needs in
// addition to StoredRecordProperty and PrimaryKeyProperty: the plan emits the
// complete stored record identified by commonPrimaryKey, without changing the
// SQL row. Those two generic properties deliberately survive projections and
// maps because their other record-level consumers need the carried base-record
// identity. That is not sufficient for SQL row-value DISTINCT: Project(ID, 1)
// and Project(ID, 2) carry the same ID primary key but are different rows.
//
// The returned string is the one base record type. The caller requires it to be
// identical across every union leg, because primary keys are unique within a
// record type; T/1 and U/1 are different records even when both plans expose a
// structurally identical visible ID value.
func mergeDistinctStoredRecordIdentity(
	plan plans.RecordQueryPlan,
	commonPrimaryKey []values.Value,
) (string, bool) {
	return mergeDistinctStoredRecordIdentityAtDepth(
		plan, commonPrimaryKey, 0,
	)
}

// Fetch, Projection, and Map deserve special care here. In the current Go
// executor Fetch is a transparent pass-through (index scans already return the
// record payload); it is not the Java restoration boundary that turns an
// arbitrary partial row back into a full record. Projection and Map always
// construct a new PositionalRow, even for their planner-level "identity"
// shapes. Consequently Fetch may recurse, but no row-shaping operator below or
// above it can participate in this proof. Bare covering indexes are rejected
// for the same reason: their base PK need not identify the emitted index row.
func mergeDistinctStoredRecordIdentityAtDepth(
	plan plans.RecordQueryPlan,
	commonPrimaryKey []values.Value,
	depth int,
) (string, bool) {
	if plan == nil || len(commonPrimaryKey) == 0 || depth >= maxMergeDistinctIdentityDepth {
		return "", false
	}

	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return mergeDistinctLeafRecordIdentity(
			p.GetRecordTypes(), p.GetPrimaryKeyValues(), commonPrimaryKey,
			p.GetKeyComponentTypes(), len(p.GetPrimaryKeyValues()),
		)

	case *plans.RecordQueryCoveringIndexPlan:
		// A bare covering scan emits a partial/index-shaped row. The base PK
		// identifies the record it came from, not necessarily that emitted row.
		return "", false

	case *plans.RecordQueryIndexPlan:
		return mergeDistinctLeafRecordIdentity(
			p.GetRecordTypes(), p.GetCommonPrimaryKeyValues(), commonPrimaryKey,
			p.GetPrimaryKeyComponentTypes(), len(p.GetPKColumnNames()),
		)

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		if p.GetFetchIndexRecords() != plans.FetchIndexRecordsPrimaryKey {
			return "", false
		}
		// The Go executor currently treats Fetch as transparent. Preserve that
		// exact runtime contract and require its input to have already proved
		// complete stored-row identity.
		return mergeDistinctUnaryChildIdentity(
			plan, commonPrimaryKey, depth+1,
		)

	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		// These operators select/remove rows but never reshape a surviving row.
		return mergeDistinctUnaryChildIdentity(
			plan, commonPrimaryKey, depth+1,
		)

	case *plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan:
		// Both executor paths construct a new one-slot PositionalRow even when
		// the planner classifies the value as an identity. Do not equate their
		// carried base-record PK with the emitted SQL row.
		return "", false

	default:
		// Joins, unions, IN operators, default-row synthesis, DML, aggregate
		// plans, and every future row-shaping operator need their own explicit
		// functional-dependency proof before a physical PK can dedup SQL rows.
		return "", false
	}
}

// mergeDistinctUnaryChildIdentity quantifies every physical member of the
// wrapper's LIVE child reference. Looking only at GetInner's current
// representative would let extraction relink the wrapper to an unproved member
// after this rule removed DISTINCT. Logical members are ignored because a
// physical parent can execute only a physical child winner; at least one such
// member must exist.
func mergeDistinctUnaryChildIdentity(
	plan plans.RecordQueryPlan,
	commonPrimaryKey []values.Value,
	depth int,
) (string, bool) {
	quantifiers := plan.GetQuantifiers()
	if len(quantifiers) != 1 {
		return "", false
	}
	ref := quantifiers[0].GetRangesOver()
	if ref == nil {
		return "", false
	}

	recordType := ""
	foundPhysical := false
	for _, member := range ref.AllMembers() {
		physical, ok := member.(physicalPlanExpression)
		if !ok {
			continue
		}
		memberRecordType, proved := mergeDistinctStoredRecordIdentityAtDepth(
			physical.GetRecordQueryPlan(), commonPrimaryKey, depth,
		)
		if !proved || foundPhysical && memberRecordType != recordType {
			return "", false
		}
		recordType = memberRecordType
		foundPhysical = true
	}
	return recordType, foundPhysical
}

func mergeDistinctLeafRecordIdentity(
	recordTypes []string,
	planPrimaryKey []values.Value,
	commonPrimaryKey []values.Value,
	physicalPrimaryKeyTypes []values.Type,
	physicalPrimaryKeyColumnCount int,
) (string, bool) {
	if len(recordTypes) != 1 || recordTypes[0] == "" ||
		!mergeDistinctPrimaryKeysEqual(planPrimaryKey, commonPrimaryKey) ||
		!properties.TupleKeyUniquenessMatchesLogicalEquality(
			physicalPrimaryKeyTypes, physicalPrimaryKeyColumnCount,
		) {
		return "", false
	}
	return recordTypes[0], true
}

func mergeDistinctPrimaryKeysEqual(left, right []values.Value) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for i := range left {
		if !values.ValuesStructurallyEqual(left[i], right[i]) {
			return false
		}
	}
	return true
}

// mergeDistinctLegProducesDistinctRecords proves that one merge input cannot
// repeat the same base record. The merge cursor removes equal heads from
// different legs, but only one row from a given leg is visible at a time: a
// second equal row in that leg is pulled on the next round and would be emitted
// again. Fan-out index scans are the canonical source of such repeats.
//
// This proof is intentionally separate from mergeDistinctStoredRecordIdentity.
// The latter says a PK identifies the emitted SQL row; this one says the leg
// emits that row at most once. A row-DISTINCT or PK-DISTINCT wrapper establishes
// the second fact, while still relying on the identity proof for NaN/raw-key and
// row-shaping safety.
func mergeDistinctLegProducesDistinctRecords(plan plans.RecordQueryPlan) bool {
	return mergeDistinctLegProducesDistinctRecordsAtDepth(plan, 0)
}

func mergeDistinctLegProducesDistinctRecordsAtDepth(
	plan plans.RecordQueryPlan,
	depth int,
) bool {
	if plan == nil || depth >= maxMergeDistinctIdentityDepth {
		return false
	}
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return true
	case *plans.RecordQueryIndexPlan:
		return p.ProducesDistinctRecords()
	case *plans.RecordQueryCoveringIndexPlan:
		// Rebuilding a partial record from an entry neither creates nor removes
		// duplicates, so duplicate-freedom is the wrapped scan's — which is what
		// ProducesDistinctRecords delegates to. Distinct from the sibling
		// identity walk, which REFUSES a bare covering leg: there the question
		// is whether the base primary key identifies the EMITTED row, and for a
		// partial row it does not. Here the question is only whether the leg
		// repeats a record, which the fan-out signal answers.
		//
		// Without this arm the fetch arm below stops at the wrapper (a field,
		// never a child), so Fetch(Covering(IndexScan)) — every index-backed
		// access — reports non-distinct.
		return p.ProducesDistinctRecords()
	case *plans.RecordQueryDistinctPlan,
		*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		return true
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryProjectionPlan,
		*plans.RecordQueryMapPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan:
		return everyMergeDistinctUnaryChildIsDistinct(plan, depth+1)
	default:
		return false
	}
}

func everyMergeDistinctUnaryChildIsDistinct(
	plan plans.RecordQueryPlan,
	depth int,
) bool {
	quantifiers := plan.GetQuantifiers()
	if len(quantifiers) != 1 {
		return false
	}
	ref := quantifiers[0].GetRangesOver()
	if ref == nil {
		return false
	}
	foundPhysical := false
	for _, member := range ref.AllMembers() {
		physical, ok := member.(physicalPlanExpression)
		if !ok {
			continue
		}
		foundPhysical = true
		if !mergeDistinctLegProducesDistinctRecordsAtDepth(
			physical.GetRecordQueryPlan(), depth,
		) {
			return false
		}
	}
	return foundPhysical
}

func richOrderingEquals(a, b *properties.RichOrdering) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	aKeys := a.GetKeys()
	bKeys := b.GetKeys()
	if len(aKeys) != len(bKeys) {
		return false
	}
	for i := range aKeys {
		if values.ExplainValue(aKeys[i]) != values.ExplainValue(bKeys[i]) {
			return false
		}
	}
	return true
}

func removeCommonEqualityBoundParts(orderings []*properties.RichOrdering) []*properties.RichOrdering {
	if len(orderings) <= 1 {
		return orderings
	}

	type fixedEntry struct {
		key     string
		binding string
	}

	var commonEntries map[fixedEntry]struct{}
	for i, o := range orderings {
		entries := make(map[fixedEntry]struct{})
		bm := o.GetBindingMap()
		for _, key := range o.GetKeys() {
			keyStr := values.ExplainValue(key)
			bindings := bm[key]
			for _, b := range bindings {
				if b.IsFixed() {
					entries[fixedEntry{keyStr, explainBinding(b)}] = struct{}{}
				}
			}
		}
		if i == 0 {
			commonEntries = entries
		} else {
			for e := range commonEntries {
				if _, ok := entries[e]; !ok {
					delete(commonEntries, e)
				}
			}
		}
	}

	if len(commonEntries) == 0 {
		return orderings
	}

	keysToRemove := make(map[string]struct{})
	for e := range commonEntries {
		keysToRemove[e.key] = struct{}{}
	}

	result := make([]*properties.RichOrdering, len(orderings))
	for i, o := range orderings {
		var filteredKeys []values.Value
		filteredBindings := make(map[values.Value][]properties.OrderingBinding)
		for _, key := range o.GetKeys() {
			keyStr := values.ExplainValue(key)
			if _, remove := keysToRemove[keyStr]; remove {
				continue
			}
			filteredKeys = append(filteredKeys, key)
			if bs, ok := o.GetBindingMap()[key]; ok {
				filteredBindings[key] = bs
			}
		}
		result[i] = properties.NewRichOrdering(filteredBindings, filteredKeys,
			o.DistinctnessClaim())
	}
	return result
}

func explainBinding(b properties.OrderingBinding) string {
	comp := b.GetComparison()
	if comp == nil {
		return "fixed"
	}
	if s, ok := comp.(fmt.Stringer); ok {
		return s.String()
	}
	return "fixed"
}

func isPrimaryKeyCompatibleWithOrdering(pkValues []values.Value, ordering *properties.RichOrdering) bool {
	if ordering == nil || len(ordering.GetKeys()) == 0 {
		return len(pkValues) == 0
	}
	orderingKeySet := make(map[string]struct{}, len(ordering.GetKeys()))
	for _, k := range ordering.GetKeys() {
		orderingKeySet[values.ExplainValue(k)] = struct{}{}
	}
	for _, pkVal := range pkValues {
		if _, ok := orderingKeySet[values.ExplainValue(pkVal)]; !ok {
			return false
		}
	}
	return true
}

func getCommonPK(partitions []*PlanPartition) []values.Value {
	if len(partitions) == 0 {
		return nil
	}
	first := partitions[0].GetPartitionPropertyValue(properties.PropPrimaryKey)
	if first == nil {
		return nil
	}
	firstPK, ok := first.([]values.Value)
	if !ok {
		return nil
	}
	for _, p := range partitions[1:] {
		other := p.GetPartitionPropertyValue(properties.PropPrimaryKey)
		if other == nil {
			return nil
		}
		otherPK, ok := other.([]values.Value)
		if !ok || len(otherPK) != len(firstPK) {
			return nil
		}
		for i := range firstPK {
			if !values.ValuesStructurallyEqual(firstPK[i], otherPK[i]) {
				return nil
			}
		}
	}
	return firstPK
}

var _ ImplementationRule = (*ImplementDistinctUnionRule)(nil)
