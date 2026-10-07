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
