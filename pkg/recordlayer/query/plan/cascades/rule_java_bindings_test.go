package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func javaBindingsRowType() *values.RecordType {
	return values.NewRecordType("TBL", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

// javaBindingsPartitionedRef holds three plans of one row type: an unbound PK
// scan, a PK scan with an equality-bound key (another ordering) and a
// default-on-empty over the first (not a stored record).
func javaBindingsPartitionedRef(t *testing.T) *expressions.Reference {
	t.Helper()
	rowType := javaBindingsRowType()
	root := mustJavaBindingsConstruct(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("java_bindings_pk"), rowType))
	id := mustJavaBindingsConstruct(values.ResolveFieldOrdinals(root, []int{0}))
	k := mustJavaBindingsConstruct(values.ResolveFieldOrdinals(root, []int{1}))
	keyTypes := []values.Type{values.NullableLong, values.NullableLong}
	unbound := mustJavaBindingsConstruct(plans.NewRecordQueryScanPlan([]string{"TBL"}, rowType, false)).
		WithPrimaryKey([]values.Value{id, k}).WithKeyComponentTypes(keyTypes)
	bound := mustJavaBindingsConstruct(plans.NewRecordQueryScanPlan([]string{"TBL"}, rowType, false)).
		WithPrimaryKey([]values.Value{id, k}).WithKeyComponentTypes(keyTypes).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
	defaulted := mustJavaBindingsConstruct(plans.NewRecordQueryDefaultOnEmptyPlan(unbound,
		mustJavaBindingsConstruct(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("java_bindings_default"), rowType))))

	ref := expressions.FinalOfAtStage(unbound, expressions.StagePlanned)
	ref.InsertFinal(bound)
	ref.InsertFinal(defaulted)
	computeRefPlanProperties(ref)
	return ref
}

// TestJavaBindingsPartitionedRefFixture pins the fixture's partitions, which the
// counts below are derived from.
func TestJavaBindingsPartitionedRefFixture(t *testing.T) {
	t.Parallel()
	ref := javaBindingsPartitionedRef(t)
	if got := len(javaPlanPartitions(ref)); got != 3 {
		t.Fatalf("plan partitions = %d, want 3", got)
	}
	if got := len(javaStoredRecordPartitions(ref)); got != 2 {
		t.Fatalf("stored-record partitions = %d, want 2", got)
	}
}

// TestJavaBindingsCountJavasMatcherBindings pins each rule's count against its
// Java matcher, one case per fan-out kind and per shape the matcher rejects.
// The partitioned reference has three plan partitions, two of them stored-record.
func TestJavaBindingsCountJavasMatcherBindings(t *testing.T) {
	t.Parallel()

	partitioned := func() *expressions.Reference { return javaBindingsPartitionedRef(t) }
	logicalOnly := func() *expressions.Reference {
		return expressions.InitialOf(mustFullUnorderedScan(t, []string{"TBL"}, javaBindingsRowType()))
	}
	exploratoryPlansOnly := func() *expressions.Reference {
		plan := mustJavaBindingsConstruct(plans.NewRecordQueryScanPlan([]string{"TBL"}, javaBindingsRowType(), false))
		return expressions.InitialOf(plan)
	}
	forEach := func(ref *expressions.Reference) expressions.Quantifier { return expressions.ForEachQuantifier(ref) }
	existential := func(ref *expressions.Reference) expressions.Quantifier {
		return expressions.ExistentialQuantifier(ref)
	}
	truePredicate := predicates.NewConstantPredicate(predicates.TriTrue)
	indexOnly := predicates.NewValuePredicate(values.NewUnmatchedAggregateValue(values.NamedCorrelationIdentifier("unmatched")))
	filter := func(q expressions.Quantifier, preds ...predicates.QueryPredicate) expressions.RelationalExpression {
		return mustJavaBindingsConstruct(expressions.NewLogicalFilterExpression(preds, q))
	}
	sel := func(qs ...expressions.Quantifier) expressions.RelationalExpression {
		result := mustJavaBindingsConstruct(qs[0].RequireFlowedObjectValue())
		return mustJavaBindingsConstruct(expressions.NewSelectExpression(result, qs, nil))
	}
	rowType := javaBindingsRowType()

	for _, tc := range []struct {
		name string
		rule javaBindingCounter
		expr expressions.RelationalExpression
		want int
	}{
		{"filter: one per plan partition", &ImplementFilterRule{}, filter(forEach(partitioned()), truePredicate), 3},
		{"filter over a reference without plans", &ImplementFilterRule{}, filter(forEach(logicalOnly()), truePredicate), 0},
		{"filter with an index-only predicate", &ImplementFilterRule{}, filter(forEach(partitioned()), indexOnly), 0},
		{"filter over exploratory plans only", &ImplementFilterRule{}, filter(forEach(exploratoryPlansOnly()), truePredicate), 0},
		{
			"type filter: stored-record partitions", &ImplementTypeFilterRule{},
			mustJavaBindingsConstruct(expressions.NewLogicalTypeFilterExpression([]string{"TBL"}, forEach(partitioned()))), 2,
		},
		{
			"distinct: stored-record partitions", &ImplementDistinctFinalRule{},
			mustJavaBindingsConstruct(expressions.NewLogicalDistinctExpression(forEach(partitioned()))), 2,
		},
		{"simple select over one quantifier", &ImplementSimpleSelectRule{}, sel(forEach(partitioned())), 3},
		{"simple select over an existential quantifier", &ImplementSimpleSelectRule{}, sel(existential(partitioned())), 3},
		{"simple select over two quantifiers", &ImplementSimpleSelectRule{}, sel(forEach(partitioned()), forEach(partitioned())), 0},
		{
			"insert: one per plan partition", &ImplementInsertRule{},
			mustJavaBindingsConstruct(expressions.NewInsertExpression(forEach(partitioned()), "TBL", rowType)), 3,
		},
		{
			"temp-table insert: one per plan partition", &ImplementTempTableInsertRule{},
			mustJavaBindingsConstruct(expressions.NewTempTableInsertExpression(forEach(partitioned()), values.NamedCorrelationIdentifier("temp"), true)), 3,
		},
		{
			"delete: stored-record partitions", &ImplementDeleteRule{},
			mustJavaBindingsConstruct(expressions.NewDeleteExpression(forEach(partitioned()), "TBL")), 2,
		},
		{
			"update: stored-record partitions", &ImplementUpdateRule{},
			mustJavaBindingsConstruct(expressions.NewUpdateExpression(forEach(partitioned()), "TBL", rowType, nil)), 2,
		},
		{
			"sort: partitions by ordering, distinct and primary key", &ImplementSortRule{},
			mustJavaBindingsConstruct(expressions.UnsortedLogicalSortExpression(forEach(partitioned()))), 3,
		},
		{"partition select over one quantifier", &PartitionSelectRule{}, sel(forEach(logicalOnly())), 1},
		{
			"partition select over three quantifiers", &PartitionSelectRule{},
			sel(forEach(logicalOnly()), forEach(logicalOnly()), forEach(logicalOnly())), 7,
		},
		{
			"ordering push through select: ForEach quantifiers", &PushRequestedOrderingThroughSelectRule{},
			sel(forEach(logicalOnly()), forEach(logicalOnly()), existential(logicalOnly())), 2,
		},
		{
			"ordering push through select: existential quantifiers", &PushRequestedOrderingThroughSelectExistentialRule{},
			sel(forEach(logicalOnly()), forEach(logicalOnly()), existential(logicalOnly())), 1,
		},
		{"nested loop join: both orders", &ImplementNestedLoopJoinRule{}, sel(forEach(partitioned()), existential(partitioned())), 2},
		{"nested loop join over a reference without plans", &ImplementNestedLoopJoinRule{}, sel(forEach(partitioned()), forEach(logicalOnly())), 0},
		{
			"nested loop join over three quantifiers", &ImplementNestedLoopJoinRule{},
			sel(forEach(partitioned()), forEach(partitioned()), forEach(partitioned())), 0,
		},
		{"partition binary select: both orders", &PartitionBinarySelectRule{}, sel(forEach(logicalOnly()), existential(logicalOnly())), 2},
		{
			"partition binary select over three quantifiers", &PartitionBinarySelectRule{},
			sel(forEach(logicalOnly()), forEach(logicalOnly()), forEach(logicalOnly())), 0,
		},
		{"wrong expression type", &ImplementFilterRule{}, sel(forEach(partitioned())), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rule.javaBindings(tc.expr); got != tc.want {
				t.Fatalf("javaBindings = %d, want %d", got, tc.want)
			}
		})
	}
}

func mustJavaBindingsConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct java-bindings fixture: " + err.Error())
	}
	return value
}
