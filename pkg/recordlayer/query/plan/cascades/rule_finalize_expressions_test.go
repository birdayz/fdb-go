package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFinalizeExpressionsRule_DisentanglesChildPartitions(t *testing.T) {
	t.Parallel()
	var sources []*expressions.Reference
	var quantifiers []expressions.Quantifier
	for _, name := range []string{"T", "U"} {
		scan := memoTestScan(t, name)
		source := expressions.ExploratoryOfAtStage(scan, expressions.StageCanonical)
		child := expressions.FinalOfAtStage(scan, expressions.StageCanonical)
		child.ConstraintsMap().SetExplored()
		for _, predicate := range []predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
			predicates.NewNot(predicates.NewConstantPredicate(predicates.TriFalse)),
		} {
			source.InsertFinal(memoTestFilter(t, []predicates.QueryPredicate{predicate}, expressions.ForEachQuantifier(child)))
		}
		source.InsertFinal(scan)
		source.ConstraintsMap().SetExplored()
		sources = append(sources, source)
		quantifiers = append(quantifiers, expressions.ForEachNullOnEmptyQuantifier(source))
	}
	original, err := expressions.NewSelectExpressionWithJoinType(&values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}, quantifiers, nil, []string{"left", "right"}, expressions.JoinLeftOuter)
	original = mustConstruct(t, original, err)
	root := expressions.ExploratoryOfAtStage(original, expressions.StageCanonical)
	yielded := fireDirectImplementationRule(t, NewFinalizeExpressionsRule(), root)
	if len(yielded) != 4 {
		t.Fatalf("finalized %d alternatives, want the 2x2 child-property partition product", len(yielded))
	}
	seen := make(map[[2]bool]bool)
	for _, alternative := range yielded {
		if alternative == original {
			t.Fatal("finalization promoted the live exploratory object instead of rebuilding its edges")
		}
		var combination [2]bool
		for i, q := range alternative.GetQuantifiers() {
			restricted := q.GetRangesOver()
			if restricted == sources[i] || len(restricted.Members()) != 0 || restricted.Stage() != sources[i].Stage() || restricted.NeedsExploration() {
				t.Fatal("finalized child must be a detached, explored, same-stage finals-only reference")
			}
			if q.GetAlias() != quantifiers[i].GetAlias() || q.Kind() != quantifiers[i].Kind() || !q.IsNullOnEmpty() {
				t.Fatal("finalization changed quantifier bindings or flags")
			}
			members := restricted.FinalMembers()
			if len(members) == 0 {
				t.Fatal("empty partition")
			}
			_, combination[i] = members[0].(expressions.RelationalExpressionWithPredicates)
			wantCount := 1
			if combination[i] {
				wantCount = 2
			}
			if len(members) != wantCount {
				t.Fatalf("partition has %d members, want %d", len(members), wantCount)
			}
			for _, member := range members {
				_, mergeable := member.(expressions.RelationalExpressionWithPredicates)
				if mergeable != combination[i] || !isFinalMember(sources[i], member) {
					t.Fatal("partition mixed mergeability or included a non-final source member")
				}
			}
		}
		if seen[combination] {
			t.Fatal("duplicate partition combination")
		}
		seen[combination] = true
	}
	for i, source := range sources {
		source.ClearFinalMembers()
		for _, alternative := range yielded {
			if len(alternative.GetQuantifiers()[i].GetRangesOver().FinalMembers()) == 0 {
				t.Fatal("mutating the source invalidated a finalized child's retained alternatives")
			}
		}
	}
}

func TestFinalizeExpressionsRule_RequiresFinalizedChildren(t *testing.T) {
	t.Parallel()
	child := expressions.InitialOf(memoTestScan(t, "T"))
	filter := memoTestFilter(t, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}, expressions.ForEachQuantifier(child))
	root := expressions.InitialOf(filter)
	if yielded := fireDirectImplementationRule(t, NewFinalizeExpressionsRule(), root); len(yielded) != 0 {
		t.Fatal("finalized a parent with no finalized child partition")
	}
}

func TestFinalizeExpressionsRule_PromotesMatchedExpression(t *testing.T) {
	t.Parallel()

	scan := mustDirectCoverageConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, directCoverageRowType()))
	ref := expressions.InitialOf(scan)

	yielded := fireDirectImplementationRule(t, NewFinalizeExpressionsRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("expected one promoted expression, got %d", len(yielded))
	}
	if yielded[0] != scan {
		t.Fatalf("promoted %T, want the exact matched scan", yielded[0])
	}
	finals := ref.FinalMembers()
	if len(finals) != 1 || finals[0] != scan {
		t.Fatalf("final members = %#v, want the exact matched scan", finals)
	}
}
