package executor

import (
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// EvaluationContext tests
// ---------------------------------------------------------------------------

func TestEmptyEvaluationContext(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	if ec == nil {
		t.Fatal("EmptyEvaluationContext returned nil")
	}
	// No bindings should exist.
	id := values.NamedCorrelationIdentifier("anything")
	if _, ok := ec.GetBinding(id); ok {
		t.Fatal("empty context should have no bindings")
	}
}

func TestWithParams(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	params := []any{"a", int64(42), 3.14}
	ec2 := ec.WithParams(params)

	// Params accessible via BindParameter on the new context.
	for i, want := range params {
		got, ok := ec2.BindParameter(i+1, "")
		if !ok {
			t.Fatalf("BindParameter(%d) returned false", i+1)
		}
		if got != want {
			t.Fatalf("BindParameter(%d) = %v, want %v", i+1, got, want)
		}
	}
}

func TestWithParams_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	_ = ec.WithParams([]any{"x"})

	// Original context should still have no params.
	if _, ok := ec.BindParameter(1, ""); ok {
		t.Fatal("WithParams mutated the original context")
	}
}

func TestBindParameter_OneBased(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{"first", "second"})

	got, ok := ec.BindParameter(1, "")
	if !ok || got != "first" {
		t.Fatalf("ordinal 1: got (%v, %v), want (first, true)", got, ok)
	}
	got, ok = ec.BindParameter(2, "")
	if !ok || got != "second" {
		t.Fatalf("ordinal 2: got (%v, %v), want (second, true)", got, ok)
	}
}

func TestBindParameter_ZeroOrdinal(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{"a"})

	if _, ok := ec.BindParameter(0, ""); ok {
		t.Fatal("ordinal 0 should return false")
	}
}

func TestBindParameter_OrdinalExceedsLength(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{"only"})

	if _, ok := ec.BindParameter(2, ""); ok {
		t.Fatal("ordinal > len(params) should return false")
	}
}

func TestBindParameter_NilParams(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext() // no WithParams call at all

	if _, ok := ec.BindParameter(1, ""); ok {
		t.Fatal("nil params, ordinal=1 should return false")
	}
}

func TestRowContext(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext().WithParams([]any{int64(99)})
	datum := map[string]any{"NAME": "alice"}

	rc := ec.RowContext(datum)
	if rc == nil {
		t.Fatal("RowContext returned nil")
	}
	if rc.Datum == nil {
		t.Fatal("RowContext.Datum is nil")
	}
	if rc.Datum["NAME"] != "alice" {
		t.Fatalf("RowContext.Datum[NAME] = %v, want alice", rc.Datum["NAME"])
	}
	if rc.Binder == nil {
		t.Fatal("RowContext.Binder is nil")
	}
	// Binder should delegate to EvaluationContext.BindParameter.
	got, ok := rc.Binder.BindParameter(1, "")
	if !ok || got != int64(99) {
		t.Fatalf("Binder.BindParameter(1) = (%v, %v), want (99, true)", got, ok)
	}
}

func TestWithBinding(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("x")
	ec2 := ec.WithBinding(id, "hello")

	got, ok := ec2.GetBinding(id)
	if !ok || got != "hello" {
		t.Fatalf("GetBinding after WithBinding = (%v, %v), want (hello, true)", got, ok)
	}
}

func TestWithBinding_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("x")
	_ = ec.WithBinding(id, "val")

	if _, ok := ec.GetBinding(id); ok {
		t.Fatal("WithBinding mutated the original context")
	}
}

func TestWithBinding_Chaining(t *testing.T) {
	t.Parallel()
	idA := values.NamedCorrelationIdentifier("a")
	idB := values.NamedCorrelationIdentifier("b")
	idC := values.NamedCorrelationIdentifier("c")

	ec := EmptyEvaluationContext().
		WithBinding(idA, int64(1)).
		WithBinding(idB, int64(2)).
		WithBinding(idC, int64(3))

	for _, tc := range []struct {
		id   values.CorrelationIdentifier
		want int64
	}{
		{idA, 1},
		{idB, 2},
		{idC, 3},
	} {
		got, ok := ec.GetBinding(tc.id)
		if !ok {
			t.Fatalf("GetBinding(%v) returned false", tc.id)
		}
		if got != tc.want {
			t.Fatalf("GetBinding(%v) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestGetBinding_MissingKey(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("missing")

	if _, ok := ec.GetBinding(id); ok {
		t.Fatal("GetBinding on missing key should return false")
	}
}

func TestGetBinding_PresentKey(t *testing.T) {
	t.Parallel()
	id := values.NamedCorrelationIdentifier("present")
	ec := EmptyEvaluationContext().WithBinding(id, int64(42))

	got, ok := ec.GetBinding(id)
	if !ok {
		t.Fatal("GetBinding on present key returned false")
	}
	if got != int64(42) {
		t.Fatalf("GetBinding = %v, want 42", got)
	}
}

func TestGetOrCreateTempTable_CreatesNew(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("tt1")

	tt := ec.GetOrCreateTempTable(id, nil)
	if tt == nil {
		t.Fatal("GetOrCreateTempTable returned nil")
	}
	if len(tt.GetList()) != 0 {
		t.Fatal("newly created TempTable should be empty")
	}
}

func TestGetOrCreateTempTable_ReturnsSameOnSecondCall(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("tt2")

	tt1 := ec.GetOrCreateTempTable(id, nil)
	tt2 := ec.GetOrCreateTempTable(id, nil)
	if tt1 != tt2 {
		t.Fatal("second GetOrCreateTempTable call returned a different *TempTable")
	}
}

func TestGetOrCreateTempTable_Functional(t *testing.T) {
	t.Parallel()
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("tt3")

	tt := ec.GetOrCreateTempTable(id, nil)
	qr := QueryResult{Datum: map[string]any{"id": int64(1)}}
	tt.Add(qr)

	list := tt.GetList()
	if len(list) != 1 {
		t.Fatalf("GetList len = %d, want 1", len(list))
	}
	d, ok := list[0].Datum.(map[string]any)
	if !ok {
		t.Fatalf("Datum type = %T, want map[string]any", list[0].Datum)
	}
	if d["id"] != int64(1) {
		t.Fatalf("Datum[id] = %v, want 1", d["id"])
	}
}

// ---------------------------------------------------------------------------
// TempTable tests
// ---------------------------------------------------------------------------

func TestNewTempTable_Empty(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	if tt == nil {
		t.Fatal("NewTempTable returned nil")
	}
	list := tt.GetList()
	if len(list) != 0 {
		t.Fatalf("new TempTable GetList len = %d, want 0", len(list))
	}
}

func TestTempTable_AddSingle(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	qr := QueryResult{Datum: map[string]any{"id": int64(1)}}
	tt.Add(qr)

	list := tt.GetList()
	if len(list) != 1 {
		t.Fatalf("GetList len = %d, want 1", len(list))
	}
	d := list[0].Datum.(map[string]any)
	if d["id"] != int64(1) {
		t.Fatalf("Datum[id] = %v, want 1", d["id"])
	}
}

func TestTempTable_AddMultiple_PreservesOrder(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	for i := int64(0); i < 5; i++ {
		tt.Add(QueryResult{Datum: map[string]any{"id": i}})
	}

	list := tt.GetList()
	if len(list) != 5 {
		t.Fatalf("GetList len = %d, want 5", len(list))
	}
	for i := int64(0); i < 5; i++ {
		d := list[i].Datum.(map[string]any)
		if d["id"] != i {
			t.Fatalf("list[%d].Datum[id] = %v, want %d", i, d["id"], i)
		}
	}
}

func TestTempTable_GetList_ReturnsCopy(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(1)}})
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(2)}})

	list := tt.GetList()
	// Mutate the returned slice — should not affect internal state.
	list[0] = QueryResult{Datum: map[string]any{"id": int64(999)}}
	_ = list[:1]

	list2 := tt.GetList()
	if len(list2) != 2 {
		t.Fatalf("after mutating copy, GetList len = %d, want 2", len(list2))
	}
	d := list2[0].Datum.(map[string]any)
	if d["id"] != int64(1) {
		t.Fatalf("internal data corrupted: Datum[id] = %v, want 1", d["id"])
	}
}

func TestTempTable_Clear(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(1)}})
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(2)}})
	tt.Clear()

	list := tt.GetList()
	if len(list) != 0 {
		t.Fatalf("after Clear, GetList len = %d, want 0", len(list))
	}
}

func TestTempTable_ClearThenAdd(t *testing.T) {
	t.Parallel()
	tt := NewTempTable()
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(1)}})
	tt.Clear()
	tt.Add(QueryResult{Datum: map[string]any{"id": int64(99)}})

	list := tt.GetList()
	if len(list) != 1 {
		t.Fatalf("after Clear+Add, GetList len = %d, want 1", len(list))
	}
	d := list[0].Datum.(map[string]any)
	if d["id"] != int64(99) {
		t.Fatalf("Datum[id] = %v, want 99", d["id"])
	}
}

func TestTempTable_ConcurrentAdd(t *testing.T) {
	t.Parallel()

	const goroutines = 10
	const itemsPerGoroutine = 100

	tt := NewTempTable()
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < itemsPerGoroutine; i++ {
				tt.Add(QueryResult{Datum: map[string]any{"id": int64(base*itemsPerGoroutine + i)}})
			}
		}(g)
	}
	wg.Wait()

	list := tt.GetList()
	want := goroutines * itemsPerGoroutine
	if len(list) != want {
		t.Fatalf("concurrent Add: GetList len = %d, want %d", len(list), want)
	}

	// Verify all items are unique (no lost writes).
	seen := make(map[int64]bool, want)
	for _, qr := range list {
		d := qr.Datum.(map[string]any)
		id := d["id"].(int64)
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != want {
		t.Fatalf("unique items = %d, want %d", len(seen), want)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkTempTable_Add(b *testing.B) {
	tt := NewTempTable()
	qr := QueryResult{Datum: map[string]any{"id": int64(1)}}
	for b.Loop() {
		tt.Add(qr)
	}
}

func BenchmarkTempTable_GetList(b *testing.B) {
	tt := NewTempTable()
	for i := 0; i < 1000; i++ {
		tt.Add(QueryResult{Datum: map[string]any{"id": int64(i)}})
	}
	b.ResetTimer()
	for b.Loop() {
		_ = tt.GetList()
	}
}

func BenchmarkEvaluationContext_WithBinding(b *testing.B) {
	ec := EmptyEvaluationContext()
	id := values.NamedCorrelationIdentifier("bench")
	for b.Loop() {
		ec = ec.WithBinding(id, int64(1))
	}
	// Prevent dead-code elimination.
	_ = ec
}
