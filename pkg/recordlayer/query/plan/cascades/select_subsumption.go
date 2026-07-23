package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// enumerateSelectSubsumptionMappings streams only Select mappings that pass
// the node-local semantic gates. It deliberately performs no child-match
// expansion and emits no PartialMatch; the eventual rule consumer owns those
// operations.
//
// The mapping handed to visit is a stable copy. A visitor may retain it after
// returning without observing later enumerator mutation.
func enumerateSelectSubsumptionMappings(
	querySelect, candidateSelect *expressions.SelectExpression,
	budget *matchIntermediateSearchBudget,
	visit func([]quantifierMapping, *AliasMap) bool,
) {
	if querySelect == nil || candidateSelect == nil ||
		budget == nil || visit == nil {
		return
	}
	enumerateQuantifierMappings(
		querySelect.GetQuantifiers(),
		candidateSelect.GetQuantifiers(),
		false,
		budget,
		func(mapping []quantifierMapping) bool {
			aliasMap, ok := validateSelectSubsumptionMapping(
				querySelect,
				candidateSelect,
				mapping,
			)
			if !ok {
				return !budget.shouldStop()
			}
			stableMapping := append([]quantifierMapping(nil), mapping...)
			return visit(stableMapping, aliasMap) && !budget.shouldStop()
		},
	)
}

// validateSelectSubsumptionMapping validates the node-local structural gates
// for one dependency-sound Select-to-Select quantifier mapping. Predicate
// implication, child PartialMatch selection, result pull-up, and compensation
// are deliberately separate gates.
//
// The returned alias map contains exactly the selected quantifier bindings.
func validateSelectSubsumptionMapping(
	querySelect, candidateSelect *expressions.SelectExpression,
	mapping []quantifierMapping,
) (*AliasMap, bool) {
	if querySelect == nil || candidateSelect == nil ||
		!selectSubsumptionJoinTypeAllowed(querySelect.GetJoinType()) ||
		!selectSubsumptionJoinTypeAllowed(candidateSelect.GetJoinType()) {
		return nil, false
	}

	queryQuantifiers := querySelect.GetQuantifiers()
	candidateQuantifiers := candidateSelect.GetQuantifiers()
	if len(queryQuantifiers) == 0 || len(candidateQuantifiers) == 0 ||
		len(mapping) == 0 {
		return nil, false
	}
	queryPredicates, ok := selectSubsumptionFlattenConjunctsMaybe(
		querySelect.GetPredicates(),
	)
	if !ok {
		return nil, false
	}

	// A Select is a logical expression. Physical edges have no Select
	// subsumption semantics and must not survive merely by being omitted from
	// a partial mapping. Preflight malformed edges before dependency analysis,
	// which must inspect their ranged-over references.
	for _, quantifier := range queryQuantifiers {
		if quantifier.GetAlias().IsZero() ||
			quantifier.GetRangesOver() == nil ||
			quantifier.Kind() == expressions.QuantifierPhysical {
			return nil, false
		}
	}
	for _, quantifier := range candidateQuantifiers {
		if quantifier.GetAlias().IsZero() ||
			quantifier.GetRangesOver() == nil ||
			quantifier.Kind() == expressions.QuantifierPhysical {
			return nil, false
		}
	}

	// The dependency orders also reject duplicate aliases and cycles. Those
	// shapes cannot form an unambiguous local ownership graph.
	queryOrder := buildQuantifierDependencyOrder(queryQuantifiers)
	candidateOrder := buildQuantifierDependencyOrder(candidateQuantifiers)
	if !queryOrder.ok || !candidateOrder.ok {
		return nil, false
	}

	selectedQuery := make(map[int]struct{}, len(mapping))
	selectedCandidate := make(map[int]struct{}, len(mapping))
	aliasBuilder := NewAliasMapBuilder()
	for _, pair := range mapping {
		if pair.queryIndex < 0 || pair.queryIndex >= len(queryQuantifiers) ||
			pair.candidateIndex < 0 ||
			pair.candidateIndex >= len(candidateQuantifiers) {
			return nil, false
		}
		if _, duplicate := selectedQuery[pair.queryIndex]; duplicate {
			return nil, false
		}
		if _, duplicate := selectedCandidate[pair.candidateIndex]; duplicate {
			return nil, false
		}

		queryQuantifier := queryQuantifiers[pair.queryIndex]
		candidateQuantifier := candidateQuantifiers[pair.candidateIndex]
		if !selectSubsumptionQuantifierPairCompatible(
			queryQuantifier,
			candidateQuantifier,
		) || !aliasBuilder.PutChecked(
			queryQuantifier.GetAlias(),
			candidateQuantifier.GetAlias(),
		) {
			return nil, false
		}
		selectedQuery[pair.queryIndex] = struct{}{}
		selectedCandidate[pair.candidateIndex] = struct{}{}
	}

	if !selectSubsumptionCoversForEach(
		queryQuantifiers,
		candidateQuantifiers,
		selectedQuery,
		selectedCandidate,
	) || !selectSubsumptionMatchedExistentialsOwned(
		queryPredicates,
		queryQuantifiers,
		selectedQuery,
	) || !selectSubsumptionUnmatchedLocalCorrelationsValid(
		queryPredicates,
		queryQuantifiers,
		selectedQuery,
		queryOrder.transitive,
	) {
		return nil, false
	}

	return aliasBuilder.Build(), true
}

func selectSubsumptionJoinTypeAllowed(joinType expressions.JoinType) bool {
	return joinType == expressions.JoinInner || joinType == expressions.JoinCross
}

// selectSubsumptionQuantifierPairCompatible implements only the pair kinds
// whose cardinality is currently proved. Existential-to-ForEach is an
// intentional closed gate: it needs the RFC-190.4c cardinality proof/repair
// before it may be enabled.
func selectSubsumptionQuantifierPairCompatible(
	queryQuantifier, candidateQuantifier expressions.Quantifier,
) bool {
	switch queryQuantifier.Kind() {
	case expressions.QuantifierForEach:
		return candidateQuantifier.Kind() == expressions.QuantifierForEach &&
			queryQuantifier.IsNullOnEmpty() ==
				candidateQuantifier.IsNullOnEmpty() &&
			queryQuantifier.IsStrictSingle() ==
				candidateQuantifier.IsStrictSingle()
	case expressions.QuantifierExistential:
		switch candidateQuantifier.Kind() {
		case expressions.QuantifierExistential:
			return true
		case expressions.QuantifierForEach:
			// Disabled until the E-to-FE cardinality gate is proved.
			return false
		default:
			return false
		}
	default:
		return false
	}
}

func selectSubsumptionCoversForEach(
	queryQuantifiers, candidateQuantifiers []expressions.Quantifier,
	selectedQuery, selectedCandidate map[int]struct{},
) bool {
	for index, quantifier := range queryQuantifiers {
		if quantifier.Kind() != expressions.QuantifierForEach {
			continue
		}
		if _, selected := selectedQuery[index]; !selected {
			return false
		}
	}
	for index, quantifier := range candidateQuantifiers {
		if quantifier.Kind() != expressions.QuantifierForEach {
			continue
		}
		if _, selected := selectedCandidate[index]; !selected {
			return false
		}
	}
	return true
}

// selectSubsumptionChildMatchesCanBeDeferred enforces Java Select's child
// compensation gate. Every selected query ForEach edge must carry a concrete
// child match whose compensation can safely move above this Select.
// Existential children do not contribute rows and do not need this gate.
func selectSubsumptionChildMatchesCanBeDeferred(
	children []quantifierPartialMatch,
) bool {
	for _, child := range children {
		if child.quantifier.Kind() != expressions.QuantifierForEach {
			continue
		}
		childImpl, ok := child.partialMatch.(*PartialMatchImpl)
		if !ok || childImpl == nil ||
			!childImpl.CompensationCanBeDeferred() {
			return false
		}
	}
	return true
}

func selectSubsumptionMatchedExistentialsOwned(
	queryPredicates []predicates.QueryPredicate,
	queryQuantifiers []expressions.Quantifier,
	selectedQuery map[int]struct{},
) bool {
	for index, quantifier := range queryQuantifiers {
		if quantifier.Kind() != expressions.QuantifierExistential {
			continue
		}
		if _, selected := selectedQuery[index]; !selected {
			continue
		}
		if !selectSubsumptionExistentialOwned(
			queryPredicates,
			quantifier.GetAlias(),
		) {
			return false
		}
	}
	return true
}

// selectSubsumptionExistentialOwned requires the exact predicate node Java
// treats as owning a matched existential. Similar-looking comparison
// predicates and wrapped existential predicates are not ownership evidence.
// The full correlation set must be exactly the owned alias.
func selectSubsumptionExistentialOwned(
	queryPredicates []predicates.QueryPredicate,
	alias values.CorrelationIdentifier,
) bool {
	for _, queryPredicate := range queryPredicates {
		existentialPredicate, ok := queryPredicate.(*predicates.ExistentialValuePredicate)
		if !ok || existentialPredicate == nil {
			continue
		}
		qov, ok := existentialPredicate.Value.(*values.QuantifiedObjectValue)
		if !ok || qov == nil || qov.Correlation != alias ||
			existentialPredicate.Comparison.Type !=
				predicates.ComparisonIsNotNull {
			continue
		}
		correlatedTo := predicates.GetCorrelatedToOfPredicate(existentialPredicate)
		if len(correlatedTo) != 1 {
			continue
		}
		if _, owns := correlatedTo[alias]; owns {
			return true
		}
	}
	return false
}

// selectSubsumptionUnmatchedLocalCorrelationsValid ports Select's asymmetric
// unmatched-query gate. A predicate may mention an unselected local alias only
// when that alias belongs to an Existential quantifier and every local alias
// in its full transitive dependency closure is selected.
func selectSubsumptionUnmatchedLocalCorrelationsValid(
	queryPredicates []predicates.QueryPredicate,
	queryQuantifiers []expressions.Quantifier,
	selectedQuery map[int]struct{},
	transitiveDependencies []map[int]struct{},
) bool {
	if len(transitiveDependencies) != len(queryQuantifiers) {
		return false
	}

	localAliasToIndex := make(map[values.CorrelationIdentifier]int, len(queryQuantifiers))
	for index, quantifier := range queryQuantifiers {
		alias := quantifier.GetAlias()
		if _, duplicate := localAliasToIndex[alias]; duplicate {
			return false
		}
		localAliasToIndex[alias] = index
	}

	for _, queryPredicate := range queryPredicates {
		for alias := range predicates.GetCorrelatedToOfPredicate(queryPredicate) {
			index, local := localAliasToIndex[alias]
			if !local {
				continue
			}
			if _, selected := selectedQuery[index]; selected {
				continue
			}
			if queryQuantifiers[index].Kind() !=
				expressions.QuantifierExistential {
				return false
			}
			for dependency := range transitiveDependencies[index] {
				if _, selected := selectedQuery[dependency]; !selected {
					return false
				}
			}
		}
	}
	return true
}

// selectSubsumptionFlattenConjunctsMaybe flattens only AND. OR/NOT remain
// ownership boundaries, so an existential nested under either is not mistaken
// for a direct conjunct. Malformed typed-nil predicate trees fail closed.
func selectSubsumptionFlattenConjunctsMaybe(
	queryPredicates []predicates.QueryPredicate,
) ([]predicates.QueryPredicate, bool) {
	result := make([]predicates.QueryPredicate, 0, len(queryPredicates))
	var flatten func(predicates.QueryPredicate) bool
	flatten = func(queryPredicate predicates.QueryPredicate) bool {
		if selectSubsumptionPredicateIsNil(queryPredicate) ||
			!selectSubsumptionPredicateTreeWellFormed(queryPredicate) {
			return false
		}
		if andPredicate, ok := queryPredicate.(*predicates.AndPredicate); ok {
			for _, child := range andPredicate.SubPredicates {
				if !flatten(child) {
					return false
				}
			}
			return true
		}
		result = append(result, queryPredicate)
		return true
	}
	for _, queryPredicate := range queryPredicates {
		if !flatten(queryPredicate) {
			return nil, false
		}
	}
	return result, true
}

func selectSubsumptionPredicateTreeWellFormed(
	queryPredicate predicates.QueryPredicate,
) bool {
	if selectSubsumptionPredicateIsNil(queryPredicate) {
		return false
	}
	if existentialPredicate, ok := queryPredicate.(*predicates.ExistentialValuePredicate); ok {
		qov, isQOV := existentialPredicate.Value.(*values.QuantifiedObjectValue)
		if !isQOV || qov == nil ||
			existentialPredicate.Comparison.Type !=
				predicates.ComparisonIsNotNull {
			return false
		}
	}
	for _, child := range queryPredicate.Children() {
		if !selectSubsumptionPredicateTreeWellFormed(child) {
			return false
		}
	}
	return true
}

func selectSubsumptionPredicateIsNil(
	queryPredicate predicates.QueryPredicate,
) bool {
	if queryPredicate == nil {
		return true
	}
	value := reflect.ValueOf(queryPredicate)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
