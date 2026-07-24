package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// selectSubsumptionPredicateAlternativeBuilder finalizes one predicate-mapping
// cross-product after the shared intermediate-match budget has admitted it.
// Keeping finalization lazy is important: fresh residual predicates, checked
// parameter merges, and PredicateMultiMap construction must not run for an
// alternative the caller cannot visit.
type selectSubsumptionPredicateAlternativeBuilder func() (
	map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	*PredicateMultiMap,
	bool,
)

// selectSubsumptionPredicateCandidateGroup holds every query-predicate mapping
// that can cover one candidate predicate. Candidate identity, rather than
// semantic equality, defines a group.
type selectSubsumptionPredicateCandidateGroup struct {
	candidate predicates.QueryPredicate
	mappings  []*PredicateMapping
}

// enumerateSelectSubsumptionPredicateAlternatives streams the predicate
// alternatives for one already-translated Select child product.
//
// Like Java SelectExpression.subsumedBy, the product is grouped by candidate
// predicate identity: each filtering candidate must choose exactly one query
// predicate that implies it, while one query predicate may cover several
// distinct candidates. Go additionally completes every product with a fresh
// TRUE residual mapping for each original query predicate that the selected
// candidate mappings did not use. PartialMatch compensation ignores a query
// predicate with no mapping, so this completion is required to preserve every
// query-side filter.
//
// Candidate groups and their mappings retain first-discovery order (query
// order, then candidate order). visit receives a stable lazy builder and may
// retain it after this function returns. false from visit stops enumeration
// immediately and is propagated to the caller.
func enumerateSelectSubsumptionPredicateAlternatives(
	originalQueryPredicates []predicates.QueryPredicate,
	translatedQueryPredicates []predicates.QueryPredicate,
	candidateSelect *expressions.SelectExpression,
	bindingAliasMap *AliasMap,
	visit func(selectSubsumptionPredicateAlternativeBuilder) bool,
) bool {
	if candidateSelect == nil ||
		bindingAliasMap == nil ||
		visit == nil ||
		len(originalQueryPredicates) != len(translatedQueryPredicates) {
		return true
	}

	originalQueryPredicates = append(
		[]predicates.QueryPredicate(nil),
		originalQueryPredicates...,
	)
	translatedQueryPredicates = append(
		[]predicates.QueryPredicate(nil),
		translatedQueryPredicates...,
	)
	// Select construction can retain a top-level AndPredicate even though
	// predicate implication is conjunct-based. Flatten after construction so
	// candidate Placeholder leaves participate directly, while preserving
	// their pointer identities for candidate-group coverage.
	candidatePredicates, candidatePredicatesOK := selectSubsumptionFlattenConjunctsMaybe(
		append(
			[]predicates.QueryPredicate(nil),
			candidateSelect.GetPredicates()...,
		),
	)
	if !candidatePredicatesOK {
		return true
	}
	candidateQuantifiers := append(
		[]expressions.Quantifier(nil),
		candidateSelect.GetQuantifiers()...,
	)

	for predicateIndex := range originalQueryPredicates {
		if !selectSubsumptionImplicationPredicateWellFormed(
			originalQueryPredicates[predicateIndex],
		) || !selectSubsumptionImplicationPredicateWellFormed(
			translatedQueryPredicates[predicateIndex],
		) {
			return true
		}
	}
	for _, candidatePredicate := range candidatePredicates {
		if !selectSubsumptionImplicationPredicateWellFormed(
			candidatePredicate,
		) {
			return true
		}
	}

	groups := make([]selectSubsumptionPredicateCandidateGroup, 0)
	groupIndexes := make(map[string]int, len(candidatePredicates))
	for queryIndex, translatedQueryPredicate := range translatedQueryPredicates {
		seenCandidates := make(map[string]struct{}, len(candidatePredicates))
		for _, candidatePredicate := range candidatePredicates {
			candidateKey := predicateKey(candidatePredicate)
			if _, duplicate := seenCandidates[candidateKey]; duplicate {
				continue
			}
			seenCandidates[candidateKey] = struct{}{}

			mapping, implied := selectSubsumptionPredicateImpliedMappingMaybe(
				originalQueryPredicates[queryIndex],
				translatedQueryPredicate,
				candidatePredicate,
				candidateQuantifiers,
				bindingAliasMap,
			)
			if !implied {
				continue
			}

			groupIndex, grouped := groupIndexes[candidateKey]
			if !grouped {
				groupIndex = len(groups)
				groupIndexes[candidateKey] = groupIndex
				groups = append(
					groups,
					selectSubsumptionPredicateCandidateGroup{
						candidate: candidatePredicate,
					},
				)
			}
			groups[groupIndex].mappings = append(
				groups[groupIndex].mappings,
				mapping,
			)
		}
	}

	selectedMappings := make([]*PredicateMapping, len(groups))
	var enumerate func(int) bool
	enumerate = func(depth int) bool {
		if depth == len(groups) {
			mappingSnapshot := append(
				[]*PredicateMapping(nil),
				selectedMappings...,
			)
			return visit(func() (
				map[values.CorrelationIdentifier]*predicates.ComparisonRange,
				*PredicateMultiMap,
				bool,
			) {
				return buildSelectSubsumptionPredicateAlternative(
					originalQueryPredicates,
					translatedQueryPredicates,
					candidatePredicates,
					mappingSnapshot,
				)
			})
		}

		for _, mapping := range groups[depth].mappings {
			selectedMappings[depth] = mapping
			if !enumerate(depth + 1) {
				return false
			}
		}
		selectedMappings[depth] = nil
		return true
	}
	return enumerate(0)
}

// buildSelectSubsumptionPredicateAlternative completes and validates one
// candidate-group product. Every original query predicate occurs in the
// resulting map: selected predicates retain their actual candidate mapping;
// absent predicates receive distinct synthetic TRUE candidates and residual
// compensation.
func buildSelectSubsumptionPredicateAlternative(
	originalQueryPredicates []predicates.QueryPredicate,
	translatedQueryPredicates []predicates.QueryPredicate,
	candidatePredicates []predicates.QueryPredicate,
	selectedMappings []*PredicateMapping,
) (
	map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	*PredicateMultiMap,
	bool,
) {
	if len(originalQueryPredicates) != len(translatedQueryPredicates) {
		return nil, nil, false
	}

	predicateMapBuilder := NewPredicateMultiMapBuilder()
	parameterBindingMap := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	mappedQueryPredicates := make(map[string]struct{}, len(originalQueryPredicates))
	coveredCandidatePredicates := make(map[string]struct{}, len(selectedMappings))

	for _, mapping := range selectedMappings {
		if mapping == nil ||
			selectSubsumptionPredicateIsNil(
				mapping.GetOriginalQueryPredicate(),
			) ||
			selectSubsumptionPredicateIsNil(
				mapping.GetTranslatedQueryPredicate(),
			) ||
			selectSubsumptionPredicateIsNil(
				mapping.GetCandidatePredicate(),
			) {
			return nil, nil, false
		}

		originalQueryPredicate := mapping.GetOriginalQueryPredicate()
		predicateMapBuilder.Put(originalQueryPredicate, mapping)
		mappedQueryPredicates[predicateKey(originalQueryPredicate)] = struct{}{}
		candidateKey := predicateKey(mapping.GetCandidatePredicate())
		coveredCandidatePredicates[candidateKey] = struct{}{}

		parameterAlias := mapping.GetParameterAlias()
		comparisonRange := mapping.GetComparisonRange()
		if (parameterAlias == nil) != (comparisonRange == nil) {
			return nil, nil, false
		}
		if parameterAlias == nil {
			continue
		}
		if parameterAlias.IsZero() || comparisonRange == nil {
			return nil, nil, false
		}

		var merged bool
		parameterBindingMap, merged = tryMergeParameterBindings(
			parameterBindingMap,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				*parameterAlias: comparisonRange,
			},
		)
		if !merged {
			return nil, nil, false
		}
	}

	seenOriginalPredicates := make(
		map[string]struct{},
		len(originalQueryPredicates),
	)
	for queryIndex, originalQueryPredicate := range originalQueryPredicates {
		queryKey := predicateKey(originalQueryPredicate)
		if _, duplicate := seenOriginalPredicates[queryKey]; duplicate {
			continue
		}
		seenOriginalPredicates[queryKey] = struct{}{}
		if _, mapped := mappedQueryPredicates[queryKey]; mapped {
			continue
		}

		// A separate object per residual is load-bearing. PredicateMultiMap
		// conflict checks use candidate identity, so sharing one TRUE object
		// across two query predicates would reject an otherwise valid product.
		freshTautology := predicates.NewConstantPredicate(predicates.TriTrue)
		residualMappingBuilder := RegularMappingBuilder(
			originalQueryPredicate,
			translatedQueryPredicates[queryIndex],
			freshTautology,
		)
		residualMapping := selectSubsumptionMappingBuilderWithCompensation(
			residualMappingBuilder,
			originalQueryPredicate,
			reapplyResidualCompensation(originalQueryPredicate),
			"residual",
		).Build()
		predicateMapBuilder.Put(
			originalQueryPredicate,
			residualMapping,
		)
		mappedQueryPredicates[queryKey] = struct{}{}
	}

	for _, candidatePredicate := range candidatePredicates {
		candidateKey := predicateKey(candidatePredicate)
		if _, covered := coveredCandidatePredicates[candidateKey]; covered {
			continue
		}
		if !selectSubsumptionCandidatePredicateIsNonFiltering(
			candidatePredicate,
		) {
			return nil, nil, false
		}
	}

	predicateMap := predicateMapBuilder.BuildMaybe()
	if predicateMap == nil {
		return nil, nil, false
	}
	return parameterBindingMap, predicateMap, true
}

// selectSubsumptionPredicateImpliedMappingMaybe implements the bounded
// predicate shapes currently supported by Go's matcher:
//   - ComparisonPredicate to an unconstraining Placeholder;
//   - any query predicate to a non-placeholder TRUE predicate, with residual;
//   - semantic equality for all other non-placeholder predicates supported by
//     predicates.SemanticEqualsUnderAliasMap.
func selectSubsumptionPredicateImpliedMappingMaybe(
	originalQueryPredicate predicates.QueryPredicate,
	translatedQueryPredicate predicates.QueryPredicate,
	candidatePredicate predicates.QueryPredicate,
	candidateQuantifiers []expressions.Quantifier,
	bindingAliasMap *AliasMap,
) (*PredicateMapping, bool) {
	if bindingAliasMap == nil ||
		!selectSubsumptionImplicationPredicateWellFormed(
			originalQueryPredicate,
		) ||
		!selectSubsumptionImplicationPredicateWellFormed(
			translatedQueryPredicate,
		) ||
		!selectSubsumptionImplicationPredicateWellFormed(
			candidatePredicate,
		) {
		return nil, false
	}

	if placeholder, isPlaceholder := candidatePredicate.(*predicates.Placeholder); isPlaceholder {
		if placeholder == nil ||
			placeholder.GetComparisonRange() == nil ||
			placeholder.IsConstraining() ||
			placeholder.GetParameterAlias().IsZero() {
			return nil, false
		}
		comparisonPredicate, isComparison := translatedQueryPredicate.(*predicates.ComparisonPredicate)
		if !isComparison || comparisonPredicate == nil {
			return nil, false
		}
		comparisonRange, bound := bindSelectSubsumptionComparisonToPlaceholder(
			comparisonPredicate,
			placeholder,
			candidateQuantifiers,
		)
		if !bound {
			return nil, false
		}

		parameterAlias := placeholder.GetParameterAlias()
		return RegularMappingBuilder(
			originalQueryPredicate,
			translatedQueryPredicate,
			candidatePredicate,
		).SetSargable(
			parameterAlias,
			comparisonRange,
		).setKnownPredicateCompensation(
			selectSubsumptionSargablePredicateCompensation(
				originalQueryPredicate,
				parameterAlias,
			),
			"select-sargable-prefix",
		).Build(), true
	}

	if selectSubsumptionCandidatePredicateIsNonFiltering(
		candidatePredicate,
	) {
		mappingBuilder := RegularMappingBuilder(
			originalQueryPredicate,
			translatedQueryPredicate,
			candidatePredicate,
		)
		return selectSubsumptionMappingBuilderWithCompensation(
			mappingBuilder,
			originalQueryPredicate,
			reapplyResidualCompensation(originalQueryPredicate),
			"residual",
		).Build(), true
	}

	if !predicates.SemanticEqualsUnderAliasMap(
		translatedQueryPredicate,
		candidatePredicate,
		bindingAliasMap.ForwardMap(),
	) {
		return nil, false
	}
	mappingBuilder := RegularMappingBuilder(
		originalQueryPredicate,
		translatedQueryPredicate,
		candidatePredicate,
	)
	return selectSubsumptionMappingBuilderWithCompensation(
		mappingBuilder,
		originalQueryPredicate,
		DefaultPredicateCompensation(),
		"default",
	).Build(), true
}

// selectSubsumptionMappingBuilderWithCompensation installs Java's
// child-aware EVP compensation factory whenever the original query predicate
// owns an existential. All other predicate kinds retain their branch-specific
// compensation.
func selectSubsumptionMappingBuilderWithCompensation(
	mappingBuilder *PredicateMappingBuilder,
	originalQueryPredicate predicates.QueryPredicate,
	fallback PredicateCompensation,
	fallbackIdentity string,
) *PredicateMappingBuilder {
	existentialPredicate, isExistential := originalQueryPredicate.(*predicates.ExistentialValuePredicate)
	if isExistential && existentialPredicate != nil {
		return mappingBuilder.setKnownPredicateCompensation(
			selectSubsumptionExistentialPredicateCompensation(
				existentialPredicate,
			),
			"select-existential-child",
		)
	}
	return mappingBuilder.setKnownPredicateCompensation(
		fallback,
		fallbackIdentity,
	)
}

type existentialCompensatingPartialMatch interface {
	CompensateExistential(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	) Compensation
}

// selectSubsumptionExistentialPredicateCompensation ports
// ExistentialValuePredicate.computeCompensationFunction. The matched owning
// existential's child decides whether EXISTS itself still needs to be
// evaluated: a missing/invalid child, impossible child compensation, or any
// child filtering requirement reapplies the original EVP. A possible child
// with no filtering requirement makes the EVP redundant, even when that child
// still needs result-only compensation.
func selectSubsumptionExistentialPredicateCompensation(
	originalQueryPredicate *predicates.ExistentialValuePredicate,
) PredicateCompensation {
	return func(
		partialMatch PartialMatch,
		boundParameterPrefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
		_ *PullUp,
	) PredicateCompensationFunc {
		reapply := func() PredicateCompensationFunc {
			return OfExistentialValuePredicateCompensation(
				originalQueryPredicate,
			)
		}
		if originalQueryPredicate == nil ||
			partialMatch == nil ||
			selectSubsumptionIsTypedNil(partialMatch) {
			return reapply()
		}

		existentialAlias := originalQueryPredicate.GetExistentialAlias()
		if existentialAlias.IsZero() {
			return reapply()
		}
		queryExpression := partialMatch.GetQueryExpression()
		if queryExpression == nil ||
			selectSubsumptionIsTypedNil(queryExpression) {
			return reapply()
		}

		ownerFound := false
		for _, queryQuantifier := range queryExpression.GetQuantifiers() {
			if queryQuantifier.GetAlias() != existentialAlias {
				continue
			}
			if ownerFound ||
				queryQuantifier.Kind() != expressions.QuantifierExistential {
				return reapply()
			}
			ownerFound = true
		}
		if !ownerFound {
			// The EVP is owned by an outer Select. This match level neither
			// consumes nor compensates it.
			return NoPredicateCompensationNeeded()
		}

		regularMatchInfo := partialMatch.GetRegularMatchInfo()
		if regularMatchInfo == nil {
			return reapply()
		}
		childPartialMatch := regularMatchInfo.GetChildPartialMatchMaybe(
			existentialAlias,
		)
		if childPartialMatch == nil ||
			selectSubsumptionIsTypedNil(childPartialMatch) {
			return reapply()
		}
		compensatingChild, ok := childPartialMatch.(existentialCompensatingPartialMatch)
		if !ok || selectSubsumptionIsTypedNil(compensatingChild) {
			return reapply()
		}
		childCompensation := compensatingChild.CompensateExistential(
			boundParameterPrefixMap,
		)
		if childCompensation == nil ||
			selectSubsumptionIsTypedNil(childCompensation) ||
			childCompensation.IsImpossible() ||
			childCompensation.IsNeededForFiltering() {
			return reapply()
		}
		return NoPredicateCompensationNeeded()
	}
}

func selectSubsumptionSargablePredicateCompensation(
	originalQueryPredicate predicates.QueryPredicate,
	parameterAlias values.CorrelationIdentifier,
) PredicateCompensation {
	return func(
		_ PartialMatch,
		boundParameterPrefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
		_ *PullUp,
	) PredicateCompensationFunc {
		if _, bound := boundParameterPrefixMap[parameterAlias]; bound {
			return NoPredicateCompensationNeeded()
		}
		return OfPredicateCompensation(originalQueryPredicate, true)
	}
}

// selectSubsumptionCandidatePredicateIsNonFiltering is deliberately local to
// Select subsumption. Go's general IsTautology helper only recognizes a
// constant TRUE, while Java Select also treats an unconstraining Placeholder
// as removable during candidate coverage.
func selectSubsumptionCandidatePredicateIsNonFiltering(
	candidatePredicate predicates.QueryPredicate,
) bool {
	if predicates.IsTautology(candidatePredicate) {
		return true
	}
	placeholder, isPlaceholder := candidatePredicate.(*predicates.Placeholder)
	return isPlaceholder &&
		placeholder != nil &&
		placeholder.GetComparisonRange() != nil &&
		!placeholder.IsConstraining()
}

func bindSelectSubsumptionComparisonToPlaceholder(
	comparisonPredicate *predicates.ComparisonPredicate,
	placeholder *predicates.Placeholder,
	candidateQuantifiers []expressions.Quantifier,
) (*predicates.ComparisonRange, bool) {
	if comparisonPredicate == nil || placeholder == nil ||
		selectSubsumptionIsTypedNil(comparisonPredicate.Operand) ||
		placeholder.GetValue() == nil ||
		selectSubsumptionIsTypedNil(placeholder.GetValue()) {
		return nil, false
	}

	candidateKinds := make(
		map[values.CorrelationIdentifier]expressions.QuantifierKind,
		len(candidateQuantifiers),
	)
	for _, candidateQuantifier := range candidateQuantifiers {
		candidateAlias := candidateQuantifier.GetAlias()
		if candidateAlias.IsZero() {
			return nil, false
		}
		if _, duplicate := candidateKinds[candidateAlias]; duplicate {
			return nil, false
		}
		candidateKinds[candidateAlias] = candidateQuantifier.Kind()
	}

	sourceAlias, hasSource := selectSubsumptionSingleLocalValueSource(
		placeholder.GetValue(),
		candidateKinds,
	)
	if !hasSource ||
		candidateKinds[sourceAlias] != expressions.QuantifierForEach {
		return nil, false
	}

	for _, orientation := range comparisonOrientations(
		comparisonPredicate,
	) {
		if !isSargableComparisonForMatch(
			orientation.comparison.Type,
		) {
			continue
		}
		columnSource, hasColumnSource := selectSubsumptionSingleLocalValueSource(
			orientation.column,
			candidateKinds,
		)
		if !hasColumnSource || columnSource != sourceAlias {
			continue
		}
		if !comparandIndependentOfSource(
			orientation.comparison.Operand,
			sourceAlias,
		) || !valuesMatchColumn(
			orientation.column,
			placeholder.GetValue(),
		) {
			continue
		}
		if fieldValue, isFieldValue := orientation.column.(*values.FieldValue); isFieldValue &&
			!comparisonTypesCompatible(
				fieldValue,
				&orientation.comparison,
			) {
			continue
		}

		comparison := orientation.comparison
		mergeResult := predicates.EmptyComparisonRange().Merge(&comparison)
		if mergeResult.Ok {
			return mergeResult.Range, true
		}
	}
	return nil, false
}

// selectSubsumptionSingleLocalValueSource returns the value's sole correlation
// when it names a candidate-owned alias. A placeholder column and a comparison
// LHS must be wholly owned by one candidate leg; a value that also reads an
// outer scope is not a single-leg column even if its accessor name happens to
// match.
func selectSubsumptionSingleLocalValueSource(
	value values.Value,
	candidateKinds map[values.CorrelationIdentifier]expressions.QuantifierKind,
) (values.CorrelationIdentifier, bool) {
	if value == nil || selectSubsumptionIsTypedNil(value) {
		var zero values.CorrelationIdentifier
		return zero, false
	}
	correlatedTo := values.GetCorrelatedToOfValue(value)
	if len(correlatedTo) != 1 {
		var zero values.CorrelationIdentifier
		return zero, false
	}
	for source := range correlatedTo {
		if _, local := candidateKinds[source]; local {
			return source, true
		}
	}
	var zero values.CorrelationIdentifier
	return zero, false
}

func selectSubsumptionImplicationPredicateWellFormed(
	queryPredicate predicates.QueryPredicate,
) bool {
	if selectSubsumptionPredicateIsNil(queryPredicate) ||
		!selectSubsumptionPredicateTreeWellFormed(queryPredicate) {
		return false
	}

	comparisonWellFormed := func(comparison predicates.Comparison) bool {
		if comparison.Operand != nil &&
			!selectSubsumptionValueTreeWellFormed(comparison.Operand) {
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
			return comparison.Type.IsUnary() ||
				comparison.Operand != nil ||
				comparison.ParameterName != ""
		}
	}
	valueWellFormed := func(value values.Value) bool {
		return selectSubsumptionValueTreeWellFormed(value)
	}

	switch typedPredicate := queryPredicate.(type) {
	case *predicates.ComparisonPredicate:
		if !valueWellFormed(typedPredicate.Operand) ||
			!comparisonWellFormed(typedPredicate.Comparison) {
			return false
		}
	case *predicates.ValuePredicate:
		if !valueWellFormed(typedPredicate.Value) {
			return false
		}
	case *predicates.ExistentialValuePredicate:
		if !valueWellFormed(typedPredicate.Value) ||
			!comparisonWellFormed(typedPredicate.Comparison) {
			return false
		}
	case *predicates.Placeholder:
		if !valueWellFormed(typedPredicate.GetValue()) ||
			typedPredicate.GetComparisonRange() == nil {
			return false
		}
		for _, comparison := range typedPredicate.GetComparisonRange().GetComparisons() {
			if comparison == nil || !comparisonWellFormed(*comparison) {
				return false
			}
		}
	case *predicates.PredicateWithValueAndRanges:
		if !valueWellFormed(typedPredicate.GetValue()) {
			return false
		}
		for _, rangeConstraint := range typedPredicate.GetRanges() {
			if rangeConstraint == nil {
				return false
			}
			for _, comparison := range rangeConstraint.GetComparisons() {
				if !comparisonWellFormed(comparison) {
					return false
				}
			}
		}
	}

	for _, child := range queryPredicate.Children() {
		if !selectSubsumptionImplicationPredicateWellFormed(child) {
			return false
		}
	}
	return true
}
