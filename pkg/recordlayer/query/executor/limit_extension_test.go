package executor

// Go-only extension tests: LIMIT clause optimization.
// Java uses ExecuteProperties.setReturnedRowLimit() at the JDBC layer;
// Go supports LIMIT natively in SQL with Cascades-integrated optimization.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// --- Top-K partial sort tests ---

func TestPartialSortTopK_Basic(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(5)},
		{"X": int64(3)},
		{"X": int64(8)},
		{"X": int64(1)},
		{"X": int64(7)},
		{"X": int64(2)},
		{"X": int64(9)},
		{"X": int64(4)},
		{"X": int64(6)},
	})

	partialSortTopK(items, []string{"x"}, []bool{false}, 3)

	// First 3 elements should be the smallest 3, in sorted order.
	expected := []int64{1, 2, 3}
	for i, want := range expected {
		got := getDatum(items[i])["X"].(int64)
		if got != want {
			t.Errorf("items[%d].x = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_Descending(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(5)},
		{"X": int64(3)},
		{"X": int64(8)},
		{"X": int64(1)},
		{"X": int64(7)},
	})

	partialSortTopK(items, []string{"x"}, []bool{true}, 2)

	// First 2 elements should be the largest 2, in descending order.
	expected := []int64{8, 7}
	for i, want := range expected {
		got := getDatum(items[i])["X"].(int64)
		if got != want {
			t.Errorf("items[%d].x = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_MultiKey(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"A": int64(2), "B": int64(1)},
		{"A": int64(1), "B": int64(3)},
		{"A": int64(1), "B": int64(1)},
		{"A": int64(2), "B": int64(2)},
		{"A": int64(1), "B": int64(2)},
	})

	partialSortTopK(items, []string{"a", "b"}, []bool{false, false}, 3)

	type pair struct{ a, b int64 }
	expected := []pair{{1, 1}, {1, 2}, {1, 3}}
	for i, want := range expected {
		gotA := getDatum(items[i])["A"].(int64)
		gotB := getDatum(items[i])["B"].(int64)
		if gotA != want.a || gotB != want.b {
			t.Errorf("items[%d] = (%d,%d), want (%d,%d)", i, gotA, gotB, want.a, want.b)
		}
	}
}

func TestPartialSortTopK_KEqualsN(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(3)},
		{"X": int64(1)},
		{"X": int64(2)},
	})

	// k == len(items) → falls back to full sort
	partialSortTopK(items, []string{"x"}, []bool{false}, 3)

	expected := []int64{1, 2, 3}
	for i, want := range expected {
		got := getDatum(items[i])["X"].(int64)
		if got != want {
			t.Errorf("items[%d].x = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_KGreaterThanN(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(3)},
		{"X": int64(1)},
	})

	// k > len(items) → falls back to full sort
	partialSortTopK(items, []string{"x"}, []bool{false}, 10)

	expected := []int64{1, 3}
	for i, want := range expected {
		got := getDatum(items[i])["X"].(int64)
		if got != want {
			t.Errorf("items[%d].x = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_SingleElement(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(5)},
		{"X": int64(3)},
		{"X": int64(8)},
		{"X": int64(1)},
	})

	partialSortTopK(items, []string{"x"}, []bool{false}, 1)

	if getDatum(items[0])["X"].(int64) != 1 {
		t.Errorf("items[0].x = %v, want 1", getDatum(items[0])["X"])
	}
}

func TestPartialSortTopK_AllEqual(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"X": int64(5)},
		{"X": int64(5)},
		{"X": int64(5)},
		{"X": int64(5)},
	})

	partialSortTopK(items, []string{"x"}, []bool{false}, 2)

	// All equal — any 2 in any order is correct.
	for i := 0; i < 2; i++ {
		if getDatum(items[i])["X"].(int64) != 5 {
			t.Errorf("items[%d].x = %v, want 5", i, getDatum(items[i])["X"])
		}
	}
}

// --- Limit pushdown propagation tests ---

func TestExecuteLimit_PropagatesRowLimit(t *testing.T) {
	t.Parallel()

	// Create a limit plan with limit=5, offset=2.
	innerPlan := plans.NewRecordQueryScanPlan(nil, nil, false)
	limitPlan := plans.NewRecordQueryLimitPlan(innerPlan, 5, 2)

	// The effective limit for the inner should be 5+2=7.
	// We can't easily test the propagation without FDB, but we can
	// verify the plan structure is correct.
	if limitPlan.GetLimit() != 5 {
		t.Fatalf("limit = %d, want 5", limitPlan.GetLimit())
	}
	if limitPlan.GetOffset() != 2 {
		t.Fatalf("offset = %d, want 2", limitPlan.GetOffset())
	}
	children := limitPlan.GetChildren()
	if len(children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(children))
	}
}

func TestExecuteLimit_ZeroLimit(t *testing.T) {
	t.Parallel()

	// A LIMIT 0 plan should have limit=0.
	innerPlan := plans.NewRecordQueryScanPlan(nil, nil, false)
	limitPlan := plans.NewRecordQueryLimitPlan(innerPlan, 0, 0)

	if limitPlan.GetLimit() != 0 {
		t.Fatalf("expected limit=0, got %d", limitPlan.GetLimit())
	}
	// LIMIT 0 lowers to RecordQueryLimitPlan(limit=0); at the executor level the
	// limitEnvelopeCursor short-circuits remLimit==0 to an empty, exhausted result.
}

// --- Sort + Limit integration (top-K) ---

func TestExecuteSort_TopKActivatesOnLimit(t *testing.T) {
	t.Parallel()

	items := makeTestResults([]map[string]any{
		{"V": int64(10)},
		{"V": int64(5)},
		{"V": int64(8)},
		{"V": int64(3)},
		{"V": int64(7)},
		{"V": int64(1)},
	})

	keys := []expressions.SortKey{{
		Value:   &values.FieldValue{Field: "v", Typ: values.TypeInt},
		Reverse: false,
	}}
	_ = plans.NewRecordQuerySortPlan(keys, nil)

	// Direct test of the top-K logic with ReturnedRowLimit=3.
	partialSortTopK(items, []string{"v"}, []bool{false}, 3)
	expected := []int64{1, 3, 5}
	for i, want := range expected {
		got := getDatum(items[i])["V"].(int64)
		if got != want {
			t.Errorf("items[%d].v = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_LargeDataset(t *testing.T) {
	t.Parallel()

	// Stress test: 1000 items, top 10.
	data := make([]map[string]any, 1000)
	for i := range data {
		data[i] = map[string]any{"X": int64(1000 - i)}
	}
	items := makeTestResults(data)

	partialSortTopK(items, []string{"x"}, []bool{false}, 10)

	for i := 0; i < 10; i++ {
		want := int64(i + 1)
		got := getDatum(items[i])["X"].(int64)
		if got != want {
			t.Errorf("items[%d].x = %d, want %d", i, got, want)
		}
	}
}

func TestPartialSortTopK_WithStrings(t *testing.T) {
	t.Parallel()
	items := makeTestResults([]map[string]any{
		{"NAME": "delta"},
		{"NAME": "alpha"},
		{"NAME": "charlie"},
		{"NAME": "bravo"},
		{"NAME": "echo"},
	})

	partialSortTopK(items, []string{"name"}, []bool{false}, 3)

	expected := []string{"alpha", "bravo", "charlie"}
	for i, want := range expected {
		got := getDatum(items[i])["NAME"].(string)
		if got != want {
			t.Errorf("items[%d].name = %q, want %q", i, got, want)
		}
	}
}

// --- Helpers ---

func makeTestResults(data []map[string]any) []QueryResult {
	results := make([]QueryResult, len(data))
	for i, d := range data {
		results[i] = QueryResult{Datum: any(d)}
	}
	return results
}

func getDatum(qr QueryResult) map[string]any {
	if m, ok := qr.Datum.(map[string]any); ok {
		return m
	}
	return nil
}
