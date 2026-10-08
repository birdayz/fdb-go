package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func accessRealizationFixture(t *testing.T, duplicates, residual bool) (*SingleMatchedAccess, *ExpressionRuleCall) {
	t.Helper()
	aType := values.Type(values.NotNullLong)
	if duplicates {
		aType = values.NewArrayType(false, values.NotNullLong)
	}
	row := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "A", FieldType: aType},
		{Name: "B", FieldType: values.NotNullLong},
	})
	candidate := NewValueIndexScanMatchCandidateWithFunctions("idx_a", []string{"T"}, []string{"A"},
		nil, []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}, row,
		false, []string{"ID"}, &duplicates).WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
	if duplicates {
		candidate.WithRootKeyExpression(keyExpressionField("A", gen.Field_FAN_OUT))
	}
	ctx := testPlanContextForMatching{candidates: []MatchCandidate{candidate}}
	leaf := expressions.ExploratoryOfAtStage(mustMatchScan(t, []string{"T"}, row), expressions.StagePlanned)
	q := expressions.ForEachQuantifier(leaf)
	qs := []expressions.Quantifier{q}
	var ps []predicates.QueryPredicate
	if duplicates {
		explode := expressions.ExploratoryOfAtStage(mustMatchExplode(t,
			mustMatchField(t, mustMatchFlowed(t, q), "A")), expressions.StagePlanned)
		eq := expressions.ForEachQuantifier(explode)
		inner := expressions.ExploratoryOfAtStage(mustMatchSelect(t, mustMatchFlowed(t, eq),
			[]expressions.Quantifier{eq}, []predicates.QueryPredicate{
				predicates.NewComparisonPredicate(mustMatchFlowed(t, eq), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
			}), expressions.StagePlanned)
		qs = append(qs, expressions.ForEachQuantifier(inner))
		mustFireExpressionRuleWithMemo(t, NewMatchLeafRule(), explode, ctx, nil)
		mustFireExpressionRuleWithMemo(t, NewMatchIntermediateRule(), inner, ctx, nil)
	} else {
		ps = append(ps, predicates.NewComparisonPredicate(mustMatchField(t, mustMatchFlowed(t, q), "A"), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
	}
	if residual {
		ps = append(ps, predicates.NewComparisonPredicate(mustMatchField(t, mustMatchFlowed(t, q), "B"), predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0))))
	}
	query := mustMatchSelect(t, mustMatchFlowed(t, q), qs, ps)
	matched := expressions.ExploratoryOfAtStage(query, expressions.StagePlanned)
	mustFireExpressionRuleWithMemo(t, NewMatchLeafRule(), leaf, ctx, nil)
	mustFireExpressionRuleWithMemo(t, NewMatchIntermediateRule(), matched, ctx, nil)
	AdjustPartialMatchesForRef(matched)
	accesses := PrepareMatchesAndCompensations(completeMatchesForCandidate(matched, candidate),
		[]*properties.RequestedOrdering{properties.PreserveOrdering()}, ctx)
	if len(accesses) == 0 {
		t.Fatal("no complete access")
	}
	for _, access := range accesses {
		// Java refuses compensation over two matched ForEach quantifiers.
		if access.GetCompensation().IsImpossible() != (duplicates && residual) || access.GetCompensation().IsNeeded() != residual {
			t.Fatalf("expected complete access with residual=%t: %v", residual, access)
		}
	}
	return accesses[0], &ExpressionRuleCall{Reference: matched, memo: NewMemo(matched)}
}

func TestAccessRealizationsMatchDirectionAndScope(t *testing.T) {
	t.Parallel()
	access, memoizer := accessRealizationFixture(t, false, true)
	realizations := make(accessRealizations)
	forward := realizations.realize(access)
	if forward.expression == nil || forward.scan == nil {
		t.Fatal("missing forward scan")
	}
	assertAccessScanDirection(t, forward.scan, false)
	if realizations.realize(access) != forward {
		t.Fatal("same match and direction reconstructed its scan")
	}
	reverseAccess := NewSingleMatchedAccess(access.GetPartialMatch(), access.GetCompensation(),
		access.GetCandidateTopAlias(), true, access.GetTopToTopTranslationMap(), access.GetSatisfyingRequestedOrderings())
	reverse := realizations.realize(reverseAccess)
	if reverse == forward || reverse.scan == nil {
		t.Fatal("reverse access reused a forward scan")
	}
	assertAccessScanDirection(t, reverse.scan, true)
	pm := access.GetPartialMatch().(*PartialMatchImpl)
	otherMatch := NewPartialMatch(pm.GetBoundAliasMap(), pm.GetMatchCandidate(), pm.GetQueryRef(),
		pm.GetQueryExpression(), pm.GetCandidateRef(), pm.GetMatchInfo())
	otherAccess := NewSingleMatchedAccess(otherMatch, access.GetCompensation(), access.GetCandidateTopAlias(),
		false, access.GetTopToTopTranslationMap(), access.GetSatisfyingRequestedOrderings())
	if other := realizations.realize(otherAccess); other == forward || other.expression == forward.expression {
		t.Fatal("independent match was interned into another access")
	}
	if independent := make(accessRealizations).realize(access); independent.expression == forward.expression {
		t.Fatal("physical access escaped its consumption-local scope")
	}
	compensated := realizations.single(memoizer, access)
	if compensated == nil || compensated == forward.expression {
		t.Fatal("residual compensation did not construct its filter")
	}
	groups := len(memoizer.memo.References())
	if repeated := realizations.single(memoizer, access); repeated != compensated || len(memoizer.memo.References()) != groups {
		t.Fatal("singleton bookkeeping reapplied compensation and registered another subtree")
	}
	if len(realizations) != 3 {
		t.Fatalf("realizations=%d, want forward, reverse and independent match", len(realizations))
	}
}

func assertAccessScanDirection(t *testing.T, scan plans.RecordQueryPlan, reverse bool) {
	t.Helper()
	fetch, ok := scan.(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("access scan is %T, want fetch", scan)
	}
	index, ok := fetch.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("fetch child is %T, want index scan", fetch.GetInner())
	}
	if index.IsReverse() != reverse {
		t.Fatalf("index scan %v, want reverse=%t", index, reverse)
	}
}

func TestAccessRealizationsDistinctAndCompensation(t *testing.T) {
	t.Parallel()
	for _, duplicates := range []bool{false, true} {
		for _, residual := range []bool{false, true} {
			t.Run(map[bool]string{false: "plain", true: "fanout"}[duplicates]+"/"+map[bool]string{false: "exact", true: "residual"}[residual], func(t *testing.T) {
				t.Parallel()
				access, memoizer := accessRealizationFixture(t, duplicates, residual)
				realizations := make(accessRealizations)
				scan := realizations.realize(access).expression
				distinct := realizations.distinctPlan(access)
				if distinct == nil || realizations.distinctPlan(access) != distinct {
					t.Fatal("intersection rebuilt its distinct access")
				}
				if duplicates {
					plan, ok := distinct.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
					if !ok || plan.GetQuantifiers()[0].GetRangesOver().FinalMembers()[0] != scan {
						t.Fatal("fanout intersection must deduplicate the original access")
					}
				} else if distinct != scan {
					t.Fatal("non-fanout intersection rebuilt its access")
				}
				compensated := realizations.single(memoizer, access)
				if duplicates && residual {
					if compensated != nil {
						t.Fatal("impossible multi-quantifier compensation produced a singleton")
					}
				} else if compensated == nil || (compensated != scan) != residual {
					t.Fatal("singleton compensation was lost or added unnecessarily")
				}
				impossible := NewSingleMatchedAccess(access.GetPartialMatch(), ImpossibleCompensation,
					access.GetCandidateTopAlias(), false, access.GetTopToTopTranslationMap(), nil)
				failed := make(accessRealizations)
				for i := range 2 {
					if failed.single(memoizer, impossible) != nil {
						t.Fatalf("impossible compensation exposed its uncompensated scan on call %d", i+1)
					}
				}
				if failed.realize(impossible).expression == nil {
					t.Fatal("impossible singleton must retain its scan for a possible compensated intersection")
				}
			})
		}
	}
}
