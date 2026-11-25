package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestExecuteUnsupportedPlan_ReturnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	plan := &unsupportedTestPlan{}
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

// ----- CollectAll (multi-item — extends existing empty test) ----------------

func TestCollectAll_MultipleItems(t *testing.T) {
	t.Parallel()
	items := []QueryResult{
		dscalar(int64(1)),
		dscalar(int64(2)),
		dscalar(int64(3)),
	}
	cursor := recordlayer.FromList(items)
	results, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3, got %d", len(results))
	}
	for i, r := range results {
		if rowScalar(r) != int64(i+1) {
			t.Errorf("item %d: got %v, want %d", i, rowScalar(r), i+1)
		}
	}
}

// --- CollectAllBounded regression tests ---

func TestCollectAllBounded_UnderLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 5)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	results, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 10, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("got %d results, want 5", len(results))
	}
}

func TestCollectAllBounded_ExactlyAtLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	_, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 10, "test")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError at exactly limit rows")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 10 {
		t.Errorf("limit = %d, want 10", mlErr.Limit)
	}
	if mlErr.Context != "test" {
		t.Errorf("context = %q, want %q", mlErr.Context, "test")
	}
}

func TestCollectAllBounded_OverLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 20)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	_, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 10, "nested loop join inner side")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 10 {
		t.Errorf("limit = %d, want 10", mlErr.Limit)
	}
	if mlErr.Context != "nested loop join inner side" {
		t.Errorf("context = %q, want %q", mlErr.Context, "nested loop join inner side")
	}
	if !strings.Contains(mlErr.Error(), "10 rows") {
		t.Errorf("error message missing row count: %q", mlErr.Error())
	}
	if !strings.Contains(mlErr.Error(), "adding an index") {
		t.Errorf("error message missing actionable advice: %q", mlErr.Error())
	}
}

func TestCollectAllBounded_EmptyCursor(t *testing.T) {
	t.Parallel()
	cursor := recordlayer.FromList([]QueryResult{})

	results, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 5, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestCollectAllBounded_LimitOne(t *testing.T) {
	t.Parallel()
	rows := []QueryResult{qr("n", int64(1)), qr("n", int64(2))}
	cursor := recordlayer.FromList(rows)

	_, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 1, "test")
	if err == nil {
		t.Fatal("expected MaterializationLimitExceededError with limit=1 and 2 rows")
	}
	var mlErr *MaterializationLimitExceededError
	if !errors.As(err, &mlErr) {
		t.Fatalf("expected *MaterializationLimitExceededError, got %T: %v", err, err)
	}
	if mlErr.Limit != 1 {
		t.Errorf("limit = %d, want 1", mlErr.Limit)
	}
}

func TestCollectAllBounded_OneBelowLimit(t *testing.T) {
	t.Parallel()
	rows := make([]QueryResult, 9)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	cursor := recordlayer.FromList(rows)

	results, _, err := CollectAllBounded(context.Background(), cursor, recordlayer.NewExecuteState(0), 10, "test")
	if err != nil {
		t.Fatalf("unexpected error with 9 rows and limit 10: %v", err)
	}
	if len(results) != 9 {
		t.Errorf("got %d results, want 9", len(results))
	}
}

func TestMaterializationLimitExceededError_ErrorsAs(t *testing.T) {
	t.Parallel()
	orig := &MaterializationLimitExceededError{Limit: 42, Context: "recursive DFS children"}
	wrapped := fmt.Errorf("executor: %w", orig)

	var mlErr *MaterializationLimitExceededError
	if !errors.As(wrapped, &mlErr) {
		t.Fatal("errors.As failed on wrapped MaterializationLimitExceededError")
	}
	if mlErr.Limit != 42 {
		t.Errorf("limit = %d, want 42", mlErr.Limit)
	}
	if mlErr.Context != "recursive DFS children" {
		t.Errorf("context = %q, want %q", mlErr.Context, "recursive DFS children")
	}
}

func TestGetMaterializationLimit_Default(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties()
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("default = %d, want %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
	}
}

func TestGetMaterializationLimit_Custom(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(500)
	if props.GetMaterializationLimit() != 500 {
		t.Errorf("custom = %d, want 500", props.GetMaterializationLimit())
	}
}

func TestGetMaterializationLimit_ZeroFallsBackToDefault(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(0)
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("zero = %d, want default %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
	}
}

func TestGetMaterializationLimit_NegativeFallsBackToDefault(t *testing.T) {
	t.Parallel()
	props := recordlayer.DefaultExecuteProperties().WithMaterializationLimit(-1)
	if props.GetMaterializationLimit() != recordlayer.DefaultMaterializationLimit {
		t.Errorf("negative = %d, want default %d", props.GetMaterializationLimit(), recordlayer.DefaultMaterializationLimit)
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

// ----- EvaluationContext (additional coverage) ------------------------------

func TestEmptyEvaluationContext_NoBindings(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	if ec == nil {
		t.Fatal("expected non-nil context")
	}
	_, ok := ec.GetBinding(values.NamedCorrelationIdentifier("anything"))
	if ok {
		t.Fatal("empty context should have no bindings")
	}
}

func TestEvaluationContext_WithParams(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	ec2 := ec.WithParams([]any{int64(10), "hello"})

	v, ok := ec2.BindParameter(1, "")
	if !ok || v != int64(10) {
		t.Fatalf("param 1: got %v, %v, want 10, true", v, ok)
	}
	v, ok = ec2.BindParameter(2, "")
	if !ok || v != "hello" {
		t.Fatalf("param 2: got %v, %v, want hello, true", v, ok)
	}

	_, ok = ec.BindParameter(1, "")
	if ok {
		t.Fatal("original context should not have params")
	}
}

func TestEvaluationContext_BindParameter_Bounds(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{int64(1)})

	_, ok := ec.BindParameter(0, "")
	if ok {
		t.Fatal("ordinal 0 should fail (1-based)")
	}
	_, ok = ec.BindParameter(2, "")
	if ok {
		t.Fatal("ordinal 2 should fail (only 1 param)")
	}
	_, ok = ec.BindParameter(-1, "")
	if ok {
		t.Fatal("negative ordinal should fail")
	}
}

func TestEvaluationContext_WithBinding_DoesNotMutateParent(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id1 := values.NamedCorrelationIdentifier("a")
	id2 := values.NamedCorrelationIdentifier("b")
	ec1 := ec.WithBinding(id1, "val1")
	ec2 := ec1.WithBinding(id2, "val2")

	if _, ok := ec1.GetBinding(id2); ok {
		t.Fatal("ec1 should not see ec2's binding")
	}
	if v, ok := ec2.GetBinding(id1); !ok || v != "val1" {
		t.Fatal("ec2 should inherit ec1's bindings")
	}
}

func TestEvaluationContext_WithParams_CopiesBindings(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("x")
	ec = ec.WithBinding(id, "kept")
	ec2 := ec.WithParams([]any{int64(42)})

	v, ok := ec2.GetBinding(id)
	if !ok || v != "kept" {
		t.Fatal("WithParams should preserve existing bindings")
	}
	v, ok = ec2.BindParameter(1, "")
	if !ok || v != int64(42) {
		t.Fatal("WithParams should set params")
	}
}

func TestEvaluationContext_RowContext(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{int64(99)})
	rc := ec.RowContext()
	v, ok := rc.Binder.BindParameter(1, "")
	if !ok || v != int64(99) {
		t.Fatal("RowContext's binder should use the EvalContext's params")
	}
}

func TestEvaluationContext_RowContext_CorrelationBinding(t *testing.T) {
	t.Parallel()
	id := values.NamedCorrelationIdentifier("explode_q1")
	ec := EmptyEvaluationContext().WithBinding(id, int64(42))
	rc := ec.RowContext()
	if rc.Correlations == nil {
		t.Fatal("RowContext should pass through correlation binder")
	}
	v, ok := rc.Correlations.GetCorrelationBinding(id)
	if !ok || v != int64(42) {
		t.Fatalf("expected correlation binding 42, got %v (ok=%v)", v, ok)
	}
	qov := mustTestQOV(t, id, values.NullableLong)
	result, err := qov.Evaluate(rc)
	if err != nil {
		t.Fatalf("QOV.Evaluate(RowEvalContext) error: %v", err)
	}
	if result != int64(42) {
		t.Fatalf("QOV.Evaluate(RowEvalContext) = %v, want 42", result)
	}
}

func TestParameterBinding_ScanComparison(t *testing.T) {
	t.Parallel()

	param1 := values.NewParameterValue(1)
	cr := predicates.EmptyComparisonRange()
	res := cr.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: param1})
	if !res.Complete() {
		t.Fatal("merge failed")
	}

	binder := EmptyEvaluationContext().WithParams([]any{int64(42)})
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{res.Range},
		[]values.Type{values.NullableLong},
		binder, false, "executor-parameter-binding",
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges := materializeAllRanges(t, spec)
	if len(ranges) != 1 {
		t.Fatalf("opened %d ranges, want 1", len(ranges))
	}
	r := ranges[0]

	if len(r.Low) != 1 || r.Low[0] != int64(42) {
		t.Fatalf("low=%v, want [42] (param resolved via binder)", r.Low)
	}
	if len(r.High) != 1 || r.High[0] != int64(42) {
		t.Fatalf("high=%v, want [42] (param resolved via binder)", r.High)
	}
}

func TestParameterBinding_Filter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rowType := exactTestRowType(values.Field{Name: "X", FieldType: values.NullableLong})
	inner := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{
			map[string]any{"X": int64(10)},
			map[string]any{"X": int64(20)},
			map[string]any{"X": int64(30)},
		},
		Typ: values.NewArrayType(false, rowType),
	}))

	param1 := values.NewParameterValue(1)
	filter := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				mustTestFieldOrdinal(t, inner.GetResultValue(), 0),
				predicates.Comparison{
					Type:    predicates.ComparisonGreaterThan,
					Operand: param1,
				},
			),
		},
		inner,
	))

	evalCtx := EmptyEvaluationContext().WithParams([]any{int64(15)})
	cursor, err := ExecutePlan(ctx, filter, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()

	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (20 and 30 > 15)", len(results))
	}
	v0, _ := rowMap(results[0])["X"].(int64)
	v1, _ := rowMap(results[1])["X"].(int64)
	if v0 != 20 || v1 != 30 {
		t.Errorf("values = [%d, %d], want [20, 30]", v0, v1)
	}
}

func TestParameterBinding_Values(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	param1 := values.NewParameterValue(1)
	param1.Typ = values.NullableLong
	vplan := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{param1}))

	evalCtx := EmptyEvaluationContext().WithParams([]any{int64(99)})
	cursor, err := ExecutePlan(ctx, vplan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
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
	datum, _ := rowMapOK(results[0])
	declared := vplan.GetResultType().(*values.RecordType)
	name := declared.Fields[0].Name
	if datum[name] != int64(99) {
		t.Errorf("parameter under declared name %q = %v, want 99", name, datum[name])
	}
}

func TestExecuteTempTable_InsertAndScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("cte1")

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	insertPlan := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(inner, alias, false))
	cursor, err := ExecutePlan(ctx, insertPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	inserted, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("insert collect: %v", err)
	}
	if len(inserted) != 1 {
		t.Fatalf("insert returned %d rows, want 1", len(inserted))
	}
	declared, ok := inner.GetResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("VALUES declared result type = %T, want record", inner.GetResultType())
	}
	if inserted[0].Positional == nil || !inserted[0].Positional.Type.Equals(declared) {
		t.Fatalf("inserted row type = %v, want VALUES declaration %v", inserted[0].Positional, declared)
	}

	scanPlan := mustTempTableScan(t, evalCtx, alias)
	cursor2, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanned, err := CollectAll(ctx, cursor2)
	if err != nil {
		t.Fatalf("scan collect: %v", err)
	}
	if len(scanned) != 1 {
		t.Fatalf("scan returned %d rows, want 1", len(scanned))
	}
	if scanned[0].Positional == nil || !scanned[0].Positional.Type.Equals(declared) {
		t.Fatalf("scanned row type = %v, want inserted declaration %v", scanned[0].Positional, declared)
	}
	row, _ := rowMapOK(scanned[0])
	declaredName := declared.Fields[0].Name
	if row[declaredName] != int64(42) {
		t.Errorf("scanned value under declared name %q = %v, want 42", declaredName, row[declaredName])
	}
}

func TestExecuteTempTable_EmptyScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("empty_tt")

	scanPlan := mustExecutorConstruct(plans.NewRecordQueryTempTableScanPlan(alias, exactTestRowType()))
	cursor, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestExecuteTempTable_MultipleInserts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("multi")

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	for insertion := 1; insertion <= 3; insertion++ {
		insertPlan := mustExecutorConstruct(plans.NewRecordQueryTempTableInsertPlan(inner, alias, false))
		cursor, err := ExecutePlan(ctx, insertPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			t.Fatalf("insert %d: %v", insertion, err)
		}
		_, err = CollectAll(ctx, cursor)
		if err != nil {
			t.Fatalf("collect %d: %v", insertion, err)
		}
	}

	scanPlan := mustTempTableScan(t, evalCtx, alias)
	cursor, err := ExecutePlan(ctx, scanPlan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	results, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
}

func TestTempTable_ClearAndReuse(t *testing.T) {
	t.Parallel()

	tt := NewTempTable()
	tt.Add(dscalar(int64(1)))
	tt.Add(dscalar(int64(2)))

	if len(tt.GetList()) != 2 {
		t.Fatalf("got %d items, want 2", len(tt.GetList()))
	}

	tt.Clear()
	if len(tt.GetList()) != 0 {
		t.Fatalf("after clear, got %d items, want 0", len(tt.GetList()))
	}

	tt.Add(dscalar(int64(3)))
	if len(tt.GetList()) != 1 {
		t.Fatalf("after re-add, got %d items, want 1", len(tt.GetList()))
	}
}

// ----- TempTable (additional coverage) --------------------------------------

func TestTempTable_AddAndGetList(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(dscalar(int64(1)))
	tt.Add(dscalar(int64(2)))

	list := tt.GetList()
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
	if rowScalar(list[0]) != int64(1) || rowScalar(list[1]) != int64(2) {
		t.Errorf("unexpected contents: %v %v", rowScalar(list[0]), rowScalar(list[1]))
	}
}

func TestTempTable_GetListReturnsSnapshot(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(dscalar(int64(1)))
	snap := tt.GetList()
	tt.Add(dscalar(int64(2)))

	if len(snap) != 1 {
		t.Fatal("snapshot should not grow when new items added")
	}
	if len(tt.GetList()) != 2 {
		t.Fatal("temp table should now have 2 items")
	}
}

func TestTempTable_EmptyList(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	list := tt.GetList()
	if len(list) != 0 {
		t.Fatal("new temp table should be empty")
	}
}

func TestEvaluationContext_GetOrCreateTempTable(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("tt1")
	tt1 := ec.GetOrCreateTempTable(id, nil)
	tt1.Add(dscalar(int64(1)))

	tt2 := ec.GetOrCreateTempTable(id, nil)
	if len(tt2.GetList()) != 1 {
		t.Fatal("second GetOrCreateTempTable should return same instance")
	}
}

func TestEvaluationContext_GetOrCreateTempTable_DistinctIDs(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id1 := values.NamedCorrelationIdentifier("tt1")
	id2 := values.NamedCorrelationIdentifier("tt2")

	tt1 := ec.GetOrCreateTempTable(id1, nil)
	tt1.Add(dscalar(int64(1)))

	tt2 := ec.GetOrCreateTempTable(id2, nil)
	if len(tt2.GetList()) != 0 {
		t.Fatal("different IDs should create distinct temp tables")
	}
}
