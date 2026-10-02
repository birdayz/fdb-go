package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
	assertProducerPhysicalQuantifiers(t, physical)
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
	assertProducerPhysicalQuantifiers(t, physical)
}

func assertProducerPhysicalQuantifiers(t testing.TB, expression expressions.RelationalExpression) {
	t.Helper()
	physical, ok := expression.(physicalPlanExpression)
	if !ok {
		t.Fatalf("producer yielded %T, want physical plan", expression)
	}
	plans.Walk(physical.GetRecordQueryPlan(), func(plan plans.RecordQueryPlan) bool {
		for i, q := range plan.GetQuantifiers() {
			if q.Kind() != expressions.QuantifierPhysical {
				t.Errorf("%T edge %d kind = %v, want Physical", plan, i, q.Kind())
			}
		}
		return true
	})
}

func TestPhysicalProducerQuantifiers_Implementations(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"projection", "projection_final", "insert", "type_filter", "temp_insert", "recursive_level", "recursive_dfs"} {
		t.Run(name, func(t *testing.T) {
			scan := directCoverageScan()
			q := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("producer_input"), expressions.FinalOf(scan))
			var logical expressions.RelationalExpression
			var rule ExpressionRule
			var finalRule ImplementationRule
			switch name {
			case "projection", "projection_final":
				root := mustDirectCoverageConstruct(q.RequireFlowedObjectValue())
				field := mustDirectCoverageConstruct(values.ResolveFieldOrdinals(root, []int{0}))
				logical = mustDirectCoverageConstruct(expressions.NewLogicalProjectionExpression([]values.Value{field}, q))
				if name == "projection" {
					rule = NewImplementProjectionRule()
				} else {
					finalRule = NewImplementProjectionFinalRule()
				}
			case "insert":
				logical = mustDirectCoverageConstruct(expressions.NewInsertExpression(q, "T", directCoverageRowType()))
				rule = NewImplementInsertRule()
			case "type_filter":
				logical = mustDirectCoverageConstruct(expressions.NewLogicalTypeFilterExpression([]string{"T"}, q))
				rule = NewImplementTypeFilterRule()
			case "temp_insert":
				logical = mustDirectCoverageConstruct(expressions.NewTempTableInsertExpression(q, values.NamedCorrelationIdentifier("producer_temp"), false))
				rule = NewImplementTempTableInsertRule()
			case "recursive_level", "recursive_dfs":
				other := expressions.ForEachQuantifier(expressions.FinalOf(directCoverageScan()))
				logical = mustDirectCoverageConstruct(expressions.NewRecursiveUnionExpressionDistinct(q, other,
					values.NamedCorrelationIdentifier("producer_scan"), values.NamedCorrelationIdentifier("producer_insert"), expressions.TraversalAny))
				if name == "recursive_level" {
					rule = NewImplementRecursiveLevelUnionRule()
				} else {
					rule = NewImplementRecursiveDfsJoinRule()
				}
			}
			var yielded []expressions.RelationalExpression
			if rule != nil {
				yielded = fireDirectExpressionRule(t, rule, expressions.InitialOf(logical))
			} else {
				yielded = fireDirectImplementationRule(t, finalRule, expressions.InitialOf(logical))
			}
			if len(yielded) != 1 {
				t.Fatalf("yielded %d plans, want 1", len(yielded))
			}
			assertProducerPhysicalQuantifiers(t, yielded[0])
			if logical.GetQuantifiers()[0].Kind() != expressions.QuantifierForEach {
				t.Fatal("implementation changed the logical input quantifier")
			}
			if name == "projection" && yielded[0].GetQuantifiers()[0].GetAlias() != q.GetAlias() {
				t.Fatal("projection lost the logical input alias")
			}
		})
	}
}

func TestPhysicalProducerQuantifiers_FetchRewrites(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"distinct", "map", "in_join", "projection", "projection_expression", "union_all", "union_residual"} {
		t.Run(name, func(t *testing.T) {
			scan := directCoverageScan()
			translate := func(v values.Value, _, _ values.CorrelationIdentifier) (values.Value, bool) {
				_, literal := v.(*values.ConstantValue)
				return v, literal
			}
			fetch := mustDirectCoverageConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(scan, translate, directCoverageRowType(), plans.FetchIndexRecordsPrimaryKey))
			var input expressions.RelationalExpression
			var rule ImplementationRule
			switch name {
			case "distinct":
				input = mustDirectCoverageConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(fetch))
				rule = NewPushDistinctThroughFetchRule()
			case "map":
				input = mustDirectCoverageConstruct(plans.NewRecordQueryMapPlan(fetch, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}))
				rule = NewPushMapThroughFetchRule()
			case "in_join":
				input = mustDirectCoverageConstruct(plans.NewRecordQueryInJoinPlanWithBindingAlias(fetch, values.NamedCorrelationIdentifier("producer_in"), true, true)).WithInValues([]any{int64(1), int64(2)})
				rule = NewPushInJoinThroughFetchRule()
			case "projection":
				input = mustDirectCoverageConstruct(plans.NewRecordQueryProjectionPlan([]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}}, fetch))
				rule = NewMergeProjectionAndFetchRule()
			case "projection_expression":
				q := expressions.ForEachQuantifier(expressions.FinalOf(fetch))
				input = mustDirectCoverageConstruct(expressions.NewLogicalProjectionExpression([]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}}, q))
				rule = NewMergeProjectionAndFetchRule()
			case "union_all", "union_residual":
				other := mustDirectCoverageConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(directCoverageScan(), nil, directCoverageRowType(), plans.FetchIndexRecordsPrimaryKey))
				inners := []plans.RecordQueryPlan{fetch, other}
				if name == "union_residual" {
					inners = append(inners, directCoverageScan())
				}
				input = mustDirectCoverageConstruct(plans.NewRecordQueryUnionPlan(inners))
				rule = NewPushUnionThroughFetchRule()
			}
			var yielded []expressions.RelationalExpression
			if name == "projection_expression" {
				yielded = fireDirectExpressionRule(t, NewImplementProjectionRule(), expressions.InitialOf(input))
			} else {
				yielded = fireDirectImplementationRule(t, rule, expressions.InitialOf(input))
			}
			if len(yielded) == 0 {
				t.Fatal("rewrite yielded no plans")
			}
			for _, result := range yielded {
				assertProducerPhysicalQuantifiers(t, result)
			}
		})
	}
}

func TestPhysicalProducerQuantifiers_DistinctModes(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		name := "hash"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			scan, field := pushDistinctScanAndField()
			var inner plans.RecordQueryPlan = scan
			if streaming {
				inner = mustDirectCoverageConstruct(plans.NewRecordQueryInMemorySortPlan(scan, []plans.SortKey{{ValueExpr: field}}))
			}
			call := &ImplementationRuleCall{}
			expr, err := newPhysicalDistinctFor(call, inner)
			if err != nil {
				t.Fatal(err)
			}
			distinct := expr.(*plans.RecordQueryDistinctPlan)
			if distinct.IsStreaming() != streaming || distinct.GetInner() != inner {
				t.Fatal("distinct changed its mode or selected input")
			}
			assertProducerPhysicalQuantifiers(t, distinct)
		})
	}
}

func TestPhysicalProducerQuantifiers_MemoEquality(t *testing.T) {
	t.Parallel()
	scan := directCoverageScan()
	logical := mustDirectCoverageConstruct(expressions.NewLogicalLimitExpression(7, 3, expressions.ForEachQuantifier(expressions.FinalOf(scan))))
	yielded := fireDirectExpressionRule(t, NewImplementLimitRule(), expressions.InitialOf(logical))
	if len(yielded) != 1 {
		t.Fatalf("yielded %d plans, want 1", len(yielded))
	}
	q := yielded[0].GetQuantifiers()[0]
	expected := mustDirectCoverageConstruct(plans.NewRecordQueryLimitPlanFromQuantifier(expressions.NamedPhysicalQuantifier(q.GetAlias(), q.GetRangesOver()), 7, 3, nil))
	if !expressions.MemoEqual(yielded[0], expected) {
		t.Fatal("rule-produced limit is not memo-equal to the same plan with a physical edge")
	}
}

func TestPhysicalProducerQuantifiers_DistinctBelowFilter(t *testing.T) {
	t.Parallel()
	scan := directCoverageScan()
	filter := mustDirectCoverageConstruct(plans.NewRecordQueryPredicatesFilterPlan(scan, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}))
	distinct := mustDirectCoverageConstruct(plans.NewRecordQueryDistinctPlan(filter))
	yielded := fireDirectImplementationRule(t, NewPushDistinctBelowFilterRule(), expressions.InitialOf(distinct))
	if len(yielded) != 1 {
		t.Fatalf("yielded %d plans, want 1", len(yielded))
	}
	assertProducerPhysicalQuantifiers(t, yielded[0])
}
