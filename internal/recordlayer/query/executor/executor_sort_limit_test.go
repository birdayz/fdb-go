package executor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"fdb.dev/internal/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestExecuteLimit_CapsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cols := []values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan(cols))
	limitPlan := mustExecutorConstruct(plans.NewRecordQueryLimitPlan(inner, 0, 0))

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

func TestExecuteSort_OverValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{
		&values.ConstantValue{Value: int64(42), Typ: values.NewPrimitiveType(values.TypeCodeInt, false)},
	}))
	sortPlan := mustExecutorConstruct(plans.NewRecordQueryInMemorySortPlan(inner, nil))

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

func TestExpressionSortFn(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		dmap(map[string]any{"NAME": "charlie", "AGE": int64(30)}),
		dmap(map[string]any{"NAME": "alice", "AGE": int64(25)}),
		dmap(map[string]any{"NAME": "bob", "AGE": int64(35)}),
	}

	// dmap lays columns out alphabetically: AGE@0, NAME@1 — the sort key is
	// BAKED to NAME's slot (no runtime name resolution).
	rowType := exactTestRowType(
		values.Field{Name: "AGE", FieldType: values.NullableLong},
		values.Field{Name: "NAME", FieldType: values.TypeString},
	)
	sortFn := expressionSortFn([]expressions.SortKey{
		{Value: mustTestFieldOrdinal(t, mustTestQOV(t, values.UniqueCorrelationIdentifier(), rowType), 1)},
	})
	if err := sortFn(items); err != nil {
		t.Fatalf("sortFn: %v", err)
	}

	names := make([]string, len(items))
	for i, item := range items {
		names[i] = rowMap(item)["NAME"].(string)
	}
	if names[0] != "alice" || names[1] != "bob" || names[2] != "charlie" {
		t.Fatalf("sort by name = %v, want [alice bob charlie]", names)
	}
}

func TestExpressionSortFn_Descending(t *testing.T) {
	t.Parallel()

	items := []QueryResult{
		dmap(map[string]any{"AGE": int64(25)}),
		dmap(map[string]any{"AGE": int64(35)}),
		dmap(map[string]any{"AGE": int64(30)}),
	}

	sortFn := expressionSortFn([]expressions.SortKey{
		{Value: mustNamedTestField(t, "AGE", values.NullableLong), Reverse: true},
	})
	if err := sortFn(items); err != nil {
		t.Fatalf("sortFn: %v", err)
	}

	ages := make([]int64, len(items))
	for i, item := range items {
		ages[i] = rowMap(item)["AGE"].(int64)
	}
	if ages[0] != 35 || ages[1] != 30 || ages[2] != 25 {
		t.Fatalf("sort by age DESC = %v, want [35 30 25]", ages)
	}
}

// ----- expressionSortFn (multi-key — extends existing single-key tests) -----

func TestExpressionSortFn_MultipleKeys(t *testing.T) {
	t.Parallel()
	items := []QueryResult{
		dmap(map[string]any{"A": int64(2), "B": int64(1)}),
		dmap(map[string]any{"A": int64(1), "B": int64(2)}),
		dmap(map[string]any{"A": int64(1), "B": int64(1)}),
	}
	sortFn := expressionSortFn([]expressions.SortKey{
		{Value: mustTestFieldOrdinal(t, mustTestQOV(t, values.UniqueCorrelationIdentifier(), exactTestRowType(
			values.Field{Name: "A", FieldType: values.NullableLong},
			values.Field{Name: "B", FieldType: values.NullableLong},
		)), 0)},
		{Value: mustTestFieldOrdinal(t, mustTestQOV(t, values.UniqueCorrelationIdentifier(), exactTestRowType(
			values.Field{Name: "A", FieldType: values.NullableLong},
			values.Field{Name: "B", FieldType: values.NullableLong},
		)), 1)},
	})
	if err := sortFn(items); err != nil {
		t.Fatalf("sortFn: %v", err)
	}

	d0, _ := rowMapOK(items[0])
	d1, _ := rowMapOK(items[1])
	d2, _ := rowMapOK(items[2])
	if d0["A"] != int64(1) || d0["B"] != int64(1) {
		t.Errorf("row 0: got A=%v B=%v, want 1,1", d0["A"], d0["B"])
	}
	if d1["A"] != int64(1) || d1["B"] != int64(2) {
		t.Errorf("row 1: got A=%v B=%v, want 1,2", d1["A"], d1["B"])
	}
	if d2["A"] != int64(2) {
		t.Errorf("row 2: got A=%v, want 2", d2["A"])
	}
}

// ===========================================================================
// memorySortCursor continuation round-trip
// ===========================================================================

func TestSortContinuation_RoundTrip(t *testing.T) {
	t.Parallel()

	buf := []QueryResult{
		qr("name", "alice", "age", int64(30)),
		qr("name", "bob", "age", int64(25)),
		qr("name", "carol", "age", int64(35)),
	}
	// Give the second entry a PrimaryKey to verify PK round-trip.
	buf[1].PrimaryKey = tuple.Tuple{"pk", int64(2)}

	innerCont := recordlayer.NewBytesContinuation([]byte{0xCA, 0xFE})

	encoded, err := encodeSortContinuation(innerCont, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotBuf, _, err := decodeSortContinuation(encoded, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(gotInner, []byte{0xCA, 0xFE}) {
		t.Errorf("innerContinuation = %x, want cafe", gotInner)
	}
	if len(gotBuf) != 3 {
		t.Fatalf("buf len = %d, want 3", len(gotBuf))
	}

	// Check datum values.
	for i, want := range []string{"alice", "bob", "carol"} {
		m, _ := rowMapOK(gotBuf[i])
		if m["name"] != want {
			t.Errorf("buf[%d].name = %v, want %q", i, m["name"], want)
		}
	}
	// Ages: the typed slot codec keeps int64 exactly (no JSON float64 detour).
	for i, want := range []int64{30, 25, 35} {
		m, _ := rowMapOK(gotBuf[i])
		if m["age"] != want {
			t.Errorf("buf[%d].age = %v (%T), want %d", i, m["age"], m["age"], want)
		}
	}

	// PrimaryKey round-trip.
	if gotBuf[1].PrimaryKey == nil {
		t.Fatal("buf[1].PrimaryKey is nil after round-trip")
	}
	if len(gotBuf[1].PrimaryKey) != 2 {
		t.Fatalf("buf[1].PrimaryKey len = %d, want 2", len(gotBuf[1].PrimaryKey))
	}
	if gotBuf[1].PrimaryKey[0] != "pk" {
		t.Errorf("buf[1].PrimaryKey[0] = %v, want \"pk\"", gotBuf[1].PrimaryKey[0])
	}
}

func TestSortContinuation_EmptyBuffer(t *testing.T) {
	t.Parallel()

	encoded, err := encodeSortContinuation(nil, nil, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotBuf, _, err := decodeSortContinuation(encoded, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotInner != nil {
		t.Errorf("innerContinuation = %x, want nil", gotInner)
	}
	if len(gotBuf) != 0 {
		t.Errorf("buf len = %d, want 0", len(gotBuf))
	}
}

func TestSortCursor_SortsCorrectly(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("NAME", "carol", "AGE", int64(35)),
		qr("NAME", "alice", "AGE", int64(30)),
		qr("NAME", "bob", "AGE", int64(25)),
	})

	// qr/dmap lays columns out alphabetically: AGE@0, NAME@1.
	c := newCustomSortCursor(inner, expressionSortFn([]expressions.SortKey{
		{Value: mustNamedTestField(t, "AGE", values.NullableLong)},
	}), nil) // ASC
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{25, 30, 35}
	for i, want := range expected {
		got := fieldVal(t, results[i], "AGE")
		if got != want {
			t.Errorf("results[%d].AGE = %d, want %d", i, got, want)
		}
	}
}

func TestSortCursor_SortsDescending(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("X", int64(1)),
		qr("X", int64(3)),
		qr("X", int64(2)),
	})

	c := newCustomSortCursor(inner, expressionSortFn([]expressions.SortKey{
		{Value: mustNamedTestField(t, "X", values.NullableLong), Reverse: true},
	}), nil) // DESC
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "X")
		if got != want {
			t.Errorf("results[%d].X = %d, want %d", i, got, want)
		}
	}
}

func TestSortCursor_EmptyInput(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{})
	c := newCustomSortCursor(inner, expressionSortFn(nil), nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

// TestSortCursor_DeclaredOrdinalMissingIsLoud pins the correct-or-loud
// contract on the sort path after RFC-232 made an unbaked FieldValue
// unrepresentable. The exact field is declared at ordinal 1, while the runtime
// row has only ordinal 0. A name fallback could still find X and silently sort;
// the sealed layout evaluator must instead report the runtime-shape mismatch.
func TestSortCursor_DeclaredOrdinalMissingIsLoud(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("X", int64(1)),
		qr("X", int64(2)),
	})
	declared := exactTestRowType(
		values.Field{Name: "Y", FieldType: values.NullableLong},
		values.Field{Name: "X", FieldType: values.NullableLong},
	)
	key := mustTestFieldOrdinal(t,
		mustTestQOV(t, values.UniqueCorrelationIdentifier(), declared), 1)
	c := newCustomSortCursor(inner, expressionSortFn([]expressions.SortKey{{Value: key}}), nil)
	defer c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected a loud error for a missing declared ordinal")
	}
	var resolutionErr *values.ResolutionError
	if !errors.As(err, &resolutionErr) {
		t.Fatalf("expected *values.ResolutionError, got %T: %v", err, err)
	}
	if resolutionErr.Code() != values.LayoutRuntimeShape {
		t.Fatalf("resolution error code = %v, want LayoutRuntimeShape: %v", resolutionErr.Code(), err)
	}
}

// ===========================================================================
// customSortCursor — pluggable comparator
// ===========================================================================

func TestCustomSortCursor_ReverseSort(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{
		qr("N", int64(1)),
		qr("N", int64(3)),
		qr("N", int64(2)),
	})

	sortFn := expressionSortFn([]expressions.SortKey{
		{Value: mustNamedTestField(t, "N", values.NullableLong), Reverse: true},
	})
	c := newCustomSortCursor(inner, sortFn, nil)
	defer c.Close()

	results := collectCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, want := range []int64{3, 2, 1} {
		got := fieldVal(t, results[i], "N")
		if got != want {
			t.Errorf("results[%d].N = %d, want %d", i, got, want)
		}
	}
}

func TestCustomSortCursor_OnNextAfterClose(t *testing.T) {
	t.Parallel()

	inner := recordlayer.FromList([]QueryResult{qr("n", int64(1))})
	c := newCustomSortCursor(inner, func([]QueryResult) error { return nil }, nil)
	c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected error from OnNext after Close")
	}
}

func TestCustomSortCursor_BufferLimitExceeded(t *testing.T) {
	t.Parallel()

	// Create an inner cursor with more rows than the limit.
	rows := make([]QueryResult, 10)
	for i := range rows {
		rows[i] = qr("n", int64(i))
	}
	inner := recordlayer.FromList(rows)

	c := newCustomSortCursor(inner, func(buf []QueryResult) error {
		// The buffer cap trips during LOAD, before any sort runs.
		return nil
	}, nil)
	c.maxBuf = 5 // limit to 5 rows
	defer c.Close()

	_, err := c.OnNext(context.Background())
	if err == nil {
		t.Fatal("expected SortBufferExceededError")
	}
	var bufErr *SortBufferExceededError
	if !errors.As(err, &bufErr) {
		t.Fatalf("expected *SortBufferExceededError, got %T: %v", err, err)
	}
	if bufErr.Limit != 5 {
		t.Errorf("limit = %d, want 5", bufErr.Limit)
	}
	if bufErr.Rows != 5 {
		t.Errorf("rows = %d, want 5", bufErr.Rows)
	}
}
