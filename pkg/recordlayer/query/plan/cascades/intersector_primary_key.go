package cascades

import (
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// WithPrimaryKeyIntersector returns an IntersectorFunc that creates physical
// intersection plans from compatible partial-match subsets. Every subset must
// share a structural primary key and a rich directional ordering whose
// comparison key contains every non-fixed primary-key component.
//
// It creates RecordQueryIntersectionPlan directly (physical, not logical).
// This avoids the task cascade that would occur if a logical intersection were
// inserted and then explored — fresh child References trigger re-exploration
// loops.
func WithPrimaryKeyIntersector(ctx PlanContext) IntersectorFunc {
	return func(
		accesses []Vectored[*SingleMatchedAccess],
		requestedOrderings []*properties.RequestedOrdering,
	) *IntersectionResult {
		if len(accesses) < 2 {
			return NoViableIntersection()
		}

		if len(requestedOrderings) == 0 {
			requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
		} else {
			nonNil := requestedOrderings[:0:0]
			for _, requested := range requestedOrderings {
				if requested != nil {
					nonNil = append(nonNil, requested)
				}
			}
			requestedOrderings = nonNil
			if len(requestedOrderings) == 0 {
				requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
			}
		}

		intersectionInfos := initializeSingletonIntersectionInfos(accesses, ctx)
		var intersectionInfoOrder []intersectionInfoKey

		// Java's AbstractDataAccessRule enumerates ChooseK(accesses, k) for
		// every k from 2 through the candidate count. Record the structurally
		// incompatible binary pairs as a sieve: every larger partition that
		// contains one can be rejected without rebuilding its scans. Do NOT
		// put compensation failures in the sieve — compensation intersection
		// is a separate semantic question, and a larger partition must still
		// get its own fold.
		badPairs := make(map[[2]int]struct{})
		for size := 2; size <= len(accesses); size++ {
			hasViablePartition := false
			forEachAccessCombination(accesses, size, func(partition []Vectored[*SingleMatchedAccess]) {
				if partitionContainsBadPair(partition, badPairs) {
					return
				}

				built := createPrimaryKeyIntersection(partition, requestedOrderings, ctx)
				if !built.structurallyCompatible {
					if size == 2 {
						badPairs[orderedPositionPair(partition[0].Position, partition[1].Position)] = struct{}{}
					}
					return
				}
				if isPrimaryKeyPartitionRedundant(
					partition,
					built.equalityBoundValues,
					intersectionInfos,
				) {
					// Java treats a redundant binary partition exactly like
					// one without a common ordering for sieve purposes: every
					// superset containing that useless pair is useless too.
					if size == 2 {
						badPairs[orderedPositionPair(partition[0].Position, partition[1].Position)] = struct{}{}
					}
					return
				}
				// A common ordering is viable even when compensation is
				// impossible and therefore yields zero expressions. Java
				// stores that proof metadata and keeps the k-level alive.
				hasViablePartition = true

				key := intersectionInfoKeyForPartition(partition)
				intersectionInfos[key] = IntersectionInfoOfIntersection(
					built.commonOrdering,
					built.compensation,
					built.expressions,
				)
				intersectionInfoOrder = append(intersectionInfoOrder, key)

				// A useful larger partition supersedes its immediate
				// subpartitions only after the Java redundancy proof above
				// says the extra access adds filtering. An impossible
				// compensation produces no replacement expression and must
				// never evict a usable smaller plan.
				if len(built.expressions) > 0 {
					for _, subpartition := range immediateSubpartitions(partition) {
						subpartitionKey := intersectionInfoKeyForPartition(subpartition)
						if subInfo := intersectionInfos[subpartitionKey]; subInfo != nil {
							subInfo.EvictExpressions()
						}
					}
				}
			})
			if !hasViablePartition {
				// Java's early-out: if no size-k partition can share the
				// useful comparison contract, no size-(k+1) partition can
				// either.
				break
			}
		}

		var resultExprs []expressions.RelationalExpression
		var resultOrdering *properties.RichOrdering
		resultCompensation := Compensation(NoCompensation)
		for _, key := range intersectionInfoOrder {
			info := intersectionInfos[key]
			if info == nil || len(info.GetExpressions()) == 0 {
				continue
			}
			if resultOrdering == nil {
				resultOrdering = info.GetOrdering()
				resultCompensation = info.GetCompensation()
			}
			resultExprs = append(resultExprs, info.GetExpressions()...)
		}
		if len(resultExprs) == 0 {
			return NoViableIntersection()
		}

		return NewIntersectionResult(resultOrdering, resultCompensation, resultExprs)
	}
}

// forEachAccessCombination streams lexicographic size-element subsets of
// accesses. The callback's partition is a fresh slice and may be retained.
func forEachAccessCombination(
	accesses []Vectored[*SingleMatchedAccess],
	size int,
	visit func([]Vectored[*SingleMatchedAccess]),
) {
	if size <= 0 || size > len(accesses) {
		return
	}
	partition := make([]Vectored[*SingleMatchedAccess], size)
	var enumerate func(depth, start int)
	enumerate = func(depth, start int) {
		if depth == size {
			visit(append([]Vectored[*SingleMatchedAccess](nil), partition...))
			return
		}
		remaining := size - depth
		for i := start; i <= len(accesses)-remaining; i++ {
			partition[depth] = accesses[i]
			enumerate(depth+1, i+1)
		}
	}
	enumerate(0, 0)
}

func orderedPositionPair(a, b int) [2]int {
	if a > b {
		a, b = b, a
	}
	return [2]int{a, b}
}

func partitionContainsBadPair(
	partition []Vectored[*SingleMatchedAccess],
	badPairs map[[2]int]struct{},
) bool {
	for i := 0; i < len(partition)-1; i++ {
		for j := i + 1; j < len(partition); j++ {
			if _, bad := badPairs[orderedPositionPair(partition[i].Position, partition[j].Position)]; bad {
				return true
			}
		}
	}
	return false
}

type intersectionInfoKey string

func intersectionInfoKeyForPartition(
	partition []Vectored[*SingleMatchedAccess],
) intersectionInfoKey {
	bits := NewBitSet()
	for _, vectored := range partition {
		bits.Set(vectored.Position)
	}
	return intersectionInfoKey(bits.String())
}

func immediateSubpartitions(
	partition []Vectored[*SingleMatchedAccess],
) [][]Vectored[*SingleMatchedAccess] {
	if len(partition) <= 1 {
		return nil
	}
	result := make([][]Vectored[*SingleMatchedAccess], 0, len(partition))
	for omitted := range partition {
		subpartition := make([]Vectored[*SingleMatchedAccess], 0, len(partition)-1)
		subpartition = append(subpartition, partition[:omitted]...)
		subpartition = append(subpartition, partition[omitted+1:]...)
		result = append(result, subpartition)
	}
	return result
}

func initializeSingletonIntersectionInfos(
	accesses []Vectored[*SingleMatchedAccess],
	ctx PlanContext,
) map[intersectionInfoKey]*IntersectionInfo {
	infos := make(map[intersectionInfoKey]*IntersectionInfo, len(accesses))
	for _, vectored := range accesses {
		partition := []Vectored[*SingleMatchedAccess]{vectored}
		access := vectored.Value
		pkValues := commonPrimaryKeyValuesForPartition(partition, ctx)
		ordering := adjustedIntersectionOrdering(
			access,
			implicitFixedPrimaryKeyValues(partition, pkValues),
		)
		if ordering == nil {
			ordering = properties.EmptyOrdering()
		}

		compensation := access.GetCompensation()
		scan := createScanForAccess(access)
		expr, ok := compensatedSingleAccessExpression(access, scan)
		if !ok {
			infos[intersectionInfoKeyForPartition(partition)] = IntersectionInfoOfImpossibleAccess(ordering, compensation)
			continue
		}
		infos[intersectionInfoKeyForPartition(partition)] = IntersectionInfoOfSingleAccess(
			ordering,
			compensation,
			expr,
			maxCardinalityForRedundancy(expr),
		)
	}
	return infos
}

func compensatedSingleAccessExpression(
	access *SingleMatchedAccess,
	scan plans.RecordQueryPlan,
) (expressions.RelationalExpression, bool) {
	if access == nil || scan == nil {
		return nil, false
	}
	var expr expressions.RelationalExpression = wrapAccessScan(access, scan)
	compensation := access.GetCompensation()
	if compensation == nil || compensation.IsImpossible() {
		return nil, false
	}
	if !compensation.IsNeeded() {
		return expr, true
	}
	forMatch, ok := compensation.(*ForMatchCompensation)
	if !ok || forMatch == nil {
		return nil, false
	}
	return forMatch.ApplyAllNeeded(
		expr,
		func(realizedAlias values.CorrelationIdentifier) TranslationMap {
			return TranslationMapOfAliases(
				access.GetCandidateTopAlias(),
				realizedAlias,
			)
		},
	)
}

// maxCardinalityForRedundancy computes only proof-grade upper bounds. Unknown
// remains unknown; the heuristic EstimateCardinality is intentionally not
// consulted because an underestimate would make redundancy pruning unsound.
func maxCardinalityForRedundancy(
	expr expressions.RelationalExpression,
) int64 {
	if expr == nil {
		return CardinalityUnknown
	}
	if physical, ok := expr.(physicalPlanExpression); ok {
		plan := physical.GetRecordQueryPlan()
		if plan == nil {
			return CardinalityUnknown
		}
		maxCardinality := computeCardinalities(
			physical,
			plan,
		).GetMaxCardinality()
		if maxCardinality.IsUnknown() {
			return CardinalityUnknown
		}
		return maxCardinality.Value()
	}

	switch expr.(type) {
	case *expressions.LogicalFilterExpression,
		*expressions.LogicalProjectionExpression,
		*expressions.LogicalTypeFilterExpression,
		*expressions.LogicalDistinctExpression,
		*expressions.LogicalUniqueExpression,
		*expressions.LogicalSortExpression:
		quantifiers := expr.GetQuantifiers()
		if len(quantifiers) != 1 {
			return CardinalityUnknown
		}
		return maxCardinalityForRedundancyQuantifier(quantifiers[0])

	case *expressions.SelectExpression:
		const maxInt64 = int64(^uint64(0) >> 1)
		result := int64(1)
		for _, quantifier := range expr.GetQuantifiers() {
			if quantifier.Kind() == expressions.QuantifierExistential {
				continue
			}
			child := maxCardinalityForRedundancyQuantifier(quantifier)
			if child == CardinalityUnknown {
				return CardinalityUnknown
			}
			if quantifier.IsNullOnEmpty() && child == 0 {
				child = 1
			}
			if result != 0 && child > maxInt64/result {
				return CardinalityUnknown
			}
			result *= child
		}
		return result
	}
	return CardinalityUnknown
}

func maxCardinalityForRedundancyQuantifier(
	quantifier expressions.Quantifier,
) int64 {
	ref := quantifier.GetRangesOver()
	if ref == nil {
		return CardinalityUnknown
	}
	member, single := onlyReferenceMember(ref)
	if !single {
		return CardinalityUnknown
	}
	return maxCardinalityForRedundancy(member)
}

func isPrimaryKeyPartitionRedundant(
	partition []Vectored[*SingleMatchedAccess],
	equalityBoundValues []values.Value,
	infos map[intersectionInfoKey]*IntersectionInfo,
) bool {
	var partitionUnmatched map[values.CorrelationIdentifier]struct{}
	for i, vectored := range partition {
		singleton := []Vectored[*SingleMatchedAccess]{vectored}
		info := infos[intersectionInfoKeyForPartition(singleton)]
		if info == nil {
			return false
		}
		maxCardinality := info.GetMaxCardinality()
		if maxCardinality != CardinalityUnknown && maxCardinality <= 1 {
			return true
		}
		unmatched, known := compensationUnmatchedIDs(info.GetCompensation())
		if !known {
			return false
		}
		if i == 0 {
			partitionUnmatched = cloneCorrelationSet(unmatched)
		} else {
			partitionUnmatched = intersectUnmatchedIDSets(
				partitionUnmatched,
				unmatched,
			)
		}
	}

	for _, subpartition := range immediateSubpartitions(partition) {
		info := infos[intersectionInfoKeyForPartition(subpartition)]
		if info == nil ||
			!orderingContainsEqualityValues(
				info.GetOrdering(),
				equalityBoundValues,
			) {
			continue
		}
		subpartitionUnmatched, known := compensationUnmatchedIDs(
			info.GetCompensation(),
		)
		// Java fails this proof open as soon as a qualifying
		// subpartition's compensation metadata is unknown.
		if !known {
			return false
		}
		if correlationSetsEqual(partitionUnmatched, subpartitionUnmatched) {
			return true
		}
	}
	return false
}

// orderingContainsEqualityValues asks whether a SUBPARTITION's ordering fixes
// every equality the FULL partition fixes — the redundancy proof that lets a
// partition be dropped in favour of a smaller one.
//
// It compares through intersectionValuesEqual (via containsIntersectionValue),
// which is the same comparator the `required` list was DEDUPED by when it was
// built. That agreement is the point: a list built under one notion of "same
// value" and probed under another can report a member missing that it holds, and
// here the two notions differ exactly where it matters. A bare structural
// comparison is domain-blind, so two legs whose row layouts collide on ordinals
// have their DIFFERENT columns declared equal, the subpartition looks like it
// fixes an equality it does not, and the partition is dropped — and at arity two
// it is also entered in badPairs, killing every superset. A lost intersection
// plan, from a proof that was never entitled to succeed.
func orderingContainsEqualityValues(
	ordering *properties.RichOrdering,
	required []values.Value,
) bool {
	if ordering == nil {
		return len(required) == 0
	}
	var provided []values.Value
	for value := range ordering.GetEqualityBoundValues() {
		provided = append(provided, value)
	}
	for _, requiredValue := range required {
		if !containsIntersectionValue(provided, requiredValue) {
			return false
		}
	}
	return true
}

func compensationUnmatchedIDs(
	compensation Compensation,
) (map[values.CorrelationIdentifier]struct{}, bool) {
	if compensation == nil {
		return nil, false
	}
	if !compensation.IsNeeded() {
		return map[values.CorrelationIdentifier]struct{}{}, true
	}
	forMatch, ok := compensation.(*ForMatchCompensation)
	if !ok || forMatch == nil || forMatch.GetGroupByMappings() == nil {
		return nil, false
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	forMatch.GetGroupByMappings().UnmatchedAggregatesMap().Range(
		func(id values.CorrelationIdentifier, _ values.Value) bool {
			result[id] = struct{}{}
			return true
		},
	)
	return result, true
}

func cloneCorrelationSet(
	source map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{}, len(source))
	for id := range source {
		result[id] = struct{}{}
	}
	return result
}

func intersectUnmatchedIDSets(
	left, right map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for id := range left {
		if _, ok := right[id]; ok {
			result[id] = struct{}{}
		}
	}
	return result
}

func correlationSetsEqual(
	left, right map[values.CorrelationIdentifier]struct{},
) bool {
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if _, ok := right[id]; !ok {
			return false
		}
	}
	return true
}

type primaryKeyIntersectionBuild struct {
	expressions            []expressions.RelationalExpression
	commonOrdering         *properties.RichOrdering
	compensation           Compensation
	equalityBoundValues    []values.Value
	structurallyCompatible bool
}

type primaryKeyComparisonAlternative struct {
	parts   []properties.ProvidedOrderingPart
	reverse bool
}

// requestedOrderingTranslationAdapter exposes the planner-level TranslationMap
// through the Value package's equivalent leaf-translation interface. The two
// interfaces differ only because values is the dependency-leaf package and
// cannot import cascades.
type requestedOrderingTranslationAdapter struct {
	translation TranslationMap
}

func (a requestedOrderingTranslationAdapter) ContainsSourceAlias(
	alias values.CorrelationIdentifier,
) bool {
	return a.translation != nil && a.translation.ContainsSourceAlias(alias)
}

func (a requestedOrderingTranslationAdapter) ApplyTranslationFunction(
	sourceAlias values.CorrelationIdentifier,
	leaf values.Value,
) values.Value {
	leafValue, ok := leaf.(values.LeafValue)
	if !ok || a.translation == nil {
		return leaf
	}
	return a.translation.ApplyTranslationFunction(sourceAlias, leafValue)
}

func (a requestedOrderingTranslationAdapter) DefinesOnlyIdentities() bool {
	return a.translation == nil || a.translation.DefinesOnlyIdentities()
}

// translateIntersectionRequestedOrdering expresses a query-top requested
// ordering in the first access candidate's top scope. Java performs this
// translation before enumerating the common comparison keys; omitting it makes
// an adjusted/non-identity match either over-decline or compare aliases from
// different frames.
func translateIntersectionRequestedOrdering(
	requested *properties.RequestedOrdering,
	translation TranslationMap,
) *properties.RequestedOrdering {
	if requested == nil || translation == nil ||
		translation.DefinesOnlyIdentities() {
		return requested
	}
	parts := requested.GetParts()
	translated := make([]properties.RequestedOrderingPart, len(parts))
	adapter := requestedOrderingTranslationAdapter{translation: translation}
	for i, part := range parts {
		translated[i] = properties.RequestedOrderingPart{
			Value: values.TranslateCorrelations(
				part.Value,
				adapter,
			),
			SortOrder: part.SortOrder,
		}
	}
	return properties.NewRequestedOrdering(
		translated,
		requested.GetDistinctness(),
		requested.IsExhaustive(),
	)
}

// createPrimaryKeyIntersection builds one arbitrary-arity partition. A
// structurally-compatible result has distinct candidate names, a common
// structural primary key, realizable scans, and at least one rich common
// comparison ordering that includes every non-fixed primary-key component and
// bakes against every child layout. It may still carry no expression when the
// compensation fold cannot be realized; callers must not turn that semantic
// miss into an ordering-sieve failure.
func createPrimaryKeyIntersection(
	partition []Vectored[*SingleMatchedAccess],
	requestedOrderings []*properties.RequestedOrdering,
	ctx PlanContext,
) primaryKeyIntersectionBuild {
	accesses := make([]*SingleMatchedAccess, 0, len(partition))
	scans := make([]plans.RecordQueryPlan, 0, len(partition))
	seenCandidates := make(map[string]struct{}, len(partition))
	for _, vectored := range partition {
		access := vectored.Value
		name := access.GetPartialMatch().GetMatchCandidate().CandidateName()
		if _, duplicate := seenCandidates[name]; duplicate {
			return primaryKeyIntersectionBuild{}
		}
		seenCandidates[name] = struct{}{}

		scan := createScanForAccess(access)
		if scan == nil {
			return primaryKeyIntersectionBuild{}
		}
		accesses = append(accesses, access)
		scans = append(scans, scan)
	}

	pkValues := commonPrimaryKeyValuesForPartition(partition, ctx)
	if len(pkValues) == 0 {
		return primaryKeyIntersectionBuild{}
	}
	implicitFixedValues := implicitFixedPrimaryKeyValues(partition, pkValues)

	var commonOrdering *properties.RichOrdering
	var equalityBoundValues []values.Value
	for _, access := range accesses {
		ordering := adjustedIntersectionOrdering(access, implicitFixedValues)
		if ordering == nil {
			return primaryKeyIntersectionBuild{}
		}
		if commonOrdering == nil {
			commonOrdering = ordering
		} else {
			commonOrdering = properties.MergeOrderingsForIntersection(commonOrdering, ordering)
		}
		for value := range ordering.GetEqualityBoundValues() {
			if !containsIntersectionValue(equalityBoundValues, value) {
				equalityBoundValues = append(equalityBoundValues, value)
			}
		}
	}
	if commonOrdering == nil {
		return primaryKeyIntersectionBuild{}
	}

	compensations := make([]Compensation, 0, len(accesses))
	for _, access := range accesses {
		compensations = append(compensations, access.GetCompensation())
	}
	intersectionCompensation := IntersectCompensations(compensations)

	var alternatives []primaryKeyComparisonAlternative
	topToTopTranslation := accesses[0].GetTopToTopTranslationMap()
	for _, requested := range requestedOrderings {
		requested = translateIntersectionRequestedOrdering(
			requested,
			topToTopTranslation,
		)
		if requested == nil {
			continue
		}
		for _, comparisonValues := range commonOrdering.
			EnumerateSatisfyingIntersectionComparisonKeyValues(requested) {
			// An empty physical merge key cannot establish cursor progress.
			// Java can represent more Value shapes than Go's executor today;
			// declining here is the bounded, safe optimization miss.
			if len(comparisonValues) == 0 ||
				!comparisonKeyContainsFreePrimaryKey(
					comparisonValues, pkValues, equalityBoundValues,
				) {
				continue
			}

			parts := commonOrdering.DirectionalOrderingParts(
				comparisonValues,
				requested,
				properties.ProvidedSortOrderFixed,
			)
			reverse := ResolveComparisonDirection(parts)
			parts = AdjustFixedBindings(parts, reverse)
			if _, ok := properties.NaturalComparisonKeyValues(parts, reverse); !ok {
				continue
			}
			if !plainFieldComparisonParts(parts) {
				continue
			}

			bakedParts := bakeIntersectionOrderingParts(parts, scans)
			if bakedParts == nil {
				continue
			}
			if containsComparisonAlternative(alternatives, bakedParts, reverse) {
				continue
			}
			alternatives = append(alternatives, primaryKeyComparisonAlternative{
				parts:   bakedParts,
				reverse: reverse,
			})
		}
	}
	if len(alternatives) == 0 {
		return primaryKeyIntersectionBuild{}
	}

	childQs := make([]expressions.Quantifier, 0, len(partition))
	for i, access := range accesses {
		var expr expressions.RelationalExpression = wrapAccessScan(
			access,
			scans[i],
		)
		if candidateCreatesDuplicates(
			access.GetPartialMatch().GetMatchCandidate(),
		) {
			// Java's distinctMatchToScanMap inserts an unordered primary-key
			// distinct on every fan-out leg before it participates in a merge
			// intersection. The merge executor has set semantics and the
			// intersection property advertises distinct records; allowing
			// duplicate PKs into a leg would violate both contracts.
			expr = plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(
					expressions.FinalOfAtStage(
						expr,
						expressions.StageCanonical,
					),
				),
			)
		}
		childQs = append(childQs, expressions.NewPhysicalQuantifier(
			expressions.FinalOfAtStage(expr, expressions.StageCanonical),
		))
	}

	built := primaryKeyIntersectionBuild{
		commonOrdering:         commonOrdering,
		compensation:           intersectionCompensation,
		equalityBoundValues:    equalityBoundValues,
		structurallyCompatible: true,
	}
	for _, alternative := range alternatives {
		intersectionPlan := plans.NewRecordQueryIntersectionPlanFromQuantifiersWithOrdering(
			childQs,
			alternative.parts,
			alternative.reverse,
		)
		if intersectionPlan == nil {
			continue
		}
		expr, viable := compensateIntersection(accesses, intersectionPlan)
		if viable {
			built.expressions = append(built.expressions, expr)
		}
	}
	return built
}

func candidateCreatesDuplicates(candidate MatchCandidate) bool {
	duplicateCandidate, ok := candidate.(interface {
		CreatesDuplicates() bool
	})
	return ok && duplicateCandidate.CreatesDuplicates()
}

func adjustedIntersectionOrdering(
	access *SingleMatchedAccess,
	implicitFixedValues []values.Value,
) *properties.RichOrdering {
	if access == nil || access.GetPartialMatch() == nil {
		return nil
	}
	pm := access.GetPartialMatch()
	matchInfo := pm.GetMatchInfo()
	if matchInfo == nil || matchInfo.GetRegularMatchInfo() == nil {
		return nil
	}
	candidate := pm.GetMatchCandidate()
	if candidate == nil {
		return nil
	}
	prefix := candidate.ComputeBoundParameterPrefixMap(
		matchInfo.GetRegularMatchInfo().GetParameterBindingMap(),
	)

	bindings := make(map[values.Value][]properties.OrderingBinding)
	var keys []values.Value
	var seen []values.Value
	for _, value := range implicitFixedValues {
		if value == nil || containsIntersectionValue(seen, value) {
			continue
		}
		seen = append(seen, value)
		keys = append(keys, value)
		bindings[value] = []properties.OrderingBinding{
			properties.FixedBinding(nil),
		}
	}
	for _, part := range matchInfo.GetMatchedOrderingParts() {
		if part == nil || part.GetValue() == nil ||
			containsIntersectionValue(seen, part.GetValue()) {
			continue
		}
		value := part.GetValue()
		seen = append(seen, value)
		keys = append(keys, value)

		// Only an equality that the physical scan's ACTUAL consumed prefix
		// contains is fixed. A matched equality after a prefix gap is not
		// enforced by the scan and must remain directional.
		actualRange := prefix[part.GetParameterId()]
		if actualRange != nil && actualRange.IsEquality() {
			bindings[value] = []properties.OrderingBinding{
				properties.FixedBinding(actualRange.GetEqualityComparison()),
			}
		} else {
			bindings[value] = []properties.OrderingBinding{
				properties.SortedBinding(
					part.GetMatchedSortOrder().ToProvidedSortOrder(
						access.IsReverseScanOrder(),
					),
				),
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return properties.NewRichOrdering(bindings, keys, false)
}

// implicitFixedPrimaryKeyValues returns structural PK components that every
// leg fixes without an explicit scan comparison. A record-type discriminator
// is constant for a candidate over exactly one common record type; Java
// prepends the candidate's implicit equality parts before intersecting
// orderings. Go currently models only this implicit component.
func implicitFixedPrimaryKeyValues(
	partition []Vectored[*SingleMatchedAccess],
	pkValues []values.Value,
) []values.Value {
	var commonRecordType string
	for i, vectored := range partition {
		recordTypes := vectored.Value.GetPartialMatch().GetMatchCandidate().GetRecordTypes()
		if len(recordTypes) != 1 {
			return nil
		}
		if i == 0 {
			commonRecordType = recordTypes[0]
		} else if recordTypes[0] != commonRecordType {
			return nil
		}
	}
	var result []values.Value
	for _, pkValue := range pkValues {
		if _, ok := pkValue.(*values.RecordTypeValue); ok {
			result = append(result, pkValue)
		}
	}
	return result
}

func comparisonKeyContainsFreePrimaryKey(
	comparisonValues []values.Value,
	pkValues []values.Value,
	equalityBoundValues []values.Value,
) bool {
	for _, pkValue := range pkValues {
		if containsIntersectionValue(equalityBoundValues, pkValue) {
			continue
		}
		if !containsIntersectionValue(comparisonValues, pkValue) {
			return false
		}
	}
	return true
}

func containsIntersectionValue(haystack []values.Value, needle values.Value) bool {
	for _, value := range haystack {
		if intersectionValuesEqual(value, needle) {
			return true
		}
	}
	return false
}

// intersectionValuesEqual is orderingValuesEqual's counterpart for the
// primary-key intersection's own value lists (comparison keys, equality-bound
// values, the implicit record-type discriminators). Same TYPE dispatch and the
// same reason: a pair of plain FieldValues is decided by column identity alone,
// because ValuesStructurallyEqual compares two baked ordinal paths without
// comparing the layouts they index.
//
// Transitivity is load-bearing HERE in a way it is not anywhere else in the
// intersector, because adjustedIntersectionOrdering builds its key list through
// a `seen` dedup (containsIntersectionValue against the keys accepted so far).
// An intransitive comparator makes membership in `seen` depend on the order the
// keys were visited, so the same partition yields different comparison keys and
// different bindings on different runs. That is not a lost merge, it is a
// nondeterministic plan — which is why the FieldValue arm may not fall through
// even when it declines.
// The ordinal-free NAME bridge that used to sit under the structural arm is
// GONE, and it is unreachable rather than merely unused:
// CanBridgeOrderingFieldValues requires BOTH operands to be *FieldValue, and
// every such pair now returns from the arm above. Keeping a call that cannot
// fire would read as a live fallback for the very class the identity arm was
// made final over.
func intersectionValuesEqual(left, right values.Value) bool {
	recordOrderingComparison(
		OrderingSiteIntersectionKeys, left, right,
		values.CanBridgeOrderingFieldValues,
	)
	if values.StatesOrderingColumn(left) && values.StatesOrderingColumn(right) {
		return values.SameOrderingColumn(left, right)
	}
	if values.ValuesStructurallyEqual(left, right) {
		return true
	}
	return values.CanBridgeOrderingFieldValues(left, right)
}

func plainFieldComparisonParts(parts []properties.ProvidedOrderingPart) bool {
	for _, part := range parts {
		field, ok := part.Value.(*values.FieldValue)
		if !ok || field.Child != nil {
			return false
		}
	}
	return true
}

func bakeIntersectionOrderingParts(
	parts []properties.ProvidedOrderingPart,
	scans []plans.RecordQueryPlan,
) []properties.ProvidedOrderingPart {
	valuesToBake := make([]values.Value, len(parts))
	for i, part := range parts {
		valuesToBake[i] = part.Value
	}
	baked := bakedIntersectionKeys(valuesToBake, scans)
	if baked == nil {
		return nil
	}
	result := make([]properties.ProvidedOrderingPart, len(parts))
	for i, part := range parts {
		result[i] = properties.ProvidedOrderingPart{
			Value:     baked[i],
			SortOrder: part.SortOrder,
		}
	}
	return result
}

func containsComparisonAlternative(
	alternatives []primaryKeyComparisonAlternative,
	parts []properties.ProvidedOrderingPart,
	reverse bool,
) bool {
	for _, alternative := range alternatives {
		if alternative.reverse != reverse || len(alternative.parts) != len(parts) {
			continue
		}
		equal := true
		for i := range parts {
			if alternative.parts[i].SortOrder != parts[i].SortOrder ||
				!values.ValuesStructurallyEqual(
					alternative.parts[i].Value,
					parts[i].Value,
				) {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
}

// compensateIntersection ports the compensation clause of Java's
// WithPrimaryKeyDataAccessRule.createIntersectionAndCompensation
// (WithPrimaryKeyDataAccessRule.java:135-139, 166, 191-194): the per-leg
// compensations fold via the intersection monoid; an impossible fold
// declines the combination outright (Java builds no expression for it — a
// leg residual that cannot be reapplied must never be silently dropped),
// and a needed fold wraps the intersection with the compensated residual
// predicates. Every leg's candidate-top alias rebases onto the realized
// base alias, exactly Java's matchedToRealizedTranslationMap (:233-242).
//
// Without this, `WHERE a=? AND b=? AND c=?` over idx(a), idx(b) planned to
// a bare Intersection(idx_a, idx_b) and the c residual VANISHED — wrong
// rows, confirmed live against FDB (see the pinned regression tests).
func compensateIntersection(
	accesses []*SingleMatchedAccess,
	intersectionExpr expressions.RelationalExpression,
) (expressions.RelationalExpression, bool) {
	comps := make([]Compensation, 0, len(accesses))
	for _, a := range accesses {
		comps = append(comps, a.GetCompensation())
	}
	comp := IntersectCompensations(comps)
	if comp.IsImpossible() {
		return nil, false
	}
	if !comp.IsNeeded() {
		return intersectionExpr, true
	}
	fmc, ok := comp.(*ForMatchCompensation)
	if !ok {
		// A needed compensation only the ForMatch form can reapply;
		// anything else cannot be realized here — decline like Java's
		// impossible arm rather than drop the residual.
		return nil, false
	}
	return fmc.ApplyAllNeeded(intersectionExpr, func(realizedAlias values.CorrelationIdentifier) TranslationMap {
		b := NewTranslationMapBuilder()
		for _, a := range accesses {
			topAlias := a.GetCandidateTopAlias()
			if topAlias.IsZero() {
				continue
			}
			b.When(topAlias).Then(func(_ values.CorrelationIdentifier, leafValue values.LeafValue) values.Value {
				return leafValue.RebaseLeaf(realizedAlias)
			})
		}
		return b.Build()
	})
}

// commonPrimaryKeyValuesForPartition prefers the candidate-carried
// STRUCTURAL primary key used by PrimaryKeyProperty. If any candidate exposes
// that contract, every leg must expose the same non-nil Value list; mixing it
// with the legacy name-only metadata would erase record-type prefixes or
// nesting. Test doubles and older specialized candidates that expose no
// structural contract at all retain the single-record-type PlanContext
// fallback below.
func commonPrimaryKeyValuesForPartition(
	accesses []Vectored[*SingleMatchedAccess],
	ctx PlanContext,
) []values.Value {
	var common []values.Value
	exposedStructural := false
	missingStructural := false
	for _, vectored := range accesses {
		candidate := vectored.Value.GetPartialMatch().GetMatchCandidate()
		provider, exposed := candidate.(interface {
			GetCommonPrimaryKeyValues() []values.Value
		})
		if !exposed {
			missingStructural = true
			continue
		}
		exposedStructural = true
		pk := provider.GetCommonPrimaryKeyValues()
		if len(pk) == 0 {
			return nil
		}
		if common == nil {
			common = append([]values.Value(nil), pk...)
			continue
		}
		if len(common) != len(pk) {
			return nil
		}
		for i := range common {
			if !values.ValuesStructurallyEqual(common[i], pk[i]) {
				return nil
			}
		}
	}
	if exposedStructural {
		if missingStructural {
			return nil
		}
		return bakeIntersectionPrimaryKeyValues(accesses, common)
	}
	return bakeIntersectionPrimaryKeyValues(accesses, commonPrimaryKeyValues(accesses, ctx))
}

// bakeIntersectionPrimaryKeyValues states the layout each primary-key
// comparison key's ordinal indexes: the partition's ONE common record row.
//
// This is the domain rule for intersection keys, applied at the first place the
// domain is knowable. Every producer upstream is deliberately layout-free: the
// metadata translation (metadataIndexDef.IndexCommonPrimaryKeyValues, over
// TranslatePrimaryKeyToValues) describes a record type's key structure and has
// no row to resolve it against, and the PlanContext fallback below has only
// column names. The row layout belongs to the CANDIDATES, so the partition is
// where a name can become an ordinal — and it must happen here, because the
// comparison keys are compared against the legs' matched ordering parts, which
// ARE domained (bakeOrderingColumnIn), and a lazy key cannot meet a baked one
// under type dispatch.
//
// The layout is the record row and never a per-index entry layout: the keys
// exist for CROSS-LEG comparison, so per-index domains would hand two legs two
// different tokens for the same primary-key column and the merged ordering would
// come out empty. It also sits consistently beside the *RecordTypeValue
// discriminators in the same key list, which are record-level too and are left
// exactly as they are — a discriminator is not a column of any row and has no
// ordinal to state.
//
// A key that cannot be resolved is left LAZY rather than guessed. That is the
// fail-closed direction and it costs an intersection candidate, not a wrong
// answer: the comparator declines the unaddressable key, the free-primary-key
// proof fails, and the partition simply does not produce an intersection.
func bakeIntersectionPrimaryKeyValues(
	accesses []Vectored[*SingleMatchedAccess],
	pk []values.Value,
) []values.Value {
	if len(pk) == 0 {
		return pk
	}
	layout := intersectionKeyLayout(accesses)
	if layout == nil {
		return pk
	}
	out := make([]values.Value, len(pk))
	for i, v := range pk {
		out[i] = v
		field, isField := v.(*values.FieldValue)
		if !isField || field == nil || field.Resolved != nil || field.Child != nil {
			continue
		}
		out[i] = bakeOrderingColumnIn(layout, field.Field)
	}
	return out
}

// intersectionKeyLayout returns the ONE record row layout every leg of the
// partition agrees its ordering keys are domained in, or nil.
//
// Unanimity is required, not majority or first: a token from one leg's layout
// stamped onto another leg's keys is precisely the ordinal-across-layouts
// conflation the domain exists to refuse. Agreement is tested on the DOMAIN
// TOKEN rather than on pointer identity of the RecordType, because the legs
// reach their layout through different candidate kinds and build it separately.
func intersectionKeyLayout(
	accesses []Vectored[*SingleMatchedAccess],
) *values.RecordType {
	var layout *values.RecordType
	var domain values.OrdinalDomain
	for _, vectored := range accesses {
		candidate := vectored.Value.GetPartialMatch().GetMatchCandidate()
		provider, ok := candidate.(orderingKeyLayoutProvider)
		if !ok {
			return nil
		}
		rt := provider.orderingKeyLayout()
		if rt == nil {
			return nil
		}
		rtDomain := values.OrdinalDomainOfType(rt)
		if !rtDomain.IsKnown() {
			return nil
		}
		if layout == nil {
			layout, domain = rt, rtDomain
			continue
		}
		if rtDomain != domain {
			return nil
		}
	}
	return layout
}

// orderingKeyLayoutProvider is implemented by the match candidates that can name
// the ONE record row layout their ordering keys are domained in. Both
// implementations return nil rather than choosing when their index or scan
// serves more than one row layout.
type orderingKeyLayoutProvider interface {
	orderingKeyLayout() *values.RecordType
}

func commonPrimaryKeyValues(accesses []Vectored[*SingleMatchedAccess], ctx PlanContext) []values.Value {
	if len(accesses) == 0 {
		return nil
	}

	var commonTypes []string
	for _, v := range accesses {
		types := v.Value.GetPartialMatch().GetMatchCandidate().GetRecordTypes()
		if len(types) == 0 {
			return nil
		}
		if commonTypes == nil {
			commonTypes = types
		} else if !slices.Equal(commonTypes, types) {
			return nil
		}
	}

	if len(commonTypes) != 1 {
		return nil
	}

	pkCols := ctx.GetPrimaryKeyColumns(commonTypes[0])
	if len(pkCols) == 0 {
		return nil
	}

	result := make([]values.Value, len(pkCols))
	for i, col := range pkCols {
		result[i] = &values.FieldValue{
			Field: strings.ToUpper(col),
			Typ:   values.UnknownType,
		}
	}
	return result
}

// bakedIntersectionKeys resolves the name-only pk comparison keys against
// the legs' flowed row layout. EVERY leg must flow the same RecordType
// (the commonPrimaryKeyValues gate already pins one record type; the
// layout check is leg-order-agnostic so a single layout-less leg —
// whatever its slot — declines the candidate). The ordinal row model has
// no runtime name-resolution fallback: an unbaked FieldValue fails LOUD
// at merge time (OrdinalResolutionError), so a comparison key that
// cannot bake is a plan-time DECLINE of the intersection candidate,
// never a runtime error. Returns nil when any key stays unbaked.
func bakedIntersectionKeys(pkValues []values.Value, legs []plans.RecordQueryPlan) []values.Value {
	var rowType *values.RecordType
	for _, leg := range legs {
		rt, isRT := leg.GetResultType().(*values.RecordType)
		if !isRT {
			return nil
		}
		if rowType == nil {
			rowType = rt
			continue
		}
		if !rowType.Equals(rt) {
			return nil
		}
	}
	baked := bakeMergeComparisonKeys(pkValues, nil, rowType)
	for _, k := range baked {
		fv, isFV := k.(*values.FieldValue)
		if !isFV || fv.Child != nil || fv.Resolved == nil ||
			len(fv.Resolved.Accessors) != 1 {
			return nil
		}
		ordinal, unique := uniqueUpperFieldIndex(rowType, fv.Field)
		if !unique || fv.Resolved.Root().Ordinal != ordinal {
			return nil
		}
	}
	return baked
}

func createScanForAccess(access *SingleMatchedAccess) plans.RecordQueryPlan {
	pm := access.GetPartialMatch()
	candidate := pm.GetMatchCandidate()
	matchInfo := pm.GetMatchInfo()
	regularInfo := matchInfo.GetRegularMatchInfo()
	bindings := regularInfo.GetParameterBindingMap()
	prefix := candidate.ComputeBoundParameterPrefixMap(bindings)
	return candidate.ToScanPlan(prefix, access.IsReverseScanOrder())
}
