package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type matchIntermediateStructuralTwoLegFixture struct {
	queryScans         []*expressions.FullUnorderedScanExpression
	queryScanRefs      []*expressions.Reference
	queryQuantifiers   []expressions.Quantifier
	querySelect        *expressions.SelectExpression
	querySelectRef     *expressions.Reference
	candidateScans     []*expressions.FullUnorderedScanExpression
	candidateScanRefs  []*expressions.Reference
	candidateQs        []expressions.Quantifier
	candidateSelect    *expressions.SelectExpression
	candidateSelectRef *expressions.Reference
	candidate          *testMatchCandidate
}

func newMatchIntermediateStructuralTwoLegFixture(
	name string,
) *matchIntermediateStructuralTwoLegFixture {
	recordTypes := []string{"A", "B"}
	fixture := &matchIntermediateStructuralTwoLegFixture{}
	for _, recordType := range recordTypes {
		queryScan := expressions.NewFullUnorderedScanExpression(
			[]string{recordType},
			values.UnknownType,
		)
		queryScanRef := expressions.InitialOf(queryScan)
		fixture.queryScans = append(fixture.queryScans, queryScan)
		fixture.queryScanRefs = append(fixture.queryScanRefs, queryScanRef)
		fixture.queryQuantifiers = append(
			fixture.queryQuantifiers,
			expressions.ForEachQuantifier(queryScanRef),
		)

		candidateScan := expressions.NewFullUnorderedScanExpression(
			[]string{recordType},
			values.UnknownType,
		)
		candidateScanRef := expressions.InitialOf(candidateScan)
		fixture.candidateScans = append(fixture.candidateScans, candidateScan)
		fixture.candidateScanRefs = append(
			fixture.candidateScanRefs,
			candidateScanRef,
		)
		fixture.candidateQs = append(
			fixture.candidateQs,
			expressions.ForEachQuantifier(candidateScanRef),
		)
	}

	fixture.querySelect = expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		fixture.queryQuantifiers,
		nil,
	)
	fixture.querySelectRef = expressions.InitialOf(fixture.querySelect)
	fixture.candidateSelect = expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		fixture.candidateQs,
		nil,
	)
	fixture.candidateSelectRef = expressions.InitialOf(
		fixture.candidateSelect,
	)
	fixture.candidate = &testMatchCandidate{
		name:      name,
		traversal: NewTraversal(fixture.candidateSelectRef),
	}
	return fixture
}

func (f *matchIntermediateStructuralTwoLegFixture) seedChild(
	t *testing.T,
	queryLeg int,
	candidateLeg int,
	boundAliasMap *AliasMap,
	parameterBindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) *PartialMatchImpl {
	t.Helper()
	if boundAliasMap == nil {
		boundAliasMap = EmptyAliasMap()
	}
	matchInfo := NewRegularMatchInfo(
		parameterBindings,
		boundAliasMap,
		nil,
		nil,
		nil,
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	child := NewPartialMatch(
		boundAliasMap,
		f.candidate,
		f.queryScanRefs[queryLeg],
		f.queryScans[queryLeg],
		f.candidateScanRefs[candidateLeg],
		matchInfo,
	)
	if !AddPartialMatchForCandidate(
		f.queryScanRefs[queryLeg],
		f.candidate,
		child,
	) {
		t.Fatal("failed to seed structurally distinct child PartialMatch")
	}
	return child
}

func (f *matchIntermediateStructuralTwoLegFixture) match(
	t *testing.T,
) *matchIntermediateSearchBudget {
	t.Helper()
	budget := &matchIntermediateSearchBudget{}
	matched := matchIntermediateStructural(
		NewExpressionRuleCall(f.querySelectRef, nil, nil),
		f.querySelect,
		f.candidate,
		f.candidateSelectRef,
		f.candidateSelect,
		budget,
	)
	if !matched {
		t.Fatal("exact structural match produced no parent")
	}
	return budget
}

func matchIntermediateStructuralEqualityRange(
	t *testing.T,
	literal int64,
) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(
		predicates.ComparisonEquals,
		literal,
	)
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatalf("failed to build equality range for %d", literal)
	}
	return merged.Range
}

func TestMatchIntermediateStructural_LaterChildSurvivesAliasConflict(t *testing.T) {
	t.Parallel()

	fixture := newMatchIntermediateStructuralTwoLegFixture(
		"idx_structural_alias_branch",
	)
	// This first A child contradicts the complete skeleton's
	// query-B -> candidate-B binding. The later A child is compatible.
	conflictingChildA := fixture.seedChild(
		t,
		0,
		0,
		AliasMapOfAliases(
			fixture.queryQuantifiers[1].GetAlias(),
			fixture.candidateQs[0].GetAlias(),
		),
		nil,
	)
	compatibleChildA := fixture.seedChild(t, 0, 0, EmptyAliasMap(), nil)
	childB := fixture.seedChild(t, 1, 1, EmptyAliasMap(), nil)

	fixture.match(t)

	parents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(parents) != 1 {
		t.Fatalf("structural parents = %d, want 1 from the later compatible A branch", len(parents))
	}
	parent := parents[0].(*PartialMatchImpl)
	matchInfo := parent.GetRegularMatchInfo()
	if got := matchInfo.GetChildPartialMatchMaybe(
		fixture.queryQuantifiers[0].GetAlias(),
	); got != compatibleChildA {
		t.Fatalf("selected A child = %p, want later compatible child %p", got, compatibleChildA)
	}
	if got := matchInfo.GetChildPartialMatchMaybe(
		fixture.queryQuantifiers[1].GetAlias(),
	); got != childB {
		t.Fatalf("selected B child = %p, want %p", got, childB)
	}
	if got := matchInfo.GetChildPartialMatchMaybe(
		fixture.queryQuantifiers[0].GetAlias(),
	); got == conflictingChildA {
		t.Fatal("parent retained the alias-conflicting first A child")
	}
	if got := parent.GetBoundAliasMap().GetTarget(
		fixture.queryQuantifiers[0].GetAlias(),
	); got != fixture.candidateQs[0].GetAlias() {
		t.Fatalf("query A mapped to %v, want candidate A", got)
	}
	if got := parent.GetBoundAliasMap().GetTarget(
		fixture.queryQuantifiers[1].GetAlias(),
	); got != fixture.candidateQs[1].GetAlias() {
		t.Fatalf("query B mapped to %v, want candidate B", got)
	}
}

func TestMatchIntermediateStructural_RetainsValidChildProducts(t *testing.T) {
	t.Parallel()

	fixture := newMatchIntermediateStructuralTwoLegFixture(
		"idx_structural_child_products",
	)
	parameter := values.UniqueCorrelationIdentifier()
	rangeOne := matchIntermediateStructuralEqualityRange(t, int64(1))
	rangeTwo := matchIntermediateStructuralEqualityRange(t, int64(2))
	childAOne := fixture.seedChild(
		t,
		0,
		0,
		EmptyAliasMap(),
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			parameter: rangeOne,
		},
	)
	childATwo := fixture.seedChild(
		t,
		0,
		0,
		EmptyAliasMap(),
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			parameter: rangeTwo,
		},
	)
	childBTwo := fixture.seedChild(
		t,
		1,
		1,
		EmptyAliasMap(),
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			parameter: rangeTwo,
		},
	)
	childBEmpty := fixture.seedChild(
		t,
		1,
		1,
		EmptyAliasMap(),
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			parameter: predicates.EmptyComparisonRange(),
		},
	)

	fixture.match(t)

	type childPair struct {
		a *PartialMatchImpl
		b *PartialMatchImpl
	}
	expected := map[childPair]bool{
		{a: childAOne, b: childBEmpty}: false,
		{a: childATwo, b: childBTwo}:   false,
		{a: childATwo, b: childBEmpty}: false,
	}
	parents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(parents) != len(expected) {
		t.Fatalf("valid structural child products = %d, want %d", len(parents), len(expected))
	}
	for _, rawParent := range parents {
		matchInfo := rawParent.GetRegularMatchInfo()
		childA, ok := matchInfo.GetChildPartialMatchMaybe(
			fixture.queryQuantifiers[0].GetAlias(),
		).(*PartialMatchImpl)
		if !ok {
			t.Fatal("parent did not retain a concrete A child")
		}
		childB, ok := matchInfo.GetChildPartialMatchMaybe(
			fixture.queryQuantifiers[1].GetAlias(),
		).(*PartialMatchImpl)
		if !ok {
			t.Fatal("parent did not retain a concrete B child")
		}
		pair := childPair{a: childA, b: childB}
		seen, valid := expected[pair]
		if !valid {
			t.Fatalf("unexpected child product A=%p B=%p", childA, childB)
		}
		if seen {
			t.Fatalf("child product A=%p B=%p was retained twice", childA, childB)
		}
		expected[pair] = true

		expectedRange := rangeTwo
		if childA == childAOne {
			expectedRange = rangeOne
		}
		if got := matchInfo.GetParameterBindingMap()[parameter]; !partialMatchComparisonRangesEqual(got, expectedRange) {
			t.Fatalf("merged product range = %v, want metadata from selected A child", got)
		}
	}
	for pair, seen := range expected {
		if !seen {
			t.Fatalf("valid child product A=%p B=%p was skipped", pair.a, pair.b)
		}
	}

	// A=1/B=2 is the sole conflicting parameter product. Its rejection must
	// not abort the A=1/B=empty branch or either A=2 branch.
	conflictingPair := childPair{a: childAOne, b: childBTwo}
	if _, retained := expected[conflictingPair]; retained {
		t.Fatal("parameter-conflicting child product was retained")
	}

	fixture.match(t)
	if got := len(GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)); got != len(expected) {
		t.Fatalf("structural refire grew child products to %d, want %d", got, len(expected))
	}
}

func TestMatchIntermediateStructural_ResultCapRefireDoesNotPage(t *testing.T) {
	t.Parallel()

	queryScan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	queryScanRef := expressions.InitialOf(queryScan)
	queryQ := expressions.ForEachQuantifier(queryScanRef)
	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{queryQ},
		nil,
	)
	querySelectRef := expressions.InitialOf(querySelect)

	candidateScan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	candidateScanRef := expressions.InitialOf(candidateScan)
	candidateQ := expressions.ForEachQuantifier(candidateScanRef)
	candidateSelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{candidateQ},
		nil,
	)
	candidateSelectRef := expressions.InitialOf(candidateSelect)
	candidate := &testMatchCandidate{
		name:      "idx_structural_result_cap",
		traversal: NewTraversal(candidateSelectRef),
	}

	parameter := values.UniqueCorrelationIdentifier()
	children := make([]*PartialMatchImpl, 0, matchIntermediateMaxUniqueResults+1)
	for i := 0; i < matchIntermediateMaxUniqueResults+1; i++ {
		boundAliasMap := EmptyAliasMap()
		matchInfo := NewRegularMatchInfo(
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameter: matchIntermediateStructuralEqualityRange(t, int64(i)),
			},
			boundAliasMap,
			nil,
			nil,
			nil,
			EmptyGroupByMappings(),
			nil,
			nil,
		)
		child := NewPartialMatch(
			boundAliasMap,
			candidate,
			queryScanRef,
			queryScan,
			candidateScanRef,
			matchInfo,
		)
		if !AddPartialMatchForCandidate(queryScanRef, candidate, child) {
			t.Fatalf("child %d was not semantically distinct", i)
		}
		children = append(children, child)
	}

	run := func() *matchIntermediateSearchBudget {
		budget := &matchIntermediateSearchBudget{}
		if !matchIntermediateStructural(
			NewExpressionRuleCall(querySelectRef, nil, nil),
			querySelect,
			candidate,
			candidateSelectRef,
			candidateSelect,
			budget,
		) {
			t.Fatal("one-leg exact Select produced no structural match")
		}
		return budget
	}

	firstBudget := run()
	if firstBudget.uniqueResults != matchIntermediateMaxUniqueResults {
		t.Fatalf("first unique results = %d, want cap %d", firstBudget.uniqueResults, matchIntermediateMaxUniqueResults)
	}
	firstParents := GetPartialMatchesForExpression(querySelectRef, querySelect)
	if len(firstParents) != matchIntermediateMaxUniqueResults {
		t.Fatalf("stored parents = %d, want cap %d", len(firstParents), matchIntermediateMaxUniqueResults)
	}
	for i, parent := range firstParents {
		selected := parent.GetRegularMatchInfo().
			GetChildPartialMatchMaybe(queryQ.GetAlias())
		if selected != children[i] {
			t.Fatalf("parent %d selected child %p, want deterministic child %p", i, selected, children[i])
		}
	}
	if selected := firstParents[len(firstParents)-1].GetRegularMatchInfo().
		GetChildPartialMatchMaybe(queryQ.GetAlias()); selected == children[matchIntermediateMaxUniqueResults] {
		t.Fatal("the 65th child escaped the first-64 result cap")
	}

	secondBudget := run()
	if secondBudget.uniqueResults != matchIntermediateMaxUniqueResults {
		t.Fatalf("refire unique results = %d, want cap %d", secondBudget.uniqueResults, matchIntermediateMaxUniqueResults)
	}
	secondParents := GetPartialMatchesForExpression(querySelectRef, querySelect)
	if len(secondParents) != len(firstParents) {
		t.Fatalf("refire stored parents = %d, want stable %d", len(secondParents), len(firstParents))
	}
	for i := range secondParents {
		if secondParents[i] != firstParents[i] {
			t.Fatalf("refire replaced or paged parent %d", i)
		}
		selected := secondParents[i].GetRegularMatchInfo().
			GetChildPartialMatchMaybe(queryQ.GetAlias())
		if selected != children[i] {
			t.Fatalf("refire parent %d selected child %p, want original child %p", i, selected, children[i])
		}
	}
}
