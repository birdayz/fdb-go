package recordlayer

import (
	"context"
	"testing"
)

// intCompKey extracts an int as the comparison key.
func intCompKey(v int) []interface{} {
	return []interface{}{v}
}

func TestUnionCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 3, 5, 7})
	c2 := FromList([]int{2, 4, 6, 8})
	union := Union([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v := range Seq(union, ctx) {
		results = append(results, v)
	}

	expected := []int{1, 2, 3, 4, 5, 6, 7, 8}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestUnionCursorDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 5})
	c2 := FromList([]int{2, 3, 4, 5})
	union := Union([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v := range Seq(union, ctx) {
		results = append(results, v)
	}

	expected := []int{1, 2, 3, 4, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestUnionCursorReverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{7, 5, 3, 1})
	c2 := FromList([]int{8, 6, 4, 2})
	union := Union([]RecordCursor[int]{c1, c2}, intCompKey, true)

	var results []int
	for v := range Seq(union, ctx) {
		results = append(results, v)
	}

	expected := []int{8, 7, 6, 5, 4, 3, 2, 1}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestUnionCursorEmptyCursors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("all_empty", func(t *testing.T) {
		t.Parallel()
		c1 := Empty[int]()
		c2 := Empty[int]()
		union := Union([]RecordCursor[int]{c1, c2}, intCompKey, false)

		result, err := union.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})

	t.Run("one_empty", func(t *testing.T) {
		t.Parallel()
		c1 := FromList([]int{1, 3, 5})
		c2 := Empty[int]()
		union := Union([]RecordCursor[int]{c1, c2}, intCompKey, false)

		var results []int
		for v := range Seq(union, ctx) {
			results = append(results, v)
		}

		if len(results) != 3 {
			t.Fatalf("got %d results, want 3: %v", len(results), results)
		}
	})

	t.Run("no_cursors", func(t *testing.T) {
		t.Parallel()
		union := Union([]RecordCursor[int]{}, intCompKey, false)
		result, err := union.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})
}

func TestUnionCursorThree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 4, 7})
	c2 := FromList([]int{2, 5, 8})
	c3 := FromList([]int{3, 6, 9})
	union := Union([]RecordCursor[int]{c1, c2, c3}, intCompKey, false)

	var results []int
	for v := range Seq(union, ctx) {
		results = append(results, v)
	}

	expected := []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestUnionCursorContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 3, 5})
	c2 := FromList([]int{2, 4, 6})
	union := Union([]RecordCursor[int]{c1, c2}, intCompKey, false)

	// Read first two results
	r1, err := union.OnNext(ctx)
	if err != nil || !r1.HasNext() {
		t.Fatal("expected result 1")
	}
	if r1.GetValue() != 1 {
		t.Fatalf("result 1: got %d, want 1", r1.GetValue())
	}

	r2, err := union.OnNext(ctx)
	if err != nil || !r2.HasNext() {
		t.Fatal("expected result 2")
	}
	if r2.GetValue() != 2 {
		t.Fatalf("result 2: got %d, want 2", r2.GetValue())
	}

	// Continuation should be non-nil
	cont := r2.GetContinuation()
	if cont == nil || cont.IsEnd() {
		t.Fatal("continuation should not be end")
	}
	if len(cont.ToBytes()) == 0 {
		t.Fatal("continuation bytes should not be empty")
	}
}

func TestIntersectionCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 4, 5})
	c2 := FromList([]int{2, 4, 6, 8})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v := range Seq(inter, ctx) {
		results = append(results, v)
	}

	expected := []int{2, 4}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorNoOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 3, 5})
	c2 := FromList([]int{2, 4, 6})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v := range Seq(inter, ctx) {
		results = append(results, v)
	}

	if len(results) != 0 {
		t.Fatalf("expected no results, got %v", results)
	}
}

func TestIntersectionCursorReverse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{5, 4, 3, 2, 1})
	c2 := FromList([]int{8, 6, 4, 2})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, true)

	var results []int
	for v := range Seq(inter, ctx) {
		results = append(results, v)
	}

	expected := []int{4, 2}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorAllMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3})
	c2 := FromList([]int{1, 2, 3})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v := range Seq(inter, ctx) {
		results = append(results, v)
	}

	expected := []int{1, 2, 3}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
}

func TestIntersectionCursorThree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 4, 5, 6})
	c2 := FromList([]int{2, 3, 5, 6, 8})
	c3 := FromList([]int{3, 5, 7, 9})
	inter := Intersection([]RecordCursor[int]{c1, c2, c3}, intCompKey, false)

	var results []int
	for v := range Seq(inter, ctx) {
		results = append(results, v)
	}

	expected := []int{3, 5}
	if len(results) != len(expected) {
		t.Fatalf("got %d results, want %d: %v", len(results), len(expected), results)
	}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("result[%d]: got %d, want %d", i, v, expected[i])
		}
	}
}

func TestIntersectionCursorEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("one_empty", func(t *testing.T) {
		t.Parallel()
		c1 := FromList([]int{1, 2, 3})
		c2 := Empty[int]()
		inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

		result, err := inter.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})

	t.Run("no_cursors", func(t *testing.T) {
		t.Parallel()
		inter := Intersection([]RecordCursor[int]{}, intCompKey, false)
		result, err := inter.OnNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.HasNext() {
			t.Fatal("expected no results")
		}
	})
}

func TestCompareKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     []interface{}
		expected int
	}{
		{"equal", []interface{}{1, "a"}, []interface{}{1, "a"}, 0},
		{"less_first", []interface{}{1, "a"}, []interface{}{2, "a"}, -1},
		{"greater_first", []interface{}{2, "a"}, []interface{}{1, "a"}, 1},
		{"less_second", []interface{}{1, "a"}, []interface{}{1, "b"}, -1},
		{"shorter", []interface{}{1}, []interface{}{1, "a"}, -1},
		{"longer", []interface{}{1, "a"}, []interface{}{1}, 1},
		{"nil_first", []interface{}{nil, "a"}, []interface{}{1, "a"}, -1},
		{"both_nil", []interface{}{nil}, []interface{}{nil}, 0},
		{"empty", []interface{}{}, []interface{}{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compareKeys(tt.a, tt.b)
			if (tt.expected < 0 && got >= 0) || (tt.expected > 0 && got <= 0) || (tt.expected == 0 && got != 0) {
				t.Fatalf("compareKeys(%v, %v): got %d, want sign of %d", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

func TestCompareFieldTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     interface{}
		expected int
	}{
		{"int64_equal", int64(5), int64(5), 0},
		{"int64_less", int64(3), int64(5), -1},
		{"float64_less", 1.0, 2.0, -1},
		{"string_less", "abc", "def", -1},
		{"bool_false_lt_true", false, true, -1},
		{"bool_equal", true, true, 0},
		{"bytes_equal", []byte{1, 2}, []byte{1, 2}, 0},
		{"bytes_less", []byte{1, 2}, []byte{1, 3}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compareField(tt.a, tt.b)
			if (tt.expected < 0 && got >= 0) || (tt.expected > 0 && got <= 0) || (tt.expected == 0 && got != 0) {
				t.Fatalf("compareField(%v, %v): got %d, want sign of %d", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}
