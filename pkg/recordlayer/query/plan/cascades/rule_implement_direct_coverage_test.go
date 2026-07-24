package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementLimitRule_DirectFire(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	limit := expressions.NewLogicalLimitExpression(
		7,
		3,
		expressions.ForEachQuantifier(expressions.FinalOf(scan)),
	)

	yielded := FireExpressionRule(NewImplementLimitRule(), expressions.InitialOf(limit))
	if len(yielded) != 1 {
		t.Fatalf("expected one physical LIMIT, got %d", len(yielded))
	}
	physical, ok := yielded[0].(*plans.RecordQueryLimitPlan)
	if !ok {
		t.Fatalf("yielded %T, want *plans.RecordQueryLimitPlan", yielded[0])
	}
	if physical.GetLimit() != 7 || physical.GetOffset() != 3 {
		t.Fatalf(
			"physical LIMIT = limit %d offset %d, want limit 7 offset 3",
			physical.GetLimit(),
			physical.GetOffset(),
		)
	}
	if physical.GetInner() != plans.RecordQueryPlan(scan) {
		t.Fatalf("physical LIMIT inner = %T, want the seeded scan", physical.GetInner())
	}
}

func TestImplementInMemorySortRule_DirectFire(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	innerRef := expressions.FinalOf(scan)
	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{
			Value:   values.NewFieldValueWithResolvedOrdinal("ID", 0, values.NullableLong),
			Reverse: true,
		}},
		expressions.ForEachQuantifier(innerRef),
	)
	constraints := NewConstraintMap()

	yielded := FireImplementationRule(
		NewImplementInMemorySortRule(),
		expressions.InitialOf(sortExpr),
		constraints,
	)
	if len(yielded) != 1 {
		t.Fatalf("expected one in-memory sort, got %d", len(yielded))
	}
	physical, ok := yielded[0].(*plans.RecordQueryInMemorySortPlan)
	if !ok {
		t.Fatalf("yielded %T, want *plans.RecordQueryInMemorySortPlan", yielded[0])
	}
	if physical.GetInner() != plans.RecordQueryPlan(scan) {
		t.Fatalf("in-memory sort inner = %T, want the seeded scan", physical.GetInner())
	}
	keys := physical.GetSortKeys()
	if len(keys) != 1 || !keys[0].Desc || keys[0].ValueExpr == nil {
		t.Fatalf("in-memory sort keys = %#v, want one baked descending key", keys)
	}
	pushed, ok := Get(constraints, innerRef, RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("requested ordering was not pushed to the inner scan: %#v", pushed)
	}
}
