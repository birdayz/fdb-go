package cascades

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctUnionRule implements Unique(Union(legs...)) as a
// merge-sorted union plan. It matches LogicalUniqueExpression over
// LogicalUnionExpression, finds compatible orderings across all union
// legs, and creates a RecordQueryMergeSortUnionPlan with deduplication.
//
// Ports Java's ImplementDistinctUnionRule. Per requested ordering it merges
// the leg partitions' orderings under the union merge, keeps the merges whose
// legs' primary keys stay within the merged keys, and yields one merge-sort
// union per satisfying comparison key. Where Java walks the cross product of
// leg partitions, the merge states are enumerated leg by leg
// (reachableUnionMergeStates).
type ImplementDistinctUnionRule struct {
	matcher matching.BindingMatcher
}

// Java's rule hangs off its LogicalDistinctExpression, a primary-key dedup;
// Go's node with that meaning is LogicalUniqueExpression, which
// PredicateToLogicalUnionRule puts over its union. Go's LogicalDistinct is a
// full-row dedup Java's Cascades has no node for.
func NewImplementDistinctUnionRule() *ImplementDistinctUnionRule {
	return &ImplementDistinctUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUniqueExpression]("implement_distinct_union"),
	}
}

func (r *ImplementDistinctUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctUnionRule) OnMatch(call *ImplementationRuleCall) {
	distinct := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)

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
		yielded := make(map[string]struct{})
		for _, legs := range distinctUnionLegsByPrimaryKey(legPartitions) {
			for _, state := range reachableUnionMergeStates(legs.orderings) {
				r.yieldFromMergedOrdering(call, legs.partitions, legs.primaryKey,
					state.merged, requestedOrdering, yielded)
			}
		}
	}
}

// distinctUnionLegs is the union's legs restricted to the partitions sharing
// one primary key, with each partition's ordering computed once.
type distinctUnionLegs struct {
	primaryKey []values.Value
	partitions [][]*PlanPartition
	orderings  [][]*properties.RichOrdering
}

func distinctUnionLegsByPrimaryKey(legPartitions [][]*PlanPartition) []distinctUnionLegs {
	var result []distinctUnionLegs
	for _, candidate := range legPartitions[0] {
		pk := partitionPrimaryKey(candidate)
		if len(pk) == 0 {
			continue
		}
		seen := false
		for _, legs := range result {
			if mergeDistinctPrimaryKeysEqual(legs.primaryKey, pk) {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		legs := distinctUnionLegs{
			primaryKey: pk,
			partitions: make([][]*PlanPartition, len(legPartitions)),
			orderings:  make([][]*properties.RichOrdering, len(legPartitions)),
		}
		complete := true
		for i, partitions := range legPartitions {
			for _, partition := range partitions {
				if !mergeDistinctPrimaryKeysEqual(partitionPrimaryKey(partition), pk) {
					continue
				}
				legs.partitions[i] = append(legs.partitions[i], partition)
				legs.orderings[i] = append(legs.orderings[i], partitionRichOrdering(partition))
			}
			if len(legs.partitions[i]) == 0 {
				complete = false
				break
			}
		}
		if complete {
			result = append(result, legs)
		}
	}
	return result
}

func partitionPrimaryKey(partition *PlanPartition) []values.Value {
	pk, _ := partition.GetPartitionPropertyValue(properties.PropPrimaryKey).([]values.Value)
	return pk
}

func partitionRichOrdering(partition *PlanPartition) *properties.RichOrdering {
	for _, expr := range partition.GetExpressions() {
		if ph, ok := expr.(physicalPlanExpression); ok {
			return computeWrapperRichOrdering(ph)
		}
	}
	o := partition.GetOrdering()
	bm := make(map[values.Value][]properties.OrderingBinding)
	for _, k := range o.Keys {
		bm[k] = []properties.OrderingBinding{properties.SortedBinding(properties.ProvidedSortOrderAscending)}
	}
	return properties.NewRichOrdering(bm, o.Keys, properties.NotDistinct())
}

// unionMergeState is what a prefix of legs contributes to the rest of the
// merge: the merged ordering and every merged leg's record identity, bound to
// the union of their coordinates.
type unionMergeState struct {
	merged   *properties.RichOrdering
	identity properties.CoordinateBoundClaim
}

func (s unionMergeState) equals(other unionMergeState) bool {
	return richOrderingsEqual(s.merged, other.merged) &&
		reflect.DeepEqual(s.merged.DistinctnessClaim(), other.merged.DistinctnessClaim()) &&
		reflect.DeepEqual(s.identity, other.identity)
}

// reachableUnionMergeStates returns the distinct merged orderings Java's walk
// over the cross product of leg partitions reaches. Java enumerates the cross
// product and prunes a failing prefix with skip(); its successes still grow as
// the product of each leg's compatible partitions. Two prefixes reaching an
// equal state have the same completions, so this keeps one state per class
// (the principle of optimality: each leg's plan is chosen per comparison key
// afterwards, not per partition).
func reachableUnionMergeStates(legOrderings [][]*properties.RichOrdering) []unionMergeState {
	var frontier []unionMergeState
	add := func(states []unionMergeState, state unionMergeState) []unionMergeState {
		for _, existing := range states {
			if existing.equals(state) {
				return states
			}
		}
		return append(states, state)
	}
	for _, ordering := range legOrderings[0] {
		frontier = add(frontier, unionMergeState{
			merged:   properties.CreateUnionOrdering(ordering),
			identity: ordering.RecordIdentityClaim(),
		})
	}
	for _, orderings := range legOrderings[1:] {
		var next []unionMergeState
		for _, state := range frontier {
			for _, ordering := range orderings {
				merged := properties.MergeOrderings(state.merged, ordering)
				identity := properties.IntersectClaims(state.identity, ordering.RecordIdentityClaim())
				// Java's isPrimaryKeyCompatibleWithOrdering, over every merged
				// leg's own primary-key coordinates.
				if !identity.Within(merged.GetKeys()) {
					continue
				}
				next = add(next, unionMergeState{merged: merged, identity: identity})
			}
		}
		frontier = next
	}
	return frontier
}

func (r *ImplementDistinctUnionRule) yieldFromMergedOrdering(
	call *ImplementationRuleCall,
	legPartitions [][]*PlanPartition,
	commonPrimaryKey []values.Value,
	mergedOrdering *properties.RichOrdering,
	requestedOrdering *properties.RequestedOrdering,
	yielded map[string]struct{},
) {
	if len(legPartitions) < 2 || len(commonPrimaryKey) == 0 {
		return
	}

	satisfyingKeys := mergedOrdering.EnumerateSatisfyingComparisonKeyValues(requestedOrdering)
	for _, comparisonKeyValues := range satisfyingKeys {
		comparisonParts := mergedOrdering.DirectionalOrderingParts(
			comparisonKeyValues, requestedOrdering, properties.ProvidedSortOrderFixed)
		isReverse := ResolveComparisonDirection(comparisonParts)
		comparisonParts = AdjustFixedBindings(comparisonParts, isReverse)

		// The comparison keys are the per-leg ordering contract of the
		// merge-front dedup: a leg whose executed order diverges from them
		// mis-merges. Each leg is the cheapest member of any of its partitions
		// that satisfies the requirement, spine-pinned and baked as the one
		// child over a FinalOf singleton; a comparison key one leg cannot
		// deliver is skipped and the in-memory-sort alternative still competes.
		legReqParts := make([]properties.RequestedOrderingPart, len(comparisonParts))
		for i, p := range comparisonParts {
			so := properties.RequestedSortOrderAny
			if p.SortOrder != properties.ProvidedSortOrderFixed {
				so = p.SortOrder.ToRequestedSortOrder()
			}
			legReqParts[i] = properties.RequestedOrderingPart{Value: p.Value, SortOrder: so}
		}
		legReq := properties.NewRequestedOrdering(legReqParts, properties.DistinctnessPreserveDistinctness, false)
		key := distinctUnionYieldKey(comparisonParts, isReverse)
		if _, done := yielded[key]; done {
			continue
		}
		yielded[key] = struct{}{}

		tieBrokenLess := lessWithHashTieBreak(call.CostModel())
		var childPlans []plans.RecordQueryPlan
		var newQuantifiers []expressions.Quantifier
		commonRecordType := ""
		haveCommonRecordType := false
		ok := true
		for _, partitions := range legPartitions {
			var candidates []expressions.RelationalExpression
			for _, partition := range partitions {
				for _, pe := range partition.GetExpressions() {
					if memberSatisfiesOrdering(pe, legReq) {
						candidates = append(candidates, pe)
					}
				}
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				return tieBrokenLess(candidates[i], candidates[j])
			})
			var pinned expressions.RelationalExpression
			var childPlan plans.RecordQueryPlan
			recordType := ""
			for _, candidate := range candidates {
				pinned, childPlan, recordType = pinDistinctUnionLeg(
					candidate, legReq, call.CostModel(), comparisonKeyValues, commonPrimaryKey)
				if pinned != nil {
					break
				}
			}
			if pinned == nil || haveCommonRecordType && recordType != commonRecordType {
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
		// key the wrong way round. A key the legs bind to different constants
		// takes the request's direction, which can disagree with a leg's scan
		// direction on the next key (TestDistinctUnionRuleRefusesMixedDirectionMerge).
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

// pinDistinctUnionLeg pins member's ordered spine for legReq and proves the
// leg can feed the merge: its record identity lies within the comparison key,
// it emits the complete stored record that key identifies, and it emits each
// record once. The comparison key is a PHYSICAL record key, a valid SQL
// UNION-DISTINCT key only while every leg emits the same full stored record for
// it; and the merge cursor holds one head per leg, so it cannot consume two
// consecutive equal rows of one leg together.
func pinDistinctUnionLeg(
	member expressions.RelationalExpression,
	legReq *properties.RequestedOrdering,
	less func(a, b expressions.RelationalExpression) bool,
	comparisonKeyValues []values.Value,
	commonPrimaryKey []values.Value,
) (expressions.RelationalExpression, plans.RecordQueryPlan, string) {
	pinned := pinOrderedSpine(member, legReq, less)
	pp, isPhys := pinned.(physicalPlanExpression)
	if pinned == nil || !isPhys {
		return nil, nil, ""
	}
	childPlan := pp.GetRecordQueryPlan()
	if !computeWrapperRichOrdering(pp).RecordIdentityWithin(comparisonKeyValues) {
		return nil, nil, ""
	}
	recordType, identityOK := mergeDistinctStoredRecordIdentity(childPlan, commonPrimaryKey)
	if !identityOK || !mergeDistinctLegProducesDistinctRecords(childPlan) {
		return nil, nil, ""
	}
	return pinned, childPlan, recordType
}

func distinctUnionYieldKey(parts []properties.ProvidedOrderingPart, reverse bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%t", reverse)
	for _, part := range parts {
		fmt.Fprintf(&b, "|%s %v", values.ExplainValue(part.Value), part.SortOrder)
	}
	return b.String()
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
			p.GetRecordTypes(), scanPrimaryKeyValues(p), commonPrimaryKey,
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

	case *plans.RecordQueryMapPlan:
		// The executor constructs a new one-slot PositionalRow even when the
		// planner classifies the value as an identity. Do not equate its
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

var _ ImplementationRule = (*ImplementDistinctUnionRule)(nil)
