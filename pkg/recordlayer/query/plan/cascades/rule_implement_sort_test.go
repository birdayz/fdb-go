package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementSortRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	keys := []expressions.SortKey{{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}}
	sort := expressions.NewLogicalSortExpression(keys, expressions.ForEachQuantifier(innerRef))
	topRef := expressions.InitialOf(sort)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementSortRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementSortRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalSortWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalSortWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if got := len(plan.GetSortKeys()); got != 1 {
		t.Fatalf("sort plan has %d keys, want 1", got)
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("sort inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

func TestPlannerWithFullBatchA_SortFilterScanChain(t *testing.T) {
	t.Parallel()
	// End-to-end through Planner: Sort(k, Filter(P, Scan)) with all
	// 3 Batch A rules converges to a Reference with a Sort-of-Filter-
	// of-Scan physical plan member.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	keys := []expressions.SortKey{{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}}
	sort := expressions.NewLogicalSortExpression(
		keys,
		expressions.ForEachQuantifier(expressions.InitialOf(filter)),
	)
	ref := expressions.InitialOf(sort)

	rules := []ExpressionRule{
		NewPrimaryScanRule(),
		NewImplementFilterRule(),
		NewImplementSortRule(),
	}
	p := NewPlanner(rules, nil)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// Look for a physicalSortWrapper.
	foundPhysSort := false
	for _, m := range ref.Members() {
		if _, ok := m.(*physicalSortWrapper); ok {
			foundPhysSort = true
			break
		}
	}
	if !foundPhysSort {
		t.Fatalf("planner did not produce a physical SortPlan after Batch A rules; %d members", len(ref.Members()))
	}
}
