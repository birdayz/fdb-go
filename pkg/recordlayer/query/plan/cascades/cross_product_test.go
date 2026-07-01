package cascades

import (
	"testing"
)

func TestCrossProduct_TwoLists(t *testing.T) {
	t.Parallel()
	result := CrossProduct([][]string{{"a", "b"}, {"x", "y"}})
	if len(result) != 4 {
		t.Fatalf("expected 4 combos, got %d", len(result))
	}
	expected := [][]string{{"a", "x"}, {"a", "y"}, {"b", "x"}, {"b", "y"}}
	for i, combo := range result {
		if len(combo) != 2 || combo[0] != expected[i][0] || combo[1] != expected[i][1] {
			t.Fatalf("combo %d: expected %v, got %v", i, expected[i], combo)
		}
	}
}

func TestCrossProduct_SingleList(t *testing.T) {
	t.Parallel()
	result := CrossProduct([][]int{{1, 2, 3}})
	if len(result) != 3 {
		t.Fatalf("expected 3 combos, got %d", len(result))
	}
	for i, combo := range result {
		if len(combo) != 1 || combo[0] != i+1 {
			t.Fatalf("combo %d: expected [%d], got %v", i, i+1, combo)
		}
	}
}

func TestCrossProduct_ThreeLists(t *testing.T) {
	t.Parallel()
	result := CrossProduct([][]int{{1, 2}, {3}, {4, 5}})
	if len(result) != 4 {
		t.Fatalf("expected 4 combos (2*1*2), got %d", len(result))
	}
}

func TestCrossProduct_EmptyInput(t *testing.T) {
	t.Parallel()
	result := CrossProduct[int](nil)
	if result != nil {
		t.Fatalf("expected nil for empty input, got %v", result)
	}
}

func TestCrossProduct_EmptyInnerList(t *testing.T) {
	t.Parallel()
	result := CrossProduct([][]int{{1, 2}, {}, {3}})
	if result != nil {
		t.Fatalf("expected nil when any inner list is empty, got %v", result)
	}
}

func TestCrossProduct_SingleElementLists(t *testing.T) {
	t.Parallel()
	result := CrossProduct([][]string{{"a"}, {"b"}, {"c"}})
	if len(result) != 1 {
		t.Fatalf("expected 1 combo, got %d", len(result))
	}
	if result[0][0] != "a" || result[0][1] != "b" || result[0][2] != "c" {
		t.Fatalf("expected [a b c], got %v", result[0])
	}
}

func TestOrderingBinding_Sorted(t *testing.T) {
	t.Parallel()
	b := SortedBinding(ProvidedSortOrderAscending)
	if !b.IsSorted() {
		t.Fatal("expected sorted")
	}
	if b.IsFixed() || b.IsChoose() {
		t.Fatal("should not be fixed or choose")
	}
	if b.GetSortOrder() != ProvidedSortOrderAscending {
		t.Fatal("wrong sort order")
	}
}

func TestOrderingBinding_Fixed(t *testing.T) {
	t.Parallel()
	b := FixedBinding("eq-5")
	if !b.IsFixed() {
		t.Fatal("expected fixed")
	}
	if b.IsSorted() || b.IsChoose() {
		t.Fatal("should not be sorted or choose")
	}
	if b.GetComparison() != "eq-5" {
		t.Fatal("wrong comparison")
	}
}

func TestOrderingBinding_Choose(t *testing.T) {
	t.Parallel()
	b := ChooseBinding()
	if !b.IsChoose() {
		t.Fatal("expected choose")
	}
	if b.IsSorted() || b.IsFixed() {
		t.Fatal("should not be sorted or fixed")
	}
}

func TestProvidedSortOrder_IsDirectional(t *testing.T) {
	t.Parallel()
	directional := []ProvidedSortOrder{
		ProvidedSortOrderAscending, ProvidedSortOrderDescending,
		ProvidedSortOrderAscendingNullsLast, ProvidedSortOrderDescendingNullsFirst,
	}
	for _, s := range directional {
		if !s.IsDirectional() {
			t.Fatalf("%d should be directional", s)
		}
	}
	nonDirectional := []ProvidedSortOrder{ProvidedSortOrderFixed, ProvidedSortOrderChoose}
	for _, s := range nonDirectional {
		if s.IsDirectional() {
			t.Fatalf("%d should not be directional", s)
		}
	}
}

func TestProvidedSortOrder_IsAnyDescending(t *testing.T) {
	t.Parallel()
	if !ProvidedSortOrderDescending.IsAnyDescending() {
		t.Fatal("descending should be any-descending")
	}
	if !ProvidedSortOrderDescendingNullsFirst.IsAnyDescending() {
		t.Fatal("descending-nulls-first should be any-descending")
	}
	if ProvidedSortOrderAscending.IsAnyDescending() {
		t.Fatal("ascending should not be any-descending")
	}
}

func TestCrossProductIterator_Basic(t *testing.T) {
	t.Parallel()
	iter := NewCrossProductIterator([][]string{{"a", "b"}, {"x", "y"}})
	var result [][]string
	for iter.HasNext() {
		result = append(result, iter.Next())
	}
	if len(result) != 4 {
		t.Fatalf("expected 4, got %d", len(result))
	}
	expected := [][]string{{"a", "x"}, {"a", "y"}, {"b", "x"}, {"b", "y"}}
	for i, combo := range result {
		if combo[0] != expected[i][0] || combo[1] != expected[i][1] {
			t.Fatalf("combo %d: expected %v, got %v", i, expected[i], combo)
		}
	}
}

func TestCrossProductIterator_Skip(t *testing.T) {
	t.Parallel()
	// [a,b,c] x [x,y] — skip at depth 1 after seeing (a,x) should jump to (b,x)
	iter := NewCrossProductIterator([][]string{{"a", "b", "c"}, {"x", "y"}})
	first := iter.Next() // (a, x)
	if first[0] != "a" || first[1] != "x" {
		t.Fatalf("first: %v", first)
	}
	iter.Skip(1) // skip past all (a, ...) → next is (b, x)
	second := iter.Next()
	if second[0] != "b" || second[1] != "x" {
		t.Fatalf("after skip(1): expected (b,x), got %v", second)
	}
}

func TestCrossProductIterator_SkipDeep(t *testing.T) {
	t.Parallel()
	// [a,b] x [x,y] x [1,2] — skip(2) after (a,x,1) skips all (a,x,...) → (a,y,1)
	iter := NewCrossProductIterator([][]string{{"a", "b"}, {"x", "y"}, {"1", "2"}})
	first := iter.Next() // (a,x,1)
	if first[0] != "a" || first[1] != "x" || first[2] != "1" {
		t.Fatalf("first: %v", first)
	}
	iter.Skip(2) // skip past (a,x,...) → next is (a,y,1)
	second := iter.Next()
	if second[0] != "a" || second[1] != "y" || second[2] != "1" {
		t.Fatalf("after skip(2): expected (a,y,1), got %v", second)
	}
}

func TestCrossProductIterator_EmptyList(t *testing.T) {
	t.Parallel()
	iter := NewCrossProductIterator([][]string{{"a"}, {}})
	if iter.HasNext() {
		t.Fatal("empty inner list should produce no elements")
	}
}

func FuzzCrossProductIterator_SkipNeverPanics(f *testing.F) {
	f.Add(uint8(3), uint8(2), uint8(1))
	f.Add(uint8(1), uint8(1), uint8(0))
	f.Add(uint8(5), uint8(3), uint8(2))
	f.Fuzz(func(t *testing.T, nLists, listSize, skipAt uint8) {
		n := int(nLists%5) + 1
		sz := int(listSize%4) + 1
		lists := make([][]int, n)
		for i := range lists {
			lists[i] = make([]int, sz)
			for j := range lists[i] {
				lists[i][j] = i*10 + j
			}
		}
		iter := NewCrossProductIterator(lists)
		count := 0
		skipAfter := int(skipAt) % (n*sz + 1)
		for iter.HasNext() {
			iter.Next()
			count++
			if count == skipAfter && n > 0 {
				depth := (count % n) + 1
				iter.Skip(depth)
			}
			if count > 10000 {
				t.Fatal("too many iterations — likely infinite loop")
			}
		}
	})
}
