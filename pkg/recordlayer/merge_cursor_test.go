package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// intCompKey extracts an int as the comparison key.
func intCompKey(v int) tuple.Tuple {
	return tuple.Tuple{v}
}

func TestIntersectionCursorBasic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	c1 := FromList([]int{1, 2, 3, 4, 5})
	c2 := FromList([]int{2, 4, 6, 8})
	inter := Intersection([]RecordCursor[int]{c1, c2}, intCompKey, false)

	var results []int
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
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
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
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
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
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
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
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
	for v, iterErr := range Seq2(inter, ctx) {
		if iterErr != nil {
			t.Fatalf("Seq2: %v", iterErr)
		}
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
		a, b     tuple.Tuple
		expected int
	}{
		{"equal", tuple.Tuple{1, "a"}, tuple.Tuple{1, "a"}, 0},
		{"less_first", tuple.Tuple{1, "a"}, tuple.Tuple{2, "a"}, -1},
		{"greater_first", tuple.Tuple{2, "a"}, tuple.Tuple{1, "a"}, 1},
		{"less_second", tuple.Tuple{1, "a"}, tuple.Tuple{1, "b"}, -1},
		{"shorter", tuple.Tuple{1}, tuple.Tuple{1, "a"}, -1},
		{"longer", tuple.Tuple{1, "a"}, tuple.Tuple{1}, 1},
		{"nil_first", tuple.Tuple{nil, "a"}, tuple.Tuple{1, "a"}, -1},
		{"both_nil", tuple.Tuple{nil}, tuple.Tuple{nil}, 0},
		{"empty", tuple.Tuple{}, tuple.Tuple{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := compareKeys(tt.a, tt.b)
			if err != nil {
				t.Fatalf("compareKeys(%v, %v): unexpected error: %v", tt.a, tt.b, err)
			}
			if (tt.expected < 0 && got >= 0) || (tt.expected > 0 && got <= 0) || (tt.expected == 0 && got != 0) {
				t.Fatalf("compareKeys(%v, %v): got %d, want sign of %d", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}
