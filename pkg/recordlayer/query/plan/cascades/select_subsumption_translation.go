package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// selectSubsumptionTranslationEntry is one checked entry in the composed
// query-to-candidate translation map. Keeping entries outside the values
// TranslationMapBuilder lets malformed duplicate mappings fail closed before
// the builder's deliberately panicking duplicate-source guard is reachable.
type selectSubsumptionTranslationEntry struct {
	source    values.CorrelationIdentifier
	translate values.TranslationFunction
}

// buildSelectSubsumptionTranslationMapMaybe composes the quantifier
// translations for one concrete Select child-PartialMatch product.
//
// A mapped ForEach quantifier is pulled through the selected child's current
// MatchInfo MaxMatchMap. A mapped existential uses a direct alias rebase, but
// Java deliberately omits that entry when the candidate quantifier is itself
// existential: existential aliases do not flow a value through the candidate
// Select. The dormant existential-to-ForEach case is implemented here; its
// cardinality gate remains closed by validateSelectSubsumptionMapping.
func buildSelectSubsumptionTranslationMapMaybe(
	queryQuantifiers, candidateQuantifiers []expressions.Quantifier,
	mapping []quantifierMapping,
	children []quantifierPartialMatch,
	bindingAliasMap *AliasMap,
) (values.TranslationMap, bool) {
	if bindingAliasMap == nil ||
		len(mapping) == 0 ||
		len(children) != len(mapping) {
		return nil, false
	}

	entries := make([]selectSubsumptionTranslationEntry, 0, len(mapping))
	selectedQueryIndexes := make(map[int]struct{}, len(mapping))
	selectedCandidateIndexes := make(map[int]struct{}, len(mapping))
	selectedQueryAliases := make(
		map[values.CorrelationIdentifier]struct{},
		len(mapping),
	)
	selectedCandidateAliases := make(
		map[values.CorrelationIdentifier]struct{},
		len(mapping),
	)

	for mappingIndex, pair := range mapping {
		if pair.queryIndex < 0 ||
			pair.queryIndex >= len(queryQuantifiers) ||
			pair.candidateIndex < 0 ||
			pair.candidateIndex >= len(candidateQuantifiers) {
			return nil, false
		}
		if _, duplicate := selectedQueryIndexes[pair.queryIndex]; duplicate {
			return nil, false
		}
		if _, duplicate := selectedCandidateIndexes[pair.candidateIndex]; duplicate {
			return nil, false
		}

		queryQuantifier := queryQuantifiers[pair.queryIndex]
		candidateQuantifier := candidateQuantifiers[pair.candidateIndex]
		queryAlias := queryQuantifier.GetAlias()
		candidateAlias := candidateQuantifier.GetAlias()
		if queryAlias.IsZero() || candidateAlias.IsZero() {
			return nil, false
		}
		if _, duplicate := selectedQueryAliases[queryAlias]; duplicate {
			return nil, false
		}
		if _, duplicate := selectedCandidateAliases[candidateAlias]; duplicate {
			return nil, false
		}
		boundCandidateAlias, bound := bindingAliasMap.GetTargetOrEmpty(queryAlias)
		if !bound || boundCandidateAlias != candidateAlias {
			return nil, false
		}

		child := children[mappingIndex]
		if child.quantifier.GetAlias() != queryAlias ||
			child.quantifier.Kind() != queryQuantifier.Kind() ||
			child.quantifier.IsNullOnEmpty() !=
				queryQuantifier.IsNullOnEmpty() ||
			child.quantifier.IsStrictSingle() !=
				queryQuantifier.IsStrictSingle() {
			return nil, false
		}
		queryRangesOver := queryQuantifier.GetRangesOver()
		candidateRangesOver := candidateQuantifier.GetRangesOver()
		childRangesOver := child.quantifier.GetRangesOver()
		if queryRangesOver == nil ||
			candidateRangesOver == nil ||
			childRangesOver == nil {
			return nil, false
		}
		queryCanonical := queryRangesOver.Canonical()
		candidateCanonical := candidateRangesOver.Canonical()
		childCanonical := childRangesOver.Canonical()
		if queryCanonical == nil ||
			candidateCanonical == nil ||
			childCanonical == nil ||
			childCanonical != queryCanonical {
			return nil, false
		}
		childPartialMatch, ok := child.partialMatch.(*PartialMatchImpl)
		if !ok || childPartialMatch == nil {
			return nil, false
		}
		childMatchInfo := childPartialMatch.GetMatchInfo()
		if childMatchInfo == nil ||
			selectSubsumptionIsTypedNil(childMatchInfo) ||
			childPartialMatch.GetQueryRef() == nil ||
			childPartialMatch.GetCandidateRef() == nil ||
			childPartialMatch.GetQueryRef().Canonical() != queryCanonical ||
			childPartialMatch.GetCandidateRef().Canonical() !=
				candidateCanonical {
			return nil, false
		}

		var translation values.TranslationFunction
		switch queryQuantifier.Kind() {
		case expressions.QuantifierForEach:
			if candidateQuantifier.Kind() != expressions.QuantifierForEach {
				return nil, false
			}
			childMaxMatchMap := childMatchInfo.GetMaxMatchMap()
			if childMaxMatchMap == nil ||
				!selectSubsumptionValueTreeWellFormed(
					childMaxMatchMap.GetQueryValue(),
				) ||
				!selectSubsumptionValueTreeWellFormed(
					childMaxMatchMap.GetCandidateValue(),
				) {
				return nil, false
			}
			for _, entry := range childMaxMatchMap.mapping {
				if !selectSubsumptionValueTreeWellFormed(
					entry.queryValue,
				) ||
					!selectSubsumptionValueTreeWellFormed(
						entry.candidateValue,
					) {
					return nil, false
				}
			}
			pulledUp := childMaxMatchMap.TranslateQueryValueMaybe(candidateAlias)
			if !selectSubsumptionValueTreeWellFormed(pulledUp) {
				return nil, false
			}
			translation = func(
				_ values.CorrelationIdentifier,
				_ values.Value,
			) values.Value {
				return pulledUp
			}

		case expressions.QuantifierExistential:
			if candidateQuantifier.Kind() !=
				expressions.QuantifierExistential &&
				candidateQuantifier.Kind() !=
					expressions.QuantifierForEach {
				return nil, false
			}
			aliasRebase := values.AliasMap{queryAlias: candidateAlias}
			translation = func(
				_ values.CorrelationIdentifier,
				leaf values.Value,
			) values.Value {
				return values.RebaseValue(leaf, aliasRebase)
			}

		default:
			return nil, false
		}

		selectedQueryIndexes[pair.queryIndex] = struct{}{}
		selectedCandidateIndexes[pair.candidateIndex] = struct{}{}
		selectedQueryAliases[queryAlias] = struct{}{}
		selectedCandidateAliases[candidateAlias] = struct{}{}

		// RelationalExpression.pullUpAndComposeTranslationMapsMaybe omits a
		// translation whose target quantifier is existential.
		if candidateQuantifier.Kind() ==
			expressions.QuantifierExistential {
			continue
		}
		entries = append(entries, selectSubsumptionTranslationEntry{
			source:    queryAlias,
			translate: translation,
		})
	}

	builder := values.NewTranslationMapBuilder()
	composedSources := make(
		map[values.CorrelationIdentifier]struct{},
		len(entries),
	)
	for _, entry := range entries {
		if entry.translate == nil {
			return nil, false
		}
		if _, duplicate := composedSources[entry.source]; duplicate {
			return nil, false
		}
		composedSources[entry.source] = struct{}{}
		builder.When(entry.source).Then(entry.translate)
	}
	return builder.Build(), true
}

// translateSelectSubsumptionInputs translates the full top-level query
// predicate slice and result value exactly once for one concrete child product.
// Predicate implication can subsequently enumerate alternatives without
// repeating these correlation rewrites. The caller retains the original
// predicate slice in parallel for PredicateMapping identity.
//
// Translation is structurally checked before rebuilding any predicate. In
// particular, a local ExistentialValuePredicate must name a local existential
// quantifier, whose only supported translations preserve its
// QuantifiedObjectValue shape. This rejects a malformed EVP over a mapped
// ForEach alias without pre-applying the map, so the shared predicate spine is
// still translated exactly once.
func translateSelectSubsumptionInputs(
	querySelect *expressions.SelectExpression,
	translation values.TranslationMap,
) ([]predicates.QueryPredicate, values.Value, bool) {
	if querySelect == nil {
		return nil, nil, false
	}
	if translation != nil && selectSubsumptionIsTypedNil(translation) {
		return nil, nil, false
	}
	queryResult := querySelect.GetResultValue()
	if !selectSubsumptionValueTreeWellFormed(queryResult) {
		return nil, nil, false
	}

	localQuantifierKinds := make(
		map[values.CorrelationIdentifier]expressions.QuantifierKind,
		len(querySelect.GetQuantifiers()),
	)
	for _, queryQuantifier := range querySelect.GetQuantifiers() {
		queryAlias := queryQuantifier.GetAlias()
		if queryAlias.IsZero() {
			return nil, nil, false
		}
		if _, duplicate := localQuantifierKinds[queryAlias]; duplicate {
			return nil, nil, false
		}
		localQuantifierKinds[queryAlias] = queryQuantifier.Kind()
	}

	queryPredicates := querySelect.GetPredicates()
	for _, queryPredicate := range queryPredicates {
		if !selectSubsumptionPredicateTranslationSafe(
			queryPredicate,
			translation,
			localQuantifierKinds,
		) {
			return nil, nil, false
		}
	}

	translatedResult := values.TranslateCorrelations(
		queryResult,
		translation,
	)
	if !selectSubsumptionValueTreeWellFormed(translatedResult) {
		return nil, nil, false
	}
	translatedPredicates := make(
		[]predicates.QueryPredicate,
		len(queryPredicates),
	)
	for predicateIndex, queryPredicate := range queryPredicates {
		translatedPredicate := predicates.TranslateLeafPredicates(
			queryPredicate,
			translation,
		)
		if !selectSubsumptionPredicateEmbeddedValuesWellFormed(
			translatedPredicate,
		) {
			return nil, nil, false
		}
		translatedPredicates[predicateIndex] = translatedPredicate
	}
	return translatedPredicates, translatedResult, true
}

func selectSubsumptionPredicateTranslationSafe(
	queryPredicate predicates.QueryPredicate,
	translation values.TranslationMap,
	localQuantifierKinds map[values.CorrelationIdentifier]expressions.QuantifierKind,
) bool {
	if queryPredicate == nil ||
		selectSubsumptionIsTypedNil(queryPredicate) {
		return false
	}

	requiredValueSafe := func(value values.Value) bool {
		return selectSubsumptionValueTreeWellFormed(value)
	}
	comparisonSafe := func(comparison predicates.Comparison) bool {
		if comparison.Operand != nil &&
			!selectSubsumptionValueTreeWellFormed(comparison.Operand) {
			return false
		}
		if !comparison.Type.IsUnary() &&
			comparison.Operand == nil &&
			comparison.ParameterName == "" {
			return false
		}
		if comparison.QueryVector != nil &&
			!selectSubsumptionValueTreeWellFormed(comparison.QueryVector) {
			return false
		}
		switch comparison.Type {
		case predicates.ComparisonDistanceRankEquals,
			predicates.ComparisonDistanceRankLessThan,
			predicates.ComparisonDistanceRankLessThanOrEq:
			return comparison.Operand != nil &&
				comparison.QueryVector != nil
		default:
			return true
		}
	}

	switch queryPredicate := queryPredicate.(type) {
	case *predicates.ComparisonPredicate:
		if !requiredValueSafe(queryPredicate.Operand) ||
			!comparisonSafe(queryPredicate.Comparison) {
			return false
		}
	case *predicates.ValuePredicate:
		if !requiredValueSafe(queryPredicate.Value) {
			return false
		}
	case *predicates.Placeholder:
		if !requiredValueSafe(queryPredicate.Value) ||
			queryPredicate.CompRange == nil {
			return false
		}
		for _, comparison := range queryPredicate.CompRange.GetComparisons() {
			if comparison == nil || !comparisonSafe(*comparison) {
				return false
			}
		}
	case *predicates.ExistentialValuePredicate:
		if !requiredValueSafe(queryPredicate.Value) ||
			!comparisonSafe(queryPredicate.Comparison) {
			return false
		}
		existentialQOV, isQOV := queryPredicate.Value.(*values.QuantifiedObjectValue)
		if !isQOV || existentialQOV == nil ||
			existentialQOV.Correlation.IsZero() {
			return false
		}
		if localKind, local := localQuantifierKinds[existentialQOV.Correlation]; local {
			if localKind != expressions.QuantifierExistential {
				return false
			}
		} else if translation != nil &&
			translation.ContainsSourceAlias(
				existentialQOV.Correlation,
			) {
			return false
		}
	case *predicates.PredicateWithValueAndRanges:
		if !requiredValueSafe(queryPredicate.GetValue()) {
			return false
		}
		for _, rangeConstraint := range queryPredicate.GetRanges() {
			if rangeConstraint == nil {
				return false
			}
			for _, comparison := range rangeConstraint.GetComparisons() {
				if !comparisonSafe(comparison) {
					return false
				}
			}
		}
	}

	for _, child := range queryPredicate.Children() {
		if !selectSubsumptionPredicateTranslationSafe(
			child,
			translation,
			localQuantifierKinds,
		) {
			return false
		}
	}
	return true
}

// selectSubsumptionValueTreeWellFormed validates every Value node before a
// translation, correlation walk, or semantic comparison can dispatch through
// it. An interface containing a typed-nil child is non-nil at the parent and
// otherwise reaches pointer-receiver methods that dereference that child.
//
// Production Value implementations are pointer-backed. The visited set also
// turns a malformed cyclic test/custom value into a bounded safe rejection.
func selectSubsumptionValueTreeWellFormed(root values.Value) bool {
	if root == nil || selectSubsumptionIsTypedNil(root) {
		return false
	}

	type valueVisit struct {
		value values.Value
		exit  bool
	}
	stack := []valueVisit{{value: root}}
	state := make(map[uintptr]uint8)
	for len(stack) > 0 {
		visit := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		current := visit.value
		if current == nil || selectSubsumptionIsTypedNil(current) {
			return false
		}

		reflected := reflect.ValueOf(current)
		if reflected.Kind() != reflect.Pointer {
			return false
		}
		identity := reflected.Pointer()
		if visit.exit {
			state[identity] = 2
			continue
		}
		switch state[identity] {
		case 1:
			return false
		case 2:
			continue
		default:
			state[identity] = 1
			stack = append(stack, valueVisit{
				value: current,
				exit:  true,
			})
		}
		children := current.Children()
		for childIndex := len(children) - 1; childIndex >= 0; childIndex-- {
			child := children[childIndex]
			if child == nil || selectSubsumptionIsTypedNil(child) {
				return false
			}
			stack = append(stack, valueVisit{value: child})
		}
	}
	return true
}

// selectSubsumptionPredicateEmbeddedValuesWellFormed checks every Value root
// owned by a predicate spine. Predicate children are handled recursively; the
// Value helper above handles each complete embedded Value tree.
func selectSubsumptionPredicateEmbeddedValuesWellFormed(
	queryPredicate predicates.QueryPredicate,
) bool {
	if selectSubsumptionPredicateIsNil(queryPredicate) {
		return false
	}

	comparisonValuesWellFormed := func(comparison predicates.Comparison) bool {
		return (comparison.Operand == nil ||
			selectSubsumptionValueTreeWellFormed(comparison.Operand)) &&
			(comparison.QueryVector == nil ||
				selectSubsumptionValueTreeWellFormed(comparison.QueryVector))
	}

	switch typedPredicate := queryPredicate.(type) {
	case *predicates.ComparisonPredicate:
		if !selectSubsumptionValueTreeWellFormed(typedPredicate.Operand) ||
			!comparisonValuesWellFormed(typedPredicate.Comparison) {
			return false
		}
	case *predicates.ValuePredicate:
		if !selectSubsumptionValueTreeWellFormed(typedPredicate.Value) {
			return false
		}
	case *predicates.ExistentialValuePredicate:
		if !selectSubsumptionValueTreeWellFormed(typedPredicate.Value) ||
			!comparisonValuesWellFormed(typedPredicate.Comparison) {
			return false
		}
	case *predicates.Placeholder:
		if !selectSubsumptionValueTreeWellFormed(typedPredicate.GetValue()) {
			return false
		}
		comparisonRange := typedPredicate.GetComparisonRange()
		if comparisonRange == nil {
			return false
		}
		for _, comparison := range comparisonRange.GetComparisons() {
			if comparison == nil ||
				!comparisonValuesWellFormed(*comparison) {
				return false
			}
		}
	case *predicates.PredicateWithValueAndRanges:
		if !selectSubsumptionValueTreeWellFormed(typedPredicate.GetValue()) {
			return false
		}
		for _, rangeConstraint := range typedPredicate.GetRanges() {
			if rangeConstraint == nil {
				return false
			}
			for _, comparison := range rangeConstraint.GetComparisons() {
				if !comparisonValuesWellFormed(comparison) {
					return false
				}
			}
		}
	}

	for _, child := range queryPredicate.Children() {
		if !selectSubsumptionPredicateEmbeddedValuesWellFormed(child) {
			return false
		}
	}
	return true
}

func selectSubsumptionIsTypedNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// buildSelectSubsumptionMaxMatchMap computes result-value coverage after the
// composed child translations have already moved the query result into
// candidate scope. Every candidate quantifier alias is ranged over, including
// unmatched candidate existentials, while the complete binding map supplies
// equivalence for deeply correlated aliases that the translation map does not
// rewrite.
func buildSelectSubsumptionMaxMatchMap(
	translatedQueryResult values.Value,
	candidateSelect *expressions.SelectExpression,
	bindingAliasMap *AliasMap,
) *MaxMatchMap {
	if candidateSelect == nil {
		return nil
	}
	candidateResult := candidateSelect.GetResultValue()
	if !selectSubsumptionValueTreeWellFormed(translatedQueryResult) ||
		!selectSubsumptionValueTreeWellFormed(candidateResult) {
		return nil
	}
	return ComputeMaxMatchMapWithEquivalence(
		translatedQueryResult,
		candidateResult,
		quantifierAliases(candidateSelect.GetQuantifiers()),
		NewAliasMapValueEquivalence(bindingAliasMap),
	)
}
