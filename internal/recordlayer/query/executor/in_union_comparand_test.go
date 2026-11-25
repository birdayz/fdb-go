package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func comparandInUnion(t *testing.T, comparand values.Value) *plans.RecordQueryInUnionPlan {
	t.Helper()
	return mustExecutorConstruct(plans.NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize(
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)),
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("in")},
		nil, false, 24,
	)).WithInSources([][]any{nil}).WithInComparands([]values.Value{comparand})
}

// An in-union source planning could not evaluate is evaluated when the plan
// opens (Java's InComparandSource.getValues): it runs one child per distinct
// value, and a value that fails raises its own error. Before, the source was
// dropped at planning and execution failed with "no planning-time values".
func TestExecuteInUnion_ComparandSourceEvaluatedAtOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	list := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)), values.LiteralValue(int64(1)), values.LiteralValue(int64(2)),
	})
	distinct := &values.ArrayDistinctValue{Child: list, Typ: list.Type()}
	cursor, err := executeInUnion(ctx, comparandInUnion(t, distinct), nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("executeInUnion: %v", err)
	}
	rows, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d child executions, want 2 (one per distinct value of [1, 1, 2])", len(rows))
	}

	failing := values.NewCastValue(values.LiteralValue("not a number"), values.NewArrayType(false, values.NotNullLong))
	_, err = executeInUnion(ctx, comparandInUnion(t, failing), nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
	if err == nil {
		t.Fatal("a comparand that fails to evaluate must raise its error at open")
	}
}

// Two in-union plans over the same comparand are the same plan: the
// comparand folds into the plan's identity as a Value, whatever it will
// evaluate to; a different comparand is a different plan.
func TestInUnionPlan_ComparandIdentity(t *testing.T) {
	t.Parallel()
	mk := func(items ...int64) values.Value {
		elems := make([]values.Value, len(items))
		for i, it := range items {
			elems[i] = values.LiteralValue(it)
		}
		return values.NewArrayConstructorValue(values.NotNullLong, elems)
	}
	a, b, c := comparandInUnion(t, mk(1, 2)), comparandInUnion(t, mk(1, 2)), comparandInUnion(t, mk(1, 3))
	if !a.EqualsPlanWithoutChildren(b) || a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("two in-unions over one comparand must be equal and hash alike")
	}
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("in-unions over different comparands must differ")
	}
	if plain := comparandInUnion(t, nil); a.EqualsPlanWithoutChildren(plain) {
		t.Fatal("an in-union with a comparand must differ from one without")
	}
}

// A runtime IN comparand that evaluates to NULL or to a non-list is a bad
// plan input, not an empty IN list: Java's InComparandSource.getValues casts
// the comparand to a List unchecked (InComparandSource.java:103-108) and the
// in-join/in-union plans then size and iterate it
// (RecordQueryInJoinPlan.java:112-132, RecordQueryInUnionPlan.java:331-346),
// so both fail. Only a genuinely empty list answers empty. Before, Go
// discarded the failed type assertion and answered zero rows.
func TestExecuteInComparand_NonListFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bad := map[string]values.Value{
		"scalar": values.LiteralValue("x"),
		"null":   values.NewNullValue(values.NewArrayType(true, values.NotNullLong)),
	}
	for name, comparand := range bad {
		t.Run("union/"+name, func(t *testing.T) {
			_, err := executeInUnion(ctx, comparandInUnion(t, comparand), nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
			if err == nil {
				t.Fatal("a non-list IN comparand must fail, not answer empty")
			}
		})
		t.Run("join/"+name, func(t *testing.T) {
			join := mustExecutorConstruct(plans.NewRecordQueryInJoinPlan(
				mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)), "in", false, false,
			)).WithInComparand(comparand)
			_, err := executeInJoin(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
			if err == nil {
				t.Fatal("a non-list IN comparand must fail, not answer empty")
			}
		})
	}

	empty := values.NewArrayConstructorValue(values.NotNullLong, nil)
	cursor, err := executeInUnion(ctx, comparandInUnion(t, empty), nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("an empty list comparand answers empty: %v", err)
	}
	if rows, err := CollectAll(ctx, cursor); err != nil || len(rows) != 0 {
		t.Fatalf("empty list comparand: %d rows, %v; want 0, nil", len(rows), err)
	}
	join := mustExecutorConstruct(plans.NewRecordQueryInJoinPlan(
		mustExecutorConstruct(plans.NewRecordQueryValuesPlan(nil)), "in", false, false,
	)).WithInComparand(empty)
	cursor, err = executeInJoin(ctx, join, nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("an empty list comparand answers empty: %v", err)
	}
	if rows, err := CollectAll(ctx, cursor); err != nil || len(rows) != 0 {
		t.Fatalf("empty list comparand: %d rows, %v; want 0, nil", len(rows), err)
	}
}
