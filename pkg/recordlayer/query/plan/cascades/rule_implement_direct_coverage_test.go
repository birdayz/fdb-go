package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func directCoverageRowType() *values.RecordType {
	return values.NewRecordType("DirectCoverageRow", false, []values.Field{{
		Name: "ID", FieldType: values.NullableLong,
	}})
}

func mustDirectCoverageConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct direct-rule fixture: " + err.Error())
	}
	return value
}

func directCoverageScan() *plans.RecordQueryScanPlan {
	return mustDirectCoverageConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, directCoverageRowType(), false))
}

func fireDirectExpressionRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func fireDirectImplementationRule(
	t testing.TB, rule ImplementationRule, ref *expressions.Reference,
	constraints ...*ConstraintMap,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireImplementationRule(rule, ref, constraints...)
	if err != nil {
		t.Fatalf("FireImplementationRule: %v", err)
	}
	return result
}

func TestImplementLimitRule_DirectFire(t *testing.T) {
	t.Parallel()

	scan := directCoverageScan()
	limit := mustDirectCoverageConstruct(expressions.NewLogicalLimitExpression(
		7,
		3,
		expressions.ForEachQuantifier(expressions.FinalOf(scan)),
	))

	yielded := fireDirectExpressionRule(t, NewImplementLimitRule(), expressions.InitialOf(limit))
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

	scan := directCoverageScan()
	innerRef := expressions.FinalOf(scan)
	innerQ := expressions.ForEachQuantifier(innerRef)
	innerRoot := mustDirectCoverageConstruct(innerQ.RequireFlowedObjectValue())
	id := mustDirectCoverageConstruct(values.ResolveFieldOrdinals(innerRoot, []int{0}))
	sortExpr := mustDirectCoverageConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{
			Value:   id,
			Reverse: true,
		}},
		innerQ,
	))
	constraints := NewConstraintMap()

	yielded := fireDirectImplementationRule(t,
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
