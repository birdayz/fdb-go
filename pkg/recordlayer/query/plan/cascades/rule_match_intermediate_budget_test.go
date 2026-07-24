package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func budgetTestChildMatch(
	candidate MatchCandidate,
	queryRef *expressions.Reference,
	queryExpr expressions.RelationalExpression,
	candidateRef *expressions.Reference,
	groupByMappings *GroupByMappings,
) *PartialMatchImpl {
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		nil,
		groupByMappings,
		nil,
		nil,
	)
	return NewPartialMatch(
		EmptyAliasMap(),
		candidate,
		queryRef,
		queryExpr,
		candidateRef,
		matchInfo,
	)
}

func TestTryIntermediateMapping_RejectedSemanticLeafChargesBudget(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name                 string
		candidateParent      func(expressions.Quantifier) expressions.RelationalExpression
		candidateMemberCount int
		groupByMappings      func(
			values.Value,
			values.Value,
		) *GroupByMappings
	}{
		{
			name: "node equality rejection",
			candidateParent: func(
				candidateQ expressions.Quantifier,
			) expressions.RelationalExpression {
				return expressions.NewSelectExpression(
					values.NewRecordConstructorValue(),
					[]expressions.Quantifier{candidateQ},
					nil,
				)
			},
			candidateMemberCount: 1,
			groupByMappings: func(
				values.Value,
				values.Value,
			) *GroupByMappings {
				return EmptyGroupByMappings()
			},
		},
		{
			name: "metadata merge rejection",
			candidateParent: func(
				candidateQ expressions.Quantifier,
			) expressions.RelationalExpression {
				return expressions.NewLogicalFilterExpression(
					[]predicates.QueryPredicate{
						predicates.NewConstantPredicate(predicates.TriTrue),
					},
					candidateQ,
				)
			},
			candidateMemberCount: 2,
			groupByMappings: func(
				queryResult,
				candidateResult values.Value,
			) *GroupByMappings {
				matchedGroupings := NewValueBiMap()
				matchedGroupings.Put(queryResult, candidateResult)
				return NewGroupByMappings(
					matchedGroupings,
					NewValueBiMap(),
					NewCorrValueBiMap(),
				)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			queryChild := expressions.NewFullUnorderedScanExpression(
				[]string{"T"},
				values.UnknownType,
			)
			queryChildRef := expressions.InitialOf(queryChild)
			queryQ := expressions.ForEachQuantifier(queryChildRef)
			queryParent := expressions.NewLogicalFilterExpression(
				[]predicates.QueryPredicate{
					predicates.NewConstantPredicate(predicates.TriTrue),
				},
				queryQ,
			)
			queryParentRef := expressions.InitialOf(queryParent)

			candidateChild := expressions.NewFullUnorderedScanExpression(
				[]string{"T"},
				values.UnknownType,
			)
			candidateChildRef := expressions.InitialOf(candidateChild)
			if test.candidateMemberCount == 2 {
				if !candidateChildRef.Insert(
					expressions.NewFullUnorderedScanExpression(
						[]string{"U"},
						values.UnknownType,
					),
				) {
					t.Fatal("failed to add metadata-rejection candidate member")
				}
			}
			if got := len(candidateChildRef.AllMembers()); got !=
				test.candidateMemberCount {
				t.Fatalf(
					"candidate child members = %d, want %d",
					got,
					test.candidateMemberCount,
				)
			}
			candidateQ := expressions.ForEachQuantifier(candidateChildRef)
			candidateParent := test.candidateParent(candidateQ)
			candidateParentRef := expressions.InitialOf(candidateParent)
			candidate := &testMatchCandidate{
				name:      "budget_" + test.name,
				traversal: NewTraversal(candidateParentRef),
			}

			child := budgetTestChildMatch(
				candidate,
				queryChildRef,
				queryChild,
				candidateChildRef,
				test.groupByMappings(
					queryChild.GetResultValue(),
					candidateChild.GetResultValue(),
				),
			)
			if !queryChildRef.AddPartialMatch(candidate, child) {
				t.Fatal("failed to seed child PartialMatch")
			}

			budget := &matchIntermediateSearchBudget{}
			matched := tryIntermediateMapping(
				&ExpressionRuleCall{Reference: queryParentRef},
				queryParent,
				candidate,
				candidateParentRef,
				candidateParent,
				queryParent.GetQuantifiers(),
				candidateParent.GetQuantifiers(),
				[]quantifierMapping{{
					queryIndex:     0,
					candidateIndex: 0,
				}},
				budget,
			)
			if matched {
				t.Fatal("rejected structural product reported a match")
			}
			if got := budget.visitedStates; got != 2 {
				t.Fatalf(
					"visited states = %d, want child inspection + semantic leaf = 2",
					got,
				)
			}
			if budget.exhausted {
				t.Fatal("two-state rejected product exhausted the budget")
			}
			if got := len(GetPartialMatchesForCandidate(
				queryParentRef,
				candidate,
			)); got != 0 {
				t.Fatalf("rejected product emitted %d parent matches", got)
			}
		})
	}
}

func TestMatchIntermediate_FilterSelectSharesStructuralBudget(t *testing.T) {
	t.Parallel()

	// Query: Filter(col0 = 5, Scan(T)).
	queryChild := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	queryChildRef := expressions.InitialOf(queryChild)
	queryQ := expressions.ForEachQuantifier(queryChildRef)
	queryPredicate := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "col0"},
		predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			int64(5),
		),
	)
	queryFilter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{queryPredicate},
		queryQ,
	)

	// Candidate: Select(Scan(T), Placeholder(col0)). The node types differ,
	// so every complete structural child product reaches and fails node
	// equality; with a fresh budget the specialized route is valid.
	candidateChild := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	candidateChildRef := expressions.InitialOf(candidateChild)
	candidateQ := expressions.ForEachQuantifier(candidateChildRef)
	parameterAlias := values.UniqueCorrelationIdentifier()
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateQ},
		[]predicates.QueryPredicate{
			predicates.NewPlaceholder(
				parameterAlias,
				&values.FieldValue{Field: "col0"},
			),
		},
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	candidate := &testMatchCandidate{
		name:      "budget_filter_select",
		traversal: NewTraversal(candidateSelectRef),
	}

	// A one-leg structural attempt spends three enumerator states, then two
	// states per alias-compatible child: one inspection and one semantic leaf.
	// This count guarantees that structural equality reaches the hard cap.
	const childVariantCount = matchIntermediateMaxVisitedStates/2 + 1
	childMatchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		nil,
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	for i := 0; i < childVariantCount; i++ {
		child := NewPartialMatch(
			EmptyAliasMap(),
			candidate,
			queryChildRef,
			queryChild,
			candidateChildRef,
			childMatchInfo,
		)
		// Use the Reference primitive deliberately: these pointer-distinct
		// variants model alternatives already retained in the memo, while the
		// semantic-dedup wrapper would correctly collapse this synthetic set.
		if !queryChildRef.AddPartialMatch(candidate, child) {
			t.Fatalf("failed to seed child variant %d", i)
		}
	}

	// Pin the exact shared work cap through the inspectable internal budget.
	structuralCall := &ExpressionRuleCall{
		Reference: expressions.InitialOf(queryFilter),
	}
	budget := &matchIntermediateSearchBudget{}
	if matchIntermediateStructural(
		structuralCall,
		queryFilter,
		candidate,
		candidateSelectRef,
		candidateSelect,
		budget,
	) {
		t.Fatal("Filter structurally matched Select")
	}
	if !budget.exhausted {
		t.Fatal("alias-compatible structural child products did not exhaust the budget")
	}
	if got := budget.visitedStates; got != matchIntermediateMaxVisitedStates {
		t.Fatalf(
			"visited states = %d, want hard cap %d",
			got,
			matchIntermediateMaxVisitedStates,
		)
	}

	// Prove that the specialized route itself is viable for this fixture.
	// Starting two states below the cap permits exactly the first compatible
	// child's inspection + semantic attempt and must emit a match.
	controlRef := expressions.InitialOf(queryFilter)
	controlBudget := &matchIntermediateSearchBudget{
		visitedStates: matchIntermediateMaxVisitedStates - 2,
	}
	matchSingleSourceAgainstSelect(
		&ExpressionRuleCall{Reference: controlRef},
		queryFilter,
		[]predicates.QueryPredicate{queryPredicate},
		candidateSelect,
		candidate,
		candidateSelectRef,
		controlBudget,
	)
	if got := len(GetPartialMatchesForCandidate(controlRef, candidate)); got != 1 {
		t.Fatalf(
			"fresh specialized-route allowance emitted %d matches, want 1",
			got,
		)
	}
	if got := controlBudget.visitedStates; got >
		matchIntermediateMaxVisitedStates {
		t.Fatalf(
			"specialized control exceeded cap: visited %d, cap %d",
			got,
			matchIntermediateMaxVisitedStates,
		)
	}

	// Exercise the real one-attempt wrapper. It must not allocate a second
	// budget to Filter->Select fallback after structural equality exhausts the
	// first one.
	attemptRef := expressions.InitialOf(queryFilter)
	matchIntermediateWithCandidate(
		&ExpressionRuleCall{Reference: attemptRef},
		queryFilter,
		candidate,
		candidateSelectRef,
		candidateSelect,
	)
	if got := len(GetPartialMatchesForCandidate(attemptRef, candidate)); got != 0 {
		t.Fatalf(
			"specialized fallback emitted %d matches after structural exhaustion",
			got,
		)
	}
}
