package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// partialMatchesSemanticallyEqual compares the parts of a PartialMatch that
// can change downstream matching, compensation, and data-access choices.
//
// Query-expression and candidate-reference identity anchor the comparison,
// while mappings and MatchInfo are compared by value. This is deliberately
// stronger than pointer identity (rules reconstruct equivalent match objects
// on every refire) and weaker than the old endpoint-only key (one pair can
// legitimately own several alias maps, child selections, or bindings).
//
// The comparison itself is the collision-safe half of the semantic
// fingerprint: callers may bucket by any stable hash in the future, but a hash
// must never be allowed to collapse two alternatives without this equality
// check.
func partialMatchesSemanticallyEqual(a, b *PartialMatchImpl) bool {
	return partialMatchesSemanticallyEqualSeen(a, b, make(map[partialMatchIdentityPair]bool))
}

type partialMatchIdentityPair struct {
	a *PartialMatchImpl
	b *PartialMatchImpl
}

func partialMatchesSemanticallyEqualSeen(
	a, b *PartialMatchImpl,
	seen map[partialMatchIdentityPair]bool,
) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a == b {
		return true
	}
	if a.GetMatchCandidate() != b.GetMatchCandidate() ||
		a.GetQueryRef().Canonical() != b.GetQueryRef().Canonical() ||
		a.GetQueryExpression() != b.GetQueryExpression() ||
		a.GetCandidateRef().Canonical() != b.GetCandidateRef().Canonical() ||
		!aliasMapsEqual(a.GetBoundAliasMap(), b.GetBoundAliasMap()) {
		return false
	}

	pair := partialMatchIdentityPair{a: a, b: b}
	if seen[pair] {
		return true
	}
	seen[pair] = true
	return matchInfosSemanticallyEqual(a.GetMatchInfo(), b.GetMatchInfo(), seen)
}

func matchInfosSemanticallyEqual(
	a, b MatchInfo,
	seen map[partialMatchIdentityPair]bool,
) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch typedA := a.(type) {
	case *RegularMatchInfo:
		typedB, ok := b.(*RegularMatchInfo)
		return ok && regularMatchInfosSemanticallyEqual(typedA, typedB, seen)
	case *AdjustedMatchInfo:
		typedB, ok := b.(*AdjustedMatchInfo)
		if !ok ||
			!matchedOrderingPartsEqual(
				typedA.matchedOrderingParts,
				typedB.matchedOrderingParts,
			) ||
			!maxMatchMapsEqual(typedA.maxMatchMap, typedB.maxMatchMap) ||
			!groupByMappingsEqual(
				typedA.groupByMappings,
				typedB.groupByMappings,
			) {
			return false
		}
		// Every adjusted layer is semantically relevant: NestPullUp walks the
		// full chain and consumes each intermediate MaxMatchMap.
		return matchInfosSemanticallyEqual(
			typedA.underlying,
			typedB.underlying,
			seen,
		)
	default:
		// Unknown implementations may hide additional state. Fail open by
		// retaining the alternative.
		return false
	}
}

func regularMatchInfosSemanticallyEqual(
	a, b *RegularMatchInfo,
	seen map[partialMatchIdentityPair]bool,
) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !comparisonRangeMapsEqual(a.parameterBindingMap, b.parameterBindingMap) ||
		!aliasMapsEqual(a.bindingAliasMap, b.bindingAliasMap) ||
		!predicateMultiMapsEqual(a.predicateMap, b.predicateMap) ||
		!matchedOrderingPartsEqual(a.matchedOrderingParts, b.matchedOrderingParts) ||
		!maxMatchMapsEqual(a.maxMatchMap, b.maxMatchMap) ||
		!groupByMappingsEqual(a.groupByMappings, b.groupByMappings) ||
		!valueSlicesEqual(a.rollUpToGroupingValues, b.rollUpToGroupingValues) ||
		!queryPlanConstraintsEqual(a.additionalPlanConstraint, b.additionalPlanConstraint) ||
		a.requiresPrimaryKeyDistinct != b.requiresPrimaryKeyDistinct ||
		len(a.childPartialMatchMap) != len(b.childPartialMatchMap) {
		return false
	}

	for alias, childA := range a.childPartialMatchMap {
		childB, ok := b.childPartialMatchMap[alias]
		if !ok {
			return false
		}
		if childA == childB {
			continue
		}
		childImplA, okA := childA.(*PartialMatchImpl)
		childImplB, okB := childB.(*PartialMatchImpl)
		if !okA || !okB ||
			!partialMatchesSemanticallyEqualSeen(childImplA, childImplB, seen) {
			return false
		}
	}
	return true
}

func aliasMapsEqual(a, b *AliasMap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Size() != b.Size() {
		return false
	}
	for _, source := range a.Sources() {
		target, ok := b.GetTargetOrEmpty(source)
		if !ok || target != a.GetTarget(source) {
			return false
		}
	}
	return true
}

func comparisonRangeMapsEqual(
	a, b map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) bool {
	if len(a) != len(b) {
		return false
	}
	for alias, rangeA := range a {
		rangeB, ok := b[alias]
		if !ok || !partialMatchComparisonRangesEqual(rangeA, rangeB) {
			return false
		}
	}
	return true
}

func partialMatchComparisonRangesEqual(a, b *predicates.ComparisonRange) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.GetRangeType() != b.GetRangeType() {
		return false
	}
	switch a.GetRangeType() {
	case predicates.ComparisonRangeEmpty:
		return true
	case predicates.ComparisonRangeEquality:
		return comparisonsEqual(a.GetEqualityComparison(), b.GetEqualityComparison())
	case predicates.ComparisonRangeInequality:
		aComparisons := a.GetInequalityComparisons()
		bComparisons := b.GetInequalityComparisons()
		if len(aComparisons) != len(bComparisons) {
			return false
		}
		used := make([]bool, len(bComparisons))
		for _, comparisonA := range aComparisons {
			found := false
			for i, comparisonB := range bComparisons {
				if used[i] || !comparisonsEqual(comparisonA, comparisonB) {
					continue
				}
				used[i] = true
				found = true
				break
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func comparisonsEqual(a, b *predicates.Comparison) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Reuse the canonical ComparisonPredicate equality surface so every
	// comparison discriminator (including vector-search knobs) participates.
	left := &predicates.ComparisonPredicate{Comparison: *a}
	right := &predicates.ComparisonPredicate{Comparison: *b}
	return predicates.SemanticEqualsUnderAliasMap(left, right, nil)
}

func predicateMultiMapsEqual(a, b *PredicateMultiMap) bool {
	if a == nil || b == nil {
		return a == b
	}
	aEntries := a.Entries()
	bEntries := b.Entries()
	if len(aEntries) != len(bEntries) {
		return false
	}
	used := make([]bool, len(bEntries))
	for _, entryA := range aEntries {
		found := false
		for i, entryB := range bEntries {
			if used[i] ||
				!partialMatchPredicatesEqual(entryA.Predicate, entryB.Predicate) ||
				!predicateMappingsEqual(entryA.Mapping, entryB.Mapping) {
				continue
			}
			used[i] = true
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func predicateMappingsEqual(a, b *PredicateMapping) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a == b {
		return true
	}
	if a.GetMappingKind() != b.GetMappingKind() ||
		!partialMatchPredicatesEqual(
			a.GetOriginalQueryPredicate(),
			b.GetOriginalQueryPredicate(),
		) ||
		!partialMatchPredicatesEqual(
			a.GetCandidatePredicate(),
			b.GetCandidatePredicate(),
		) ||
		!partialMatchPredicatesEqual(
			a.GetTranslatedQueryPredicate(),
			b.GetTranslatedQueryPredicate(),
		) ||
		!optionalAliasesEqual(a.GetParameterAlias(), b.GetParameterAlias()) ||
		!partialMatchComparisonRangesEqual(a.GetComparisonRange(), b.GetComparisonRange()) ||
		!queryPlanConstraintsEqual(a.GetConstraint(), b.GetConstraint()) {
		return false
	}

	// Function values carry opaque closure state. Only package-owned,
	// explicitly tagged factories are safe to compare across allocations.
	// Unknown compensation remains distinct: a safe duplicate is preferable
	// to dropping a semantically different alternative.
	return a.predicateCompensationIdentity != "" &&
		a.predicateCompensationIdentity == b.predicateCompensationIdentity
}

func partialMatchPredicatesEqual(
	a, b predicates.QueryPredicate,
) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a == b {
		return true
	}
	placeholderA, okA := a.(*predicates.Placeholder)
	placeholderB, okB := b.(*predicates.Placeholder)
	if okA || okB {
		return okA && okB &&
			placeholderA.GetParameterAlias() == placeholderB.GetParameterAlias() &&
			values.SemanticEqualsUnderAliasMap(
				placeholderA.GetValue(),
				placeholderB.GetValue(),
				nil,
			) &&
			partialMatchComparisonRangesEqual(
				placeholderA.GetComparisonRange(),
				placeholderB.GetComparisonRange(),
			)
	}
	return predicates.SemanticEqualsUnderAliasMap(a, b, nil)
}

func optionalAliasesEqual(
	a, b *values.CorrelationIdentifier,
) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func matchedOrderingPartsEqual(a, b []*MatchedOrderingPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if a[i] != b[i] {
				return false
			}
			continue
		}
		// parameterId is provenance only in the Go port. No production
		// consumer reads it, and synthesized primary-key suffix parts receive
		// a fresh identifier on every adjustment. The effective ordering is
		// fully described by value, range, and sort direction.
		if a[i].GetMatchedSortOrder() != b[i].GetMatchedSortOrder() ||
			!values.SemanticEqualsUnderAliasMap(a[i].GetValue(), b[i].GetValue(), nil) ||
			!partialMatchComparisonRangesEqual(a[i].GetComparisonRange(), b[i].GetComparisonRange()) {
			return false
		}
	}
	return true
}

func maxMatchMapsEqual(a, b *MaxMatchMap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !values.SemanticEqualsUnderAliasMap(a.queryValue, b.queryValue, nil) ||
		!values.SemanticEqualsUnderAliasMap(a.candidateValue, b.candidateValue, nil) ||
		len(a.rangedOverAliases) != len(b.rangedOverAliases) ||
		len(a.mapping) != len(b.mapping) {
		return false
	}
	for alias := range a.rangedOverAliases {
		if _, ok := b.rangedOverAliases[alias]; !ok {
			return false
		}
	}
	entriesB := make([]maxMatchEntry, 0, len(b.mapping))
	for _, entry := range b.mapping {
		entriesB = append(entriesB, entry)
	}
	used := make([]bool, len(entriesB))
	for _, entryA := range a.mapping {
		found := false
		for i, entryB := range entriesB {
			if used[i] ||
				!values.SemanticEqualsUnderAliasMap(
					entryA.queryValue,
					entryB.queryValue,
					nil,
				) ||
				!values.SemanticEqualsUnderAliasMap(
					entryA.candidateValue,
					entryB.candidateValue,
					nil,
				) {
				continue
			}
			used[i] = true
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func groupByMappingsEqual(a, b *GroupByMappings) bool {
	if a == nil || b == nil {
		return a == b
	}
	return valueBiMapsEqual(a.MatchedGroupingsMap(), b.MatchedGroupingsMap()) &&
		valueBiMapsEqual(a.MatchedAggregatesMap(), b.MatchedAggregatesMap()) &&
		correlationValueBiMapsEqual(a.UnmatchedAggregatesMap(), b.UnmatchedAggregatesMap())
}

func valueBiMapsEqual(a, b *BiMap[values.Value, values.Value]) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Len() != b.Len() {
		return false
	}
	type entry struct {
		query     values.Value
		candidate values.Value
	}
	entriesB := make([]entry, 0, b.Len())
	b.Range(func(queryValue, candidateValue values.Value) bool {
		entriesB = append(entriesB, entry{queryValue, candidateValue})
		return true
	})
	used := make([]bool, len(entriesB))
	equal := true
	a.Range(func(queryValue, candidateValue values.Value) bool {
		for i, other := range entriesB {
			if used[i] ||
				!values.SemanticEqualsUnderAliasMap(
					queryValue,
					other.query,
					nil,
				) ||
				!values.SemanticEqualsUnderAliasMap(
					candidateValue,
					other.candidate,
					nil,
				) {
				continue
			}
			used[i] = true
			return true
		}
		equal = false
		return false
	})
	return equal
}

func correlationValueBiMapsEqual(
	a, b *BiMap[values.CorrelationIdentifier, values.Value],
) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Len() != b.Len() {
		return false
	}
	entriesB := make(map[values.CorrelationIdentifier][]values.Value, b.Len())
	b.Range(func(alias values.CorrelationIdentifier, value values.Value) bool {
		entriesB[alias] = append(entriesB[alias], value)
		return true
	})
	equal := true
	a.Range(func(alias values.CorrelationIdentifier, value values.Value) bool {
		candidates := entriesB[alias]
		for i, other := range candidates {
			if values.SemanticEqualsUnderAliasMap(value, other, nil) {
				entriesB[alias] = append(candidates[:i], candidates[i+1:]...)
				return true
			}
		}
		equal = false
		return false
	})
	return equal
}

func valueSlicesEqual(a, b []values.Value) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !values.SemanticEqualsUnderAliasMap(a[i], b[i], nil) {
			return false
		}
	}
	return true
}

func queryPlanConstraintsEqual(a, b *QueryPlanConstraint) bool {
	if a == b {
		return true
	}
	return partialMatchPredicatesEqual(
		constraintPredicate(a),
		constraintPredicate(b),
	)
}

func constraintPredicate(c *QueryPlanConstraint) predicates.QueryPredicate {
	if c == nil || c.GetPredicate() == nil {
		return predicates.NewConstantPredicate(predicates.TriTrue)
	}
	return c.GetPredicate()
}
