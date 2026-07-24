package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mergeTestRange(
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

func mergeTestGroupPartialMatch(
	t *testing.T,
	name string,
	candidateMemberCount int,
	groupByMappings func(values.Value, values.Value) *GroupByMappings,
) (*PartialMatchImpl, values.Value, values.Value) {
	t.Helper()

	queryResult := values.LiteralValue(int64(101))
	queryExpression := expressions.NewSelectExpression(queryResult, nil, nil)
	queryRef := expressions.InitialOf(queryExpression)

	candidateResult := values.LiteralValue(int64(202))
	candidateExpression := expressions.NewSelectExpression(
		candidateResult,
		nil,
		nil,
	)
	candidateRef := expressions.InitialOf(candidateExpression)
	if candidateMemberCount == 2 {
		if !candidateRef.Insert(expressions.NewSelectExpression(
			values.LiteralValue(int64(303)),
			nil,
			nil,
		)) {
			t.Fatal("failed to add the second candidate member")
		}
	}
	if got := len(candidateRef.AllMembers()); got != candidateMemberCount {
		t.Fatalf(
			"candidate fixture has %d members, want %d",
			got,
			candidateMemberCount,
		)
	}

	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		groupByMappings(queryResult, candidateResult),
		nil,
		nil,
	)
	return NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: name},
		queryRef,
		queryExpression,
		candidateRef,
		matchInfo,
	), queryResult, candidateResult
}

func TestTryMergeParameterBindings(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("p")
	eq7 := mergeTestRange(
		t,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7)),
	)
	eq7Copy := mergeTestRange(
		t,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7)),
	)
	eq9 := mergeTestRange(
		t,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(9)),
	)
	greater5 := mergeTestRange(
		t,
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
	)
	less10 := mergeTestRange(
		t,
		predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(10)),
	)

	tests := []struct {
		name            string
		left            *predicates.ComparisonRange
		right           *predicates.ComparisonRange
		wantOK          bool
		wantType        predicates.ComparisonRangeType
		wantComparisons int
	}{
		{
			name:     "empty is left identity",
			left:     predicates.EmptyComparisonRange(),
			right:    eq7,
			wantOK:   true,
			wantType: predicates.ComparisonRangeEquality,
		},
		{
			name:     "empty is right identity",
			left:     eq7,
			right:    predicates.EmptyComparisonRange(),
			wantOK:   true,
			wantType: predicates.ComparisonRangeEquality,
		},
		{
			name:     "exact equality is idempotent",
			left:     eq7,
			right:    eq7Copy,
			wantOK:   true,
			wantType: predicates.ComparisonRangeEquality,
		},
		{
			name:   "distinct equality conflicts",
			left:   eq7,
			right:  eq9,
			wantOK: false,
		},
		{
			name:   "equality and inequality conflict",
			left:   eq7,
			right:  greater5,
			wantOK: false,
		},
		{
			name:   "inequality and equality conflict",
			left:   greater5,
			right:  eq7,
			wantOK: false,
		},
		{
			name:            "inequalities union",
			left:            greater5,
			right:           less10,
			wantOK:          true,
			wantType:        predicates.ComparisonRangeInequality,
			wantComparisons: 2,
		},
		{
			name:            "exact inequality is deduplicated",
			left:            greater5,
			right:           mergeTestRange(t, predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))),
			wantOK:          true,
			wantType:        predicates.ComparisonRangeInequality,
			wantComparisons: 1,
		},
		{
			name:   "nil is rejected",
			left:   nil,
			right:  nil,
			wantOK: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			got, ok := tryMergeParameterBindings(
				map[values.CorrelationIdentifier]*predicates.ComparisonRange{
					alias: test.left,
				},
				map[values.CorrelationIdentifier]*predicates.ComparisonRange{
					alias: test.right,
				},
			)
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v", ok, test.wantOK)
			}
			if !ok {
				if got != nil {
					t.Fatalf("failed merge returned %v, want nil", got)
				}
				return
			}
			merged := got[alias]
			if merged.GetRangeType() != test.wantType {
				t.Fatalf("range type = %v, want %v", merged.GetRangeType(), test.wantType)
			}
			if test.wantComparisons > 0 {
				if count := len(merged.GetInequalityComparisons()); count != test.wantComparisons {
					t.Fatalf("inequality comparisons = %d, want %d", count, test.wantComparisons)
				}
			}
		})
	}

	// Comparison identity includes parameter/vector configuration, not merely
	// the operand. ComparisonRange.Merge itself is intentionally looser, so the
	// RegularMatchInfo merge must pin the stronger contract.
	base := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	parameterized := base
	parameterized.ParameterName = "runtime_p"
	if _, ok := tryMergeParameterBindings(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: mergeTestRange(t, base),
		},
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: mergeTestRange(t, parameterized),
		},
	); ok {
		t.Fatal("same operand with different ParameterName should conflict")
	}

	// A late conflict must not mutate either input map/range.
	otherAlias := values.NamedCorrelationIdentifier("other")
	left := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		alias:      eq7,
		otherAlias: greater5,
	}
	right := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		alias:      eq9,
		otherAlias: less10,
	}
	if _, ok := tryMergeParameterBindings(left, right); ok {
		t.Fatal("late parameter conflict should reject")
	}
	if left[alias] != eq7 || left[otherAlias] != greater5 ||
		right[alias] != eq9 || right[otherAlias] != less10 {
		t.Fatal("failed parameter merge mutated its inputs")
	}
}

func mergeTestChildPartialMatch(
	candidate MatchCandidate,
	queryRef *expressions.Reference,
	queryExpr expressions.RelationalExpression,
	candidateRef *expressions.Reference,
	matchInfo MatchInfo,
) *PartialMatchImpl {
	return NewPartialMatch(
		EmptyAliasMap(),
		candidate,
		queryRef,
		queryExpr,
		candidateRef,
		matchInfo,
	)
}

func TestTryMergeRegularMatchInfo_MetadataAndChildInvariants(t *testing.T) {
	t.Parallel()

	candidate := stubMatchCandidate{name: "merge_metadata"}
	newLeaf := func(recordType string) (
		*expressions.Reference,
		expressions.RelationalExpression,
		*expressions.Reference,
	) {
		queryExpr := expressions.NewFullUnorderedScanExpression(
			[]string{recordType},
			values.UnknownType,
		)
		candidateExpr := expressions.NewFullUnorderedScanExpression(
			[]string{recordType},
			values.UnknownType,
		)
		return expressions.InitialOf(queryExpr), queryExpr, expressions.InitialOf(candidateExpr)
	}

	queryRefA, queryExprA, candidateRefA := newLeaf("A")
	queryRefB, queryExprB, candidateRefB := newLeaf("B")
	ordering := []*MatchedOrderingPart{NewMatchedOrderingPart(
		values.NamedCorrelationIdentifier("order_p"),
		values.LiteralValue(int64(1)),
		predicates.EmptyComparisonRange(),
		MatchedSortOrderAscending,
	)}
	childConstraintPredicate := predicates.NewConstantPredicate(predicates.TriFalse)
	childInfoA := newRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			values.NamedCorrelationIdentifier("child_p"): mergeTestRange(
				t,
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
		},
		EmptyAliasMap(),
		nil,
		ordering,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		[]values.Value{values.LiteralValue(int64(11))},
		NewQueryPlanConstraint(childConstraintPredicate),
		true,
	)
	childInfoB := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	childA := mergeTestChildPartialMatch(
		candidate,
		queryRefA,
		queryExprA,
		candidateRefA,
		childInfoA,
	)
	childB := mergeTestChildPartialMatch(
		candidate,
		queryRefB,
		queryExprB,
		candidateRefB,
		childInfoB,
	)

	queryQA := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("qa"),
		queryRefA,
	)
	queryQB := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("qb"),
		queryRefB,
	)
	candidateAliasA := values.NamedCorrelationIdentifier("ca")
	candidateAliasB := values.NamedCorrelationIdentifier("cb")
	bindingsBuilder := NewAliasMapBuilder()
	bindingsBuilder.Put(queryQA.GetAlias(), candidateAliasA)
	bindingsBuilder.Put(queryQB.GetAlias(), candidateAliasB)
	bindings := bindingsBuilder.Build()

	localParameter := values.NamedCorrelationIdentifier("local_p")
	predicateConstraintPredicate := predicates.NewConstantPredicate(predicates.TriUnknown)
	localConstraintPredicate := predicates.NewValuePredicate(values.NewBooleanValue(true))
	queryPredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	candidatePredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	mapping := RegularMappingBuilder(
		queryPredicate,
		queryPredicate,
		candidatePredicate,
	).SetConstraint(NewQueryPlanConstraint(predicateConstraintPredicate)).Build()
	predicateMapBuilder := NewPredicateMultiMapBuilder()
	predicateMapBuilder.Put(queryPredicate, mapping)
	predicateMap := predicateMapBuilder.Build()

	merged, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{{
			quantifier:   queryQA,
			partialMatch: childA,
		}},
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			localParameter: mergeTestRange(
				t,
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
			),
		},
		predicateMap,
		NewMaxMatchMap(nil, values.LiteralValue(int64(3)), values.LiteralValue(int64(3))),
		EmptyGroupByMappings(),
		nil,
		NewQueryPlanConstraint(localConstraintPredicate),
	)
	if !ok {
		t.Fatal("compatible metadata merge rejected")
	}
	if got := merged.GetChildPartialMatchMaybe(queryQA.GetAlias()); got != childA {
		t.Fatal("selected child PartialMatch was not retained")
	}
	if merged.RequiresPrimaryKeyDistinct() {
		t.Fatal("child PK-distinct obligation leaked into a cardinality-safe parent")
	}
	if got := len(merged.GetParameterBindingMap()); got != 2 {
		t.Fatalf("merged parameter bindings = %d, want child+local = 2", got)
	}
	if got := merged.GetMatchedOrderingParts(); len(got) != 1 || got[0] != ordering[0] {
		t.Fatalf("sole regular child's ordering = %v, want inherited ordering", got)
	}
	if got := merged.GetRollUpToGroupingValues(); len(got) != 1 {
		t.Fatalf("sole child roll-up = %v, want inherited", got)
	}
	effectiveConstraint, ok := merged.GetConstraint().GetPredicate().(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("effective constraint = %T, want *AndPredicate", merged.GetConstraint().GetPredicate())
	}
	wantConstraintOrder := []predicates.QueryPredicate{
		predicateConstraintPredicate,
		childConstraintPredicate,
		localConstraintPredicate,
	}
	if len(effectiveConstraint.SubPredicates) != len(wantConstraintOrder) {
		t.Fatalf("constraint conjuncts = %d, want %d", len(effectiveConstraint.SubPredicates), len(wantConstraintOrder))
	}
	for i, want := range wantConstraintOrder {
		if effectiveConstraint.SubPredicates[i] != want {
			t.Fatalf("constraint[%d] = %p, want %p", i, effectiveConstraint.SubPredicates[i], want)
		}
	}

	// More than one regular child clears ordering, matching Java.
	twoChildren, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{
			{quantifier: queryQA, partialMatch: childA},
			{quantifier: queryQB, partialMatch: childB},
		},
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	if !ok {
		t.Fatal("compatible two-child merge rejected")
	}
	if got := len(twoChildren.GetMatchedOrderingParts()); got != 0 {
		t.Fatalf("two regular children inherited %d ordering parts, want 0", got)
	}

	// An existential child does not count as a second regular ordering source.
	queryExistential := expressions.NamedExistentialQuantifier(
		queryQB.GetAlias(),
		queryRefB,
	)
	regularAndExistential, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{
			{quantifier: queryQA, partialMatch: childA},
			{quantifier: queryExistential, partialMatch: childB},
		},
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	if !ok || len(regularAndExistential.GetMatchedOrderingParts()) != 1 {
		t.Fatal("one regular plus one existential should inherit regular ordering")
	}

	// Local and child roll-ups cannot both own provenance.
	if _, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{{quantifier: queryQA, partialMatch: childA}},
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		[]values.Value{values.LiteralValue(int64(22))},
		nil,
	); ok {
		t.Fatal("local plus child roll-up should conflict")
	}

	// The child map is an identity bimap: duplicate aliases and duplicate PM
	// values reject rather than silently overwrite.
	if _, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{
			{quantifier: queryQA, partialMatch: childA},
			{quantifier: queryQA, partialMatch: childB},
		},
		nil, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, nil,
	); ok {
		t.Fatal("duplicate query alias should reject")
	}
	if _, ok := tryMergeRegularMatchInfo(
		bindings,
		[]quantifierPartialMatch{
			{quantifier: queryQA, partialMatch: childA},
			{quantifier: queryQB, partialMatch: childA},
		},
		nil, nil, NewMaxMatchMap(nil, nil, nil), EmptyGroupByMappings(), nil, nil,
	); ok {
		t.Fatal("same child PartialMatch under two quantifiers should reject")
	}

	copyOfChildren := merged.GetChildPartialMatchMap()
	delete(copyOfChildren, queryQA.GetAlias())
	if merged.GetChildPartialMatchMaybe(queryQA.GetAlias()) == nil {
		t.Fatal("mutating defensive child-map copy changed RegularMatchInfo")
	}
}

func TestPutValueBiMapChecked_ExplainCollisionFailsClosed(t *testing.T) {
	t.Parallel()

	type opaqueLiteral struct {
		X int
	}
	firstKey := &values.ConstantValue{
		Value: opaqueLiteral{X: 1},
		Typ:   values.UnknownType,
	}
	secondKey := &values.ConstantValue{
		Value: opaqueLiteral{X: 2},
		Typ:   values.UnknownType,
	}
	if values.ExplainValue(firstKey) != values.ExplainValue(secondKey) {
		t.Fatalf(
			"fixture does not collide: %q vs %q",
			values.ExplainValue(firstKey),
			values.ExplainValue(secondKey),
		)
	}
	if values.SemanticEqualsUnderAliasMap(firstKey, secondKey, nil) {
		t.Fatal("fixture keys are semantically equal; need a real rendering collision")
	}

	target := NewValueBiMap()
	if !putValueBiMapChecked(
		target,
		firstKey,
		values.LiteralValue(int64(1)),
	) {
		t.Fatal("first mapping rejected")
	}
	if putValueBiMapChecked(
		target,
		secondKey,
		values.LiteralValue(int64(1)),
	) {
		t.Fatal("Explain-colliding distinct key should fail closed")
	}
	if target.Len() != 1 {
		t.Fatalf("failed collision changed BiMap length to %d, want 1", target.Len())
	}

	correlationTarget := NewCorrValueBiMap()
	if !putCorrelationValueBiMapChecked(
		correlationTarget,
		values.NamedCorrelationIdentifier("a"),
		firstKey,
	) {
		t.Fatal("first correlation mapping rejected")
	}
	if putCorrelationValueBiMapChecked(
		correlationTarget,
		values.NamedCorrelationIdentifier("b"),
		secondKey,
	) {
		t.Fatal("Explain-colliding distinct value should fail closed")
	}
	if correlationTarget.Len() != 1 {
		t.Fatalf("failed correlation collision changed length to %d, want 1", correlationTarget.Len())
	}
}

func TestPullUpChildGroupByMappings_MultiMemberCandidateRef(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("group_query")
	candidateAlias := values.NamedCorrelationIdentifier("group_candidate")

	t.Run("empty mappings succeed", func(t *testing.T) {
		partialMatch, _, _ := mergeTestGroupPartialMatch(
			t,
			"empty_group_mappings",
			2,
			func(values.Value, values.Value) *GroupByMappings {
				return EmptyGroupByMappings()
			},
		)

		pulled, ok := pullUpChildGroupByMappings(
			partialMatch,
			queryAlias,
			candidateAlias,
		)
		if !ok {
			t.Fatal("empty group-by mappings rejected")
		}
		if pulled.MatchedGroupingsMap().Len() != 0 ||
			pulled.MatchedAggregatesMap().Len() != 0 ||
			pulled.UnmatchedAggregatesMap().Len() != 0 {
			t.Fatal("empty group-by mappings produced a non-empty result")
		}
	})

	t.Run("unmatched aggregate only succeeds", func(t *testing.T) {
		unmatchedAlias := values.NamedCorrelationIdentifier(
			"unmatched_aggregate",
		)
		partialMatch, queryResult, _ := mergeTestGroupPartialMatch(
			t,
			"unmatched_group_mapping",
			2,
			func(queryResult, _ values.Value) *GroupByMappings {
				unmatched := NewCorrValueBiMap()
				unmatched.Put(unmatchedAlias, queryResult)
				return NewGroupByMappings(
					NewValueBiMap(),
					NewValueBiMap(),
					unmatched,
				)
			},
		)

		pulled, ok := pullUpChildGroupByMappings(
			partialMatch,
			queryAlias,
			candidateAlias,
		)
		if !ok {
			t.Fatal("unmatched-aggregate-only group-by mappings rejected")
		}
		if got := pulled.UnmatchedAggregatesMap().Len(); got != 1 {
			t.Fatalf("pulled unmatched aggregates = %d, want 1", got)
		}
		got, ok := pulled.UnmatchedAggregatesMap().Get(unmatchedAlias)
		if !ok {
			t.Fatal("pulled unmatched aggregate is missing")
		}
		// Correlation-free values are constants under Java's pull-up rule
		// and therefore retain their identity instead of becoming a field
		// of the upper quantifier.
		if !values.SemanticEqualsUnderAliasMap(got, queryResult, nil) {
			t.Fatalf(
				"pulled unmatched aggregate = %s, want %s",
				values.ExplainValue(got),
				values.ExplainValue(queryResult),
			)
		}
	})

	for _, test := range []struct {
		name       string
		putMapping func(
			*BiMap[values.Value, values.Value],
			*BiMap[values.Value, values.Value],
			values.Value,
			values.Value,
		)
	}{
		{
			name: "matched grouping rejects",
			putMapping: func(
				groupings *BiMap[values.Value, values.Value],
				_ *BiMap[values.Value, values.Value],
				queryValue values.Value,
				candidateValue values.Value,
			) {
				groupings.Put(queryValue, candidateValue)
			},
		},
		{
			name: "matched aggregate rejects",
			putMapping: func(
				_ *BiMap[values.Value, values.Value],
				aggregates *BiMap[values.Value, values.Value],
				queryValue values.Value,
				candidateValue values.Value,
			) {
				aggregates.Put(queryValue, candidateValue)
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			partialMatch, _, _ := mergeTestGroupPartialMatch(
				t,
				test.name,
				2,
				func(
					queryResult,
					candidateResult values.Value,
				) *GroupByMappings {
					groupings := NewValueBiMap()
					aggregates := NewValueBiMap()
					test.putMapping(
						groupings,
						aggregates,
						queryResult,
						candidateResult,
					)
					return NewGroupByMappings(
						groupings,
						aggregates,
						NewCorrValueBiMap(),
					)
				},
			)

			pulled, ok := pullUpChildGroupByMappings(
				partialMatch,
				queryAlias,
				candidateAlias,
			)
			if ok || pulled != nil {
				t.Fatal("matched mapping accepted a multi-member candidate ref")
			}
		})
	}
}

func TestTryMergeRegularMatchInfo_MatchedMapExplainCollision(t *testing.T) {
	t.Parallel()

	type opaqueLiteral struct {
		X int
	}
	pulledKey := &values.ConstantValue{
		Value: opaqueLiteral{X: 1},
		Typ:   values.UnknownType,
	}
	childPartialMatch, _, _ := mergeTestGroupPartialMatch(
		t,
		"matched_map_explain_collision",
		1,
		func(_, candidateResult values.Value) *GroupByMappings {
			matchedGroupings := NewValueBiMap()
			matchedGroupings.Put(pulledKey, candidateResult)
			return NewGroupByMappings(
				matchedGroupings,
				NewValueBiMap(),
				NewCorrValueBiMap(),
			)
		},
	)

	queryAlias := values.NamedCorrelationIdentifier("explain_collision")
	candidateAlias := values.NamedCorrelationIdentifier(
		"candidate_explain_collision",
	)
	queryQuantifier := expressions.NamedForEachQuantifier(
		queryAlias,
		childPartialMatch.GetQueryRef(),
	)

	localKey := &values.ConstantValue{
		Value: opaqueLiteral{X: 2},
		Typ:   values.UnknownType,
	}
	if values.ExplainValue(localKey) != values.ExplainValue(pulledKey) {
		t.Fatalf(
			"fixture keys do not collide: %q vs %q",
			values.ExplainValue(localKey),
			values.ExplainValue(pulledKey),
		)
	}
	if values.SemanticEqualsUnderAliasMap(localKey, pulledKey, nil) {
		t.Fatal("fixture keys are semantically equal")
	}

	localMatchedGroupings := NewValueBiMap()
	localMatchedGroupings.Put(
		localKey,
		values.NewQuantifiedObjectValue(candidateAlias),
	)
	additional := NewGroupByMappings(
		localMatchedGroupings,
		NewValueBiMap(),
		NewCorrValueBiMap(),
	)

	merged, ok := tryMergeRegularMatchInfo(
		AliasMapOfAliases(queryAlias, candidateAlias),
		[]quantifierPartialMatch{{
			quantifier:   queryQuantifier,
			partialMatch: childPartialMatch,
		}},
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		additional,
		nil,
		nil,
	)
	if ok || merged != nil {
		t.Fatal("matched-map Explain-key collision was accepted")
	}
}
