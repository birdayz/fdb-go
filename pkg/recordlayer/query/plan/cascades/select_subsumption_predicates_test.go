package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type selectSubsumptionPredicateTestAlternative struct {
	parameterBindings map[values.CorrelationIdentifier]*predicates.ComparisonRange
	predicateMap      *PredicateMultiMap
}

func selectSubsumptionPredicateTestRef() *expressions.Reference {
	return expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"T"},
			values.UnknownType,
		),
	)
}

func selectSubsumptionPredicateTestSelect(
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		quantifiers,
		queryPredicates,
	)
}

func selectSubsumptionPredicateTestComparison(
	alias values.CorrelationIdentifier,
	field string,
	literal int64,
) *predicates.ComparisonPredicate {
	return predicates.NewComparisonPredicate(
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(alias),
			field,
			values.UnknownType,
		),
		predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			literal,
		),
	)
}

func collectSelectSubsumptionPredicateTestAlternatives(
	t *testing.T,
	originalQueryPredicates []predicates.QueryPredicate,
	translatedQueryPredicates []predicates.QueryPredicate,
	candidateSelect *expressions.SelectExpression,
	bindingAliasMap *AliasMap,
) []selectSubsumptionPredicateTestAlternative {
	t.Helper()
	var alternatives []selectSubsumptionPredicateTestAlternative
	exhausted := enumerateSelectSubsumptionPredicateAlternatives(
		originalQueryPredicates,
		translatedQueryPredicates,
		candidateSelect,
		bindingAliasMap,
		func(builder selectSubsumptionPredicateAlternativeBuilder) bool {
			parameterBindings, predicateMap, ok := builder()
			if ok {
				alternatives = append(
					alternatives,
					selectSubsumptionPredicateTestAlternative{
						parameterBindings: parameterBindings,
						predicateMap:      predicateMap,
					},
				)
			}
			return true
		},
	)
	if !exhausted {
		t.Fatal("predicate alternative enumeration stopped unexpectedly")
	}
	return alternatives
}

func selectSubsumptionPredicateTestMappingTo(
	t *testing.T,
	predicateMap *PredicateMultiMap,
	originalQueryPredicate predicates.QueryPredicate,
	candidatePredicate predicates.QueryPredicate,
) *PredicateMapping {
	t.Helper()
	for _, mapping := range predicateMap.Get(originalQueryPredicate) {
		if mapping.GetCandidatePredicate() == candidatePredicate {
			return mapping
		}
	}
	t.Fatalf(
		"no mapping from %p to candidate %p",
		originalQueryPredicate,
		candidatePredicate,
	)
	return nil
}

func selectSubsumptionPredicateTestEqualityLiteral(
	t *testing.T,
	comparisonRange *predicates.ComparisonRange,
) int64 {
	t.Helper()
	if comparisonRange == nil || !comparisonRange.IsEquality() {
		t.Fatalf("comparison range = %#v, want equality", comparisonRange)
	}
	comparison := comparisonRange.GetEqualityComparison()
	literal, ok := comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf(
			"equality operand = %T, want *values.ConstantValue",
			comparison.Operand,
		)
	}
	result, ok := literal.Value.(int64)
	if !ok {
		t.Fatalf("equality literal = %T, want int64", literal.Value)
	}
	return result
}

func TestSelectSubsumptionPredicateAlternativesSharedCandidateTrueRetainsEveryQuery(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	candidateTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{candidateTrue},
	)
	queryOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	queryTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"y",
		2,
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryOne, queryTwo},
		[]predicates.QueryPredicate{queryOne, queryTwo},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 2 {
		t.Fatalf("alternatives = %d, want 2", len(alternatives))
	}

	for alternativeIndex, alternative := range alternatives {
		if alternative.predicateMap.PredicateCount() != 2 ||
			alternative.predicateMap.Size() != 2 {
			t.Fatalf(
				"alternative %d predicate map = %d predicates/%d mappings, want 2/2",
				alternativeIndex,
				alternative.predicateMap.PredicateCount(),
				alternative.predicateMap.Size(),
			)
		}
		selectedQuery := predicates.QueryPredicate(queryOne)
		residualQuery := predicates.QueryPredicate(queryTwo)
		if alternativeIndex == 1 {
			selectedQuery, residualQuery = residualQuery, selectedQuery
		}
		selectedMapping := selectSubsumptionPredicateTestMappingTo(
			t,
			alternative.predicateMap,
			selectedQuery,
			candidateTrue,
		)
		if !selectedMapping.GetPredicateCompensation()(
			nil,
			nil,
			nil,
		).IsNeeded() {
			t.Fatalf(
				"alternative %d mapping to candidate TRUE must reapply the query",
				alternativeIndex,
			)
		}

		residualMappings := alternative.predicateMap.Get(residualQuery)
		if len(residualMappings) != 1 {
			t.Fatalf(
				"alternative %d residual mappings = %d, want 1",
				alternativeIndex,
				len(residualMappings),
			)
		}
		residualCandidate := residualMappings[0].GetCandidatePredicate()
		if residualCandidate == candidateTrue ||
			!predicates.IsTautology(residualCandidate) {
			t.Fatalf(
				"alternative %d residual candidate = %p, want fresh TRUE distinct from %p",
				alternativeIndex,
				residualCandidate,
				candidateTrue,
			)
		}
	}
}

func TestSelectSubsumptionPredicateAlternativesFreshResidualsHaveDistinctIdentity(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{
			expressions.NamedForEachQuantifier(candidateAlias, ref),
		},
		nil,
	)
	queryOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	queryTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"y",
		2,
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryOne, queryTwo},
		[]predicates.QueryPredicate{queryOne, queryTwo},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want 1", len(alternatives))
	}
	predicateMap := alternatives[0].predicateMap
	if predicateMap.PredicateCount() != 2 ||
		predicateMap.Size() != 2 {
		t.Fatalf(
			"predicate map = %d predicates/%d mappings, want 2/2",
			predicateMap.PredicateCount(),
			predicateMap.Size(),
		)
	}
	oneCandidate := predicateMap.Get(queryOne)[0].GetCandidatePredicate()
	twoCandidate := predicateMap.Get(queryTwo)[0].GetCandidatePredicate()
	if oneCandidate == twoCandidate {
		t.Fatal("two residual mappings reused one candidate TRUE identity")
	}
	if !predicates.IsTautology(oneCandidate) ||
		!predicates.IsTautology(twoCandidate) {
		t.Fatal("fresh residual candidates must both be TRUE")
	}
}

func TestSelectSubsumptionPredicateAlternativesSharedPlaceholderRetainsResidual(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(
		parameterAlias,
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAlias),
			"x",
			values.UnknownType,
		),
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholder},
	)
	queryOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	queryTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		2,
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryOne, queryTwo},
		[]predicates.QueryPredicate{queryOne, queryTwo},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 2 {
		t.Fatalf("alternatives = %d, want 2", len(alternatives))
	}
	for alternativeIndex, alternative := range alternatives {
		wantLiteral := int64(alternativeIndex + 1)
		if got := selectSubsumptionPredicateTestEqualityLiteral(
			t,
			alternative.parameterBindings[parameterAlias],
		); got != wantLiteral {
			t.Fatalf(
				"alternative %d bound literal = %d, want %d",
				alternativeIndex,
				got,
				wantLiteral,
			)
		}

		selectedQuery := predicates.QueryPredicate(queryOne)
		residualQuery := predicates.QueryPredicate(queryTwo)
		if alternativeIndex == 1 {
			selectedQuery, residualQuery = residualQuery, selectedQuery
		}
		sargableMapping := selectSubsumptionPredicateTestMappingTo(
			t,
			alternative.predicateMap,
			selectedQuery,
			placeholder,
		)
		compensation := sargableMapping.GetPredicateCompensation()
		if compensation(
			nil,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameterAlias: alternative.parameterBindings[parameterAlias],
			},
			nil,
		).IsNeeded() {
			t.Fatalf(
				"alternative %d sargable mapping compensated despite a bound prefix",
				alternativeIndex,
			)
		}
		if !compensation(nil, nil, nil).IsNeeded() {
			t.Fatalf(
				"alternative %d sargable mapping did not reapply without a bound prefix",
				alternativeIndex,
			)
		}

		residualMappings := alternative.predicateMap.Get(residualQuery)
		if len(residualMappings) != 1 ||
			!predicates.IsTautology(
				residualMappings[0].GetCandidatePredicate(),
			) ||
			residualMappings[0].GetCandidatePredicate() == placeholder {
			t.Fatalf(
				"alternative %d did not retain the unselected query as a fresh residual",
				alternativeIndex,
			)
		}
	}
}

func TestSelectSubsumptionPredicateAlternativesParameterComparisonBindsPlaceholder(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("candidate_parameter")
	placeholder := predicates.NewPlaceholder(
		parameterAlias,
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAlias),
			"x",
			values.UnknownType,
		),
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholder},
	)
	queryPredicate := predicates.NewComparisonPredicate(
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAlias),
			"x",
			values.UnknownType,
		),
		predicates.Comparison{
			Type:          predicates.ComparisonEquals,
			ParameterName: "query_parameter",
		},
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryPredicate},
		[]predicates.QueryPredicate{queryPredicate},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want 1", len(alternatives))
	}
	comparisonRange := alternatives[0].parameterBindings[parameterAlias]
	if comparisonRange == nil || !comparisonRange.IsEquality() {
		t.Fatalf(
			"parameter comparison range = %#v, want equality",
			comparisonRange,
		)
	}
	comparison := comparisonRange.GetEqualityComparison()
	if comparison.ParameterName != "query_parameter" ||
		comparison.Operand != nil {
		t.Fatalf(
			"bound comparison = %#v, want named parameter with nil value operand",
			comparison,
		)
	}
	selectSubsumptionPredicateTestMappingTo(
		t,
		alternatives[0].predicateMap,
		queryPredicate,
		placeholder,
	)
}

func TestSelectSubsumptionPredicateAlternativesCandidateCoverageUsesIdentity(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	queryPredicate := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	candidateOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	candidateTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{candidateOne, candidateTwo},
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryPredicate},
		[]predicates.QueryPredicate{queryPredicate},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want 1", len(alternatives))
	}
	mappings := alternatives[0].predicateMap.Get(queryPredicate)
	if len(mappings) != 2 {
		t.Fatalf("query mappings = %d, want both candidate identities", len(mappings))
	}
	if selectSubsumptionPredicateTestMappingTo(
		t,
		alternatives[0].predicateMap,
		queryPredicate,
		candidateOne,
	) == nil || selectSubsumptionPredicateTestMappingTo(
		t,
		alternatives[0].predicateMap,
		queryPredicate,
		candidateTwo,
	) == nil {
		t.Fatal("both distinct candidate predicate identities must be covered")
	}

	unmatchedFiltering := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"y",
		9,
	)
	candidateWithUnmatched := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{
			candidateOne,
			candidateTwo,
			unmatchedFiltering,
		},
	)
	if got := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryPredicate},
		[]predicates.QueryPredicate{queryPredicate},
		candidateWithUnmatched,
		EmptyAliasMap(),
	); len(got) != 0 {
		t.Fatalf(
			"unmapped filtering candidate produced %d alternatives",
			len(got),
		)
	}
}

func TestSelectSubsumptionPredicateAlternativesCandidateNonFilteringGate(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(
		parameterAlias,
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAlias),
			"x",
			values.UnknownType,
		),
	)
	candidateTrue := predicates.NewConstantPredicate(predicates.TriTrue)

	t.Run("constant true and unconstraining placeholder", func(t *testing.T) {
		candidateSelect := selectSubsumptionPredicateTestSelect(
			[]expressions.Quantifier{candidateQuantifier},
			[]predicates.QueryPredicate{candidateTrue, placeholder},
		)
		alternatives := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			nil,
			nil,
			candidateSelect,
			EmptyAliasMap(),
		)
		if len(alternatives) != 1 {
			t.Fatalf("alternatives = %d, want 1", len(alternatives))
		}
		if alternatives[0].predicateMap.Size() != 0 {
			t.Fatalf(
				"empty query predicate map size = %d, want 0",
				alternatives[0].predicateMap.Size(),
			)
		}
	})

	t.Run("filtering constant", func(t *testing.T) {
		candidateSelect := selectSubsumptionPredicateTestSelect(
			[]expressions.Quantifier{candidateQuantifier},
			[]predicates.QueryPredicate{
				predicates.NewConstantPredicate(predicates.TriFalse),
			},
		)
		if got := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			nil,
			nil,
			candidateSelect,
			EmptyAliasMap(),
		); len(got) != 0 {
			t.Fatalf("filtering constant produced %d alternatives", len(got))
		}
	})

	t.Run("constraining placeholder", func(t *testing.T) {
		comparison := predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			int64(1),
		)
		mergeResult := predicates.EmptyComparisonRange().Merge(&comparison)
		constrainingPlaceholder := placeholder.WithRange(mergeResult.Range)
		candidateSelect := selectSubsumptionPredicateTestSelect(
			[]expressions.Quantifier{candidateQuantifier},
			[]predicates.QueryPredicate{constrainingPlaceholder},
		)
		if got := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			nil,
			nil,
			candidateSelect,
			EmptyAliasMap(),
		); len(got) != 0 {
			t.Fatalf(
				"constraining placeholder produced %d alternatives",
				len(got),
			)
		}
	})

	t.Run("nil placeholder range", func(t *testing.T) {
		nilRangePlaceholder := &predicates.Placeholder{
			ParameterAlias: parameterAlias,
			Value:          placeholder.GetValue(),
			CompRange:      nil,
		}
		candidateSelect := selectSubsumptionPredicateTestSelect(
			[]expressions.Quantifier{candidateQuantifier},
			[]predicates.QueryPredicate{nilRangePlaceholder},
		)
		if got := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			nil,
			nil,
			candidateSelect,
			EmptyAliasMap(),
		); len(got) != 0 {
			t.Fatalf(
				"nil-range placeholder produced %d alternatives",
				len(got),
			)
		}
	})
}

func TestSelectSubsumptionPredicateAlternativesNestedTypedNilValuesFailClosed(
	t *testing.T,
) {
	t.Parallel()

	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate_nested_bad")
	candidateQuantifier := expressions.NamedForEachQuantifier(
		candidateAlias,
		ref,
	)
	validQuery := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	var typedNilValue *values.QuantifiedObjectValue
	nestedTypedNilValue := &values.FieldValue{
		Field: "nested_bad",
		Typ:   values.UnknownType,
		Child: typedNilValue,
	}
	malformedQuery := predicates.NewComparisonPredicate(
		nestedTypedNilValue,
		predicates.Comparison{
			Type: predicates.ComparisonIsNull,
		},
	)
	malformedPlaceholder := &predicates.Placeholder{
		ParameterAlias: values.NamedCorrelationIdentifier("parameter_nested_bad"),
		Value:          nestedTypedNilValue,
		CompRange:      predicates.EmptyComparisonRange(),
	}
	nestedComparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: nestedTypedNilValue,
	}
	malformedRangePlaceholder := &predicates.Placeholder{
		ParameterAlias: values.NamedCorrelationIdentifier("parameter_range_nested_bad"),
		Value: values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAlias),
			"x",
			values.UnknownType,
		),
		CompRange: predicates.EmptyComparisonRange().
			Merge(&nestedComparison).
			Range,
	}

	tests := []struct {
		name                 string
		originalPredicates   []predicates.QueryPredicate
		translatedPredicates []predicates.QueryPredicate
		candidatePredicates  []predicates.QueryPredicate
	}{
		{
			name:                 "query value descendant",
			originalPredicates:   []predicates.QueryPredicate{malformedQuery},
			translatedPredicates: []predicates.QueryPredicate{malformedQuery},
			candidatePredicates: []predicates.QueryPredicate{
				predicates.NewConstantPredicate(predicates.TriTrue),
			},
		},
		{
			name:                 "candidate value descendant",
			originalPredicates:   []predicates.QueryPredicate{validQuery},
			translatedPredicates: []predicates.QueryPredicate{validQuery},
			candidatePredicates: []predicates.QueryPredicate{
				malformedPlaceholder,
			},
		},
		{
			name:                 "candidate range comparand descendant",
			originalPredicates:   []predicates.QueryPredicate{validQuery},
			translatedPredicates: []predicates.QueryPredicate{validQuery},
			candidatePredicates: []predicates.QueryPredicate{
				malformedRangePlaceholder,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf(
						"nested typed-nil predicate value panicked instead of failing closed: %v",
						recovered,
					)
				}
			}()
			candidateSelect := selectSubsumptionPredicateTestSelect(
				[]expressions.Quantifier{candidateQuantifier},
				test.candidatePredicates,
			)
			if alternatives := collectSelectSubsumptionPredicateTestAlternatives(
				t,
				test.originalPredicates,
				test.translatedPredicates,
				candidateSelect,
				EmptyAliasMap(),
			); len(alternatives) != 0 {
				t.Fatalf(
					"malformed value produced %d predicate alternatives",
					len(alternatives),
				)
			}
		})
	}
}

func TestSelectSubsumptionPredicateAlternativesMergeParameterBindingsChecked(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("shared_parameter")
	placeholderValue := values.NewFieldValue(
		values.NewQuantifiedObjectValue(candidateAlias),
		"x",
		values.UnknownType,
	)
	placeholderOne := predicates.NewPlaceholder(parameterAlias, placeholderValue)
	placeholderTwo := predicates.NewPlaceholder(parameterAlias, placeholderValue)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholderOne, placeholderTwo},
	)
	queryOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	queryTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		2,
	)

	// The two candidate identities each have both query mappings, creating
	// four raw products. Only the two products whose repeated parameter
	// bindings agree may survive.
	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{queryOne, queryTwo},
		[]predicates.QueryPredicate{queryOne, queryTwo},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(alternatives) != 2 {
		t.Fatalf(
			"checked parameter merge retained %d alternatives, want 2",
			len(alternatives),
		)
	}
	for alternativeIndex, alternative := range alternatives {
		wantLiteral := int64(alternativeIndex + 1)
		if got := selectSubsumptionPredicateTestEqualityLiteral(
			t,
			alternative.parameterBindings[parameterAlias],
		); got != wantLiteral {
			t.Fatalf(
				"alternative %d literal = %d, want %d",
				alternativeIndex,
				got,
				wantLiteral,
			)
		}
	}
}

func TestSelectSubsumptionPredicateAlternativesPlaceholderIsLegSpecific(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAliasA := values.NamedCorrelationIdentifier("candidate_a")
	candidateAliasB := values.NamedCorrelationIdentifier("candidate_b")
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(candidateAliasA, ref),
		expressions.NamedForEachQuantifier(candidateAliasB, ref),
	}
	parameterAlias := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(
		parameterAlias,
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(candidateAliasA),
			"x",
			values.UnknownType,
		),
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		candidateQuantifiers,
		[]predicates.QueryPredicate{placeholder},
	)

	wrongLegQuery := selectSubsumptionPredicateTestComparison(
		candidateAliasB,
		"x",
		1,
	)
	wrongLegAlternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{wrongLegQuery},
		[]predicates.QueryPredicate{wrongLegQuery},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(wrongLegAlternatives) != 1 {
		t.Fatalf(
			"wrong-leg alternatives = %d, want residual-only alternative",
			len(wrongLegAlternatives),
		)
	}
	if len(wrongLegAlternatives[0].parameterBindings) != 0 {
		t.Fatalf(
			"wrong-leg comparison produced bindings %#v",
			wrongLegAlternatives[0].parameterBindings,
		)
	}
	wrongLegMappings := wrongLegAlternatives[0].predicateMap.Get(wrongLegQuery)
	if len(wrongLegMappings) != 1 ||
		wrongLegMappings[0].GetCandidatePredicate() == placeholder {
		t.Fatal("wrong-leg comparison bound to the same-name placeholder")
	}

	rightLegQuery := selectSubsumptionPredicateTestComparison(
		candidateAliasA,
		"x",
		1,
	)
	rightLegAlternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{rightLegQuery},
		[]predicates.QueryPredicate{rightLegQuery},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(rightLegAlternatives) != 1 {
		t.Fatalf(
			"right-leg alternatives = %d, want 1",
			len(rightLegAlternatives),
		)
	}
	if _, bound := rightLegAlternatives[0].parameterBindings[parameterAlias]; !bound {
		t.Fatal("right-leg comparison did not bind the placeholder")
	}
	selectSubsumptionPredicateTestMappingTo(
		t,
		rightLegAlternatives[0].predicateMap,
		rightLegQuery,
		placeholder,
	)

	outerAlias := values.NamedCorrelationIdentifier("outer")
	localAndOuterValue := values.NewFieldValue(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "local",
				Value: values.NewQuantifiedObjectValue(candidateAliasA),
			},
			values.RecordConstructorField{
				Name:  "outer",
				Value: values.NewQuantifiedObjectValue(outerAlias),
			},
		),
		"x",
		values.UnknownType,
	)
	localAndOuterQuery := predicates.NewComparisonPredicate(
		localAndOuterValue,
		predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			int64(1),
		),
	)
	localAndOuterAlternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{localAndOuterQuery},
		[]predicates.QueryPredicate{localAndOuterQuery},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(localAndOuterAlternatives) != 1 {
		t.Fatalf(
			"local+outer alternatives = %d, want residual-only alternative",
			len(localAndOuterAlternatives),
		)
	}
	if len(localAndOuterAlternatives[0].parameterBindings) != 0 {
		t.Fatalf(
			"local+outer LHS produced bindings %#v",
			localAndOuterAlternatives[0].parameterBindings,
		)
	}
	localAndOuterMappings := localAndOuterAlternatives[0].predicateMap.Get(localAndOuterQuery)
	if len(localAndOuterMappings) != 1 ||
		localAndOuterMappings[0].GetCandidatePredicate() == placeholder {
		t.Fatal("local+outer-correlated LHS bound to a single-leg placeholder")
	}
}

func TestSelectSubsumptionPredicateAlternativesPreserveOriginalAndTranslated(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	originalQueryPredicate := selectSubsumptionPredicateTestComparison(
		queryAlias,
		"x",
		1,
	)
	translatedQueryPredicate := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	candidatePredicate := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{candidatePredicate},
	)

	alternatives := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{originalQueryPredicate},
		[]predicates.QueryPredicate{translatedQueryPredicate},
		candidateSelect,
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want 1", len(alternatives))
	}
	mapping := selectSubsumptionPredicateTestMappingTo(
		t,
		alternatives[0].predicateMap,
		originalQueryPredicate,
		candidatePredicate,
	)
	if mapping.GetOriginalQueryPredicate() != originalQueryPredicate {
		t.Fatal("mapping did not preserve original query predicate identity")
	}
	if mapping.GetTranslatedQueryPredicate() != translatedQueryPredicate {
		t.Fatal("mapping did not preserve translated query predicate identity")
	}
	if mapping.GetPredicateCompensation()(nil, nil, nil).IsNeeded() {
		t.Fatal("semantically equal predicates should not need compensation")
	}
}

func TestSelectSubsumptionPredicateAlternativesBuildersAreLazyStableAndStoppable(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	candidateTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{candidateTrue},
	)
	queryOne := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"x",
		1,
	)
	queryTwo := selectSubsumptionPredicateTestComparison(
		candidateAlias,
		"y",
		2,
	)
	original := []predicates.QueryPredicate{queryOne, queryTwo}

	var builders []selectSubsumptionPredicateAlternativeBuilder
	if exhausted := enumerateSelectSubsumptionPredicateAlternatives(
		original,
		original,
		candidateSelect,
		EmptyAliasMap(),
		func(builder selectSubsumptionPredicateAlternativeBuilder) bool {
			// Retain without invoking. The recursive product's scratch slice
			// is overwritten before these builders run.
			builders = append(builders, builder)
			return true
		},
	); !exhausted {
		t.Fatal("enumeration stopped while retaining builders")
	}
	if len(builders) != 2 {
		t.Fatalf("retained builders = %d, want 2", len(builders))
	}
	_, secondMap, secondOK := builders[1]()
	_, firstMap, firstOK := builders[0]()
	if !firstOK || !secondOK {
		t.Fatal("retained builder did not finalize")
	}
	selectSubsumptionPredicateTestMappingTo(
		t,
		firstMap,
		queryOne,
		candidateTrue,
	)
	selectSubsumptionPredicateTestMappingTo(
		t,
		secondMap,
		queryTwo,
		candidateTrue,
	)

	visits := 0
	if exhausted := enumerateSelectSubsumptionPredicateAlternatives(
		original,
		original,
		candidateSelect,
		EmptyAliasMap(),
		func(_ selectSubsumptionPredicateAlternativeBuilder) bool {
			visits++
			return false
		},
	); exhausted {
		t.Fatal("stopped enumeration reported complete exhaustion")
	}
	if visits != 1 {
		t.Fatalf("stopped enumeration visited %d builders, want 1", visits)
	}
}
