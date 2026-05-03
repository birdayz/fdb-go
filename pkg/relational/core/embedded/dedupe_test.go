package embedded

import "testing"

func TestDedupeAny_Nil(t *testing.T) {
	t.Parallel()
	got := dedupeAny(nil)
	if got != nil {
		t.Fatalf("dedupeAny(nil) = %v, want nil", got)
	}
}

func TestDedupeAny_Empty(t *testing.T) {
	t.Parallel()
	in := []any{}
	got := dedupeAny(in)
	if len(got) != 0 {
		t.Fatalf("dedupeAny([]) len = %d, want 0", len(got))
	}
}

func TestDedupeAny_SingleElement(t *testing.T) {
	t.Parallel()
	in := []any{int64(42)}
	got := dedupeAny(in)
	if len(got) != 1 || got[0] != int64(42) {
		t.Fatalf("dedupeAny([42]) = %v, want [42]", got)
	}
}

func TestDedupeAny_NoDuplicates(t *testing.T) {
	t.Parallel()
	in := []any{int64(1), int64(2), int64(3)}
	got := dedupeAny(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestDedupeAny_Int64Duplicates(t *testing.T) {
	t.Parallel()
	in := []any{int64(1), int64(2), int64(1), int64(3), int64(2)}
	got := dedupeAny(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []int64{1, 2, 3}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %v, want %d", i, got[i], w)
		}
	}
}

func TestDedupeAny_StringDuplicates(t *testing.T) {
	t.Parallel()
	in := []any{"a", "b", "a", "c", "b", "a"}
	got := dedupeAny(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %v, want %q", i, got[i], w)
		}
	}
}

func TestDedupeAny_MixedTypes(t *testing.T) {
	t.Parallel()
	in := []any{int64(1), "1", int64(1), "1"}
	got := dedupeAny(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (int64(1) and \"1\" are distinct types)", len(got))
	}
}

func TestDedupeAny_NumericPromotion(t *testing.T) {
	t.Parallel()
	in := []any{int64(5), float64(5.0), int64(5)}
	got := dedupeAny(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (int64(5) == float64(5.0) via valuesEqual)", len(got))
	}
}

func TestDedupeAny_PreservesOrder(t *testing.T) {
	t.Parallel()
	in := []any{int64(3), int64(1), int64(2), int64(1), int64(3)}
	got := dedupeAny(in)
	want := []int64{3, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %v, want %d", i, got[i], w)
		}
	}
}

func TestDedupeAny_AllDuplicates(t *testing.T) {
	t.Parallel()
	in := []any{int64(7), int64(7), int64(7), int64(7)}
	got := dedupeAny(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}

func TestDedupeAny_ByteSlices(t *testing.T) {
	t.Parallel()
	in := []any{[]byte{1, 2}, []byte{3, 4}, []byte{1, 2}}
	got := dedupeAny(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestDedupeAny_BoolValues(t *testing.T) {
	t.Parallel()
	in := []any{true, false, true, false, true}
	got := dedupeAny(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func BenchmarkDedupeAny_Small(b *testing.B) {
	in := []any{int64(1), int64(2), int64(3), int64(1), int64(2)}
	for b.Loop() {
		_ = dedupeAny(in)
	}
}

func BenchmarkDedupeAny_Large(b *testing.B) {
	in := make([]any, 100)
	for i := range in {
		in[i] = int64(i % 20)
	}
	for b.Loop() {
		_ = dedupeAny(in)
	}
}
