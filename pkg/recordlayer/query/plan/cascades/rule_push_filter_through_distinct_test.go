package cascades

import (
	"context"
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// pushFilterTestRowType is the exact row shared by the Distinct, GroupBy, and
// TypeFilter pushdown fixtures.  The legacy tests used UNKNOWN rows and
// childless/name-only FieldValues, so they could not state which quantifier a
// rewritten predicate reads.  A common exact descriptor makes both the input
// slot and every post-rewrite correlation observable.
func pushFilterTestRowType() *values.RecordType {
	return values.NewRecordType("PUSH_FILTER_ROW", false, []values.Field{
		{Name: "Region", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "AMOUNT", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 3},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 4},
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 5},
		{Name: "J", FieldType: values.NotNullLong, Ordinal: 6},
	})
}

func mustPushFilterScan(
	t testing.TB,
	recordTypes []string,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression(recordTypes, pushFilterTestRowType())
	return mustConstruct(t, scan, err)
}

func mustPushFilterInitial(
	t testing.TB,
	expression expressions.RelationalExpression,
) *expressions.Reference {
	t.Helper()
	reference, err := InitialOf(expression)
	return mustConstruct(t, reference, err)
}

func mustPushFilterFlowed(
	t testing.TB,
	quantifier expressions.Quantifier,
) values.QuantifiedObjectValue {
	t.Helper()
	flowed, err := quantifier.RequireFlowedObjectValue()
	return mustConstruct(t, flowed, err)
}

func mustPushFilterQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, pushFilterTestRowType())
	return mustConstruct(t, qov, err)
}

func mustPushFilterField(
	t testing.TB,
	root values.Value,
	name string,
) values.Value {
	t.Helper()
	request, err := values.FieldByName(name)
	request = mustConstruct(t, request, err)
	field, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	return mustConstruct(t, field, err)
}

func mustPushFilterLogicalFilter(
	t testing.TB,
	queryPredicates []predicates.QueryPredicate,
	inner expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	filter, err := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	return mustConstruct(t, filter, err)
}

func mustPushFilterDistinct(
	t testing.TB,
	inner expressions.Quantifier,
) *expressions.LogicalDistinctExpression {
	t.Helper()
	distinct, err := expressions.NewLogicalDistinctExpression(inner)
	return mustConstruct(t, distinct, err)
}

func mustPushFilterTypeFilter(
	t testing.TB,
	recordTypes []string,
	inner expressions.Quantifier,
) *expressions.LogicalTypeFilterExpression {
	t.Helper()
	typeFilter, err := expressions.NewLogicalTypeFilterExpression(recordTypes, inner)
	return mustConstruct(t, typeFilter, err)
}

func mustPushFilterGroupBy(
	t testing.TB,
	groupingKeys []values.Value,
	aggregates []expressions.AggregateSpec,
	inner expressions.Quantifier,
) *expressions.GroupByExpression {
	t.Helper()
	groupBy, err := expressions.NewGroupByExpression(groupingKeys, aggregates, inner)
	return mustConstruct(t, groupBy, err)
}

func pushFilterComparison(
	t testing.TB,
	root values.Value,
	fieldName string,
	comparisonType predicates.ComparisonType,
	literal any,
) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		mustPushFilterField(t, root, fieldName),
		predicates.NewLiteralComparison(comparisonType, literal),
	)
}

func requirePushFilterPredicateAlias(
	t testing.TB,
	predicate predicates.QueryPredicate,
	want values.CorrelationIdentifier,
) {
	t.Helper()
	correlated := predicates.GetCorrelatedToOfPredicate(predicate)
	if len(correlated) != 1 {
		t.Fatalf("predicate correlations = %v, want only %q", correlated, want.Name())
	}
	if _, ok := correlated[want]; !ok {
		t.Fatalf("predicate correlations = %v, want only %q", correlated, want.Name())
	}
}

func requirePushFilterComparison(
	t testing.TB,
	predicate predicates.QueryPredicate,
	wantPath []string,
	wantType predicates.ComparisonType,
	wantLiteral any,
) {
	t.Helper()
	comparison, ok := predicate.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("predicate = %T, want *ComparisonPredicate", predicate)
	}
	if !values.AccessorNamePathMatchesNames(comparison.Operand, wantPath) {
		t.Fatalf("comparison operand = %v, want accessor path %v", comparison.Operand, wantPath)
	}
	if comparison.Comparison.Type != wantType {
		t.Fatalf("comparison type = %v, want %v", comparison.Comparison.Type, wantType)
	}
	if comparison.Comparison.Operand == nil {
		t.Fatalf("comparison RHS = nil, want literal %#v", wantLiteral)
	}
	gotLiteral, err := comparison.Comparison.Operand.Evaluate(nil)
	if err != nil {
		t.Fatalf("evaluate comparison literal: %v", err)
	}
	if !reflect.DeepEqual(gotLiteral, wantLiteral) {
		t.Fatalf("comparison literal = %#v, want %#v", gotLiteral, wantLiteral)
	}
}

func requirePushFilterValueAlias(
	t testing.TB,
	value values.Value,
	want values.CorrelationIdentifier,
) {
	t.Helper()
	correlated := values.GetCorrelatedToOfValue(value)
	if len(correlated) != 1 {
		t.Fatalf("value correlations = %v, want only %q", correlated, want.Name())
	}
	if _, ok := correlated[want]; !ok {
		t.Fatalf("value correlations = %v, want only %q", correlated, want.Name())
	}
}

// explorePushFilterRewriting drives only the rewriting phase, which is the
// termination surface these rules own. Keeping the driver local makes this
// cluster independently compilable while exercising the same production task
// stack as Planner.
func explorePushFilterRewriting(
	planner *Planner,
	root *expressions.Reference,
) (int, bool) {
	if root == nil {
		return 0, true
	}
	if planner.memo == nil {
		planner.memo = NewMemo(root)
	}
	if planner.constraintMap == nil {
		planner.constraintMap = NewConstraintMap()
	}
	if planner.dataAccessConsumed == nil {
		planner.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	planner.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: root})
	planner.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: root})
	for len(planner.stack) > 0 {
		if planner.tasksRun >= planner.MaxTasks {
			return planner.tasksRun, false
		}
		planner.pop().Run(context.Background(), planner)
		planner.tasksRun++
	}
	return planner.tasksRun, true
}

// filterOverDistinct builds Filter(ID > 0, Distinct(Scan)) with the predicate
// resolved against the Distinct output quantifier.
func filterOverDistinct(t testing.TB) *expressions.LogicalFilterExpression {
	t.Helper()
	scan := mustPushFilterScan(t, []string{"T"})
	innerQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	distinct := mustPushFilterDistinct(t, innerQ)
	distinctQ := expressions.ForEachQuantifier(mustPushFilterInitial(t, distinct))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, distinctQ), "ID",
		predicates.ComparisonGreaterThan, int64(0))
	return mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, distinctQ)
}

func TestPushFilterThroughDistinctRule_Fires(t *testing.T) {
	t.Parallel()
	src := filterOverDistinct(t)
	reference := mustPushFilterInitial(t, src)
	yielded := mustFireExpressionRule(t, NewPushFilterThroughDistinctRule(), reference)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	distinct, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalDistinctExpression", yielded[0])
	}
	innerFilter, ok := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("distinct inner = %T, want *LogicalFilterExpression", distinct.GetInner().GetRangesOver().Get())
	}
	got := innerFilter.GetPredicates()
	if len(got) != 1 {
		t.Fatalf("filter predicates = %d, want 1", len(got))
	}
	requirePushFilterComparison(t, got[0], []string{"ID"},
		predicates.ComparisonGreaterThan, int64(0))
	requirePushFilterPredicateAlias(t, got[0], innerFilter.GetInner().GetAlias())
	if _, ok := innerFilter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want Scan", innerFilter.GetInner().GetRangesOver().Get())
	}
}

func TestPushFilterThroughDistinctRule_DeclinesOnNonDistinctInner(t *testing.T) {
	t.Parallel()
	scan := mustPushFilterScan(t, []string{"T"})
	quantifier := expressions.ForEachQuantifier(mustPushFilterInitial(t, scan))
	predicate := pushFilterComparison(t, mustPushFilterFlowed(t, quantifier), "ID",
		predicates.ComparisonGreaterThan, int64(0))
	filter := mustPushFilterLogicalFilter(t, []predicates.QueryPredicate{predicate}, quantifier)
	reference := mustPushFilterInitial(t, filter)
	yielded := mustFireExpressionRule(t, NewPushFilterThroughDistinctRule(), reference)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Distinct inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughDistinctRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	src := filterOverDistinct(t)
	reference := mustPushFilterInitial(t, src)
	progress, converged := explorePushFilterRewriting(
		NewPlanner([]ExpressionRule{NewPushFilterThroughDistinctRule()}, nil), reference)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(reference.Members()))
	}
}
