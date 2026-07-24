package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type partialMatchIdentityFixture struct {
	candidate    MatchCandidate
	queryExpr    expressions.RelationalExpression
	queryRef     *expressions.Reference
	candidateRef *expressions.Reference
}

func newPartialMatchIdentityFixture(name string) partialMatchIdentityFixture {
	queryExpr := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	candidateExpr := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	return partialMatchIdentityFixture{
		candidate:    stubMatchCandidate{name: name},
		queryExpr:    queryExpr,
		queryRef:     expressions.InitialOf(queryExpr),
		candidateRef: expressions.InitialOf(candidateExpr),
	}
}

func (f partialMatchIdentityFixture) partialMatch(
	matchInfo MatchInfo,
) *PartialMatchImpl {
	return NewPartialMatch(
		EmptyAliasMap(),
		f.candidate,
		f.queryRef,
		f.queryExpr,
		f.candidateRef,
		matchInfo,
	)
}

func identityTestRegularInfo(
	maxMatchMap *MaxMatchMap,
	groupByMappings *GroupByMappings,
	predicateMap *PredicateMultiMap,
) *RegularMatchInfo {
	if groupByMappings == nil {
		groupByMappings = EmptyGroupByMappings()
	}
	return NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		predicateMap,
		nil,
		maxMatchMap,
		groupByMappings,
		nil,
		nil,
	)
}

func identityTestComparisonRange(
	t *testing.T,
	comparisons ...predicates.Comparison,
) *predicates.ComparisonRange {
	t.Helper()
	result := predicates.EmptyComparisonRange()
	for i := range comparisons {
		merged := result.Merge(&comparisons[i])
		if !merged.Ok {
			t.Fatalf("comparison %d did not form a range", i)
		}
		result = merged.Range
	}
	return result
}

func TestPartialMatchSemanticIdentity_AdjustedChainIsLayerSensitive(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("adjusted_identity")
	newMMM := func(literal int64) *MaxMatchMap {
		value := values.LiteralValue(literal)
		return NewMaxMatchMap(
			map[values.Value]values.Value{value: value},
			value,
			value,
		)
	}
	newChain := func(innerLiteral int64) MatchInfo {
		regular := identityTestRegularInfo(
			newMMM(0),
			EmptyGroupByMappings(),
			nil,
		)
		inner := NewAdjustedMatchInfo(
			regular,
			nil,
			newMMM(innerLiteral),
			EmptyGroupByMappings(),
		)
		return NewAdjustedMatchInfo(
			inner,
			nil,
			newMMM(99),
			EmptyGroupByMappings(),
		)
	}

	first := fixture.partialMatch(newChain(1))
	differentInnerLayer := fixture.partialMatch(newChain(2))
	exactReconstruction := fixture.partialMatch(newChain(1))

	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first adjusted match rejected")
	}
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		differentInnerLayer,
	) {
		t.Fatal("distinct intermediate AdjustedMatchInfo layer was collapsed")
	}
	if AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		exactReconstruction,
	) {
		t.Fatal("semantically reconstructed adjusted chain was not deduplicated")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 2 {
		t.Fatalf("stored adjusted alternatives = %d, want 2", got)
	}
}

func TestPartialMatchSemanticIdentity_AdjustedOrderingIgnoresFreshParameterIDs(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("adjusted_ordering_parameter_id")
	firstParameterID := values.UniqueCorrelationIdentifier()
	reconstructedParameterID := values.UniqueCorrelationIdentifier()
	if firstParameterID == reconstructedParameterID {
		t.Fatal("test requires distinct synthesized parameter identifiers")
	}

	newAdjustedMatch := func(
		parameterID values.CorrelationIdentifier,
	) *PartialMatchImpl {
		orderingValue := values.NewFieldValue(nil, "ID", values.UnknownType)
		orderingRange := identityTestComparisonRange(
			t,
			predicates.NewLiteralComparison(
				predicates.ComparisonGreaterThan,
				int64(5),
			),
		)
		matchInfo := NewAdjustedMatchInfo(
			identityTestRegularInfo(nil, EmptyGroupByMappings(), nil),
			[]*MatchedOrderingPart{NewMatchedOrderingPart(
				parameterID,
				orderingValue,
				orderingRange,
				MatchedSortOrderAscending,
			)},
			NewMaxMatchMap(nil, nil, nil),
			EmptyGroupByMappings(),
		)
		return fixture.partialMatch(matchInfo)
	}

	first := newAdjustedMatch(firstParameterID)
	reconstructed := newAdjustedMatch(reconstructedParameterID)
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		first,
	) {
		t.Fatal("first adjusted ordering match rejected")
	}
	if AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		reconstructed,
	) {
		t.Fatal("provenance-only ordering parameter ID changed semantic identity")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 1 {
		t.Fatalf("stored adjusted ordering alternatives = %d, want 1", got)
	}
}

func TestPartialMatchSemanticIdentity_GroupMapOrderIsIrrelevant(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("group_map_order")
	buildGroups := func(reverse bool) *GroupByMappings {
		matched := NewValueBiMap()
		entries := [][2]values.Value{
			{values.LiteralValue(int64(1)), values.LiteralValue(int64(11))},
			{values.LiteralValue(int64(2)), values.LiteralValue(int64(22))},
		}
		if reverse {
			entries[0], entries[1] = entries[1], entries[0]
		}
		for _, entry := range entries {
			matched.Put(entry[0], entry[1])
		}
		return NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		)
	}

	first := fixture.partialMatch(identityTestRegularInfo(
		NewMaxMatchMap(nil, nil, nil),
		buildGroups(false),
		nil,
	))
	reordered := fixture.partialMatch(identityTestRegularInfo(
		NewMaxMatchMap(nil, nil, nil),
		buildGroups(true),
		nil,
	))
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first group-map match rejected")
	}
	if AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, reordered) {
		t.Fatal("map insertion order changed semantic PartialMatch identity")
	}
}

func TestPartialMatchSemanticIdentity_OpaqueCompensationFailsOpen(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("opaque_compensation")
	queryPredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	candidatePredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	newPredicateMap := func(marker int) *PredicateMultiMap {
		mapping := RegularMappingBuilder(
			queryPredicate,
			queryPredicate,
			candidatePredicate,
		).SetPredicateCompensation(
			func(
				PartialMatch,
				map[values.CorrelationIdentifier]*predicates.ComparisonRange,
				*PullUp,
			) PredicateCompensationFunc {
				if marker == 0 {
					return NoPredicateCompensationNeeded()
				}
				return ImpossiblePredicateCompensation()
			},
		).Build()
		builder := NewPredicateMultiMapBuilder()
		builder.Put(queryPredicate, mapping)
		return builder.Build()
	}

	first := fixture.partialMatch(identityTestRegularInfo(
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		newPredicateMap(0),
	))
	second := fixture.partialMatch(identityTestRegularInfo(
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		newPredicateMap(1),
	))
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first opaque-compensation match rejected")
	}
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, second) {
		t.Fatal("opaque compensation closures were unsafely conflated")
	}
}

func TestPartialMatchSemanticIdentity_InequalityOrderIsIrrelevant(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("inequality_order")
	parameterAlias := values.NamedCorrelationIdentifier("p")
	greaterFive := predicates.NewLiteralComparison(
		predicates.ComparisonGreaterThan,
		int64(5),
	)
	lessTen := predicates.NewLiteralComparison(
		predicates.ComparisonLessThan,
		int64(10),
	)
	newMatch := func(comparisons ...predicates.Comparison) *PartialMatchImpl {
		matchInfo := NewRegularMatchInfo(
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameterAlias: identityTestComparisonRange(t, comparisons...),
			},
			EmptyAliasMap(),
			nil,
			nil,
			nil,
			EmptyGroupByMappings(),
			nil,
			nil,
		)
		return fixture.partialMatch(matchInfo)
	}

	first := newMatch(greaterFive, lessTen)
	reversed := newMatch(lessTen, greaterFive)
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first inequality match rejected")
	}
	if AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, reversed) {
		t.Fatal("inequality insertion order changed semantic PartialMatch identity")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 1 {
		t.Fatalf("stored inequality matches = %d, want 1", got)
	}
}

func TestPartialMatchSemanticIdentity_PredicateMapOrderIsIrrelevant(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("predicate_map_order")
	newPredicateMap := func(reverse bool) *PredicateMultiMap {
		entries := []predicates.TriBool{predicates.TriTrue, predicates.TriFalse}
		if reverse {
			entries[0], entries[1] = entries[1], entries[0]
		}
		builder := NewPredicateMultiMapBuilder()
		for _, value := range entries {
			original := predicates.NewConstantPredicate(value)
			translated := predicates.NewConstantPredicate(value)
			candidate := predicates.NewConstantPredicate(value)
			mapping := RegularMappingBuilder(
				original,
				translated,
				candidate,
			).Build()
			builder.Put(original, mapping)
		}
		return builder.Build()
	}

	first := fixture.partialMatch(identityTestRegularInfo(
		nil,
		EmptyGroupByMappings(),
		newPredicateMap(false),
	))
	reversed := fixture.partialMatch(identityTestRegularInfo(
		nil,
		EmptyGroupByMappings(),
		newPredicateMap(true),
	))
	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first predicate-map match rejected")
	}
	if AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, reversed) {
		t.Fatal("PredicateMultiMap insertion order changed semantic identity")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 1 {
		t.Fatalf("stored predicate-map matches = %d, want 1", got)
	}
}

func TestPartialMatchSemanticIdentity_ChildSelectionAndReconstruction(t *testing.T) {
	t.Parallel()

	candidate := stubMatchCandidate{name: "child_selection"}
	childQueryExpr := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	childCandidateExpr := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	childQueryRef := expressions.InitialOf(childQueryExpr)
	childCandidateRef := expressions.InitialOf(childCandidateExpr)

	queryAlias := values.NamedCorrelationIdentifier("query_child")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_child")
	queryQuantifier := expressions.NamedForEachQuantifier(queryAlias, childQueryRef)
	candidateQuantifier := expressions.NamedForEachQuantifier(
		candidateAlias,
		childCandidateRef,
	)
	parentQueryExpr := expressions.NewSelectExpression(
		queryQuantifier.GetFlowedObjectValue(),
		[]expressions.Quantifier{queryQuantifier},
		nil,
	)
	parentCandidateExpr := expressions.NewSelectExpression(
		candidateQuantifier.GetFlowedObjectValue(),
		[]expressions.Quantifier{candidateQuantifier},
		nil,
	)
	parentQueryRef := expressions.InitialOf(parentQueryExpr)
	parentCandidateRef := expressions.InitialOf(parentCandidateExpr)
	parentAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)
	childParameter := values.NamedCorrelationIdentifier("child_parameter")

	newChild := func(literal int64) *PartialMatchImpl {
		comparison := predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			literal,
		)
		childInfo := NewRegularMatchInfo(
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				childParameter: identityTestComparisonRange(t, comparison),
			},
			EmptyAliasMap(),
			nil,
			nil,
			nil,
			EmptyGroupByMappings(),
			nil,
			nil,
		)
		return NewPartialMatch(
			EmptyAliasMap(),
			candidate,
			childQueryRef,
			childQueryExpr,
			childCandidateRef,
			childInfo,
		)
	}
	newParent := func(child PartialMatch) *PartialMatchImpl {
		parentInfo := NewRegularMatchInfo(
			nil,
			parentAliasMap,
			nil,
			nil,
			nil,
			EmptyGroupByMappings(),
			nil,
			nil,
		)
		parentInfo.SetChildPartialMatch(queryAlias, child)
		return NewPartialMatch(
			parentAliasMap,
			candidate,
			parentQueryRef,
			parentQueryExpr,
			parentCandidateRef,
			parentInfo,
		)
	}

	first := newParent(newChild(7))
	differentChildSelection := newParent(newChild(9))
	exactReconstruction := newParent(newChild(7))
	if !AddPartialMatchForCandidate(parentQueryRef, candidate, first) {
		t.Fatal("first child-selection match rejected")
	}
	if !AddPartialMatchForCandidate(
		parentQueryRef,
		candidate,
		differentChildSelection,
	) {
		t.Fatal("semantically distinct child selection was collapsed")
	}
	if AddPartialMatchForCandidate(
		parentQueryRef,
		candidate,
		exactReconstruction,
	) {
		t.Fatal("reconstructed equivalent child graph was not deduplicated")
	}
	if got := len(GetPartialMatchesForCandidate(parentQueryRef, candidate)); got != 2 {
		t.Fatalf("stored child-selection alternatives = %d, want 2", got)
	}
}

func TestPartialMatchSemanticIdentity_DefaultAndResidualCompensationDiffer(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("compensation_identity")
	newPredicateMap := func(residual bool) *PredicateMultiMap {
		original := predicates.NewConstantPredicate(predicates.TriTrue)
		translated := predicates.NewConstantPredicate(predicates.TriTrue)
		candidate := predicates.NewConstantPredicate(predicates.TriTrue)
		mappingBuilder := RegularMappingBuilder(
			original,
			translated,
			candidate,
		)
		if residual {
			mappingBuilder.setKnownPredicateCompensation(
				reapplyResidualCompensation(original),
				"residual",
			)
		}
		builder := NewPredicateMultiMapBuilder()
		builder.Put(original, mappingBuilder.Build())
		return builder.Build()
	}
	newMatch := func(residual bool) *PartialMatchImpl {
		return fixture.partialMatch(identityTestRegularInfo(
			nil,
			EmptyGroupByMappings(),
			newPredicateMap(residual),
		))
	}

	defaultCompensation := newMatch(false)
	residualCompensation := newMatch(true)
	exactResidualReconstruction := newMatch(true)
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		defaultCompensation,
	) {
		t.Fatal("default-compensation match rejected")
	}
	if !AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		residualCompensation,
	) {
		t.Fatal("default and residual compensation were conflated")
	}
	if AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		exactResidualReconstruction,
	) {
		t.Fatal("reconstructed residual compensation was not deduplicated")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 2 {
		t.Fatalf("stored compensation alternatives = %d, want 2", got)
	}
}

func TestPartialMatchSemanticIdentity_ForwardedReferencesCanonicalize(t *testing.T) {
	t.Parallel()

	fixture := newPartialMatchIdentityFixture("forwarded_reconstruction")
	forwardedQueryRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"T"},
			values.UnknownType,
		),
	)
	forwardedCandidateRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"T"},
			values.UnknownType,
		),
	)
	first := NewPartialMatch(
		EmptyAliasMap(),
		fixture.candidate,
		forwardedQueryRef,
		fixture.queryExpr,
		forwardedCandidateRef,
		identityTestRegularInfo(nil, EmptyGroupByMappings(), nil),
	)

	fixture.queryRef.Absorb(forwardedQueryRef)
	fixture.candidateRef.Absorb(forwardedCandidateRef)
	if !forwardedQueryRef.IsForwarded() || !forwardedCandidateRef.IsForwarded() {
		t.Fatal("test references did not forward to their canonical survivors")
	}
	reconstructed := fixture.partialMatch(
		identityTestRegularInfo(nil, EmptyGroupByMappings(), nil),
	)

	if !AddPartialMatchForCandidate(fixture.queryRef, fixture.candidate, first) {
		t.Fatal("first forwarded-reference match rejected")
	}
	if AddPartialMatchForCandidate(
		fixture.queryRef,
		fixture.candidate,
		reconstructed,
	) {
		t.Fatal("canonical reconstruction of forwarded references was not deduplicated")
	}
	if got := len(GetPartialMatchesForCandidate(fixture.queryRef, fixture.candidate)); got != 1 {
		t.Fatalf("stored forwarded-reference matches = %d, want 1", got)
	}
}
