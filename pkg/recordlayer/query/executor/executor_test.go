package executor

import (
	"context"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestExecuteValues_SingleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	plan := plans.NewRecordQueryValuesPlan(cols)
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	row, ok := results[0].Datum.(map[string]any)
	if !ok {
		t.Fatalf("datum = %T, want map[string]any", results[0].Datum)
	}
	if row["constant"] != int64(42) {
		t.Errorf("row['constant'] = %v, want 42", row["constant"])
	}
}

func TestExecuteValues_EmptyColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryValuesPlan(nil)
	cursor, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (empty row)", len(results))
	}
}

func TestExecuteFilter_OverValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	filterPlan := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	)

	cursor, err := ExecutePlan(ctx, filterPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (filter=TRUE passes all)", len(results))
	}
}

func TestExecuteFilter_RejectsAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(10), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	filterPlan := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		inner,
	)

	cursor, err := ExecutePlan(ctx, filterPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (filter=FALSE rejects all)", len(results))
	}
}

func TestExecuteLimit_CapsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	limitPlan := plans.NewRecordQueryLimitPlan(inner, 0, 0)

	cursor, err := ExecutePlan(ctx, limitPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (limit=0)", len(results))
	}
}

func TestExecuteDistinct_DedupsValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := plans.NewRecordQueryValuesPlan(cols)
	distinctPlan := plans.NewRecordQueryDistinctPlan(inner)

	cursor, err := ExecutePlan(ctx, distinctPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestExecuteProjection_FieldExtraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(100), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	projPlan := plans.NewRecordQueryProjectionPlan(
		[]values.Value{
			&values.ConstantValue{Value: "projected", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		inner,
	)

	cursor, err := ExecutePlan(ctx, projPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	row := results[0].Datum.(map[string]any)
	if row["constant"] != "projected" {
		t.Errorf("projection result = %v, want 'projected'", row["constant"])
	}
}

func TestExecuteSort_OverValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})
	sortPlan := plans.NewRecordQuerySortPlan(nil, inner)

	cursor, err := ExecutePlan(ctx, sortPlan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestExecuteUnsupportedPlan_ReturnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := plans.NewRecordQueryExplodePlan(nil)
	_, err := ExecutePlan(ctx, plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err == nil {
		t.Fatal("expected error for unsupported plan type")
	}
}

func TestCollectAll_EmptyCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cursor := recordlayer.Empty[QueryResult]()
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestEvaluationContext_WithBinding(t *testing.T) {
	t.Parallel()

	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("q1")
	ec2 := ec.WithBinding(id, map[string]any{"x": 42})

	if _, ok := ec.GetBinding(id); ok {
		t.Fatal("original context should not have binding")
	}
	v, ok := ec2.GetBinding(id)
	if !ok {
		t.Fatal("derived context should have binding")
	}
	m, ok := v.(map[string]any)
	if !ok || m["x"] != 42 {
		t.Fatalf("binding value = %v, want map[x:42]", v)
	}
}

func TestQueryResult_FromStoredRecord_NilSafe(t *testing.T) {
	t.Parallel()

	m := protoToMap(nil)
	if m != nil {
		t.Fatalf("protoToMap(nil) = %v, want nil", m)
	}
}

func TestCompareAny_Integers(t *testing.T) {
	t.Parallel()

	if compareAny(int64(1), int64(2)) >= 0 {
		t.Fatal("1 should be < 2")
	}
	if compareAny(int64(2), int64(1)) <= 0 {
		t.Fatal("2 should be > 1")
	}
	if compareAny(int64(1), int64(1)) != 0 {
		t.Fatal("1 should equal 1")
	}
}

func TestCompareAny_Strings(t *testing.T) {
	t.Parallel()

	if compareAny("a", "b") >= 0 {
		t.Fatal("'a' should be < 'b'")
	}
	if compareAny("b", "a") <= 0 {
		t.Fatal("'b' should be > 'a'")
	}
}

func TestCompareAny_NilHandling(t *testing.T) {
	t.Parallel()

	if compareAny(nil, nil) != 0 {
		t.Fatal("nil should equal nil")
	}
	if compareAny(nil, int64(1)) >= 0 {
		t.Fatal("nil should sort before non-nil")
	}
	if compareAny(int64(1), nil) <= 0 {
		t.Fatal("non-nil should sort after nil")
	}
}

func TestSortByKeys(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		{Datum: map[string]any{"NAME": "charlie", "AGE": int64(30)}},
		{Datum: map[string]any{"NAME": "alice", "AGE": int64(25)}},
		{Datum: map[string]any{"NAME": "bob", "AGE": int64(35)}},
	}

	sortByKeys(items, []string{"name"}, nil)

	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Datum.(map[string]any)["NAME"].(string)
	}
	if names[0] != "alice" || names[1] != "bob" || names[2] != "charlie" {
		t.Fatalf("sort by name = %v, want [alice bob charlie]", names)
	}
}

func TestExecute_CompositeFilterSortLimitProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(99), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	})

	filtered := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		inner,
	)

	sorted := plans.NewRecordQuerySortPlan(nil, filtered)

	limited := plans.NewRecordQueryLimitPlan(sorted, 10, 0)

	projected := plans.NewRecordQueryProjectionPlan(
		[]values.Value{
			&values.ConstantValue{Value: "result", Typ: values.NewPrimitiveType(values.TypeCodeString, false)},
		},
		limited,
	)

	cursor, err := ExecutePlan(ctx, projected, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	row := results[0].Datum.(map[string]any)
	if row["constant"] != "result" {
		t.Errorf("composite pipeline result = %v, want 'result'", row["constant"])
	}
}

func TestSortByKeys_Descending(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		{Datum: map[string]any{"AGE": int64(25)}},
		{Datum: map[string]any{"AGE": int64(35)}},
		{Datum: map[string]any{"AGE": int64(30)}},
	}

	sortByKeys(items, []string{"age"}, []bool{true})

	ages := make([]int64, len(items))
	for i, item := range items {
		ages[i] = item.Datum.(map[string]any)["AGE"].(int64)
	}
	if ages[0] != 35 || ages[1] != 30 || ages[2] != 25 {
		t.Fatalf("sort by age DESC = %v, want [35 30 25]", ages)
	}
}
