package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// explodeOrdinals runs an ordinality explode and returns each row's ordinal and
// the continuation after the last row.
func explodeOrdinals(t *testing.T, plan plans.RecordQueryPlan, continuation []byte, props recordlayer.ExecuteProperties) ([]int64, []byte) {
	t.Helper()
	ctx := context.Background()
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), continuation, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()
	var ordinals []int64
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			t.Fatalf("OnNext: %v", err)
		}
		if !result.HasNext() {
			next, err := result.GetContinuation().ToBytes()
			if err != nil {
				t.Fatalf("continuation: %v", err)
			}
			return ordinals, next
		}
		row := result.GetValue().Positional
		if row == nil || len(row.Slots) != 2 {
			t.Fatalf("row = %#v, want an (element, ordinal) row", result.GetValue())
		}
		ordinals = append(ordinals, row.Slots[1].(int64))
	}
}

// Java's zero-based explode (RecordQueryExplodePlan.executePlan, `firstOrdinal
// + i`) numbers the whole list from 0 before the continuation and skip/limit
// apply, so a resumed page continues the numbering instead of restarting it.
func TestExecuteExplode_ZeroBasedOrdinality(t *testing.T) {
	t.Parallel()
	array := &values.ConstantValue{
		Value: []any{int64(10), int64(20), int64(30)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}
	zeroBased := mustExecutorConstruct(plans.NewRecordQueryExplodePlanWithOrdinalityBase(array, true, true))
	oneBased := mustExecutorConstruct(plans.NewRecordQueryExplodePlanWithOrdinalityBase(array, true, false))

	if got, _ := explodeOrdinals(t, oneBased, nil, recordlayer.DefaultExecuteProperties()); !equalInt64s(got, []int64{1, 2, 3}) {
		t.Fatalf("one-based ordinals = %v, want [1 2 3]", got)
	}
	if got, _ := explodeOrdinals(t, zeroBased, nil, recordlayer.DefaultExecuteProperties()); !equalInt64s(got, []int64{0, 1, 2}) {
		t.Fatalf("zero-based ordinals = %v, want [0 1 2]", got)
	}

	page := recordlayer.ExecuteProperties{ReturnedRowLimit: 2}
	first, continuation := explodeOrdinals(t, zeroBased, nil, page)
	if !equalInt64s(first, []int64{0, 1}) {
		t.Fatalf("first page = %v, want [0 1]", first)
	}
	if rest, _ := explodeOrdinals(t, zeroBased, continuation, page); !equalInt64s(rest, []int64{2}) {
		t.Fatalf("resumed page = %v, want [2] (a resume does not renumber)", rest)
	}
	if skipped, _ := explodeOrdinals(t, zeroBased, nil, recordlayer.ExecuteProperties{Skip: 1}); !equalInt64s(skipped, []int64{1, 2}) {
		t.Fatalf("after skipping one = %v, want [1 2]", skipped)
	}

	scalar := mustExecutorConstruct(plans.NewRecordQueryExplodePlanWithOrdinalityBase(&values.ConstantValue{
		Value: int64(7),
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}, true, true))
	if got, _ := explodeOrdinals(t, scalar, nil, recordlayer.DefaultExecuteProperties()); !equalInt64s(got, []int64{0}) {
		t.Fatalf("zero-based scalar ordinal = %v, want [0]", got)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
