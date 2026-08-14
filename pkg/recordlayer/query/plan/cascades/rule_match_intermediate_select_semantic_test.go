package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type selectSemanticFixedPlanCandidate struct {
	*testMatchCandidate
	plan plans.RecordQueryPlan
}

func (c *selectSemanticFixedPlanCandidate) ToScanPlan(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	_ bool,
) plans.RecordQueryPlan {
	return c.plan
}

type selectSemanticRouteExactSubsetFixture struct {
	queryForEach        expressions.Quantifier
	queryExistential    expressions.Quantifier
	querySelect         *expressions.SelectExpression
	querySelectRef      *expressions.Reference
	queryForEachRef     *expressions.Reference
	queryExistentialRef *expressions.Reference

	candidateForEach        expressions.Quantifier
	candidateExistential    expressions.Quantifier
	candidateSelect         *expressions.SelectExpression
	candidateSelectRef      *expressions.Reference
	candidateForEachRef     *expressions.Reference
	candidateExistentialRef *expressions.Reference
	candidate               MatchCandidate
}

func newSelectSemanticRouteExactSubsetFixture(
	t testing.TB,
	name string,
) *selectSemanticRouteExactSubsetFixture {
	t.Helper()
	queryForEachScan := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	queryForEachRef := mustMatchInitial(t, queryForEachScan)
	queryForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name+"_query_fe"),
		queryForEachRef,
	)
	queryExistentialScan := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	queryExistentialRef := mustMatchInitial(t, queryExistentialScan)
	queryExistential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier(name+"_query_e"),
		queryExistentialRef,
	)
	querySelect := mustMatchSelect(t,
		mustMatchFlowed(t, queryForEach),
		[]expressions.Quantifier{
			queryForEach,
			queryExistential,
		},
		nil,
	)
	querySelectRef := mustMatchInitial(t, querySelect)

	candidateForEachScan := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	candidateForEachRef := mustMatchInitial(t, candidateForEachScan)
	candidateForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name+"_candidate_fe"),
		candidateForEachRef,
	)
	candidateExistentialScan := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	candidateExistentialRef := mustMatchInitial(t, candidateExistentialScan)
	candidateExistential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier(name+"_candidate_e"),
		candidateExistentialRef,
	)
	candidateSelect := mustMatchSelect(t,
		mustMatchFlowed(t, candidateForEach),
		[]expressions.Quantifier{
			candidateForEach,
			candidateExistential,
		},
		nil,
	)
	candidateSelectRef := mustMatchInitial(t, candidateSelect)
	candidate := &testMatchCandidate{
		name:      name,
		traversal: NewTraversal(candidateSelectRef),
	}

	return &selectSemanticRouteExactSubsetFixture{
		queryForEach:            queryForEach,
		queryExistential:        queryExistential,
		querySelect:             querySelect,
		querySelectRef:          querySelectRef,
		queryForEachRef:         queryForEachRef,
		queryExistentialRef:     queryExistentialRef,
		candidateForEach:        candidateForEach,
		candidateExistential:    candidateExistential,
		candidateSelect:         candidateSelect,
		candidateSelectRef:      candidateSelectRef,
		candidateForEachRef:     candidateForEachRef,
		candidateExistentialRef: candidateExistentialRef,
		candidate:               candidate,
	}
}

func selectSemanticRouteOnlyMember(
	t *testing.T,
	ref *expressions.Reference,
) expressions.RelationalExpression {
	t.Helper()
	members := ref.AllMembers()
	if len(members) != 1 {
		t.Fatalf("reference members = %d, want 1", len(members))
	}
	return members[0]
}

func selectSemanticRouteSeedChild(
	t *testing.T,
	candidate MatchCandidate,
	queryRef *expressions.Reference,
	candidateRef *expressions.Reference,
	boundAliasMap *AliasMap,
	parameterBindings map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) *PartialMatchImpl {
	t.Helper()
	if boundAliasMap == nil {
		boundAliasMap = EmptyAliasMap()
	}
	queryExpression := selectSemanticRouteOnlyMember(t, queryRef)
	candidateExpression := selectSemanticRouteOnlyMember(t, candidateRef)
	candidateResult := candidateExpression.GetResultValue()
	childMaxMatchMap := buildMatchMaxMatchMap(
		candidateResult,
		candidateResult,
		EmptyAliasMap(),
	)
	child := NewPartialMatch(
		boundAliasMap,
		candidate,
		queryRef,
		queryExpression,
		candidateRef,
		NewRegularMatchInfo(
			parameterBindings,
			boundAliasMap,
			nil,
			nil,
			childMaxMatchMap,
			EmptyGroupByMappings(),
			nil,
			nil,
		),
	)
	if !AddPartialMatchForCandidate(queryRef, candidate, child) {
		t.Fatal("failed to seed semantically distinct child PartialMatch")
	}
	return child
}

type selectSemanticRouteOneLegSelect struct {
	child      expressions.RelationalExpression
	childRef   *expressions.Reference
	quantifier expressions.Quantifier
	selectExpr *expressions.SelectExpression
	selectRef  *expressions.Reference
}

func newSelectSemanticRouteOneLegSelect(
	t testing.TB,
	name string,
	recordType string,
	predicateBuilder func(
		expressions.Quantifier,
	) []predicates.QueryPredicate,
) *selectSemanticRouteOneLegSelect {
	t.Helper()
	child := mustMatchScan(t,
		[]string{recordType},
		matchRuleRowType(),
	)
	childRef := mustMatchInitial(t, child)
	quantifier := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(name),
		childRef,
	)
	var queryPredicates []predicates.QueryPredicate
	if predicateBuilder != nil {
		queryPredicates = predicateBuilder(quantifier)
	}
	selectExpr := mustMatchSelect(t,
		mustMatchFlowed(t, quantifier),
		[]expressions.Quantifier{quantifier},
		queryPredicates,
	)
	return &selectSemanticRouteOneLegSelect{
		child:      child,
		childRef:   childRef,
		quantifier: quantifier,
		selectExpr: selectExpr,
		selectRef:  mustMatchInitial(t, selectExpr),
	}
}

func selectSemanticRouteMatch(
	query *selectSemanticRouteOneLegSelect,
	candidate MatchCandidate,
	candidateSelectRef *expressions.Reference,
	candidateSelect *expressions.SelectExpression,
) {
	matchIntermediateWithCandidate(
		NewExpressionRuleCall(query.selectRef, nil, nil),
		query.selectExpr,
		candidate,
		candidateSelectRef,
		candidateSelect,
	)
}

func (f *selectSemanticRouteExactSubsetFixture) seedExactChildren(
	t *testing.T,
) {
	t.Helper()
	selectSemanticRouteSeedChild(
		t,
		f.candidate,
		f.queryForEachRef,
		f.candidateForEachRef,
		EmptyAliasMap(),
		nil,
	)
	selectSemanticRouteSeedChild(
		t,
		f.candidate,
		f.queryExistentialRef,
		f.candidateExistentialRef,
		EmptyAliasMap(),
		nil,
	)
}

func (f *selectSemanticRouteExactSubsetFixture) match() {
	matchIntermediateWithCandidate(
		NewExpressionRuleCall(f.querySelectRef, nil, nil),
		f.querySelect,
		f.candidate,
		f.candidateSelectRef,
		f.candidateSelect,
	)
}

// The complete structural mapping and the FE-only semantic mapping are both
// valid here. The query existential is deliberately unowned, which rejects it
// from Select subsumption while leaving exact structural equality valid. This
// makes the semantic subset observably different from the exact parent.
func TestMatchIntermediateSelectSemantic_ExactDoesNotSuppressSubset(
	t *testing.T,
) {
	fixture := newSelectSemanticRouteExactSubsetFixture(t,
		"idx_select_exact_and_subset",
	)
	fixture.seedExactChildren(t)
	fixture.match()

	parents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(parents) != 2 {
		t.Fatalf(
			"exact + semantic subset parents = %d, want exactly 2",
			len(parents),
		)
	}

	var exact, subset *PartialMatchImpl
	for _, rawParent := range parents {
		parent := rawParent.(*PartialMatchImpl)
		switch len(parent.GetMatchedQuantifiers()) {
		case 2:
			if exact != nil {
				t.Fatal("duplicate exact Select route survived")
			}
			exact = parent
		case 1:
			if subset != nil {
				t.Fatal("duplicate semantic Select adapter route survived")
			}
			subset = parent
		default:
			t.Fatalf(
				"parent matched %d quantifiers, want exact 2 or subset 1",
				len(parent.GetMatchedQuantifiers()),
			)
		}
	}
	if exact == nil || subset == nil {
		t.Fatalf("exact/subset parents = %p/%p, want both", exact, subset)
	}
	if got := exact.GetRegularMatchInfo().GetChildPartialMatchMaybe(
		fixture.queryExistential.GetAlias(),
	); got == nil {
		t.Fatal("exact parent did not retain its existential child")
	}
	if got := subset.GetRegularMatchInfo().GetChildPartialMatchMaybe(
		fixture.queryExistential.GetAlias(),
	); got != nil {
		t.Fatal("semantic subset unexpectedly retained the unowned existential")
	}
	if unmatched := subset.GetUnmatchedQuantifiers(); len(unmatched) != 1 ||
		unmatched[0].GetAlias() != fixture.queryExistential.GetAlias() {
		t.Fatalf(
			"semantic subset unmatched quantifiers = %v, want only query existential",
			unmatched,
		)
	}
	if !subset.CompensationCanBeDeferred() {
		t.Fatal("an unmatched existential incorrectly changed row cardinality")
	}

	firstParents := append([]PartialMatch(nil), parents...)
	fixture.match()
	secondParents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(secondParents) != len(firstParents) {
		t.Fatalf(
			"Select refire grew parents to %d, want stable %d",
			len(secondParents),
			len(firstParents),
		)
	}
	for index := range firstParents {
		if secondParents[index] != firstParents[index] {
			t.Fatalf("Select refire replaced or reordered parent %d", index)
		}
	}
}

func TestMatchIntermediateSelectSemantic_ExactPredicateProductIsNotDuplicated(
	t *testing.T,
) {
	buildPredicate := func(
		quantifier expressions.Quantifier,
	) []predicates.QueryPredicate {
		return []predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				mustMatchField(t, mustMatchFlowed(t, quantifier), "key"),
				predicates.NewLiteralComparison(
					predicates.ComparisonEquals,
					int64(11),
				),
			),
		}
	}
	query := newSelectSemanticRouteOneLegSelect(t,
		"exact_predicate_query_fe",
		"T",
		buildPredicate,
	)
	candidateLeg := newSelectSemanticRouteOneLegSelect(t,
		"exact_predicate_candidate_fe",
		"T",
		buildPredicate,
	)
	candidate := &testMatchCandidate{
		name:      "idx_select_exact_predicate",
		traversal: NewTraversal(candidateLeg.selectRef),
	}
	selectSemanticRouteSeedChild(
		t,
		candidate,
		query.childRef,
		candidateLeg.childRef,
		EmptyAliasMap(),
		nil,
	)

	selectSemanticRouteMatch(
		query,
		candidate,
		candidateLeg.selectRef,
		candidateLeg.selectExpr,
	)
	parents := GetPartialMatchesForExpression(query.selectRef, query.selectExpr)
	if len(parents) != 1 {
		t.Fatalf(
			"structurally exact Select parents = %d, want one structural parent",
			len(parents),
		)
	}
	parent := parents[0].(*PartialMatchImpl)
	if predicateMap := parent.GetRegularMatchInfo().GetPredicateMap(); predicateMap != nil {
		t.Fatalf(
			"exact parent predicate map = %v, want structural representation",
			predicateMap,
		)
	}

	selectSemanticRouteMatch(
		query,
		candidate,
		candidateLeg.selectRef,
		candidateLeg.selectExpr,
	)
	if got := len(GetPartialMatchesForExpression(
		query.selectRef,
		query.selectExpr,
	)); got != 1 {
		t.Fatalf("exact Select refire retained %d parents, want stable one", got)
	}
}

// The exact route owns the first tier of the shared result budget. Once 64
// distinct exact child products are admitted, the wrapper must stop rather
// than page to the 65th exact product or start the viable FE-only semantic
// subset with a fresh allowance.
func TestMatchIntermediateSelectSemantic_SharedBudgetStopsAfterExactTier(
	t *testing.T,
) {
	fixture := newSelectSemanticRouteExactSubsetFixture(t,
		"idx_select_exact_budget",
	)
	parameter := values.NamedCorrelationIdentifier("select_exact_budget_parameter")
	children := make([]*PartialMatchImpl, 0, matchIntermediateMaxUniqueResults+1)
	for i := 0; i < matchIntermediateMaxUniqueResults+1; i++ {
		child := selectSemanticRouteSeedChild(
			t,
			fixture.candidate,
			fixture.queryForEachRef,
			fixture.candidateForEachRef,
			EmptyAliasMap(),
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameter: matchIntermediateStructuralEqualityRange(
					t,
					int64(i),
				),
			},
		)
		children = append(children, child)
	}
	selectSemanticRouteSeedChild(
		t,
		fixture.candidate,
		fixture.queryExistentialRef,
		fixture.candidateExistentialRef,
		EmptyAliasMap(),
		nil,
	)

	fixture.match()
	parents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(parents) != matchIntermediateMaxUniqueResults {
		t.Fatalf(
			"parents at shared result cap = %d, want %d",
			len(parents),
			matchIntermediateMaxUniqueResults,
		)
	}
	for index, rawParent := range parents {
		parent := rawParent.(*PartialMatchImpl)
		if got := len(parent.GetMatchedQuantifiers()); got != 2 {
			t.Fatalf(
				"parent %d matched %d quantifiers, want exact-tier 2",
				index,
				got,
			)
		}
		if got := parent.GetRegularMatchInfo().GetChildPartialMatchMaybe(
			fixture.queryForEach.GetAlias(),
		); got != children[index] {
			t.Fatalf(
				"parent %d selected child %p, want deterministic exact child %p",
				index,
				got,
				children[index],
			)
		}
	}

	firstParents := append([]PartialMatch(nil), parents...)
	fixture.match()
	secondParents := GetPartialMatchesForExpression(
		fixture.querySelectRef,
		fixture.querySelect,
	)
	if len(secondParents) != len(firstParents) {
		t.Fatalf(
			"budgeted refire stored %d parents, want stable %d",
			len(secondParents),
			len(firstParents),
		)
	}
	for index := range firstParents {
		if secondParents[index] != firstParents[index] {
			t.Fatalf("budgeted refire paged or replaced parent %d", index)
		}
	}
}

// This route cannot use exact structural equality because only the query has a
// predicate. The semantic parent must therefore expose all of the metadata its
// later compensation/cardinality consumers need: the original residual
// predicate, the selected FE child, the unmatched existential, and a parent
// MaxMatchMap computed after the child translation moved the result into
// candidate scope.
func TestMatchIntermediateSelectSemantic_EmitsResidualCardinalityAndResultState(
	t *testing.T,
) {
	queryScan := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	queryScanRef := mustMatchInitial(t, queryScan)
	queryForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("result_state_query_fe"),
		queryScanRef,
	)
	queryExistsRef := mustMatchInitial(t,
		mustMatchScan(t,
			[]string{"U"},
			matchRuleRowType(),
		),
	)
	queryExists := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("result_state_query_e"),
		queryExistsRef,
	)
	queryResult := mustMatchField(t, mustMatchFlowed(t, queryForEach), "projected")
	queryPredicate := predicates.NewComparisonPredicate(
		mustMatchField(t, mustMatchFlowed(t, queryForEach), "filtered"),
		predicates.NewLiteralComparison(
			predicates.ComparisonEquals,
			int64(42),
		),
	)
	querySelect := mustMatchSelect(t,
		queryResult,
		[]expressions.Quantifier{
			queryForEach,
			queryExists,
		},
		[]predicates.QueryPredicate{queryPredicate},
	)
	querySelectRef := mustMatchInitial(t, querySelect)

	candidateScan := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	candidateScanRef := mustMatchInitial(t, candidateScan)
	candidateForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("result_state_candidate_fe"),
		candidateScanRef,
	)
	candidateExistsRef := mustMatchInitial(t,
		mustMatchScan(t,
			[]string{"U"},
			matchRuleRowType(),
		),
	)
	candidateExists := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("result_state_candidate_e"),
		candidateExistsRef,
	)
	candidateResult := mustMatchField(t, mustMatchFlowed(t, candidateForEach), "projected")
	candidateSelect := mustMatchSelect(t,
		candidateResult,
		[]expressions.Quantifier{
			candidateForEach,
			candidateExists,
		},
		nil,
	)
	candidateSelectRef := mustMatchInitial(t, candidateSelect)
	candidate := &testMatchCandidate{
		name:      "idx_select_result_state",
		traversal: NewTraversal(candidateSelectRef),
	}
	selectedChild := selectSemanticRouteSeedChild(
		t,
		candidate,
		queryScanRef,
		candidateScanRef,
		EmptyAliasMap(),
		nil,
	)

	matchIntermediateWithCandidate(
		NewExpressionRuleCall(querySelectRef, nil, nil),
		querySelect,
		candidate,
		candidateSelectRef,
		candidateSelect,
	)
	parents := GetPartialMatchesForExpression(querySelectRef, querySelect)
	if len(parents) != 1 {
		t.Fatalf("semantic result-state parents = %d, want 1", len(parents))
	}
	parent := parents[0].(*PartialMatchImpl)
	matchInfo := parent.GetRegularMatchInfo()

	if got := matchInfo.GetChildPartialMatchMaybe(
		queryForEach.GetAlias(),
	); got != selectedChild {
		t.Fatalf("selected FE child = %p, want %p", got, selectedChild)
	}
	if got := matchInfo.GetChildPartialMatchMaybe(
		queryExists.GetAlias(),
	); got != nil {
		t.Fatal("unowned query existential unexpectedly acquired a child")
	}
	if matched := parent.GetMatchedQuantifiers(); len(matched) != 1 ||
		matched[0].GetAlias() != queryForEach.GetAlias() {
		t.Fatalf("matched quantifiers = %v, want only query FE", matched)
	}
	if unmatched := parent.GetUnmatchedQuantifiers(); len(unmatched) != 1 ||
		unmatched[0].GetAlias() != queryExists.GetAlias() {
		t.Fatalf("unmatched quantifiers = %v, want only query existential", unmatched)
	}
	if !parent.CompensationCanBeDeferred() {
		t.Fatal("unmatched existential incorrectly blocks compensation deferral")
	}

	predicateMap := matchInfo.GetPredicateMap()
	if predicateMap == nil ||
		predicateMap.PredicateCount() != 1 ||
		predicateMap.Size() != 1 {
		t.Fatalf(
			"residual predicate map = %v (%d predicates/%d mappings), want 1/1",
			predicateMap,
			func() int {
				if predicateMap == nil {
					return 0
				}
				return predicateMap.PredicateCount()
			}(),
			func() int {
				if predicateMap == nil {
					return 0
				}
				return predicateMap.Size()
			}(),
		)
	}
	mappings := predicateMap.Get(queryPredicate)
	if len(mappings) != 1 {
		t.Fatalf("query residual mappings = %d, want 1", len(mappings))
	}
	residualMapping := mappings[0]
	if residualMapping.GetOriginalQueryPredicate() != queryPredicate ||
		!predicates.IsTautology(residualMapping.GetCandidatePredicate()) {
		t.Fatal("semantic parent did not retain the original query as a TRUE residual")
	}
	residualCompensation := residualMapping.GetPredicateCompensation()(
		parent,
		nil,
		nil,
	)
	if !residualCompensation.IsNeeded() ||
		residualCompensation.IsImpossible() {
		t.Fatalf(
			"residual compensation = needed:%v impossible:%v, want needed/possible",
			residualCompensation.IsNeeded(),
			residualCompensation.IsImpossible(),
		)
	}

	maxMatchMap := matchInfo.GetMaxMatchMap()
	if maxMatchMap == nil || maxMatchMap.Size() == 0 {
		t.Fatal("composed child translation produced no parent MaxMatchMap")
	}
	if got := values.ExplainValue(
		maxMatchMap.GetCandidateValue(),
	); got != values.ExplainValue(candidateResult) {
		t.Fatalf(
			"MaxMatchMap candidate result = %s, want %s",
			got,
			values.ExplainValue(candidateResult),
		)
	}
	translatedCorrelations := values.GetCorrelatedToOfValue(
		maxMatchMap.GetQueryValue(),
	)
	if _, anchored := translatedCorrelations[candidateForEach.GetAlias()]; !anchored {
		t.Fatalf(
			"translated query result correlations = %v, want candidate FE %s",
			translatedCorrelations,
			candidateForEach.GetAlias().Name(),
		)
	}
	if _, stale := translatedCorrelations[queryForEach.GetAlias()]; stale {
		t.Fatal("parent MaxMatchMap retained the pre-translation query FE alias")
	}
}

func TestMatchIntermediateSelectSemantic_ExistentialToForEachMarksDistinctRepair(
	t *testing.T,
) {
	queryBase := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	queryBaseRef := mustMatchInitial(t, queryBase)
	queryForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("e_to_fe_route_query_fe"),
		queryBaseRef,
	)
	queryFanout := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	queryFanoutRef := mustMatchInitial(t, queryFanout)
	queryExistential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("e_to_fe_route_query_e"),
		queryFanoutRef,
	)
	querySelect := mustMatchSelect(t,
		mustMatchFlowed(t, queryForEach),
		[]expressions.Quantifier{queryForEach, queryExistential},
		[]predicates.QueryPredicate{
			mustExistentialAlias(t, queryExistential.GetAlias()),
		},
	)
	querySelectRef := mustMatchInitial(t, querySelect)

	candidateBase := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	candidateBaseRef := mustMatchInitial(t, candidateBase)
	candidateForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("e_to_fe_route_candidate_fe"),
		candidateBaseRef,
	)
	candidateFanout := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	candidateFanoutRef := mustMatchInitial(t, candidateFanout)
	candidateFanoutForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("e_to_fe_route_candidate_fanout"),
		candidateFanoutRef,
	)
	candidateSelect := mustMatchSelect(t,
		mustMatchFlowed(t, candidateForEach),
		[]expressions.Quantifier{candidateForEach, candidateFanoutForEach},
		nil,
	)
	candidateSelectRef := mustMatchInitial(t, candidateSelect)
	scanPlan := mustMatchScanPlan(t,
		[]string{"T"},
		matchRuleRowType(),
		false,
	)
	scanPlan = scanPlan.WithPrimaryKey([]values.Value{
		mustMatchField(t, scanPlan.GetResultValue(), "ID"),
	})
	candidate := &selectSemanticFixedPlanCandidate{
		testMatchCandidate: &testMatchCandidate{
			name:      "idx_select_e_to_fe_distinct_repair",
			traversal: NewTraversal(candidateSelectRef),
		},
		plan: scanPlan,
	}
	baseChild := selectSemanticRouteSeedChild(
		t,
		candidate,
		queryBaseRef,
		candidateBaseRef,
		EmptyAliasMap(),
		nil,
	)
	fanoutChild := selectSemanticRouteSeedChild(
		t,
		candidate,
		queryFanoutRef,
		candidateFanoutRef,
		EmptyAliasMap(),
		nil,
	)

	matchIntermediateWithCandidate(
		NewExpressionRuleCall(querySelectRef, nil, nil),
		querySelect,
		candidate,
		candidateSelectRef,
		candidateSelect,
	)
	parents := GetPartialMatchesForExpression(querySelectRef, querySelect)
	if len(parents) != 1 {
		t.Fatalf("E-to-ForEach semantic parents = %d, want 1", len(parents))
	}
	matchInfo := parents[0].GetRegularMatchInfo()
	if !matchInfo.RequiresPrimaryKeyDistinct() {
		t.Fatal("E-to-ForEach semantic mapping lost its PK-distinct repair obligation")
	}
	if got := matchInfo.GetChildPartialMatchMaybe(
		queryForEach.GetAlias(),
	); got != baseChild {
		t.Fatalf("base child = %p, want %p", got, baseChild)
	}
	if got := matchInfo.GetChildPartialMatchMaybe(
		queryExistential.GetAlias(),
	); got != fanoutChild {
		t.Fatalf("fanout child = %p, want %p", got, fanoutChild)
	}

	// Carry the semantic match through the real single-data-access path. The
	// compensation must survive as a required logical Unique, then lower to
	// an executable primary-key distinct plan over the exact PK-proven scan.
	dataAccesses := DataAccessForMatchPartition(
		[]*properties.RequestedOrdering{properties.PreserveOrdering()},
		[]PartialMatch{parents[0]},
		EmptyPlanContext(),
		nil,
	)
	if len(dataAccesses) != 1 {
		t.Fatalf("E-to-ForEach data accesses = %d, want 1", len(dataAccesses))
	}
	unique, ok := dataAccesses[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("compensated data access = %T, want LogicalUniqueExpression", dataAccesses[0])
	}
	if !unique.IsRequired() {
		t.Fatal("E-to-ForEach data access emitted an absorbable Unique")
	}
	if got := unique.GetInner().GetAlias(); got != queryForEach.GetAlias() {
		t.Fatalf("compensated Unique alias = %q, want %q", got.Name(), queryForEach.GetAlias().Name())
	}

	innerRef := unique.GetInner().GetRangesOver()
	planProperties := NewPlanPropertiesMap()
	for _, member := range innerRef.AllMembers() {
		physical, ok := member.(physicalPlanExpression)
		if !ok {
			t.Fatalf("required Unique input = %T, want physical plan", member)
		}
		planProperties.Add(physical)
	}
	innerRef.SetPlanProperties(planProperties)

	implemented := mustFireImplementationRule(t,
		NewImplementUniqueRule(),
		mustMatchInitial(t, unique),
	)
	if len(implemented) != 1 {
		t.Fatalf("required Unique implementations = %d, want 1", len(implemented))
	}
	distinctPlan, ok := implemented[0].(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
	if !ok {
		t.Fatalf("required Unique implementation = %T, want PK-distinct plan", implemented[0])
	}
	if got := distinctPlan.GetInner(); got != scanPlan {
		t.Fatalf("PK-distinct child = %T, want exact semantic candidate scan", got)
	}
}

func TestMatchIntermediateSelectSemantic_ExistentialToExistentialNeedsNoDistinctRepair(
	t *testing.T,
) {
	queryBase := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	queryBaseRef := mustMatchInitial(t, queryBase)
	queryForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("e_to_e_route_query_fe"),
		queryBaseRef,
	)
	queryExists := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	queryExistsRef := mustMatchInitial(t, queryExists)
	queryExistential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("e_to_e_route_query_e"),
		queryExistsRef,
	)
	querySelect := mustMatchSelect(t,
		mustMatchFlowed(t, queryForEach),
		[]expressions.Quantifier{queryForEach, queryExistential},
		[]predicates.QueryPredicate{
			mustExistentialAlias(t, queryExistential.GetAlias()),
		},
	)
	querySelectRef := mustMatchInitial(t, querySelect)

	candidateBase := mustMatchScan(t,
		[]string{"T"},
		matchRuleRowType(),
	)
	candidateBaseRef := mustMatchInitial(t, candidateBase)
	candidateForEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("e_to_e_route_candidate_fe"),
		candidateBaseRef,
	)
	candidateExists := mustMatchScan(t,
		[]string{"U"},
		matchRuleRowType(),
	)
	candidateExistsRef := mustMatchInitial(t, candidateExists)
	candidateExistential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("e_to_e_route_candidate_e"),
		candidateExistsRef,
	)
	candidateSelect := mustMatchSelect(t,
		mustMatchFlowed(t, candidateForEach),
		[]expressions.Quantifier{candidateForEach, candidateExistential},
		nil,
	)
	candidateSelectRef := mustMatchInitial(t, candidateSelect)
	candidate := &testMatchCandidate{
		name:      "idx_select_e_to_e_no_distinct_repair",
		traversal: NewTraversal(candidateSelectRef),
	}
	selectSemanticRouteSeedChild(
		t,
		candidate,
		queryBaseRef,
		candidateBaseRef,
		EmptyAliasMap(),
		nil,
	)
	existentialChild := selectSemanticRouteSeedChild(
		t,
		candidate,
		queryExistsRef,
		candidateExistsRef,
		EmptyAliasMap(),
		nil,
	)

	matchIntermediateWithCandidate(
		NewExpressionRuleCall(querySelectRef, nil, nil),
		querySelect,
		candidate,
		candidateSelectRef,
		candidateSelect,
	)
	parents := GetPartialMatchesForExpression(querySelectRef, querySelect)
	if len(parents) == 0 {
		t.Fatal("ordinary E-to-E semantic mapping produced no parent")
	}
	var eToEParent PartialMatch
	for _, parent := range parents {
		if parent.GetRegularMatchInfo().GetChildPartialMatchMaybe(
			queryExistential.GetAlias(),
		) == existentialChild {
			eToEParent = parent
			break
		}
	}
	if eToEParent == nil {
		t.Fatal("ordinary E-to-E child product was not retained")
	}
	if eToEParent.GetRegularMatchInfo().RequiresPrimaryKeyDistinct() {
		t.Fatal("ordinary E-to-E mapping acquired a PK-distinct repair obligation")
	}
}

// A one-leg Select with a comparison used to reach the legacy pass-through
// adapter. The general semantic consumer now owns this shape. Pin the complete
// route, including the original-predicate identity and sargable range carried
// by the emitted parent, so removing the adapter cannot silently remove
// correlated join-inner index probes.
func TestMatchIntermediateSelectSemantic_BindsPlaceholderWithoutLegacyAdapter(
	t *testing.T,
) {
	parameter := values.NamedCorrelationIdentifier(
		"semantic_placeholder_parameter",
	)
	var queryPredicate *predicates.ComparisonPredicate
	query := newSelectSemanticRouteOneLegSelect(t,
		"semantic_placeholder_query_fe",
		"T",
		func(
			queryQuantifier expressions.Quantifier,
		) []predicates.QueryPredicate {
			queryPredicate = predicates.NewComparisonPredicate(
				mustMatchField(t, mustMatchFlowed(t, queryQuantifier), "key"),
				predicates.NewLiteralComparison(
					predicates.ComparisonEquals,
					int64(17),
				),
			)
			return []predicates.QueryPredicate{queryPredicate}
		},
	)
	var candidatePlaceholder *predicates.Placeholder
	candidateLeg := newSelectSemanticRouteOneLegSelect(t,
		"semantic_placeholder_candidate_fe",
		"T",
		func(
			candidateQuantifier expressions.Quantifier,
		) []predicates.QueryPredicate {
			candidatePlaceholder = predicates.NewPlaceholder(
				parameter,
				mustMatchField(t, mustMatchFlowed(t, candidateQuantifier), "key"),
			)
			return []predicates.QueryPredicate{candidatePlaceholder}
		},
	)
	candidate := &testMatchCandidate{
		name:      "idx_select_semantic_placeholder",
		traversal: NewTraversal(candidateLeg.selectRef),
	}
	selectedChild := selectSemanticRouteSeedChild(
		t,
		candidate,
		query.childRef,
		candidateLeg.childRef,
		EmptyAliasMap(),
		nil,
	)

	selectSemanticRouteMatch(
		query,
		candidate,
		candidateLeg.selectRef,
		candidateLeg.selectExpr,
	)
	parents := GetPartialMatchesForExpression(query.selectRef, query.selectExpr)
	if len(parents) != 1 {
		t.Fatalf(
			"general Select placeholder parents = %d, want exactly 1",
			len(parents),
		)
	}
	parent := parents[0].(*PartialMatchImpl)
	matchInfo := parent.GetRegularMatchInfo()
	if got := matchInfo.GetChildPartialMatchMaybe(
		query.quantifier.GetAlias(),
	); got != selectedChild {
		t.Fatalf("placeholder parent child = %p, want %p", got, selectedChild)
	}

	expectedRange := matchIntermediateStructuralEqualityRange(t, int64(17))
	boundRange := matchInfo.GetParameterBindingMap()[parameter]
	if !partialMatchComparisonRangesEqual(boundRange, expectedRange) {
		t.Fatalf("placeholder binding = %v, want equality 17", boundRange)
	}
	predicateMap := matchInfo.GetPredicateMap()
	if predicateMap == nil {
		t.Fatal("placeholder parent has no predicate map")
	}
	mappings := predicateMap.Get(queryPredicate)
	if len(mappings) != 1 {
		t.Fatalf("placeholder mappings for original query = %d, want 1", len(mappings))
	}
	mapping := mappings[0]
	if mapping.GetOriginalQueryPredicate() != queryPredicate ||
		mapping.GetCandidatePredicate() != candidatePlaceholder ||
		mapping.GetTranslatedQueryPredicate() == nil {
		t.Fatal("placeholder mapping lost original/translated/candidate predicate state")
	}
	if alias := mapping.GetParameterAlias(); alias == nil ||
		*alias != parameter {
		t.Fatalf("mapping parameter alias = %v, want %s", alias, parameter.Name())
	}
	if !partialMatchComparisonRangesEqual(
		mapping.GetComparisonRange(),
		expectedRange,
	) {
		t.Fatalf(
			"mapping comparison range = %v, want equality 17",
			mapping.GetComparisonRange(),
		)
	}
	compensation := mapping.GetPredicateCompensation()(
		parent,
		parent.GetBoundParameterPrefixMap(),
		nil,
	)
	if compensation.IsNeeded() || compensation.IsImpossible() {
		t.Fatalf(
			"bound placeholder compensation = needed:%v impossible:%v, want neither",
			compensation.IsNeeded(),
			compensation.IsImpossible(),
		)
	}
}

func TestMatchIntermediateSelectSemantic_FlattensTranslatedAndConjuncts(
	t *testing.T,
) {
	parameter := values.NamedCorrelationIdentifier("and_join_parameter")
	outerAlias := values.NamedCorrelationIdentifier("and_join_outer")
	var joinKey, residual *predicates.ComparisonPredicate
	var topLevelAnd *predicates.AndPredicate
	query := newSelectSemanticRouteOneLegSelect(t,
		"and_join_query_fe",
		"T",
		func(
			queryQuantifier expressions.Quantifier,
		) []predicates.QueryPredicate {
			joinKey = predicates.NewComparisonPredicate(
				mustMatchField(t, mustMatchFlowed(t, queryQuantifier), "join_key"),
				predicates.Comparison{
					Type: predicates.ComparisonEquals,
					Operand: mustMatchField(t,
						mustMatchQOV(t, outerAlias, matchRuleRowType()), "probe_key"),
				},
			)
			residual = predicates.NewComparisonPredicate(
				mustMatchField(t, mustMatchFlowed(t, queryQuantifier), "payload"),
				predicates.NewLiteralComparison(
					predicates.ComparisonGreaterThan,
					int64(3),
				),
			)
			topLevelAnd = predicates.NewAnd(joinKey, residual)
			return []predicates.QueryPredicate{topLevelAnd}
		},
	)

	var candidatePlaceholder *predicates.Placeholder
	candidateLeg := newSelectSemanticRouteOneLegSelect(t,
		"and_join_candidate_fe",
		"T",
		func(
			candidateQuantifier expressions.Quantifier,
		) []predicates.QueryPredicate {
			candidatePlaceholder = predicates.NewPlaceholder(
				parameter,
				mustMatchField(t, mustMatchFlowed(t, candidateQuantifier), "join_key"),
			)
			// Candidate Selects can retain the same top-level AND shape.
			return []predicates.QueryPredicate{
				predicates.NewAnd(candidatePlaceholder),
			}
		},
	)
	candidate := &testMatchCandidate{
		name:      "idx_select_and_join",
		traversal: NewTraversal(candidateLeg.selectRef),
	}
	selectSemanticRouteSeedChild(
		t,
		candidate,
		query.childRef,
		candidateLeg.childRef,
		EmptyAliasMap(),
		nil,
	)

	selectSemanticRouteMatch(
		query,
		candidate,
		candidateLeg.selectRef,
		candidateLeg.selectExpr,
	)
	parents := GetPartialMatchesForExpression(query.selectRef, query.selectExpr)
	if len(parents) != 1 {
		t.Fatalf("top-level AND semantic parents = %d, want 1", len(parents))
	}
	parent := parents[0].(*PartialMatchImpl)
	matchInfo := parent.GetRegularMatchInfo()

	boundRange := matchInfo.GetParameterBindingMap()[parameter]
	if boundRange == nil || !boundRange.IsEquality() {
		t.Fatalf("correlated join-key binding = %v, want equality", boundRange)
	}
	predicateMap := matchInfo.GetPredicateMap()
	if predicateMap == nil ||
		predicateMap.PredicateCount() != 2 ||
		predicateMap.Size() != 2 {
		t.Fatalf(
			"flattened predicate map = %v (%d predicates/%d mappings), want 2/2",
			predicateMap,
			func() int {
				if predicateMap == nil {
					return 0
				}
				return predicateMap.PredicateCount()
			}(),
			func() int {
				if predicateMap == nil {
					return 0
				}
				return predicateMap.Size()
			}(),
		)
	}
	if mappings := predicateMap.Get(topLevelAnd); len(mappings) != 0 {
		t.Fatalf("top-level AND retained %d mappings, want leaf mappings", len(mappings))
	}
	joinMappings := predicateMap.Get(joinKey)
	if len(joinMappings) != 1 ||
		joinMappings[0].GetCandidatePredicate() != candidatePlaceholder {
		t.Fatal("correlated join-key leaf did not bind the candidate placeholder")
	}
	residualMappings := predicateMap.Get(residual)
	if len(residualMappings) != 1 ||
		!predicates.IsTautology(
			residualMappings[0].GetCandidatePredicate(),
		) {
		t.Fatal("residual leaf was not retained through a TRUE mapping")
	}

	compensation := parent.CompensateCompleteMatch(
		nil,
		values.NamedCorrelationIdentifier("and_join_candidate_top"),
	)
	if compensation.IsImpossible() || !compensation.IsNeeded() {
		t.Fatalf(
			"top-level AND compensation = needed:%v impossible:%v, want needed/possible",
			compensation.IsNeeded(),
			compensation.IsImpossible(),
		)
	}
	forMatch, ok := compensation.(*ForMatchCompensation)
	if !ok {
		t.Fatalf(
			"top-level AND compensation = %T, want *ForMatchCompensation",
			compensation,
		)
	}
	predicateCompensation := forMatch.GetPredicateCompensationMap()
	if predicateCompensation.Get(joinKey) != nil ||
		predicateCompensation.Get(residual) == nil ||
		predicateCompensation.Get(topLevelAnd) != nil {
		t.Fatal("compensation did not retain exactly the residual AND leaf")
	}
	applied, ok := forMatch.Apply(candidateLeg.child, nil)
	if !ok || applied == nil {
		t.Fatal("possible top-level AND compensation could not be applied")
	}
	filter, ok := applied.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("applied compensation = %T, want residual LogicalFilter", applied)
	}
	appliedPredicates := filter.GetPredicates()
	if len(appliedPredicates) != 1 || appliedPredicates[0] != residual {
		t.Fatalf(
			"applied residuals = %v, want the original residual leaf",
			appliedPredicates,
		)
	}
}

// Every case reaches the public routing wrapper with a seeded child match. The
// invalidity is therefore owned by one of the Select semantic gates, not by a
// test that stopped before matchIntermediateWithCandidate.
func TestMatchIntermediateSelectSemantic_RejectsUnsafeMappingsAtRoute(
	t *testing.T,
) {
	t.Run("candidate ForEach must be fully covered", func(t *testing.T) {
		query := newSelectSemanticRouteOneLegSelect(t,
			"coverage_query_fe",
			"T",
			nil,
		)

		candidateTRef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"T"},
				matchRuleRowType(),
			),
		)
		candidateURef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"U"},
				matchRuleRowType(),
			),
		)
		candidateT := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("coverage_candidate_t"),
			candidateTRef,
		)
		candidateU := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("coverage_candidate_u"),
			candidateURef,
		)
		candidateSelect := mustMatchSelect(t,
			mustMatchFlowed(t, candidateT),
			[]expressions.Quantifier{candidateT, candidateU},
			nil,
		)
		candidateSelectRef := mustMatchInitial(t, candidateSelect)
		candidate := &testMatchCandidate{
			name:      "idx_select_incomplete_fe",
			traversal: NewTraversal(candidateSelectRef),
		}
		selectSemanticRouteSeedChild(
			t,
			candidate,
			query.childRef,
			candidateTRef,
			EmptyAliasMap(),
			nil,
		)

		selectSemanticRouteMatch(
			query,
			candidate,
			candidateSelectRef,
			candidateSelect,
		)
		if got := len(GetPartialMatchesForExpression(
			query.selectRef,
			query.selectExpr,
		)); got != 0 {
			t.Fatalf("incomplete candidate FE coverage emitted %d parents", got)
		}
	})

	t.Run("unmatched candidate existential cannot filter", func(t *testing.T) {
		query := newSelectSemanticRouteOneLegSelect(t,
			"filtering_exists_query_fe",
			"T",
			nil,
		)
		candidateTRef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"T"},
				matchRuleRowType(),
			),
		)
		candidateERef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"U"},
				matchRuleRowType(),
			),
		)
		candidateForEach := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(
				"filtering_exists_candidate_fe",
			),
			candidateTRef,
		)
		candidateExists := expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier(
				"filtering_exists_candidate_e",
			),
			candidateERef,
		)
		candidateSelect := mustMatchSelect(t,
			mustMatchFlowed(t, candidateForEach),
			[]expressions.Quantifier{
				candidateForEach,
				candidateExists,
			},
			[]predicates.QueryPredicate{
				mustExistentialAlias(t,
					candidateExists.GetAlias(),
				),
			},
		)
		candidateSelectRef := mustMatchInitial(t, candidateSelect)
		candidate := &testMatchCandidate{
			name:      "idx_select_filtering_exists",
			traversal: NewTraversal(candidateSelectRef),
		}
		selectSemanticRouteSeedChild(
			t,
			candidate,
			query.childRef,
			candidateTRef,
			EmptyAliasMap(),
			nil,
		)

		selectSemanticRouteMatch(
			query,
			candidate,
			candidateSelectRef,
			candidateSelect,
		)
		if got := len(GetPartialMatchesForExpression(
			query.selectRef,
			query.selectExpr,
		)); got != 0 {
			t.Fatalf("filtering unmatched existential emitted %d parents", got)
		}
	})

	t.Run("selected candidate leg cannot depend on unmatched existential", func(t *testing.T) {
		query := newSelectSemanticRouteOneLegSelect(t,
			"dependency_query_fe",
			"T",
			nil,
		)
		candidateExistsRef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"U"},
				matchRuleRowType(),
			),
		)
		candidateExists := expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier("dependency_candidate_e"),
			candidateExistsRef,
		)
		dependentScanRef := mustMatchInitial(t,
			mustMatchScan(t,
				[]string{"T"},
				matchRuleRowType(),
			),
		)
		dependentFilter := mustMatchFilter(t,
			[]predicates.QueryPredicate{
				mustExistentialAlias(t,
					candidateExists.GetAlias(),
				),
			},
			expressions.ForEachQuantifier(dependentScanRef),
		)
		dependentFilterRef := mustMatchInitial(t, dependentFilter)
		candidateForEach := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("dependency_candidate_fe"),
			dependentFilterRef,
		)
		candidateSelect := mustMatchSelect(t,
			mustMatchFlowed(t, candidateForEach),
			[]expressions.Quantifier{
				candidateForEach,
				candidateExists,
			},
			nil,
		)
		candidateSelectRef := mustMatchInitial(t, candidateSelect)
		candidate := &testMatchCandidate{
			name:      "idx_select_unmatched_dependency",
			traversal: NewTraversal(candidateSelectRef),
		}
		selectSemanticRouteSeedChild(
			t,
			candidate,
			query.childRef,
			dependentFilterRef,
			EmptyAliasMap(),
			nil,
		)

		selectSemanticRouteMatch(
			query,
			candidate,
			candidateSelectRef,
			candidateSelect,
		)
		if got := len(GetPartialMatchesForExpression(
			query.selectRef,
			query.selectExpr,
		)); got != 0 {
			t.Fatalf("unmatched existential dependency emitted %d parents", got)
		}
	})

	t.Run("conflicting child parameter binding rejects parent", func(t *testing.T) {
		parameter := values.NamedCorrelationIdentifier(
			"child_conflict_parameter",
		)
		query := newSelectSemanticRouteOneLegSelect(t,
			"child_conflict_query_fe",
			"T",
			func(
				queryQuantifier expressions.Quantifier,
			) []predicates.QueryPredicate {
				return []predicates.QueryPredicate{
					predicates.NewComparisonPredicate(
						mustMatchField(t, mustMatchFlowed(t, queryQuantifier), "key"),
						predicates.NewLiteralComparison(
							predicates.ComparisonEquals,
							int64(9),
						),
					),
				}
			},
		)
		candidateLeg := newSelectSemanticRouteOneLegSelect(t,
			"child_conflict_candidate_fe",
			"T",
			func(
				candidateQuantifier expressions.Quantifier,
			) []predicates.QueryPredicate {
				return []predicates.QueryPredicate{
					predicates.NewPlaceholder(
						parameter,
						mustMatchField(t, mustMatchFlowed(t, candidateQuantifier), "key"),
					),
				}
			},
		)
		candidate := &testMatchCandidate{
			name:      "idx_select_child_conflict",
			traversal: NewTraversal(candidateLeg.selectRef),
		}
		conflictingChild := selectSemanticRouteSeedChild(
			t,
			candidate,
			query.childRef,
			candidateLeg.childRef,
			EmptyAliasMap(),
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameter: matchIntermediateStructuralEqualityRange(
					t,
					int64(7),
				),
			},
		)
		compatibleRange := matchIntermediateStructuralEqualityRange(
			t,
			int64(9),
		)
		compatibleChild := selectSemanticRouteSeedChild(
			t,
			candidate,
			query.childRef,
			candidateLeg.childRef,
			EmptyAliasMap(),
			map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				parameter: compatibleRange,
			},
		)

		selectSemanticRouteMatch(
			query,
			candidate,
			candidateLeg.selectRef,
			candidateLeg.selectExpr,
		)
		parents := GetPartialMatchesForExpression(
			query.selectRef,
			query.selectExpr,
		)
		if len(parents) != 1 {
			t.Fatalf(
				"child-conflict branches emitted %d parents, want later compatible branch only",
				len(parents),
			)
		}
		parent := parents[0].(*PartialMatchImpl)
		retainedChild := parent.GetRegularMatchInfo().
			GetChildPartialMatchMaybe(query.quantifier.GetAlias())
		if retainedChild != compatibleChild || retainedChild == conflictingChild {
			t.Fatalf(
				"retained child = %p, want compatible %p and not conflicting %p",
				retainedChild,
				compatibleChild,
				conflictingChild,
			)
		}
		if got := parent.GetRegularMatchInfo().
			GetParameterBindingMap()[parameter]; !partialMatchComparisonRangesEqual(
			got,
			compatibleRange,
		) {
			t.Fatalf("merged parent parameter range = %v, want p=9", got)
		}
	})
}
