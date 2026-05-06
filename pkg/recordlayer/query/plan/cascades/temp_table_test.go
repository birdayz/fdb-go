package cascades_test

import (
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
)

func TestTempTable_AddSingle(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add("hello")
	if tt.Len() != 1 {
		t.Fatalf("Len()=%d, want 1", tt.Len())
	}
	got := tt.List()
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("List()=%v, want [hello]", got)
	}
}

func TestTempTable_AddMultiple_OrderPreserved(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add(1)
	tt.Add(2)
	tt.Add(3)
	if tt.Len() != 3 {
		t.Fatalf("Len()=%d, want 3", tt.Len())
	}
	got := tt.List()
	for i, want := range []any{1, 2, 3} {
		if got[i] != want {
			t.Fatalf("List()[%d]=%v, want %v", i, got[i], want)
		}
	}
}

func TestTempTable_Clear(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add("a")
	tt.Add("b")
	tt.Clear()
	if tt.Len() != 0 {
		t.Fatalf("Len() after Clear()=%d, want 0", tt.Len())
	}
	if !tt.IsEmpty() {
		t.Fatal("IsEmpty() after Clear() should be true")
	}
	got := tt.List()
	if len(got) != 0 {
		t.Fatalf("List() after Clear()=%v, want []", got)
	}
}

func TestTempTable_IsEmpty_Empty(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	if !tt.IsEmpty() {
		t.Fatal("fresh TempTable should be empty")
	}
}

func TestTempTable_IsEmpty_NonEmpty(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add(42)
	if tt.IsEmpty() {
		t.Fatal("TempTable with one element should not be empty")
	}
}

func TestTempTable_ListReturnsCopy(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add("x")
	tt.Add("y")
	got := tt.List()
	// Mutate the returned slice.
	got[0] = "MUTATED"
	_ = append(got, "extra")
	// The table should be unaffected.
	original := tt.List()
	if len(original) != 2 {
		t.Fatalf("mutation of List() return affected table: len=%d, want 2", len(original))
	}
	if original[0] != "x" {
		t.Fatalf("mutation of List() return affected table: [0]=%v, want x", original[0])
	}
}

func TestTempTable_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	const goroutines = 50
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				tt.Add(i)
			}
		}()
	}
	wg.Wait()

	want := goroutines * perGoroutine
	if tt.Len() != want {
		t.Fatalf("Len()=%d after concurrent adds, want %d", tt.Len(), want)
	}
	if len(tt.List()) != want {
		t.Fatalf("len(List())=%d, want %d", len(tt.List()), want)
	}
}

func TestTempTable_ClearThenReuse(t *testing.T) {
	t.Parallel()
	tt := cascades.NewTempTable()
	tt.Add("first")
	tt.Clear()
	tt.Add("second")
	if tt.Len() != 1 {
		t.Fatalf("Len()=%d after clear+add, want 1", tt.Len())
	}
	got := tt.List()
	if got[0] != "second" {
		t.Fatalf("List()[0]=%v, want second", got[0])
	}
}
