package cascades

import (
	"fmt"

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
// semantic equality, defines a group. Each alternative is the list of
// mappings the product selects for the candidate together: one mapping, or
// the members of a simple placeholder comparison fold
// (see selectSubsumptionGroupAlternatives).
type selectSubsumptionPredicateCandidateGroup struct {
	candidate    predicates.QueryPredicate
	alternatives [][]*PredicateMapping
}

// enumerateSelectSubsumptionPredicateAlternatives streams the predicate
// alternatives for one already-translated Select child product.
//
// Like Java SelectExpression.subsumedBy, the product is grouped by candidate
// predicate identity: each filtering candidate must choose exactly one query
// predicate that implies it, while one query predicate may cover several
// distinct candidates. The Select constructor groups same-value comparisons
// into PredicateWithValueAndRanges, as Java does. For callers still supplying
// individual ComparisonPredicates, a placeholder's simple comparison mappings
// fold into ONE alternative (foldPlaceholderBindings), the members sharing
// the merged range. Range predicates retain their own mappings, including
// residual-only mappings with no scan range. Go completes every product with a
// fresh TRUE residual mapping for each original query predicate that the
// selected candidate mappings did not use — the fold's non-members among
// them. PartialMatch compensation ignores a query predicate with no mapping,
// so this completion is required to preserve every query-side filter.
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
	// The candidate's list is its conjunction (the Select constructor lifts
	// every top-level AND), so each Placeholder leaf participates directly
	// with its pointer identity, which candidate-group coverage keys on.
	candidatePredicates, candidatePredicatesOK := selectSubsumptionPredicatesWellFormedMaybe(
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
	groupMappings := make([][]*PredicateMapping, 0)
	groupIndexes := make(map[string]int, len(candidatePredicates))
	for queryIndex, translatedQueryPredicate := range translatedQueryPredicates {
		var impliedMappings []*PredicateMapping
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
			// Java findImpliedMappings deduplicates semantic MappingKeys for
			// this original predicate before grouping by candidate identity.
			duplicate := false
			for _, previous := range impliedMappings {
				if previous.GetMappingKind() == mapping.GetMappingKind() &&
					predicates.SemanticEqualsUnderAliasMap(previous.GetCandidatePredicate(), mapping.GetCandidatePredicate(), nil) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			impliedMappings = append(impliedMappings, mapping)

			candidateKey = predicateKey(mapping.GetCandidatePredicate())
			groupIndex, grouped := groupIndexes[candidateKey]
			if !grouped {
				groupIndex = len(groups)
				groupIndexes[candidateKey] = groupIndex
				groups = append(
					groups,
					selectSubsumptionPredicateCandidateGroup{
						candidate: mapping.GetCandidatePredicate(),
					},
				)
				groupMappings = append(groupMappings, nil)
			}
			groupMappings[groupIndex] = append(groupMappings[groupIndex], mapping)
		}
	}
	for groupIndex := range groups {
		groups[groupIndex].alternatives = selectSubsumptionGroupAlternatives(
			groups[groupIndex].candidate,
			groupMappings[groupIndex],
		)
	}

	selectedMappings := make([][]*PredicateMapping, len(groups))
	var enumerate func(int) bool
	enumerate = func(depth int) bool {
		if depth == len(groups) {
			var mappingSnapshot []*PredicateMapping
			for _, selected := range selectedMappings {
				mappingSnapshot = append(mappingSnapshot, selected...)
			}
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

		for _, alternative := range groups[depth].alternatives {
			selectedMappings[depth] = alternative
			if !enumerate(depth + 1) {
				return false
			}
		}
		selectedMappings[depth] = nil
		return true
	}
	return enumerate(0)
}

// selectSubsumptionGroupAlternatives turns one candidate's mappings into the
// alternatives the product enumerates. A non-placeholder candidate offers
// each mapping on its own, as Java's cross product does. A placeholder group
// consisting solely of single-comparison bindings folds into ONE alternative
// holding the members of the fold, each rebuilt over the merged range.
// Range predicates and residual-only mappings use the cross product unchanged;
// rebuilding them as simple bindings would lose their compensation. The
// non-members carry no mapping and are completed as residuals by
// buildSelectSubsumptionPredicateAlternative.
func selectSubsumptionGroupAlternatives(
	candidate predicates.QueryPredicate,
	mappings []*PredicateMapping,
) [][]*PredicateMapping {
	oneEach := func() [][]*PredicateMapping {
		alternatives := make([][]*PredicateMapping, 0, len(mappings))
		for _, mapping := range mappings {
			alternatives = append(alternatives, []*PredicateMapping{mapping})
		}
		return alternatives
	}
	placeholder, isPlaceholder := candidate.(*predicates.Placeholder)
	if !isPlaceholder || placeholder == nil || len(mappings) < 2 {
		return oneEach()
	}

	bound := make([]placeholderBinding, 0, len(mappings))
	for _, mapping := range mappings {
		cp, isComparison := mapping.GetTranslatedQueryPredicate().(*predicates.ComparisonPredicate)
		comparisonRange := mapping.GetComparisonRange()
		if !isComparison || cp == nil || comparisonRange == nil ||
			len(comparisonRange.GetComparisons()) != 1 {
			// A normalized range predicate can have its own partial residual
			// or no scan range at all. Keep its mapping and compensation intact.
			return oneEach()
		}
		bound = append(bound, placeholderBinding{
			pred:       mapping.GetOriginalQueryPredicate(),
			cp:         cp,
			comparison: comparisonRange.GetComparisons()[0],
		})
	}
	merged, members := foldPlaceholderBindings(bound)
	if len(members) == 0 {
		return oneEach()
	}
	alternative := make([]*PredicateMapping, 0, len(members))
	for _, member := range members {
		alternative = append(alternative, selectSubsumptionSargableMapping(
			member.pred,
			member.cp,
			placeholder,
			merged,
		))
	}
	return [][]*PredicateMapping{alternative}
}

// selectSubsumptionSargableMapping builds the mapping of one query
// comparison onto a candidate placeholder over the given range: the
// compensation re-applies the predicate unless the placeholder's alias is in
// the scan prefix.
func selectSubsumptionSargableMapping(
	originalQueryPredicate predicates.QueryPredicate,
	translatedQueryPredicate predicates.QueryPredicate,
	placeholder *predicates.Placeholder,
	comparisonRange *predicates.ComparisonRange,
) *PredicateMapping {
	parameterAlias := placeholder.GetParameterAlias()
	return RegularMappingBuilder(
		originalQueryPredicate,
		translatedQueryPredicate,
		placeholder,
	).SetSargable(
		parameterAlias,
		comparisonRange,
	).setKnownPredicateCompensation(
		selectSubsumptionSargablePredicateCompensation(
			originalQueryPredicate,
			parameterAlias,
		),
		"select-sargable-prefix",
	).Build()
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
		if parameterAlias == nil {
			if comparisonRange != nil {
				return nil, nil, false
			}
			continue
		}
		if parameterAlias.IsZero() {
			return nil, nil, false
		}
		if comparisonRange == nil {
			// Java retains the placeholder alias for a multi-range predicate
			// even though its entire predicate must be compensated.
			placeholder, ok := mapping.GetCandidatePredicate().(*predicates.Placeholder)
			if !ok || !selectSubsumptionCandidatePredicateIsNonFiltering(placeholder) ||
				placeholder.GetParameterAlias() != *parameterAlias {
				return nil, nil, false
			}
			continue
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
//   - ComparisonPredicate or PredicateWithValueAndRanges to an unconstraining
//     Placeholder, retaining any comparisons the scan cannot consume;
//   - OR leaf value to a Placeholder, as a residual-only exploration hint;
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
		if disjunction, isOr := translatedQueryPredicate.(*predicates.OrPredicate); isOr {
			aliases, err := bindingAliasMap.ForwardMap()
			if err != nil || !selectSubsumptionOrHasMatchingLeaf(disjunction, placeholder.GetValue(), aliases) {
				return nil, false
			}
			// Java's OR-term hint licenses exploration, never a scan bound.
			return OrTermMappingBuilder(originalQueryPredicate, translatedQueryPredicate,
				predicates.NewConstantPredicate(predicates.TriTrue)).
				setKnownPredicateCompensation(reapplyResidualCompensation(originalQueryPredicate), "residual").Build(), true
		}
		if ranges, ok := translatedQueryPredicate.(*predicates.PredicateWithValueAndRanges); ok {
			return selectSubsumptionRangeMapping(originalQueryPredicate, ranges, placeholder, candidateQuantifiers)
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

		return selectSubsumptionSargableMapping(
			originalQueryPredicate,
			translatedQueryPredicate,
			placeholder,
			comparisonRange,
		), true
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

	valuesAliasMap, err := bindingAliasMap.ForwardMap()
	if err != nil {
		return nil, false
	}
	// Literal bounds are immutable, so their containment needs no runtime
	// parameter constraint. Retain the query predicate as compensation when
	// the candidate's range is strictly wider (Java range implication).
	queryValue, queryRanges := selectSubsumptionValueRanges(translatedQueryPredicate)
	candidateValue, candidateRanges := selectSubsumptionValueRanges(candidatePredicate)
	if len(queryRanges) > 0 && len(candidateRanges) > 0 && values.SemanticEqualsUnderAliasMap(queryValue, candidateValue, valuesAliasMap) {
		if selectSubsumptionRangesEnclose(candidateRanges, queryRanges) {
			compensation := DefaultPredicateCompensation()
			identity := "default"
			if !selectSubsumptionRangesEnclose(queryRanges, candidateRanges) {
				compensation = reapplyResidualCompensation(originalQueryPredicate)
				identity = "residual"
			}
			return selectSubsumptionMappingBuilderWithCompensation(
				RegularMappingBuilder(originalQueryPredicate, translatedQueryPredicate, candidatePredicate),
				originalQueryPredicate, compensation, identity,
			).Build(), true
		}
	}
	if !predicates.SemanticEqualsUnderAliasMap(
		translatedQueryPredicate,
		candidatePredicate,
		valuesAliasMap,
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

func selectSubsumptionValueRanges(predicate predicates.QueryPredicate) (values.Value, []*predicates.RangeConstraints) {
	switch p := predicate.(type) {
	case *predicates.ComparisonPredicate:
		return p.Operand, []*predicates.RangeConstraints{predicates.NewRangeConstraints([]predicates.Comparison{p.Comparison}, nil)}
	case *predicates.PredicateWithValueAndRanges:
		return p.GetValue(), p.GetRanges()
	default:
		return nil, nil
	}
}

func selectSubsumptionRangesEnclose(outer, inner []*predicates.RangeConstraints) bool {
	for _, rangeToCover := range inner {
		covered := false
		for _, covering := range outer {
			if covering.Encloses(rangeToCover) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func selectSubsumptionRangeMapping(original predicates.QueryPredicate, translated *predicates.PredicateWithValueAndRanges, placeholder *predicates.Placeholder, quantifiers []expressions.Quantifier) (*PredicateMapping, bool) {
	if len(translated.GetRanges()) == 0 {
		return nil, false
	}
	builder := RegularMappingBuilder(original, translated, placeholder).
		SetParameterAlias(placeholder.GetParameterAlias())
	if len(translated.GetRanges()) > 1 {
		if _, ok := selectSubsumptionValueMatchesPlaceholder(translated.GetValue(), placeholder, quantifiers); !ok {
			return nil, false
		}
		return builder.setKnownPredicateCompensation(
			selectSubsumptionRangePredicateCompensation(translated, translated, placeholder.GetParameterAlias()),
			"select-range-prefix",
		).Build(), true
	}

	var comparisons []*predicates.Comparison
	var residuals []predicates.Comparison
	// MergeAll returns the very comparisons it was handed, including bounds
	// displaced by a later equality. Preserve their pre-orientation forms so
	// a residual of `5 > x` never becomes `5 < 5` after binding `x < 5`.
	unoriented := make(map[*predicates.Comparison]predicates.Comparison)
	for _, comparison := range translated.GetRanges()[0].GetComparisons() {
		bound, ok := bindSelectSubsumptionComparisonToPlaceholder(
			predicates.NewComparisonPredicate(translated.GetValue(), comparison), placeholder, quantifiers,
		)
		if !ok {
			residuals = append(residuals, comparison)
			continue
		}
		for _, boundComparison := range bound.GetComparisons() {
			comparisons = append(comparisons, boundComparison)
			unoriented[boundComparison] = comparison
		}
	}
	if len(comparisons) == 0 {
		return nil, false
	}
	merged := predicates.MergeAll(comparisons)
	for _, comparison := range merged.Residuals {
		residuals = append(residuals, unoriented[comparison])
	}
	var residual predicates.QueryPredicate
	if len(residuals) > 0 {
		residual = predicates.NewPredicateWithValueAndRanges(translated.GetValue(), []*predicates.RangeConstraints{
			predicates.NewRangeConstraints(residuals, nil),
		})
	}
	return builder.SetComparisonRange(merged.Range).setKnownPredicateCompensation(
		selectSubsumptionRangePredicateCompensation(translated, residual, placeholder.GetParameterAlias()),
		"select-range-prefix",
	).Build(), true
}

// selectSubsumptionRangePredicateCompensation follows Java's
// mapPredicateToPlaceholder and LeafQueryPredicate.computeCompensationFunctionForLeaf:
// a bound prefix leaves only the unconsumed comparisons; an unbound prefix
// leaves the whole query predicate. Both are already translated into candidate
// scope and must be pulled through the candidate before apply-time rebasing.
// Reversing the binding alias map cannot undo general Value translations.
func selectSubsumptionRangePredicateCompensation(
	translated predicates.QueryPredicate,
	residual predicates.QueryPredicate,
	parameterAlias values.CorrelationIdentifier,
) PredicateCompensation {
	return func(_ PartialMatch, prefix map[values.CorrelationIdentifier]*predicates.ComparisonRange, pullUp *PullUp) PredicateCompensationFunc {
		predicate := translated
		if _, bound := prefix[parameterAlias]; bound {
			predicate = residual
		}
		if predicate == nil {
			return NoPredicateCompensationNeeded()
		}
		if pullUp == nil {
			return ImpossiblePredicateCompensation()
		}
		predicate, err := predicates.ToResidualPredicate(predicate)
		if err != nil {
			return ImpossiblePredicateCompensation()
		}
		pulledUp, err := predicates.TransformEmbeddedValuesChecked(predicate, func(value values.Value) (values.Value, error) {
			// Literals and external correlations need no candidate output.
			// Go's MaxMatchMap refuses an empty mapping even for these values;
			// establish independence from the entire pull-up chain explicitly.
			correlatedTo := values.GetCorrelatedToOfValue(value)
			independent := true
			for level := pullUp; level != nil; level = level.GetParent() {
				for alias := range level.GetRangedOverAliases() {
					if _, correlated := correlatedTo[alias]; correlated {
						independent = false
					}
				}
			}
			if independent {
				return value, nil
			}
			result := pullUp.PullUpValueMaybe(value)
			if result == nil {
				return nil, fmt.Errorf("select range residual value cannot be pulled through the candidate")
			}
			return result, nil
		})
		if err != nil {
			return ImpossiblePredicateCompensation()
		}
		return OfPredicateCompensation(pulledUp, true)
	}
}

func selectSubsumptionOrHasMatchingLeaf(predicate predicates.QueryPredicate, candidate values.Value, aliases values.AliasMap) bool {
	if len(predicate.Children()) == 0 {
		var operand values.Value
		switch leaf := predicate.(type) {
		case *predicates.ComparisonPredicate:
			operand = leaf.Operand
		case *predicates.ValuePredicate:
			operand = leaf.Value
		case *predicates.PredicateWithValueAndRanges:
			operand = leaf.GetValue()
		}
		return operand != nil && values.SemanticEqualsUnderAliasMap(operand, candidate, aliases)
	}
	for _, child := range predicate.Children() {
		if selectSubsumptionOrHasMatchingLeaf(child, candidate, aliases) {
			return true
		}
	}
	return false
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
			// The EVP sits in this Select's predicates, so no other Select
			// applies it; its existential is an outer binding (PartitionSelectRule
			// moves a predicate correlated to an upper existential into the
			// lower Select). Java returns noCompensationNeeded here and drops the
			// filter; reapplied, it is an ordinary residual over the outer row.
			return reapply()
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
	if comparisonPredicate == nil || selectSubsumptionIsTypedNil(comparisonPredicate.Operand) {
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
		sourceAlias, matches := selectSubsumptionValueMatchesPlaceholder(
			orientation.column, placeholder, candidateQuantifiers,
		)
		if !matches {
			continue
		}
		if !comparandIndependentOfSource(
			orientation.comparison.Operand,
			sourceAlias,
		) {
			continue
		}
		if fieldValue, isFieldValue := values.AsFieldValue(orientation.column); isFieldValue &&
			!comparisonTypesCompatible(
				fieldValue,
				&orientation.comparison,
			) {
			continue
		}

		comparison := orientation.comparison
		mergeResult := predicates.EmptyComparisonRange().Merge(&comparison)
		if mergeResult.Complete() {
			return mergeResult.Range, true
		}
	}
	return nil, false
}

func selectSubsumptionValueMatchesPlaceholder(
	value values.Value,
	placeholder *predicates.Placeholder,
	candidateQuantifiers []expressions.Quantifier,
) (values.CorrelationIdentifier, bool) {
	var zero values.CorrelationIdentifier
	if value == nil || selectSubsumptionIsTypedNil(value) || placeholder == nil ||
		placeholder.GetValue() == nil || selectSubsumptionIsTypedNil(placeholder.GetValue()) {
		return zero, false
	}
	candidateKinds := make(map[values.CorrelationIdentifier]expressions.QuantifierKind, len(candidateQuantifiers))
	for _, quantifier := range candidateQuantifiers {
		alias := quantifier.GetAlias()
		if alias.IsZero() {
			return zero, false
		}
		if _, duplicate := candidateKinds[alias]; duplicate {
			return zero, false
		}
		candidateKinds[alias] = quantifier.Kind()
	}
	source, hasSource := selectSubsumptionSingleLocalValueSource(placeholder.GetValue(), candidateKinds)
	if !hasSource || candidateKinds[source] != expressions.QuantifierForEach {
		return zero, false
	}
	columnSource, hasColumnSource := selectSubsumptionSingleLocalValueSource(value, candidateKinds)
	if !hasColumnSource || columnSource != source || !valuesMatchColumn(value, placeholder.GetValue()) {
		return zero, false
	}
	return source, true
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
	correlatedTo := make(map[values.CorrelationIdentifier]struct{})
	values.CollectCorrelatedToOfValue(value, correlatedTo)
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
