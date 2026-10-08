package cascades

import (
	"fmt"
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
	t.Parallel()
	ref := selectSubsumptionPredicateTestRef()
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, ref)
	// Opaque ORs retain distinct identities; value predicates are coalesced by Select.
	makePredicate := func() predicates.QueryPredicate {
		return predicates.NewOr(
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 1),
			selectSubsumptionPredicateTestComparison(candidateAlias, "x", 2),
		)
	}
	queryPredicate := makePredicate()
	candidateOne := makePredicate()
	candidateTwo := makePredicate()
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
	// Java deduplicates semantic MappingKeys before its identity-based
	// coverage gate, so the second opaque candidate remains uncovered.
	if len(alternatives) != 0 {
		t.Fatalf("alternatives = %d, want no match with an uncovered candidate identity", len(alternatives))
	}
	query := []predicates.QueryPredicate{queryPredicate}
	candidates := []predicates.QueryPredicate{candidateOne, candidateTwo}
	mappingOne := RegularMappingBuilder(queryPredicate, queryPredicate, candidateOne).Build()
	mappingTwo := RegularMappingBuilder(queryPredicate, queryPredicate, candidateTwo).Build()
	if _, _, ok := buildSelectSubsumptionPredicateAlternative(query, query, candidates, []*PredicateMapping{mappingOne}); ok {
		t.Fatal("one candidate identity must not cover its semantically equal sibling")
	}
	_, predicateMap, ok := buildSelectSubsumptionPredicateAlternative(query, query, candidates, []*PredicateMapping{mappingOne, mappingTwo})
	if !ok || len(predicateMap.Get(queryPredicate)) != 2 {
		t.Fatal("explicit mappings to both candidate identities must cover both")
	}
	selectSubsumptionPredicateTestMappingTo(t, predicateMap, queryPredicate, candidateOne)
	selectSubsumptionPredicateTestMappingTo(t, predicateMap, queryPredicate, candidateTwo)

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
		candidateSelect.GetPredicates()[0],
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
// pins the simple comparison mappings the group folds. Normalized range
// predicates, including mappings with no scan range, retain their individual
// alternatives and compensation instead of entering that fold.
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

func TestSelectSubsumptionOrTermMappingRetainsResidual(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("candidate")
	placeholder := predicates.NewPlaceholder(values.NamedCorrelationIdentifier("parameter"), selectSubsumptionTestField(alias, "x"))
	candidate := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef())}, []predicates.QueryPredicate{placeholder})
	for _, tc := range []struct {
		name  string
		field string
		want  MappingKind
	}{
		{"matching leaf", "x", MappingOrTermImpliesCandidate},
		{"unrelated leaf", "y", MappingRegularImpliesCandidate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			query := predicates.NewOr(predicates.NewAnd(selectSubsumptionPredicateTestComparison(alias, tc.field, 1), selectSubsumptionPredicateTestComparison(alias, "y", 2)), selectSubsumptionPredicateTestComparison(alias, "y", 3))
			alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, []predicates.QueryPredicate{query}, []predicates.QueryPredicate{query}, candidate, EmptyAliasMap())
			if len(alternatives) != 1 {
				t.Fatalf("alternatives = %d, want 1", len(alternatives))
			}
			alternative := alternatives[0]
			mappings := alternative.predicateMap.Get(query)
			if len(mappings) != 1 {
				t.Fatalf("mappings = %d, want 1", len(mappings))
			}
			mapping := mappings[0]
			if mapping.GetMappingKind() != tc.want {
				t.Fatalf("kind = %v, want %v", mapping.GetMappingKind(), tc.want)
			}
			if !predicates.IsTautology(mapping.GetCandidatePredicate()) || mapping.GetParameterAlias() != nil || mapping.GetComparisonRange() != nil || len(alternative.parameterBindings) != 0 {
				t.Fatal("OR-term hint must not bind or imply the placeholder")
			}
			if mapping.GetOriginalQueryPredicate() != query || mapping.GetTranslatedQueryPredicate() != query {
				t.Fatal("OR identity lost")
			}
			compensation := mapping.GetPredicateCompensation()(nil, nil, nil)
			if !compensation.IsNeeded() || compensation.IsImpossible() {
				t.Fatal("entire OR must remain a possible residual")
			}
		})
	}
}

func TestSelectSubsumptionOrHintsDeduplicateSemanticMappingKeys(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("candidate")
	for _, atomic := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("atomic=%t/reverse=%t", atomic, reverse), func(t *testing.T) {
				t.Parallel()
				placeholders := []predicates.QueryPredicate{
					predicates.NewPlaceholder(values.NamedCorrelationIdentifier("x_parameter"), selectSubsumptionTestField(alias, "x")),
					predicates.NewPlaceholder(values.NamedCorrelationIdentifier("y_parameter"), selectSubsumptionTestField(alias, "y")),
				}
				if reverse {
					placeholders[0], placeholders[1] = placeholders[1], placeholders[0]
				}
				candidate := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{
					expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef()),
				}, placeholders)
				makeOr := func() predicates.QueryPredicate {
					return predicates.WithAtomicity(predicates.NewOr(
						selectSubsumptionPredicateTestComparison(alias, "y", 2),
						predicates.NewComparisonPredicate(selectSubsumptionTestField(alias, "x"), predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(1))),
						predicates.NewComparisonPredicate(selectSubsumptionTestField(alias, "x"), predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(4))),
					), atomic)
				}
				queries := []predicates.QueryPredicate{makeOr(), makeOr()}
				alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, queries, queries, candidate, EmptyAliasMap())
				if len(alternatives) != 1 {
					t.Fatalf("alternatives = %d, want 1", len(alternatives))
				}
				alternative := alternatives[0]
				if len(alternative.parameterBindings) != 0 {
					t.Fatal("OR hints must not produce scan bounds")
				}
				for _, query := range queries {
					mappings := alternative.predicateMap.Get(query)
					if len(mappings) != 1 {
						t.Fatalf("OR mappings = %d, want one semantic mapping key per original predicate", len(mappings))
					}
					mapping := mappings[0]
					if mapping.GetMappingKind() != MappingOrTermImpliesCandidate || mapping.GetOriginalQueryPredicate() != query || !predicates.IsTautology(mapping.GetCandidatePredicate()) {
						t.Fatal("deduplication lost original identity or OR hint kind")
					}
					compensation := mapping.GetPredicateCompensation()(nil, nil, nil)
					if !compensation.IsNeeded() || compensation.IsImpossible() {
						t.Fatal("deduplicated OR hint must retain its residual compensation")
					}
				}
				withTrue := selectSubsumptionPredicateTestSelect(candidate.GetQuantifiers(), append(placeholders, predicates.NewConstantPredicate(predicates.TriTrue)))
				alternatives = collectSelectSubsumptionPredicateTestAlternatives(t, queries[:1], queries[:1], withTrue, EmptyAliasMap())
				if len(alternatives) != 1 {
					t.Fatalf("alternatives with TRUE = %d, want 1", len(alternatives))
				}
				mappings := alternatives[0].predicateMap.Get(queries[0])
				kinds := make(map[MappingKind]bool)
				for _, mapping := range mappings {
					kinds[mapping.GetMappingKind()] = true
				}
				if len(mappings) != 2 || !kinds[MappingRegularImpliesCandidate] || !kinds[MappingOrTermImpliesCandidate] {
					t.Fatal("equal TRUE targets with different mapping kinds must remain distinct")
				}
			})
		}
	}
}

func TestSelectSubsumptionMappingKeysRetainDistinctParameterAliases(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("candidate")
	parameters := []values.CorrelationIdentifier{values.NamedCorrelationIdentifier("first"), values.NamedCorrelationIdentifier("second")}
	var placeholders []predicates.QueryPredicate
	for _, parameter := range parameters {
		placeholders = append(placeholders, predicates.NewPlaceholder(parameter, selectSubsumptionTestField(alias, "x")))
	}
	candidate := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{
		expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef()),
	}, placeholders)
	query := []predicates.QueryPredicate{selectSubsumptionPredicateTestComparison(alias, "x", 42)}
	alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, query, query, candidate, EmptyAliasMap())
	if len(alternatives) != 1 || len(alternatives[0].predicateMap.Get(query[0])) != 2 || len(alternatives[0].parameterBindings) != 2 {
		t.Fatal("distinct placeholder parameters must retain separate mappings and bounds")
	}
	for _, parameter := range parameters {
		if got := selectSubsumptionPredicateTestEqualityLiteral(t, alternatives[0].parameterBindings[parameter]); got != 42 {
			t.Fatalf("bound for %v = %d, want 42", parameter, got)
		}
	}
}

func TestSelectSubsumptionOrTermDoesNotSplitPlaceholderFold(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("candidate")
	parameter := values.NamedCorrelationIdentifier("parameter")
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(alias, "x"))
	candidate := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef())}, []predicates.QueryPredicate{placeholder})
	first := selectSubsumptionPredicateTestComparison(alias, "x", 1)
	second := selectSubsumptionPredicateTestComparison(alias, "x", 1)
	disjunction := predicates.NewOr(selectSubsumptionPredicateTestComparison(alias, "x", 2), selectSubsumptionPredicateTestComparison(alias, "y", 3))
	query := []predicates.QueryPredicate{first, second, disjunction}
	alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, query, query, candidate, EmptyAliasMap())
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want a single folded bound plus OR hint", len(alternatives))
	}
	alternative := alternatives[0]
	if literal := selectSubsumptionPredicateTestEqualityLiteral(t, alternative.parameterBindings[parameter]); literal != 1 {
		t.Fatalf("bound = %d, want 1", literal)
	}
	for _, comparison := range []predicates.QueryPredicate{first, second} {
		selectSubsumptionPredicateTestMappingTo(t, alternative.predicateMap, comparison, placeholder)
	}
	mappings := alternative.predicateMap.Get(disjunction)
	if len(mappings) != 1 || mappings[0].GetMappingKind() != MappingOrTermImpliesCandidate {
		t.Fatalf("OR hint lost: %v", mappings)
	}
}

func selectSubsumptionRangeTestPredicate(value values.Value, ranges ...[]predicates.Comparison) *predicates.PredicateWithValueAndRanges {
	constraints := make([]*predicates.RangeConstraints, len(ranges))
	for i, comparisons := range ranges {
		constraints[i] = predicates.NewRangeConstraints(comparisons, nil)
	}
	return predicates.NewPredicateWithValueAndRanges(value, constraints)
}

func selectSubsumptionRangeTestResidual(t *testing.T, mapping *PredicateMapping, prefix map[values.CorrelationIdentifier]*predicates.ComparisonRange, pullUp *PullUp, realized values.CorrelationIdentifier, want predicates.QueryPredicate) {
	t.Helper()
	compensation := mapping.GetPredicateCompensation()(nil, prefix, pullUp)
	if want == nil {
		if compensation.IsNeeded() || compensation.IsImpossible() {
			t.Fatal("consumed predicate still needs compensation")
		}
		return
	}
	if !compensation.IsNeeded() || compensation.IsImpossible() {
		t.Fatalf("residual compensation needed = %v, impossible = %v", compensation.IsNeeded(), compensation.IsImpossible())
	}
	applied, ok := compensation.ApplyCompensationForPredicate(TranslationMapOfAliases(pullUp.GetRoot().GetCandidateAlias(), realized))
	if !ok || len(applied) != 1 {
		t.Fatalf("applied residuals = %v, ok = %v", applied, ok)
	}
	got, err := predicates.ToResidualPredicate(applied[0])
	if err != nil {
		t.Fatal(err)
	}
	want, err = predicates.ToResidualPredicate(want)
	if err != nil {
		t.Fatal(err)
	}
	if !predicates.SemanticEqualsUnderAliasMap(got, want, nil) {
		t.Fatalf("residual = %s, want %s", got.Explain(), want.Explain())
	}
}

func TestSelectSubsumptionRangeMappingMultiRange(t *testing.T) {
	t.Parallel()
	queryAlias := values.NamedCorrelationIdentifier("range_query")
	candidateAlias := values.NamedCorrelationIdentifier("range_candidate")
	parameter := values.NamedCorrelationIdentifier("range_parameter")
	top := values.NamedCorrelationIdentifier("range_top")
	realized := values.NamedCorrelationIdentifier("range_realized")
	quantifiers := []expressions.Quantifier{expressions.NamedForEachQuantifier(candidateAlias, selectSubsumptionPredicateTestRef())}
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(candidateAlias, "x"))
	candidate := selectSubsumptionPredicateTestSelect(quantifiers, []predicates.QueryPredicate{placeholder})
	first := []predicates.Comparison{predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))}
	second := []predicates.Comparison{predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(3))}
	original := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(queryAlias, "x"), first, second)
	translated := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(candidateAlias, "x"), first, second)
	pullUp := ForUnification(top, selectSubsumptionTestQOV(candidateAlias, selectSubsumptionTestRowType()), map[values.CorrelationIdentifier]struct{}{candidateAlias: {}})
	alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, []predicates.QueryPredicate{original}, []predicates.QueryPredicate{translated}, candidate, AliasMapOfAliases(queryAlias, candidateAlias))
	if len(alternatives) != 1 {
		t.Fatalf("alternatives = %d, want 1", len(alternatives))
	}
	mapping := selectSubsumptionPredicateTestMappingTo(t, alternatives[0].predicateMap, original, placeholder)
	if mapping.GetOriginalQueryPredicate() != original || mapping.GetTranslatedQueryPredicate() != translated || mapping.GetMappingKind() != MappingRegularImpliesCandidate {
		t.Fatal("multi-range mapping lost predicate identities or kind")
	}
	if mapping.GetParameterAlias() == nil || *mapping.GetParameterAlias() != parameter || mapping.GetComparisonRange() != nil || len(alternatives[0].parameterBindings) != 0 {
		t.Fatal("multi-range mapping must retain its parameter alias without binding a scan range")
	}
	want := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "x"), first, second)
	selectSubsumptionRangeTestResidual(t, mapping, nil, pullUp, realized, want)
	// A different mapping may bind this parameter; that cannot consume this OR.
	bound := predicates.MergeAll([]*predicates.Comparison{&first[0]}).Range
	selectSubsumptionRangeTestResidual(t, mapping, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: bound}, pullUp, realized, want)

	for _, field := range []string{"y", "x"} {
		alias := candidateAlias
		if field == "x" {
			alias = values.NamedCorrelationIdentifier("other_leg")
		}
		wrong := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(alias, field), first, second)
		if _, ok := selectSubsumptionPredicateImpliedMappingMaybe(wrong, wrong, placeholder, quantifiers, EmptyAliasMap()); ok {
			t.Fatalf("nonmatching value %s mapped to placeholder", wrong.GetValue())
		}
	}
}

func TestSelectSubsumptionRangeMappingPartialResidual(t *testing.T) {
	t.Parallel()
	queryAlias := values.NamedCorrelationIdentifier("partial_query")
	candidateAlias := values.NamedCorrelationIdentifier("partial_candidate")
	parameter := values.NamedCorrelationIdentifier("partial_parameter")
	top := values.NamedCorrelationIdentifier("partial_top")
	realized := values.NamedCorrelationIdentifier("partial_realized")
	quantifiers := []expressions.Quantifier{expressions.NamedForEachQuantifier(candidateAlias, selectSubsumptionPredicateTestRef())}
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(candidateAlias, "x"))
	gt := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1))
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	lt := predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(9))
	otherEq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(6))
	pullUp := ForUnification(top, selectSubsumptionTestQOV(candidateAlias, selectSubsumptionTestRowType()), map[values.CorrelationIdentifier]struct{}{candidateAlias: {}})
	for _, tc := range []struct {
		name        string
		comparisons []predicates.Comparison
		residual    []predicates.Comparison
	}{
		{"equality displaces inequalities", []predicates.Comparison{gt, eq, lt}, []predicates.Comparison{gt, lt}},
		{"equality precedes inequalities", []predicates.Comparison{eq, gt, lt}, []predicates.Comparison{gt, lt}},
		{"second equality remains", []predicates.Comparison{eq, otherEq}, []predicates.Comparison{otherEq}},
		{"all inequalities consumed", []predicates.Comparison{gt, lt}, nil},
		{"duplicate equality consumed", []predicates.Comparison{eq, eq}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(queryAlias, "x"), tc.comparisons)
			translated := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(candidateAlias, "x"), tc.comparisons)
			mapping, ok := selectSubsumptionPredicateImpliedMappingMaybe(original, translated, placeholder, quantifiers, AliasMapOfAliases(queryAlias, candidateAlias))
			if !ok || mapping.GetComparisonRange() == nil {
				t.Fatal("range predicate did not bind")
			}
			if mapping.GetOriginalQueryPredicate() != original || mapping.GetTranslatedQueryPredicate() != translated || mapping.GetCandidatePredicate() != placeholder {
				t.Fatal("range mapping lost predicate identities")
			}
			var want predicates.QueryPredicate
			if len(tc.residual) > 0 {
				want = selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "x"), tc.residual)
			}
			selectSubsumptionRangeTestResidual(t, mapping, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: mapping.GetComparisonRange()}, pullUp, realized, want)
			selectSubsumptionRangeTestResidual(t, mapping, nil, pullUp, realized, selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "x"), tc.comparisons))
		})
	}
}

func TestSelectSubsumptionRangeMappingValueTranslation(t *testing.T) {
	t.Parallel()
	queryAlias := values.NamedCorrelationIdentifier("value_query")
	candidateAlias := values.NamedCorrelationIdentifier("value_candidate")
	parameter := values.NamedCorrelationIdentifier("value_parameter")
	top := values.NamedCorrelationIdentifier("value_top")
	realized := values.NamedCorrelationIdentifier("value_realized")
	// A child projection swaps x and y. Rebasing the original's alias cannot
	// express this translation, including the same-row comparison's RHS.
	fields := make([]values.RecordConstructorField, 0)
	for _, field := range selectSubsumptionTestRowType().Fields {
		name := field.Name
		if name == "x" {
			name = "y"
		} else if name == "y" {
			name = "x"
		}
		fields = append(fields, values.RecordConstructorField{Name: field.Name, Value: selectSubsumptionTestField(candidateAlias, name)})
	}
	translation := values.NewTranslationMapBuilder().When(queryAlias).Then(func(values.CorrelationIdentifier, values.Value) values.Value {
		return values.NewRecordConstructorValue(fields...)
	}).Build()
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	rowComparison := predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: selectSubsumptionTestField(queryAlias, "y")}
	original := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(queryAlias, "x"), []predicates.Comparison{eq, rowComparison})
	translated, err := predicates.TranslateLeafPredicatesChecked(original, translation)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(candidateAlias, "y"))
	quantifiers := []expressions.Quantifier{expressions.NamedForEachQuantifier(candidateAlias, selectSubsumptionPredicateTestRef())}
	mapping, ok := selectSubsumptionPredicateImpliedMappingMaybe(original, translated, placeholder, quantifiers, AliasMapOfAliases(queryAlias, candidateAlias))
	if !ok || mapping.GetComparisonRange() == nil {
		t.Fatal("translated range did not bind")
	}
	if mapping.GetOriginalQueryPredicate() != original || mapping.GetTranslatedQueryPredicate() != translated {
		t.Fatal("Value translation lost mapping identities")
	}
	if len(mapping.GetComparisonRange().GetComparisons()) != 1 {
		t.Fatal("same-row comparison entered the scan range")
	}
	pullUp := ForUnification(top, selectSubsumptionTestQOV(candidateAlias, selectSubsumptionTestRowType()), map[values.CorrelationIdentifier]struct{}{candidateAlias: {}})
	wantComparison := predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: selectSubsumptionTestField(realized, "x")}
	selectSubsumptionRangeTestResidual(t, mapping, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: mapping.GetComparisonRange()}, pullUp, realized, selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "y"), []predicates.Comparison{wantComparison}))
	selectSubsumptionRangeTestResidual(t, mapping, nil, pullUp, realized, selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "y"), []predicates.Comparison{eq, wantComparison}))
	// A candidate projecting away x cannot express the bound residual y < x.
	unpullable := ForUnification(top, selectSubsumptionTestField(candidateAlias, "y"), map[values.CorrelationIdentifier]struct{}{candidateAlias: {}})
	if !mapping.GetPredicateCompensation()(nil, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: mapping.GetComparisonRange()}, unpullable).IsImpossible() {
		t.Fatal("unavailable residual value did not fail closed")
	}
}

func TestSelectSubsumptionRangeMappingGroupRetainsResidualOnlyMapping(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("group_candidate")
	parameter := values.NamedCorrelationIdentifier("group_parameter")
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(alias, "x"))
	candidate := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef())}, []predicates.QueryPredicate{placeholder})
	first := selectSubsumptionPredicateTestComparison(alias, "x", 1)
	second := selectSubsumptionPredicateTestComparison(alias, "x", 3)
	multi := selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(alias, "x"), []predicates.Comparison{first.Comparison}, []predicates.Comparison{second.Comparison})
	for _, query := range [][]predicates.QueryPredicate{{multi, first, second}, {first, second, multi}} {
		alternatives := collectSelectSubsumptionPredicateTestAlternatives(t, query, query, candidate, EmptyAliasMap())
		if len(alternatives) != 3 {
			t.Fatalf("alternatives = %d, want one per mapping including residual-only", len(alternatives))
		}
		found := false
		for _, alternative := range alternatives {
			if alternative.predicateMap.PredicateCount() != 3 {
				t.Fatal("group completion lost a predicate")
			}
			for _, mapping := range alternative.predicateMap.Get(multi) {
				if mapping.GetCandidatePredicate() == placeholder {
					found = true
					if mapping.GetParameterAlias() == nil || mapping.GetComparisonRange() != nil || len(alternative.parameterBindings) != 0 {
						t.Fatal("residual-only group member became a scan bound")
					}
				}
			}
		}
		if !found {
			t.Fatal("candidate-group folding erased residual-only mapping")
		}
	}
}

func TestSelectSubsumptionRangeMappingBindingGuards(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("guard_candidate")
	other := values.NamedCorrelationIdentifier("guard_other")
	parameter := values.NamedCorrelationIdentifier("guard_parameter")
	ref := selectSubsumptionPredicateTestRef()
	quantifier := expressions.NamedForEachQuantifier(alias, ref)
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(alias, "x"))
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	for _, tc := range []struct {
		name        string
		value       values.Value
		comparison  predicates.Comparison
		quantifiers []expressions.Quantifier
		multi       bool
	}{
		{"wrong value", selectSubsumptionTestField(alias, "y"), eq, []expressions.Quantifier{quantifier}, true},
		{"wrong source", selectSubsumptionTestField(other, "x"), eq, []expressions.Quantifier{quantifier}, true},
		{"existential source", selectSubsumptionTestField(alias, "x"), eq, []expressions.Quantifier{expressions.NamedExistentialQuantifier(alias, ref)}, true},
		{"duplicate source", selectSubsumptionTestField(alias, "x"), eq, []expressions.Quantifier{quantifier, quantifier}, true},
		{"incompatible comparand", selectSubsumptionTestField(alias, "x"), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: "string", Typ: values.NotNullString}}, []expressions.Quantifier{quantifier}, false},
		{"same-row comparand", selectSubsumptionTestField(alias, "x"), predicates.Comparison{Type: predicates.ComparisonEquals, Operand: selectSubsumptionTestField(alias, "y")}, []expressions.Quantifier{quantifier}, false},
		{"noncommutable orientation", &values.ConstantValue{Value: "prefix"}, predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: selectSubsumptionTestField(alias, "x")}, []expressions.Quantifier{quantifier}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ranges := [][]predicates.Comparison{{tc.comparison}}
			if tc.multi {
				ranges = append(ranges, []predicates.Comparison{eq})
			}
			query := selectSubsumptionRangeTestPredicate(tc.value, ranges...)
			if _, ok := selectSubsumptionPredicateImpliedMappingMaybe(query, query, placeholder, tc.quantifiers, EmptyAliasMap()); ok {
				t.Fatal("unsafe range mapping accepted")
			}
		})
	}
}

func TestSelectSubsumptionRangeMappingCommutedResidual(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("commuted_candidate")
	parameter := values.NamedCorrelationIdentifier("commuted_parameter")
	top := values.NamedCorrelationIdentifier("commuted_top")
	realized := values.NamedCorrelationIdentifier("commuted_realized")
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(alias, "x"))
	quantifiers := []expressions.Quantifier{expressions.NamedForEachQuantifier(alias, selectSubsumptionPredicateTestRef())}
	gt := predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: selectSubsumptionTestField(alias, "x")}
	eq := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: selectSubsumptionTestField(alias, "x")}
	query := selectSubsumptionRangeTestPredicate(&values.ConstantValue{Value: int64(5)}, []predicates.Comparison{gt, eq})
	mapping, ok := selectSubsumptionPredicateImpliedMappingMaybe(query, query, placeholder, quantifiers, EmptyAliasMap())
	if !ok || selectSubsumptionPredicateTestEqualityLiteral(t, mapping.GetComparisonRange()) != 5 {
		t.Fatal("commuted equality did not bind")
	}
	pullUp := ForUnification(top, selectSubsumptionTestQOV(alias, selectSubsumptionTestRowType()), map[values.CorrelationIdentifier]struct{}{alias: {}})
	want := predicates.NewComparisonPredicate(&values.ConstantValue{Value: int64(5)}, predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: selectSubsumptionTestField(realized, "x")})
	selectSubsumptionRangeTestResidual(t, mapping, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: mapping.GetComparisonRange()}, pullUp, realized, want)
}

func TestSelectSubsumptionRangeMappingTranslatedComparisonReclassification(t *testing.T) {
	t.Parallel()
	queryAlias := values.NamedCorrelationIdentifier("reclassify_query")
	candidateAlias := values.NamedCorrelationIdentifier("reclassify_candidate")
	outer := values.NamedCorrelationIdentifier("reclassify_outer")
	parameter := values.NamedCorrelationIdentifier("reclassify_parameter")
	top := values.NamedCorrelationIdentifier("reclassify_top")
	realized := values.NamedCorrelationIdentifier("reclassify_realized")
	ref := selectSubsumptionPredicateTestRef()
	placeholder := predicates.NewPlaceholder(parameter, selectSubsumptionTestField(candidateAlias, "x"))
	quantifiers := []expressions.Quantifier{expressions.NamedForEachQuantifier(candidateAlias, ref)}
	pullUp := ForUnification(top, selectSubsumptionTestQOV(candidateAlias, selectSubsumptionTestRowType()), map[values.CorrelationIdentifier]struct{}{candidateAlias: {}})
	for _, tc := range []struct {
		name    string
		literal int64
	}{{"reordered", 5}, {"deduplicated", 9}} {
		t.Run(tc.name, func(t *testing.T) {
			literalComparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: tc.literal, Typ: values.NotNullLong}}
			deferredComparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: selectSubsumptionTestQOV(outer, values.NotNullLong)}
			query := selectSubsumptionPredicateTestSelect([]expressions.Quantifier{expressions.NamedForEachQuantifier(queryAlias, ref)}, []predicates.QueryPredicate{
				predicates.NewComparisonPredicate(selectSubsumptionTestField(queryAlias, "x"), deferredComparison),
				predicates.NewComparisonPredicate(selectSubsumptionTestField(queryAlias, "x"), literalComparison),
			})
			if len(query.GetPredicates()) != 1 {
				t.Fatal("Select did not normalize same-value comparisons")
			}
			original, ok := query.GetPredicates()[0].(*predicates.PredicateWithValueAndRanges)
			if !ok || len(original.GetRanges()[0].GetDeferredRanges()) != 1 {
				t.Fatal("expected one normalized range with a deferred comparison")
			}
			translatedComparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(9), Typ: values.NotNullLong}}
			translation := values.NewTranslationMapBuilder().When(queryAlias).Then(func(values.CorrelationIdentifier, values.Value) values.Value {
				return selectSubsumptionTestQOV(candidateAlias, selectSubsumptionTestRowType())
			}).When(outer).Then(func(values.CorrelationIdentifier, values.Value) values.Value {
				return translatedComparison.Operand
			}).Build()
			translated, err := predicates.TranslateLeafPredicatesChecked(original, translation)
			if err != nil {
				t.Fatal(err)
			}
			mapping, ok := selectSubsumptionPredicateImpliedMappingMaybe(original, translated, placeholder, quantifiers, AliasMapOfAliases(queryAlias, candidateAlias))
			if !ok {
				t.Fatal("normalized translated range did not map")
			}
			if got := selectSubsumptionPredicateTestEqualityLiteral(t, mapping.GetComparisonRange()); got != tc.literal {
				t.Fatalf("bound = %d, want %d after comparison reclassification", got, tc.literal)
			}
			var want predicates.QueryPredicate
			if tc.literal != 9 {
				want = predicates.NewComparisonPredicate(selectSubsumptionTestField(realized, "x"), translatedComparison)
			}
			selectSubsumptionRangeTestResidual(t, mapping, map[values.CorrelationIdentifier]*predicates.ComparisonRange{parameter: mapping.GetComparisonRange()}, pullUp, realized, want)
			full := []predicates.Comparison{literalComparison}
			if tc.literal != 9 {
				full = append(full, translatedComparison)
			}
			selectSubsumptionRangeTestResidual(t, mapping, nil, pullUp, realized, selectSubsumptionRangeTestPredicate(selectSubsumptionTestField(realized, "x"), full))
		})
	}
}
