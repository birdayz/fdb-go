package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func pushFilterCountGroupBy(
	t testing.TB,
	groupingKeyName string,
	aggregateOperandName string,
) *expressions.GroupByExpression {
	t.Helper()
	scan := mustPushFilterScan(t, []string{"T"})
	scanQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	scanRow := mustPushFilterFlowed(t, scanQ)
	return mustPushFilterGroupBy(t,
		[]values.Value{mustPushFilterField(t, scanRow, groupingKeyName)},
		[]expressions.AggregateSpec{{
			Function:       expressions.AggCount,
			Operand:        mustPushFilterField(t, scanRow, aggregateOperandName),
			Alias:          "TOTAL",
			OperandName:    aggregateOperandName,
			OperandIntType: values.TypeCodeLong,
		}},
		scanQ,
	)
}

func requirePushFilterGroupByInnerAliases(
	t testing.TB,
	groupBy *expressions.GroupByExpression,
	wantKey string,
	wantFunction expressions.AggregateFunction,
	wantOperand string,
) {
	t.Helper()
	want := groupBy.GetInner().GetAlias()
	keys := groupBy.GetGroupingKeys()
	if len(keys) != 1 {
		t.Fatalf("grouping keys = %d, want 1", len(keys))
	}
	if !values.AccessorNamePathMatchesNames(keys[0], []string{wantKey}) {
		t.Fatalf("grouping key = %v, want accessor path [%s]", keys[0], wantKey)
	}
	requirePushFilterValueAlias(t, keys[0], want)
	aggregates := groupBy.GetAggregates()
	if len(aggregates) != 1 || aggregates[0].Operand == nil {
		t.Fatalf("aggregates = %v, want one aggregate with an operand", aggregates)
	}
	if aggregates[0].Function != wantFunction || aggregates[0].Alias != "TOTAL" {
		t.Fatalf("aggregate = {%v alias %q}, want {%v alias %q}",
			aggregates[0].Function, aggregates[0].Alias, wantFunction, "TOTAL")
	}
	if aggregates[0].OperandName != wantOperand || aggregates[0].OperandIntType != values.TypeCodeLong {
		t.Fatalf("aggregate metadata = {operand name %q, integer type %v}, want {%q, %v}",
			aggregates[0].OperandName, aggregates[0].OperandIntType, wantOperand, values.TypeCodeLong)
	}
	if !values.AccessorNamePathMatchesNames(aggregates[0].Operand, []string{wantOperand}) {
		t.Fatalf("aggregate operand = %v, want accessor path [%s]", aggregates[0].Operand, wantOperand)
	}
	requirePushFilterValueAlias(t, aggregates[0].Operand, want)
}

func TestPushFilterThroughGroupBy_AllPredsOnGroupKeys(t *testing.T) {
	t.Parallel()

	groupBy := pushFilterCountGroupBy(t, "Region", "ID")
	groupByQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, groupBy))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, groupByQ), "Region",
		predicates.ComparisonEquals, "US")
	filter := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, groupByQ)

	results := mustFireExpressionRule(t, NewPushFilterThroughGroupByRule(),
		mustPushFilterInitial(t, filter))
	if len(results) != 1 {
		t.Fatalf("PushFilterThroughGroupByRule yielded %d expressions, want 1", len(results))
	}

	// The yielded expression is GroupBy(Filter(Scan)).  Both the pushed
	// predicate and the GroupBy's own key/aggregate values must read the
	// quantifier immediately below their respective operator.
	newGroupBy, ok := results[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("yielded %T, want *GroupByExpression", results[0])
	}
	innerFilter, ok := newGroupBy.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("group-by inner = %T, want *LogicalFilterExpression", newGroupBy.GetInner().GetRangesOver().Get())
	}
	if len(innerFilter.GetPredicates()) != 1 {
		t.Fatalf("pushed predicates = %d, want 1", len(innerFilter.GetPredicates()))
	}
	requirePushFilterComparison(t, innerFilter.GetPredicates()[0], []string{"REGION"},
		predicates.ComparisonEquals, "US")
	requirePushFilterPredicateAlias(t, innerFilter.GetPredicates()[0], innerFilter.GetInner().GetAlias())
	requirePushFilterGroupByInnerAliases(t, newGroupBy, "REGION", expressions.AggCount, "ID")
}

func TestPushFilterThroughGroupBy_PredOnNonKey_DoesNotFire(t *testing.T) {
	t.Parallel()

	groupBy := pushFilterCountGroupBy(t, "Region", "ID")
	groupByQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, groupBy))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, groupByQ), "TOTAL",
		predicates.ComparisonGreaterThan, int64(10))
	filter := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, groupByQ)

	results := mustFireExpressionRule(t, NewPushFilterThroughGroupByRule(),
		mustPushFilterInitial(t, filter))
	if len(results) != 0 {
		t.Fatalf("PushFilterThroughGroupByRule yielded %d expressions for a non-key predicate, want 0", len(results))
	}
}

func TestPushFilterThroughGroupBy_PartialPushdown(t *testing.T) {
	t.Parallel()

	groupBy := pushFilterCountGroupBy(t, "Region", "ID")
	groupByQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, groupBy))
	groupByRow := mustPushFilterFlowed(t, groupByQ)
	keyPredicate := pushFilterComparison(t, groupByRow, "Region",
		predicates.ComparisonEquals, "US")
	aggregatePredicate := pushFilterComparison(t, groupByRow, "TOTAL",
		predicates.ComparisonGreaterThan, int64(100))
	filter := mustPushFilterLogicalFilter(t,
		[]predicates.QueryPredicate{keyPredicate, aggregatePredicate}, groupByQ)

	results := mustFireExpressionRule(t, NewPushFilterThroughGroupByRule(),
		mustPushFilterInitial(t, filter))
	if len(results) != 1 {
		t.Fatalf("PushFilterThroughGroupByRule yielded %d expressions for partial pushdown, want 1", len(results))
	}

	// Result: Filter(total > 100, GroupBy(region, ..., Filter(region = 'US', Scan))).
	outerFilter, ok := results[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want outer *LogicalFilterExpression", results[0])
	}
	residual := outerFilter.GetPredicates()
	if len(residual) != 1 {
		t.Fatalf("residual predicates = %d, want 1", len(residual))
	}
	requirePushFilterComparison(t, residual[0], []string{"TOTAL"},
		predicates.ComparisonGreaterThan, int64(100))
	requirePushFilterPredicateAlias(t, residual[0], outerFilter.GetInner().GetAlias())

	innerGroupBy, ok := outerFilter.GetInner().GetRangesOver().Get().(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("outer filter inner = %T, want *GroupByExpression", outerFilter.GetInner().GetRangesOver().Get())
	}
	innerFilter, ok := innerGroupBy.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("group-by inner = %T, want *LogicalFilterExpression", innerGroupBy.GetInner().GetRangesOver().Get())
	}
	pushed := innerFilter.GetPredicates()
	if len(pushed) != 1 {
		t.Fatalf("pushed predicates = %d, want 1", len(pushed))
	}
	requirePushFilterComparison(t, pushed[0], []string{"REGION"},
		predicates.ComparisonEquals, "US")
	requirePushFilterPredicateAlias(t, pushed[0], innerFilter.GetInner().GetAlias())
	requirePushFilterGroupByInnerAliases(t, innerGroupBy, "REGION", expressions.AggCount, "ID")
}

func TestPushFilterThroughGroupBy_GroupingKeyPublishedVerbatim(t *testing.T) {
	t.Parallel()

	scan := mustPushFilterScan(t, []string{"T"})
	scanQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	scanRow := mustPushFilterFlowed(t, scanQ)
	groupBy := mustPushFilterGroupBy(t,
		[]values.Value{mustPushFilterField(t, scanRow, "Region")},
		[]expressions.AggregateSpec{{
			Function:       expressions.AggSum,
			Operand:        mustPushFilterField(t, scanRow, "AMOUNT"),
			Alias:          "TOTAL",
			OperandName:    "AMOUNT",
			OperandIntType: values.TypeCodeLong,
		}},
		scanQ,
	)
	groupByQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, groupBy))
	// THE GROUPING KEY IS PUBLISHED VERBATIM, so a reference to it resolves by
	// its own spelling.
	//
	// This arm was written the other way round: it asserted that a mixed-case
	// key request met an output authority that published REGION, and that
	// accessor identity bridged the two case-insensitively. The fold is gone —
	// AggregateKeyColumnName now takes the key value's name as it is — so the
	// bridge it exercised is no longer on this path at all, and an assertion
	// that a mixed-case reference still resolves would be asserting nothing.
	// What replaces it is the property that made the bridge unnecessary: the
	// published name is the key's name, and the reference resolves EXACTLY.
	groupByRow := mustPushFilterFlowed(t, groupByQ)
	if record, ok := groupByRow.Type().(*values.RecordType); ok {
		if record.Fields[0].Name != "Region" {
			t.Fatalf("grouping key published as %q, want the key's own spelling Region",
				record.Fields[0].Name)
		}
	} else {
		t.Fatalf("group-by flowed type = %T, want *values.RecordType", groupByRow.Type())
	}
	predicate := pushFilterComparison(t, groupByRow, "Region",
		predicates.ComparisonEquals, "EU")
	filter := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, groupByQ)

	results := mustFireExpressionRule(t, NewPushFilterThroughGroupByRule(),
		mustPushFilterInitial(t, filter))
	if len(results) != 1 {
		t.Fatalf("PushFilterThroughGroupByRule yielded %d expressions for case-insensitive key match, want 1", len(results))
	}
	newGroupBy, ok := results[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("yielded %T, want *GroupByExpression", results[0])
	}
	innerFilter, ok := newGroupBy.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok || len(innerFilter.GetPredicates()) != 1 {
		t.Fatalf("group-by inner = %T, want one-predicate *LogicalFilterExpression", newGroupBy.GetInner().GetRangesOver().Get())
	}
	requirePushFilterComparison(t, innerFilter.GetPredicates()[0], []string{"REGION"},
		predicates.ComparisonEquals, "EU")
	requirePushFilterPredicateAlias(t, innerFilter.GetPredicates()[0], innerFilter.GetInner().GetAlias())
	requirePushFilterGroupByInnerAliases(t, newGroupBy, "REGION", expressions.AggSum, "AMOUNT")
}

func TestPushFilterThroughGroupBy_ConstantPredNotPushed(t *testing.T) {
	t.Parallel()

	groupBy := pushFilterCountGroupBy(t, "X", "Y")
	groupByQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, groupBy))
	// A ConstantPredicate references no grouping column, so it remains above the
	// aggregate (RFC-166). Pushing an eliminating constant below a scalar
	// aggregate would change empty-group semantics.
	filter := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	}, groupByQ)

	results := mustFireExpressionRule(t, NewPushFilterThroughGroupByRule(),
		mustPushFilterInitial(t, filter))
	if len(results) != 0 {
		t.Fatalf("PushFilterThroughGroupByRule pushed a constant predicate; got %d results, want 0", len(results))
	}
}
