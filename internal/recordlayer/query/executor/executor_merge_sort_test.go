package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestMergeSortCursor_TwoSortedInputs(t *testing.T) {
	t.Parallel()

	// Left:  id=1, id=3, id=5
	// Right: id=2, id=4, id=6
	// Expected merged ASC: 1,2,3,4,5,6
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(3)),
		qr("id", int64(5)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(4)),
		qr("id", int64(6)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false, // ascending
		false, // no dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}
	expected := []int64{1, 2, 3, 4, 5, 6}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_Deduplication(t *testing.T) {
	t.Parallel()

	// Both inputs have overlapping keys: 1,2,3 and 2,3,4
	// With dedup, should produce 1,2,3,4
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(3)),
		qr("id", int64(4)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false, // ascending
		true,  // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4; values: %v", len(results), results)
	}
	expected := []int64{1, 2, 3, 4}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_Reverse(t *testing.T) {
	t.Parallel()

	// Left:  id=5, id=3, id=1 (descending)
	// Right: id=6, id=4, id=2 (descending)
	// Expected merged DESC: 6,5,4,3,2,1
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(5)),
		qr("id", int64(3)),
		qr("id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(6)),
		qr("id", int64(4)),
		qr("id", int64(2)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		true,  // reverse (descending)
		false, // no dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}
	expected := []int64{6, 5, 4, 3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_EmptyInputs(t *testing.T) {
	t.Parallel()

	// Both inputs empty.
	left := recordlayer.FromList([]QueryResult{})
	right := recordlayer.FromList([]QueryResult{})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestMergeSortCursor_ZeroCursors(t *testing.T) {
	t.Parallel()

	// No cursors at all.
	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		nil,
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestMergeSortCursor_SingleInputPassthrough(t *testing.T) {
	t.Parallel()

	// Single input: should just pass through in order.
	input := recordlayer.FromList([]QueryResult{
		qr("id", int64(10)),
		qr("id", int64(20)),
		qr("id", int64(30)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{input},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	expected := []int64{10, 20, 30}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_NullComparisonKeys(t *testing.T) {
	t.Parallel()

	// NULL values in the comparison key. compareValues treats nil < non-nil.
	// Left:  id=nil, id=3
	// Right: id=1, id=nil (note: not properly sorted but tests nil handling)
	//
	// With ascending: nil < 1 < 3 < nil-second
	// Since nil < any, left's nil comes first, then right's 1,
	// then left's 3, then right's nil.
	// But right's second nil is NOT less than 3 (nil < 3 = true),
	// so it should come before 3? Let's trace carefully.
	//
	// Actually left=[nil, 3], right=[1, nil]
	// Peek: left=nil, right=1. isBetter(nil, 1): mustCompareValues(t, nil, 1)=-1, cmp<0 → true → pick left(nil)
	// Peek: left=3, right=1. isBetter(3, 1): mustCompareValues(t, 3, 1)=1, cmp<0 → false → pick right(1)
	// Peek: left=3, right=nil. isBetter(3, nil): mustCompareValues(t, 3, nil)=1, cmp<0 → false.
	//   isBetter(nil, 3): mustCompareValues(t, nil, 3)=-1, cmp<0 → true → pick right(nil)
	// Peek: left=3, right exhausted. Pick left(3).
	// Result: nil, 1, nil, 3
	left := recordlayer.FromList([]QueryResult{
		qr("id", nil),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", nil),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}

	// Verify nil values come first and non-nil values are ordered.
	// Expected order: nil, 1, nil, 3
	m0, _ := rowMapOK(results[0])
	if m0["id"] != nil {
		t.Errorf("results[0].id = %v, want nil", m0["id"])
	}
	if fieldVal(t, results[1], "id") != 1 {
		t.Errorf("results[1].id = %v, want 1", results[1].Positional)
	}
	m2, _ := rowMapOK(results[2])
	if m2["id"] != nil {
		t.Errorf("results[2].id = %v, want nil", m2["id"])
	}
	if fieldVal(t, results[3], "id") != 3 {
		t.Errorf("results[3].id = %v, want 3", results[3].Positional)
	}
}

func TestMergeSortCursor_UnequalLengthInputs(t *testing.T) {
	t.Parallel()

	// Left:  id=1, id=5
	// Right: id=2, id=3, id=4, id=6, id=7
	// Expected merged ASC: 1,2,3,4,5,6,7
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(5)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(3)),
		qr("id", int64(4)),
		qr("id", int64(6)),
		qr("id", int64(7)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 7 {
		t.Fatalf("got %d results, want 7", len(results))
	}
	expected := []int64{1, 2, 3, 4, 5, 6, 7}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_DedupWithAllDuplicates(t *testing.T) {
	t.Parallel()

	// Both inputs have the same keys: 1,2,3
	// With dedup, should produce 1,2,3
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
		qr("id", int64(3)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		true, // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3; values: %v", len(results), results)
	}
	expected := []int64{1, 2, 3}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

// TestMergeSortCursor_DedupTieBreak_PicksOrigIndexLeastLeg pins WHICH leg's
// row a tied dedup emits, not just that dedup drops the duplicate. Every
// other merge-sort fixture in this suite uses the comparison key as the
// row's ONLY field, so a tied pair is byte-identical and the choice between
// legs is unobservable — chosen[len(chosen)-1] (last-tied-leg, i.e. highest
// origIndex) would pass every one of them exactly as well as chosen[0]
// (origIndex-least, matching Java's chosenStates.get(0) —
// UnionCursorBase.java:70-72 — and mergeSortHeap's doc comment).
//
// Here leg 0 and leg 1 both hold id=5 but carry a distinguishing "tag" field
// the comparison key never touches, so the emitted row's tag reveals which
// leg's VALUE the cursor actually returned.
func TestMergeSortCursor_DedupTieBreak_PicksOrigIndexLeastLeg(t *testing.T) {
	t.Parallel()

	legA := recordlayer.FromList([]QueryResult{
		qr("id", int64(5), "tag", "A"),
	})
	legB := recordlayer.FromList([]QueryResult{
		qr("id", int64(5), "tag", "B"),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{legA, legB},
		[]values.Value{compKey},
		false, // ascending
		true,  // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (tied rows must dedup to one emission); values: %v", len(results), results)
	}
	m, ok := rowMapOK(results[0])
	if !ok {
		t.Fatalf("datum type %T, want map[string]any", results[0].Positional)
	}
	if got := m["tag"]; got != "A" {
		t.Fatalf("emitted tied row has tag %v, want %q (leg 0, the origIndex-least tied leg, "+
			"matching Java's chosenStates.get(0)) — a tied dedup emitted leg %v's row instead of leg 0's",
			got, "A", got)
	}
}

func TestMergeSortCursor_ThreeInputs(t *testing.T) {
	t.Parallel()

	// Three sorted inputs merged.
	a := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(4)),
		qr("id", int64(7)),
	})
	b := recordlayer.FromList([]QueryResult{
		qr("id", int64(2)),
		qr("id", int64(5)),
		qr("id", int64(8)),
	})
	ch := recordlayer.FromList([]QueryResult{
		qr("id", int64(3)),
		qr("id", int64(6)),
		qr("id", int64(9)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{a, b, ch},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 9 {
		t.Fatalf("got %d results, want 9", len(results))
	}
	for i := 0; i < 9; i++ {
		want := int64(i + 1)
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}

func TestMergeSortCursor_MultipleComparisonKeys(t *testing.T) {
	t.Parallel()

	// Sort by (group ASC, id ASC). Both inputs sorted by (group, id).
	left := recordlayer.FromList([]QueryResult{
		qr("group", int64(1), "id", int64(1)),
		qr("group", int64(1), "id", int64(3)),
		qr("group", int64(2), "id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("group", int64(1), "id", int64(2)),
		qr("group", int64(2), "id", int64(2)),
		qr("group", int64(3), "id", int64(1)),
	})

	compKeys := []values.Value{
		mustTestFieldOrdinal(t, mustTestQOV(t, values.UniqueCorrelationIdentifier(), exactTestRowType(
			values.Field{Name: "group", FieldType: values.NullableLong},
			values.Field{Name: "id", FieldType: values.NullableLong},
		)), 0),
		mustTestFieldOrdinal(t, mustTestQOV(t, values.UniqueCorrelationIdentifier(), exactTestRowType(
			values.Field{Name: "group", FieldType: values.NullableLong},
			values.Field{Name: "id", FieldType: values.NullableLong},
		)), 1),
	}
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		compKeys,
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 6 {
		t.Fatalf("got %d results, want 6", len(results))
	}

	type pair struct{ group, id int64 }
	expected := []pair{
		{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 2}, {3, 1},
	}
	for i, want := range expected {
		gotG := fieldVal(t, results[i], "group")
		gotI := fieldVal(t, results[i], "id")
		if gotG != want.group || gotI != want.id {
			t.Errorf("results[%d] = (%d,%d), want (%d,%d)", i, gotG, gotI, want.group, want.id)
		}
	}
}

func TestMergeSortCursor_OneEmptyOneNonEmpty(t *testing.T) {
	t.Parallel()

	// Left is empty, right has data.
	left := recordlayer.FromList([]QueryResult{})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(1)),
		qr("id", int64(2)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if fieldVal(t, results[0], "id") != 1 {
		t.Errorf("results[0].id = %d, want 1", fieldVal(t, results[0], "id"))
	}
	if fieldVal(t, results[1], "id") != 2 {
		t.Errorf("results[1].id = %d, want 2", fieldVal(t, results[1], "id"))
	}
}

func TestMergeSortCursor_StringComparisonKeys(t *testing.T) {
	t.Parallel()

	// Sort by string comparison key.
	left := recordlayer.FromList([]QueryResult{
		qr("name", "alice"),
		qr("name", "charlie"),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("name", "bob"),
		qr("name", "dave"),
	})

	compKey := mustNamedTestField(t, "name", values.TypeString)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		false,
		false,
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	expectedNames := []string{"alice", "bob", "charlie", "dave"}
	for i, want := range expectedNames {
		m, _ := rowMapOK(results[i])
		got := m["name"].(string)
		if got != want {
			t.Errorf("results[%d].name = %q, want %q", i, got, want)
		}
	}
}

func TestMergeSortCursor_CloseIdempotent(t *testing.T) {
	t.Parallel()

	input := recordlayer.FromList([]QueryResult{qr("id", int64(1))})
	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{input},
		[]values.Value{compKey},
		false,
		false,
	)

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !c.IsClosed() {
		t.Error("IsClosed = false after Close")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestMergeSortCursor_ReverseDedup(t *testing.T) {
	t.Parallel()

	// Reverse + dedup. Inputs sorted descending with overlaps.
	// Left:  id=5, id=3, id=1
	// Right: id=4, id=3, id=2
	// Merged DESC without dedup: 5,4,3,3,2,1
	// With dedup: 5,4,3,2,1
	left := recordlayer.FromList([]QueryResult{
		qr("id", int64(5)),
		qr("id", int64(3)),
		qr("id", int64(1)),
	})
	right := recordlayer.FromList([]QueryResult{
		qr("id", int64(4)),
		qr("id", int64(3)),
		qr("id", int64(2)),
	})

	compKey := mustNamedTestField(t, "id", values.NullableLong)
	c := newMergeSortCursor(
		[]recordlayer.RecordCursor[QueryResult]{left, right},
		[]values.Value{compKey},
		true, // reverse
		true, // dedup
	)
	defer c.Close()

	results := collectMergeSortCursor(t, c)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5; values: %v", len(results), results)
	}
	expected := []int64{5, 4, 3, 2, 1}
	for i, want := range expected {
		got := fieldVal(t, results[i], "id")
		if got != want {
			t.Errorf("results[%d].id = %d, want %d", i, got, want)
		}
	}
}
