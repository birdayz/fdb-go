package cascades

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// quantifierPartialMatch records one branch selected from the Cartesian
// product of compatible child PartialMatches.
type quantifierPartialMatch struct {
	quantifier   expressions.Quantifier
	partialMatch PartialMatch
}

// tryMergeRegularMatchInfo is the Go counterpart of Java
// RegularMatchInfo.tryMerge. It combines child metadata with the metadata
// produced at the current expression, rejecting conflicts instead of silently
// choosing one branch.
func tryMergeRegularMatchInfo(
	bindingAliasMap *AliasMap,
	children []quantifierPartialMatch,
	parameterBindingMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	predicateMap *PredicateMultiMap,
	maxMatchMap *MaxMatchMap,
	additionalGroupByMappings *GroupByMappings,
	rollUpToGroupingValues []values.Value,
	additionalPlanConstraint *QueryPlanConstraint,
) (*RegularMatchInfo, bool) {
	return tryMergeRegularMatchInfoWithPrimaryKeyDistinct(
		bindingAliasMap,
		children,
		parameterBindingMap,
		predicateMap,
		maxMatchMap,
		additionalGroupByMappings,
		rollUpToGroupingValues,
		additionalPlanConstraint,
		false,
	)
}

// tryMergeRegularMatchInfoWithPrimaryKeyDistinct is the node-local merge path
// for semantic mappings that require a primary-key distinct repair. The flag
// is supplied by the current expression only; child flags must not leak into a
// parent whose own mapping preserves cardinality.
func tryMergeRegularMatchInfoWithPrimaryKeyDistinct(
	bindingAliasMap *AliasMap,
	children []quantifierPartialMatch,
	parameterBindingMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	predicateMap *PredicateMultiMap,
	maxMatchMap *MaxMatchMap,
	additionalGroupByMappings *GroupByMappings,
	rollUpToGroupingValues []values.Value,
	additionalPlanConstraint *QueryPlanConstraint,
	requiresPrimaryKeyDistinct bool,
) (*RegularMatchInfo, bool) {
	if bindingAliasMap == nil {
		bindingAliasMap = EmptyAliasMap()
	}
	seenAliases := make(map[values.CorrelationIdentifier]struct{}, len(children))
	seenChildren := make(map[*PartialMatchImpl]struct{}, len(children))
	for _, child := range children {
		alias := child.quantifier.GetAlias()
		if _, duplicate := seenAliases[alias]; duplicate {
			return nil, false
		}
		seenAliases[alias] = struct{}{}
		if _, bound := bindingAliasMap.GetTargetOrEmpty(alias); !bound {
			return nil, false
		}
		childImpl, ok := child.partialMatch.(*PartialMatchImpl)
		if !ok {
			return nil, false
		}
		if _, duplicate := seenChildren[childImpl]; duplicate {
			return nil, false
		}
		seenChildren[childImpl] = struct{}{}
	}

	parameterMaps := make(
		[]map[values.CorrelationIdentifier]*predicates.ComparisonRange,
		0,
		len(children)+1,
	)
	for _, child := range children {
		if child.partialMatch == nil ||
			child.partialMatch.GetRegularMatchInfo() == nil {
			return nil, false
		}
		parameterMaps = append(
			parameterMaps,
			child.partialMatch.GetRegularMatchInfo().GetParameterBindingMap(),
		)
	}
	parameterMaps = append(parameterMaps, parameterBindingMap)
	mergedParameters, ok := tryMergeParameterBindings(parameterMaps...)
	if !ok {
		return nil, false
	}

	regularChildren := make([]quantifierPartialMatch, 0, len(children))
	for _, child := range children {
		if child.quantifier.Kind() == expressions.QuantifierForEach ||
			child.quantifier.Kind() == expressions.QuantifierPhysical {
			regularChildren = append(regularChildren, child)
		}
	}

	var orderingParts []*MatchedOrderingPart
	if len(regularChildren) == 1 {
		orderingParts = regularChildren[0].partialMatch.
			GetMatchInfo().
			GetMatchedOrderingParts()
	}

	groupByMappings, ok := pullUpAndMergeGroupByMappings(
		bindingAliasMap,
		children,
		additionalGroupByMappings,
	)
	if !ok {
		return nil, false
	}

	resolvedRollUp := rollUpToGroupingValues
	if rollUpToGroupingValues != nil {
		for _, child := range regularChildren {
			if child.partialMatch.GetRegularMatchInfo().
				GetRollUpToGroupingValues() != nil {
				return nil, false
			}
		}
	} else {
		for _, child := range regularChildren {
			childRollUp := child.partialMatch.GetRegularMatchInfo().
				GetRollUpToGroupingValues()
			if childRollUp == nil {
				continue
			}
			if resolvedRollUp != nil {
				return nil, false
			}
			resolvedRollUp = childRollUp
		}
	}

	mi := newRegularMatchInfo(
		mergedParameters,
		bindingAliasMap,
		predicateMap,
		orderingParts,
		maxMatchMap,
		groupByMappings,
		resolvedRollUp,
		additionalPlanConstraint,
		requiresPrimaryKeyDistinct,
	)
	for _, child := range children {
		mi.SetChildPartialMatch(
			child.quantifier.GetAlias(),
			child.partialMatch,
		)
	}
	return mi, true
}

// tryMergeParameterBindings intersects parameter ranges from every child and
// the current expression. A residual/conflicting merge rejects only this
// child branch; another branch may still be viable.
func tryMergeParameterBindings(
	parameterMaps ...map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) (map[values.CorrelationIdentifier]*predicates.ComparisonRange, bool) {
	result := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange)
	for _, parameterMap := range parameterMaps {
		for alias, incoming := range parameterMap {
			if incoming == nil {
				return nil, false
			}
			existing, ok := result[alias]
			if !ok {
				result[alias] = incoming
				continue
			}
			merged, ok := mergeComparisonRanges(existing, incoming)
			if !ok {
				return nil, false
			}
			result[alias] = merged
		}
	}
	return result, true
}

func mergeComparisonRanges(
	left, right *predicates.ComparisonRange,
) (*predicates.ComparisonRange, bool) {
	if left == nil || right == nil {
		return nil, false
	}
	if partialMatchComparisonRangesEqual(left, right) {
		return left, true
	}
	if left.IsEmpty() {
		return right, true
	}
	if right.IsEmpty() {
		return left, true
	}
	if left.IsEquality() || right.IsEquality() {
		// The exact-equality case returned above. Any remaining equality pair
		// is conflicting, and equality/inequality is not representable by
		// ComparisonRange without a residual.
		return nil, false
	}

	if !left.IsInequality() || !right.IsInequality() {
		return nil, false
	}
	comparisons := append(
		[]*predicates.Comparison(nil),
		left.GetInequalityComparisons()...,
	)
	for _, incoming := range right.GetInequalityComparisons() {
		duplicate := false
		for _, existing := range comparisons {
			if comparisonsEqual(existing, incoming) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			comparisons = append(comparisons, incoming)
		}
	}
	result := predicates.EmptyComparisonRange()
	for _, comparison := range comparisons {
		merged := result.Merge(comparison)
		if !merged.Ok || !merged.Range.IsInequality() {
			return nil, false
		}
		result = merged.Range
	}
	return result, true
}

func pullUpAndMergeGroupByMappings(
	bindingAliasMap *AliasMap,
	children []quantifierPartialMatch,
	additional *GroupByMappings,
) (*GroupByMappings, bool) {
	if additional == nil {
		additional = EmptyGroupByMappings()
	}
	result := EmptyGroupByMappings()
	if !mergeGroupByMappingsChecked(result, additional) {
		return nil, false
	}

	for _, child := range children {
		if child.quantifier.Kind() != expressions.QuantifierForEach {
			continue
		}
		candidateAlias, ok := bindingAliasMap.GetTargetOrEmpty(
			child.quantifier.GetAlias(),
		)
		if !ok {
			return nil, false
		}
		pulled, ok := pullUpChildGroupByMappings(
			child.partialMatch,
			child.quantifier.GetAlias(),
			candidateAlias,
		)
		if !ok || !mergeGroupByMappingsChecked(result, pulled) {
			return nil, false
		}
	}
	return result, true
}

func pullUpChildGroupByMappings(
	partialMatch PartialMatch,
	queryAlias values.CorrelationIdentifier,
	candidateAlias values.CorrelationIdentifier,
) (*GroupByMappings, bool) {
	groupByMappings := partialMatch.GetMatchInfo().GetGroupByMappings()
	if groupByMappings == nil {
		return EmptyGroupByMappings(), true
	}
	if groupByMappings.MatchedGroupingsMap().Len() == 0 &&
		groupByMappings.MatchedAggregatesMap().Len() == 0 &&
		groupByMappings.UnmatchedAggregatesMap().Len() == 0 {
		// Java only dereferences the candidate member while iterating an
		// actual group mapping. Empty metadata is valid even when the
		// candidate reference has grown to several equivalent members.
		return EmptyGroupByMappings(), true
	}

	queryExpression := partialMatch.GetQueryExpression()
	if queryExpression == nil {
		return nil, false
	}
	queryResult := queryExpression.GetResultValue()
	queryExpressionCorrelations := expressions.GetCorrelatedToOfExpression(queryExpression)
	matchedQueryConstantAliases := intersectCorrelationSets(
		values.GetCorrelatedToOfValue(queryResult),
		queryExpressionCorrelations,
	)
	var candidateExpression expressions.RelationalExpression
	var candidateResult values.Value
	var candidateExpressionCorrelations map[values.CorrelationIdentifier]struct{}
	if groupByMappings.MatchedGroupingsMap().Len() > 0 ||
		groupByMappings.MatchedAggregatesMap().Len() > 0 {
		var single bool
		candidateExpression, single = onlyReferenceMember(
			partialMatch.GetCandidateRef(),
		)
		if !single {
			return nil, false
		}
		candidateResult = candidateExpression.GetResultValue()
		candidateExpressionCorrelations = expressions.GetCorrelatedToOfExpression(candidateExpression)
	}

	pullMatched := func(
		input *BiMap[values.Value, values.Value],
	) (*BiMap[values.Value, values.Value], bool) {
		output := NewValueBiMap()
		ok := true
		input.Range(func(queryValue, candidateValue values.Value) bool {
			pulledQuery, queryOK := pullUpGroupByValue(
				queryValue,
				queryResult,
				queryAlias,
				matchedQueryConstantAliases,
			)
			candidateConstantAliases := differenceCorrelationSets(
				values.GetCorrelatedToWithoutChildrenOfValue(candidateValue),
				candidateExpressionCorrelations,
			)
			pulledCandidate, candidateOK := pullUpGroupByValue(
				candidateValue,
				candidateResult,
				candidateAlias,
				candidateConstantAliases,
			)
			if !queryOK || !candidateOK {
				ok = false
				return false
			}
			if pulledQuery != nil && pulledCandidate != nil {
				ok = putValueBiMapChecked(
					output,
					pulledQuery,
					pulledCandidate,
				)
			}
			return ok
		})
		return output, ok
	}

	unmatched := NewCorrValueBiMap()
	unpullableUnmatched := false
	unmatchedCollision := false
	groupByMappings.UnmatchedAggregatesMap().Range(
		func(alias values.CorrelationIdentifier, queryValue values.Value) bool {
			constantAliases := differenceCorrelationSets(
				values.GetCorrelatedToWithoutChildrenOfValue(queryValue),
				queryExpressionCorrelations,
			)
			pulledQuery, pullOK := pullUpGroupByValue(
				queryValue,
				queryResult,
				queryAlias,
				constantAliases,
			)
			if !pullOK {
				unmatchedCollision = true
				return false
			}
			if pulledQuery == nil {
				// Java discards this child's aggregate mappings if an
				// unmatched aggregate cannot be pulled through the result.
				unpullableUnmatched = true
				return false
			}
			if !putCorrelationValueBiMapChecked(
				unmatched,
				alias,
				pulledQuery,
			) {
				unmatchedCollision = true
				return false
			}
			return true
		},
	)
	if unmatchedCollision {
		return nil, false
	}
	if unpullableUnmatched {
		return EmptyGroupByMappings(), true
	}

	matchedGroupings, ok := pullMatched(
		groupByMappings.MatchedGroupingsMap(),
	)
	if !ok {
		return nil, false
	}
	matchedAggregates, ok := pullMatched(
		groupByMappings.MatchedAggregatesMap(),
	)
	if !ok {
		return nil, false
	}
	return NewGroupByMappings(
		matchedGroupings,
		matchedAggregates,
		unmatched,
	), true
}

// pullUpGroupByValue is the group-metadata-only counterpart of Java
// Value.pullUp(..., constantAliases, upperBaseAlias). Constant expressions
// retain their identity and override ordinary pull-up results, matching
// MatchConstantValueRule's last-yield-wins behavior. The general PullUpValue
// API intentionally remains unchanged because its other callers do not carry
// this constant-alias context.
//
// ok=false means the one-result Go representation encountered an ambiguous or
// unsafe pull-up and the metadata merge must fail closed. A nil value with
// ok=true is an ordinary unpullable value, which matched maps skip and
// unmatched maps handle according to their Java contract.
func pullUpGroupByValue(
	value values.Value,
	resultValue values.Value,
	upperAlias values.CorrelationIdentifier,
	constantAliases map[values.CorrelationIdentifier]struct{},
) (pulled values.Value, ok bool) {
	if groupPullUpValueIsConstant(value, constantAliases) {
		return value, true
	}
	if value == nil || resultValue == nil {
		return nil, true
	}

	if values.ValuesStructurallyEqual(value, resultValue) {
		if quantifiedRecord, ok := resultValue.(*values.QuantifiedRecordValue); ok {
			// QuantifiedRecordValue's top-level QuantifiedValue.pullUp
			// shortcut translates the alias while preserving the concrete
			// queried-record carrier. The generic MatchValue rule would
			// instead produce a QuantifiedObjectValue.
			return values.RebaseValue(
				quantifiedRecord,
				values.AliasMap{quantifiedRecord.Alias: upperAlias},
			), true
		}
		return values.NewQuantifiedObjectValueOfType(
			upperAlias,
			resultValue.Type(),
		), true
	}

	switch result := resultValue.(type) {
	case *values.QuantifiedObjectValue:
		if containsUnanchoredFieldValue(value) {
			return nil, true
		}
		allowedAliases := copyCorrelationSet(constantAliases)
		allowedAliases[result.Correlation] = struct{}{}
		if !correlationSetContainsAll(
			allowedAliases,
			values.GetCorrelatedToOfValue(value),
		) {
			return nil, true
		}
		return values.RebaseValue(
			value,
			values.AliasMap{result.Correlation: upperAlias},
		), true
	case *values.QuantifiedRecordValue:
		if containsUnanchoredFieldValue(value) {
			return nil, true
		}
		allowedAliases := copyCorrelationSet(constantAliases)
		allowedAliases[result.Alias] = struct{}{}
		if !correlationSetContainsAll(
			allowedAliases,
			values.GetCorrelatedToOfValue(value),
		) {
			return nil, true
		}
		return values.RebaseValue(
			value,
			values.AliasMap{result.Alias: upperAlias},
		), true
	case *values.ObjectValue:
		// Java ObjectValue is a plain LeafValue, not a QuantifiedValue.
		// It has neither the quantified passthrough shortcut nor a
		// pull-up matcher rule.
		return nil, true
	}

	compensations := collectGroupPullUpCompensations(value, resultValue)
	if len(compensations) == 0 {
		// Retain the legacy pull-up behavior for value kinds outside the
		// Java rules ported below. Record constructors and field values are
		// deliberately not special-cased here: their recursive rule
		// traversal must detect every result, including ambiguity, before a
		// one-result API can return.
		return values.PullUpValue(value, resultValue, upperAlias), true
	}

	upperBase := values.NewQuantifiedObjectValueOfType(
		upperAlias,
		resultValue.Type(),
	)
	var unique []values.Value
	for _, compensation := range compensations {
		candidate := compensation(upperBase)
		if candidate == nil {
			continue
		}
		duplicate := false
		for _, existing := range unique {
			if values.ValuesStructurallyEqual(existing, candidate) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		unique = append(unique, candidate)
		if len(unique) > 1 {
			// Java retains every compensation in a multimap and the
			// MatchInfo consumers require exactly one. Reject instead of
			// selecting an arbitrary constructor path.
			return nil, false
		}
	}
	if len(unique) == 0 {
		return nil, true
	}
	return unique[0], true
}

// groupPullUpCompensation is the direct Go counterpart of Java's
// ValueCompensation. It rewrites the upper result value through the path that
// exposed a requested lower value.
type groupPullUpCompensation func(values.Value) values.Value

func collectGroupPullUpCompensations(
	requested values.Value,
	result values.Value,
) []groupPullUpCompensation {
	if requested == nil || result == nil {
		return nil
	}
	if values.ValuesStructurallyEqual(requested, result) {
		return []groupPullUpCompensation{
			func(upper values.Value) values.Value { return upper },
		}
	}

	switch result := result.(type) {
	case *values.RecordConstructorValue:
		var compensations []groupPullUpCompensation
		for ordinal, field := range result.Fields {
			childCompensations := collectGroupPullUpCompensations(
				requested,
				field.Value,
			)
			for _, childCompensation := range childCompensations {
				fieldName := field.Name
				fieldOrdinal := ordinal
				fieldType := field.Value.Type()
				downstream := childCompensation
				compensations = append(
					compensations,
					func(upper values.Value) values.Value {
						fieldValue := applyGroupFieldPath(
							upper,
							[]groupFieldPathStep{{
								field:    fieldName,
								ordinal:  fieldOrdinal,
								resolved: true,
							}},
							fieldType,
						)
						return downstream(fieldValue)
					},
				)
			}
		}
		return compensations

	case *values.FieldValue:
		// MatchOrCompensateFieldValueRule also consumes a field
		// compensation produced by the result's child. The most important
		// form is field(record{x: lower}, x): the constructor first prefixes
		// a child's compensation with x, then the enclosing field removes
		// that same prefix. Go's value simplifier performs the equivalent
		// constructor projection, after which the recursive matcher retains
		// only the downstream compensation.
		if simplified := values.SimplifyValue(result); simplified != result {
			return collectGroupPullUpCompensations(requested, simplified)
		}

		requestedField, ok := requested.(*values.FieldValue)
		if !ok {
			return nil
		}
		resultBase, resultPath, resultOK := decomposeGroupFieldPath(result)
		requestedBase, requestedPath, requestedOK := decomposeGroupFieldPath(requestedField)
		if !resultOK ||
			!requestedOK ||
			resultBase == nil ||
			!values.ValuesStructurallyEqual(resultBase, requestedBase) {
			return nil
		}
		suffix, prefixOK := stripGroupFieldPathPrefix(
			requestedPath,
			resultPath,
		)
		if !prefixOK {
			return nil
		}
		return []groupPullUpCompensation{
			func(upper values.Value) values.Value {
				return applyGroupFieldPath(
					upper,
					suffix,
					requested.Type(),
				)
			},
		}

	case *values.QuantifiedObjectValue:
		if requestedField, ok := requested.(*values.FieldValue); ok {
			requestedBase, requestedPath, pathOK := decomposeGroupFieldPath(requestedField)
			if !pathOK ||
				requestedBase == nil ||
				!values.ValuesStructurallyEqual(result, requestedBase) {
				return nil
			}
			return []groupPullUpCompensation{
				func(upper values.Value) values.Value {
					return applyGroupFieldPath(
						upper,
						requestedPath,
						requested.Type(),
					)
				},
			}
		}
		if _, isQuantifiedObject := requested.(*values.QuantifiedObjectValue); isQuantifiedObject {
			return nil
		}
		correlatedTo := values.GetCorrelatedToOfValue(requested)
		if len(correlatedTo) != 1 {
			return nil
		}
		if _, matchesResultAlias := correlatedTo[result.Correlation]; !matchesResultAlias {
			return nil
		}
		return []groupPullUpCompensation{
			func(upper values.Value) values.Value {
				translation := values.NewTranslationMapBuilder().
					When(result.Correlation).
					Then(func(
						_ values.CorrelationIdentifier,
						_ values.Value,
					) values.Value {
						return upper
					}).
					Build()
				return values.TranslateCorrelations(requested, translation)
			},
		}
	}

	return nil
}

type groupFieldPathStep struct {
	field    string
	ordinal  int
	resolved bool
}

// decomposeGroupFieldPath normalizes Go's canonical fused FieldValue and its
// legacy chained form into one root-to-leaf path. Childless fields remain
// unsafe here: an exact match can still be pulled through an explicitly known
// constructor output, but prefix arithmetic without a base could conflate two
// sources.
func decomposeGroupFieldPath(
	field *values.FieldValue,
) (values.Value, []groupFieldPathStep, bool) {
	if field == nil || field.Child == nil {
		return nil, nil, false
	}

	base := field.Child
	var path []groupFieldPathStep
	if childField, ok := field.Child.(*values.FieldValue); ok {
		var childOK bool
		base, path, childOK = decomposeGroupFieldPath(childField)
		if !childOK {
			return nil, nil, false
		}
	}

	if field.Resolved == nil {
		path = append(path, groupFieldPathStep{field: field.Field})
		return base, path, true
	}
	if len(field.Resolved.Accessors) == 0 {
		return nil, nil, false
	}
	for _, accessor := range field.Resolved.Accessors {
		path = append(path, groupFieldPathStep{
			field:    accessor.Field,
			ordinal:  accessor.Ordinal,
			resolved: true,
		})
	}
	return base, path, true
}

func stripGroupFieldPathPrefix(
	path []groupFieldPathStep,
	prefix []groupFieldPathStep,
) ([]groupFieldPathStep, bool) {
	if len(prefix) > len(path) {
		return nil, false
	}
	for i := range prefix {
		if prefix[i].resolved != path[i].resolved {
			return nil, false
		}
		if prefix[i].resolved {
			if prefix[i].ordinal != path[i].ordinal {
				return nil, false
			}
			continue
		}
		if prefix[i].field != path[i].field {
			return nil, false
		}
	}
	return path[len(prefix):], true
}

func applyGroupFieldPath(
	base values.Value,
	path []groupFieldPathStep,
	resultType values.Type,
) values.Value {
	current := base
	for i, step := range path {
		stepType := values.UnknownType
		if i == len(path)-1 {
			stepType = resultType
		}
		if !step.resolved {
			current = &values.FieldValue{
				Field: step.field,
				Typ:   stepType,
				Child: current,
			}
			continue
		}

		stepPath := values.NewFieldPathOfSingle(
			step.field,
			step.ordinal,
			false,
		)
		if inner, ok := current.(*values.FieldValue); ok &&
			inner.Resolved != nil &&
			inner.Child != nil {
			fused := inner.Resolved.WithSuffix(stepPath)
			current = &values.FieldValue{
				Field:    fused.Last().Field,
				Typ:      stepType,
				Child:    inner.Child,
				Resolved: fused,
			}
			continue
		}
		current = &values.FieldValue{
			Field:    step.field,
			Typ:      stepType,
			Child:    current,
			Resolved: stepPath,
		}
	}
	return current
}

func groupPullUpValueIsConstant(
	value values.Value,
	constantAliases map[values.CorrelationIdentifier]struct{},
) bool {
	if value == nil ||
		!correlationSetContainsAll(
			constantAliases,
			values.GetCorrelatedToOfValue(value),
		) {
		return false
	}
	allowed := true
	values.WalkValue(value, func(node values.Value) bool {
		switch node.(type) {
		case *values.AggregateValue,
			*values.IndexOnlyAggregateValue,
			*values.QueriedValue:
			allowed = false
			return false
		case *values.FieldValue:
			field := node.(*values.FieldValue)
			if field.Child == nil {
				// Java FieldValue always has a base. Go's legacy flat
				// representation carries no correlation and therefore
				// cannot be proven constant in a multi-source parent.
				allowed = false
				return false
			}
			return true
		default:
			return true
		}
	})
	return allowed
}

func containsUnanchoredFieldValue(value values.Value) bool {
	found := false
	values.WalkValue(value, func(node values.Value) bool {
		field, ok := node.(*values.FieldValue)
		if ok && field.Child == nil {
			found = true
			return false
		}
		return true
	})
	return found
}

func copyCorrelationSet(
	input map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{}, len(input)+1)
	for alias := range input {
		result[alias] = struct{}{}
	}
	return result
}

func intersectCorrelationSets(
	left, right map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range left {
		if _, present := right[alias]; present {
			result[alias] = struct{}{}
		}
	}
	return result
}

func differenceCorrelationSets(
	left, right map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	result := make(map[values.CorrelationIdentifier]struct{})
	for alias := range left {
		if _, present := right[alias]; !present {
			result[alias] = struct{}{}
		}
	}
	return result
}

func correlationSetContainsAll(
	superset, subset map[values.CorrelationIdentifier]struct{},
) bool {
	for alias := range subset {
		if _, present := superset[alias]; !present {
			return false
		}
	}
	return true
}

func mergeGroupByMappingsChecked(
	target, source *GroupByMappings,
) bool {
	if target == nil || source == nil {
		return target == source
	}
	ok := true
	source.MatchedGroupingsMap().Range(func(k, v values.Value) bool {
		ok = putValueBiMapChecked(target.MatchedGroupingsMap(), k, v)
		return ok
	})
	if !ok {
		return false
	}
	source.MatchedAggregatesMap().Range(func(k, v values.Value) bool {
		ok = putValueBiMapChecked(target.MatchedAggregatesMap(), k, v)
		return ok
	})
	if !ok {
		return false
	}
	source.UnmatchedAggregatesMap().Range(
		func(k values.CorrelationIdentifier, v values.Value) bool {
			ok = putCorrelationValueBiMapChecked(
				target.UnmatchedAggregatesMap(),
				k,
				v,
			)
			return ok
		},
	)
	return ok
}

func putValueBiMapChecked(
	target *BiMap[values.Value, values.Value],
	key, value values.Value,
) bool {
	equivalent := false
	valid := true
	target.Range(func(existingKey, existingValue values.Value) bool {
		keysEqual := values.SemanticEqualsUnderAliasMap(
			existingKey,
			key,
			nil,
		)
		valuesEqual := values.SemanticEqualsUnderAliasMap(
			existingValue,
			value,
			nil,
		)
		if keysEqual || valuesEqual {
			valid = keysEqual && valuesEqual
			equivalent = valid
			return false
		}
		// BiMap stores by rendered strings. If that representation would
		// collide despite semantic inequality, fail closed before Put can
		// overwrite/drop a distinct mapping.
		if target.keyStr(existingKey) == target.keyStr(key) ||
			target.valStr(existingValue) == target.valStr(value) {
			valid = false
			return false
		}
		return true
	})
	if !valid || equivalent {
		return valid
	}
	target.Put(key, value)
	return true
}

func putCorrelationValueBiMapChecked(
	target *BiMap[values.CorrelationIdentifier, values.Value],
	key values.CorrelationIdentifier,
	value values.Value,
) bool {
	equivalent := false
	valid := true
	target.Range(func(
		existingKey values.CorrelationIdentifier,
		existingValue values.Value,
	) bool {
		keysEqual := existingKey == key
		valuesEqual := values.SemanticEqualsUnderAliasMap(
			existingValue,
			value,
			nil,
		)
		if keysEqual || valuesEqual {
			valid = keysEqual && valuesEqual
			equivalent = valid
			return false
		}
		if target.keyStr(existingKey) == target.keyStr(key) ||
			target.valStr(existingValue) == target.valStr(value) {
			valid = false
			return false
		}
		return true
	})
	if !valid || equivalent {
		return valid
	}
	target.Put(key, value)
	return true
}

// composeQueryPlanConstraints produces a deterministic conjunction, omitting
// tautologies so nil and explicit TRUE have the same effective semantics.
func composeQueryPlanConstraints(
	constraints ...*QueryPlanConstraint,
) *QueryPlanConstraint {
	var conjuncts []predicates.QueryPredicate
	for _, constraint := range constraints {
		if constraint == nil || constraint.IsTautology() {
			continue
		}
		conjuncts = append(conjuncts, constraint.GetPredicate())
	}
	if len(conjuncts) == 0 {
		return Tautology()
	}
	if len(conjuncts) == 1 {
		return NewQueryPlanConstraint(conjuncts[0])
	}
	return NewQueryPlanConstraint(predicates.NewAnd(conjuncts...))
}

// sortedChildAliases keeps effective-constraint construction independent of
// Go map iteration order.
func sortedChildAliases(
	children map[values.CorrelationIdentifier]PartialMatch,
) []values.CorrelationIdentifier {
	aliases := make([]values.CorrelationIdentifier, 0, len(children))
	for alias := range children {
		aliases = append(aliases, alias)
	}
	sort.Slice(aliases, func(i, j int) bool {
		return aliases[i].Name() < aliases[j].Name()
	})
	return aliases
}
