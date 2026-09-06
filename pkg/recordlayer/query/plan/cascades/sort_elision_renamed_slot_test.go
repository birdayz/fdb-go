package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestSortElisionCrossesARenamingProjection pins that a sort over a
// projection's RENAMED slot is elided when the projection's source already
// provides that order: `ORDER BY u.h` over `(SELECT status AS h FROM t) u`
// reaches the sorted source as STATUS. The delegator walk once carried the
// request through the projection untranslated, so the output name H was
// matched against the source's key STATUS and every renamed column kept an
// in-memory sort over an input already in that order; the same query with the
// column unrenamed elided, because the two names happened to coincide.
//
// Both seams are driven: the rule-time winner lookup (OrderedChildWinnerForSort
// through requestedOrderingBelow) and extraction's spine rebuild
// (ExtractBestPlanFromSelector).
func TestSortElisionCrossesARenamingProjection(t *testing.T) {
	t.Parallel()

	sorted := referenceWinnerSortedMemberOn(t, "STATUS")
	sortedRef := expressions.InitialOf(sorted)
	projectionQ := expressions.ForEachQuantifier(sortedRef)
	renaming := mustReferenceWinnerConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{referenceWinnerQuantifiedField(t, projectionQ, 0)},
		[]string{"H"},
		projectionQ,
	))
	rowType := renaming.GetResultValue().Type()
	record, ok := rowType.(*values.RecordType)
	if !ok || len(record.Fields) != 1 || record.Fields[0].Name != "H" {
		t.Fatalf("the projection must publish its slot under the alias H; got %v", rowType)
	}
	projectionRef := expressions.InitialOf(renaming)
	projectionRef.InsertFinal(renaming)
	projectionRef.SetWinner(renaming)

	sortQ := expressions.ForEachQuantifier(projectionRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, sortQ, 0)},
		},
		sortQ,
	))
	sortRef := expressions.InitialOf(sort)
	sortRef.SetWinner(sort)

	p := NewPlanner(DefaultExpressionRules(), nil)
	if w := p.OrderedChildWinnerForSort(sort, projectionRef); w != renaming {
		t.Fatalf("the renaming projection over a STATUS-sorted source must satisfy ORDER BY H: the request crosses the projection's result value as STATUS; got %T", w)
	}

	plan, err := ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if _, isSort := plan.(*expressions.LogicalSortExpression); isSort || plan == nil {
		t.Fatalf("the sort must be ELIDED over the renaming projection; got %T", plan)
	}
	if _, ok := plan.(*plans.RecordQueryProjectionPlan); !ok {
		t.Fatalf("expected the elided root to be the projection, got %T", plan)
	}
}

// TestSortElisionDeclinesAComputedSlot pins the other half: a request over a
// slot the projection COMPUTES cannot be restated in the source's row, so the
// walk declines and the sort stays — never a satisfaction claimed on a name.
func TestSortElisionDeclinesAComputedSlot(t *testing.T) {
	t.Parallel()

	sorted := referenceWinnerSortedMemberOn(t, "STATUS")
	sortedRef := expressions.InitialOf(sorted)
	projectionQ := expressions.ForEachQuantifier(sortedRef)
	status := referenceWinnerQuantifiedField(t, projectionQ, 0)
	computed := mustReferenceWinnerConstruct(plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{&values.ArithmeticValue{Op: values.OpAdd, Left: status, Right: status}},
		[]string{"H"},
		projectionQ,
	))
	projectionRef := expressions.InitialOf(computed)
	projectionRef.InsertFinal(computed)
	projectionRef.SetWinner(computed)

	sortQ := expressions.ForEachQuantifier(projectionRef)
	sort := mustReferenceWinnerConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: referenceWinnerQuantifiedField(t, sortQ, 0)},
		},
		sortQ,
	))
	if w := p0OrderedChildWinnerForSort(t, sort, projectionRef); w != nil {
		t.Fatalf("a computed slot has no order in the source's row; the sort must stay, got %T", w)
	}
}

func p0OrderedChildWinnerForSort(t *testing.T, sort *expressions.LogicalSortExpression, ref *expressions.Reference) expressions.RelationalExpression {
	t.Helper()
	return NewPlanner(DefaultExpressionRules(), nil).OrderedChildWinnerForSort(sort, ref)
}
