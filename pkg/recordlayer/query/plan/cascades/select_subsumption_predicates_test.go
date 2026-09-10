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
	return selectSubsumptionTestInitial(selectSubsumptionTestScan("T"))
}

func selectSubsumptionPredicateTestSelect(
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return selectSubsumptionMust(expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		quantifiers,
		queryPredicates,
	))
}

func selectSubsumptionPredicateTestComparison(
	alias values.CorrelationIdentifier,
	field string,
	literal int64,
) *predicates.ComparisonPredicate {
	return predicates.NewComparisonPredicate(
		selectSubsumptionTestField(alias, field),
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

// TestSelectSubsumptionPredicateAlternativesSharedPlaceholderFolds pins that
// every query comparison binding one placeholder folds into ONE alternative
// (Java's single sargable per column): two different equalities produce one
// product whose range is the FIRST equality, the second re-applied as a fresh
// residual; two inequalities produce one product whose range carries BOTH,
// each query predicate mapped to the placeholder over that same range.
func TestSelectSubsumptionPredicateAlternativesSharedPlaceholderFolds(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(
		parameterAlias,
		selectSubsumptionTestField(candidateAlias, "x"),
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholder},
	)

	t.Run("two equalities: the first binds, the second is a residual", func(t *testing.T) {
		queryOne := selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1)
		queryTwo := selectSubsumptionPredicateTestComparison(candidateAlias, "x", 2)
		alternatives := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			[]predicates.QueryPredicate{queryOne, queryTwo},
			[]predicates.QueryPredicate{queryOne, queryTwo},
			candidateSelect,
			EmptyAliasMap(),
		)
		if len(alternatives) != 1 {
			t.Fatalf("alternatives = %d, want 1 (one fold per placeholder)", len(alternatives))
		}
		alternative := alternatives[0]
		if got := selectSubsumptionPredicateTestEqualityLiteral(
			t, alternative.parameterBindings[parameterAlias],
		); got != 1 {
			t.Fatalf("bound literal = %d, want the first equality", got)
		}
		sargableMapping := selectSubsumptionPredicateTestMappingTo(
			t, alternative.predicateMap, queryOne, placeholder,
		)
		compensation := sargableMapping.GetPredicateCompensation()
		if compensation(
			nil,
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameterAlias: alternative.parameterBindings[parameterAlias],
			},
			nil,
		).IsNeeded() {
			t.Fatal("sargable mapping compensated despite a bound prefix")
		}
		if !compensation(nil, nil, nil).IsNeeded() {
			t.Fatal("sargable mapping did not reapply without a bound prefix")
		}
		residualMappings := alternative.predicateMap.Get(queryTwo)
		if len(residualMappings) != 1 ||
			!predicates.IsTautology(residualMappings[0].GetCandidatePredicate()) ||
			residualMappings[0].GetCandidatePredicate() == placeholder {
			t.Fatal("the second equality must be a fresh residual, not a placeholder binding")
		}
	})

	t.Run("two inequalities: one range carries both", func(t *testing.T) {
		gt := predicates.NewComparisonPredicate(
			selectSubsumptionTestField(candidateAlias, "x"),
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1)),
		)
		lt := predicates.NewComparisonPredicate(
			selectSubsumptionTestField(candidateAlias, "x"),
			predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(9)),
		)
		alternatives := collectSelectSubsumptionPredicateTestAlternatives(
			t,
			[]predicates.QueryPredicate{gt, lt},
			[]predicates.QueryPredicate{gt, lt},
			candidateSelect,
			EmptyAliasMap(),
		)
		if len(alternatives) != 1 {
			t.Fatalf("alternatives = %d, want 1", len(alternatives))
		}
		alternative := alternatives[0]
		bound := alternative.parameterBindings[parameterAlias]
		if bound == nil || !bound.IsInequality() {
			t.Fatalf("binding = %#v, want an inequality range", bound)
		}
		comparisons := bound.GetInequalityComparisons()
		if len(comparisons) != 2 ||
			comparisons[0].Type != predicates.ComparisonGreaterThan ||
			comparisons[1].Type != predicates.ComparisonLessThan {
			t.Fatalf("range comparisons = %v, want [> 1, < 9]", comparisons)
		}
		gtMapping := selectSubsumptionPredicateTestMappingTo(t, alternative.predicateMap, gt, placeholder)
		ltMapping := selectSubsumptionPredicateTestMappingTo(t, alternative.predicateMap, lt, placeholder)
		if gtMapping.GetComparisonRange() != bound || ltMapping.GetComparisonRange() != bound {
			t.Fatal("both members must carry the SAME merged range object — that is what makes them one fold group")
		}
		if alternative.predicateMap.Size() != 2 {
			t.Fatalf("predicate map has %d mappings, want exactly the two members", alternative.predicateMap.Size())
		}
	})
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
		selectSubsumptionTestField(candidateAlias, "x"),
	)
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholder},
	)
	queryPredicate := predicates.NewComparisonPredicate(
		selectSubsumptionTestField(candidateAlias, "x"),
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
		selectSubsumptionTestField(candidateAlias, "x"),
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

func TestSelectSubsumptionPredicateAlternativesRejectMalformedValuesAtAdmission(
	t *testing.T,
) {
	t.Parallel()

	request, err := values.FieldByName("nested_bad")
	if err != nil {
		t.Fatalf("FieldByName() unexpected error: %v", err)
	}
	if malformed, err := values.ResolveFieldAccess(
		nil,
		[]values.FieldRequest{request},
	); err == nil || malformed != nil {
		t.Fatalf(
			"ResolveFieldAccess(nil) = (%v, %v), want (nil, error)",
			malformed,
			err,
		)
	}
	alias := values.NamedCorrelationIdentifier("candidate_nested_bad")
	if malformed, err := values.NewQuantifiedObjectValue(alias, nil); err == nil || malformed != nil {
		t.Fatalf(
			"NewQuantifiedObjectValue(nil type) = (%v, %v), want (nil, error)",
			malformed,
			err,
		)
	}
}

func TestSelectSubsumptionPredicateAlternativesMergeParameterBindingsChecked(
	t *testing.T,
) {
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	parameterAlias := values.NamedCorrelationIdentifier("shared_parameter")
	// Two placeholders over DIFFERENT columns that share one parameter alias:
	// each is its own candidate group, so the product binds the alias twice,
	// and the checked parameter merge decides whether the two bindings agree.
	placeholderX := predicates.NewPlaceholder(parameterAlias, selectSubsumptionTestField(candidateAlias, "x"))
	placeholderY := predicates.NewPlaceholder(parameterAlias, selectSubsumptionTestField(candidateAlias, "y"))
	candidateSelect := selectSubsumptionPredicateTestSelect(
		[]expressions.Quantifier{candidateQuantifier},
		[]predicates.QueryPredicate{placeholderX, placeholderY},
	)

	agreeing := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1),
			selectSubsumptionPredicateTestComparison(candidateAlias, "y", 1),
		},
		[]predicates.QueryPredicate{
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1),
			selectSubsumptionPredicateTestComparison(candidateAlias, "y", 1),
		},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(agreeing) != 1 {
		t.Fatalf("agreeing bindings retained %d alternatives, want 1", len(agreeing))
	}
	if got := selectSubsumptionPredicateTestEqualityLiteral(
		t, agreeing[0].parameterBindings[parameterAlias],
	); got != 1 {
		t.Fatalf("agreeing literal = %d, want 1", got)
	}

	conflicting := collectSelectSubsumptionPredicateTestAlternatives(
		t,
		[]predicates.QueryPredicate{
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1),
			selectSubsumptionPredicateTestComparison(candidateAlias, "y", 2),
		},
		[]predicates.QueryPredicate{
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1),
			selectSubsumptionPredicateTestComparison(candidateAlias, "y", 2),
		},
		candidateSelect,
		EmptyAliasMap(),
	)
	if len(conflicting) != 0 {
		t.Fatalf("conflicting bindings of one alias retained %d alternatives, want 0", len(conflicting))
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
		selectSubsumptionTestField(candidateAliasA, "x"),
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
	localAndOuterValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name: "local",
			Value: selectSubsumptionTestQOV(
				candidateAliasA,
				selectSubsumptionTestRowType(),
			),
		},
		values.RecordConstructorField{
			Name: "outer",
			Value: selectSubsumptionTestQOV(
				outerAlias,
				selectSubsumptionTestRowType(),
			),
		},
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

// TestSelectSubsumptionGroupAlternatives_FoldsOnlyWellFormedPlaceholderMappings
// pins the shape selectSubsumptionGroupAlternatives folds and the one it
// refuses to. Every placeholder mapping the implication builder produces
// carries exactly ONE bound comparison, and a group of those folds into a
// single alternative over the merged range. A mapping shaped otherwise — a
// range holding two comparisons, or a non-comparison translated predicate —
// cannot be folded without silently dropping what the fold does not read, so
// the group falls back to one mapping per alternative, the cross product as
// it always was. That fallback is unreachable from the builder today; this
// pins that it stays a fallback and not a drop.
func TestSelectSubsumptionGroupAlternatives_FoldsOnlyWellFormedPlaceholderMappings(t *testing.T) {
	t.Parallel()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	parameterAlias := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(parameterAlias, selectSubsumptionTestField(candidateAlias, "x"))
	gt := predicates.NewComparisonPredicate(
		selectSubsumptionTestField(candidateAlias, "x"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1)),
	)
	lt := predicates.NewComparisonPredicate(
		selectSubsumptionTestField(candidateAlias, "x"),
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(9)),
	)
	oneComparison := func(cp *predicates.ComparisonPredicate) *predicates.ComparisonRange {
		return predicates.EmptyComparisonRange().Merge(&cp.Comparison).Range
	}
	wellFormed := []*PredicateMapping{
		selectSubsumptionSargableMapping(gt, gt, placeholder, oneComparison(gt)),
		selectSubsumptionSargableMapping(lt, lt, placeholder, oneComparison(lt)),
	}
	alternatives := selectSubsumptionGroupAlternatives(placeholder, wellFormed)
	if len(alternatives) != 1 || len(alternatives[0]) != 2 {
		t.Fatalf("two one-comparison mappings must fold into ONE alternative of two members, got %d alternatives", len(alternatives))
	}
	if alternatives[0][0].GetComparisonRange() != alternatives[0][1].GetComparisonRange() ||
		len(alternatives[0][0].GetComparisonRange().GetComparisons()) != 2 {
		t.Fatal("the fold's members must share one merged range carrying both comparisons")
	}

	twoComparisons := predicates.MergeAll([]*predicates.Comparison{&gt.Comparison, &lt.Comparison}).Range
	malformed := []*PredicateMapping{
		selectSubsumptionSargableMapping(gt, gt, placeholder, twoComparisons),
		selectSubsumptionSargableMapping(lt, lt, placeholder, oneComparison(lt)),
	}
	alternatives = selectSubsumptionGroupAlternatives(placeholder, malformed)
	if len(alternatives) != 2 || len(alternatives[0]) != 1 || len(alternatives[1]) != 1 {
		t.Fatalf("a mapping over a two-comparison range must not be folded; want one mapping per alternative, got %v", alternatives)
	}
	if alternatives[0][0] != malformed[0] || alternatives[1][0] != malformed[1] {
		t.Fatal("the fallback must hand back the mappings as given, untouched")
	}

	// A non-placeholder candidate is never folded: one mapping per alternative.
	tautology := predicates.NewConstantPredicate(predicates.TriTrue)
	residuals := []*PredicateMapping{
		RegularMappingBuilder(gt, gt, tautology).Build(),
		RegularMappingBuilder(lt, lt, tautology).Build(),
	}
	alternatives = selectSubsumptionGroupAlternatives(tautology, residuals)
	if len(alternatives) != 2 || len(alternatives[0]) != 1 || len(alternatives[1]) != 1 ||
		alternatives[0][0] != residuals[0] || alternatives[1][0] != residuals[1] {
		t.Fatalf("non-placeholder group: want the two mappings back one per alternative, in order, got %v", alternatives)
	}
}
