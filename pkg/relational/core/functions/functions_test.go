package functions

import "testing"

// ---------------------------------------------------------------------------
// CompareValues
// ---------------------------------------------------------------------------

func TestCompareValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b any
		want int
	}{
		// NULL ordering.
		{"nil_nil", nil, nil, 0},
		{"nil_int", nil, int64(1), -1},
		{"int_nil", int64(1), nil, 1},

		// int64 × int64.
		{"int64_equal", int64(42), int64(42), 0},
		{"int64_less", int64(1), int64(2), -1},
		{"int64_greater", int64(5), int64(3), 1},
		{"int64_negative", int64(-10), int64(-5), -1},
		{"int64_zero", int64(0), int64(0), 0},

		// int64 × float64 promotion.
		{"int64_float64_equal", int64(3), float64(3.0), 0},
		{"int64_float64_less", int64(2), float64(2.5), -1},
		{"float64_int64_greater", float64(5.5), int64(5), 1},

		// float64 × float64.
		{"float64_equal", float64(1.5), float64(1.5), 0},
		{"float64_less", float64(1.0), float64(2.0), -1},
		{"float64_greater", float64(3.0), float64(1.0), 1},

		// string.
		{"string_equal", "abc", "abc", 0},
		{"string_less", "abc", "abd", -1},
		{"string_greater", "abd", "abc", 1},
		{"string_empty", "", "", 0},
		{"string_empty_vs_nonempty", "", "a", -1},

		// bool (false < true).
		{"bool_equal_true", true, true, 0},
		{"bool_equal_false", false, false, 0},
		{"bool_false_lt_true", false, true, -1},
		{"bool_true_gt_false", true, false, 1},

		// []byte.
		{"bytes_equal", []byte{1, 2, 3}, []byte{1, 2, 3}, 0},
		{"bytes_less", []byte{1, 2}, []byte{1, 3}, -1},
		{"bytes_greater", []byte{1, 3}, []byte{1, 2}, 1},
		{"bytes_empty", []byte{}, []byte{}, 0},

		// Cross-type: stable non-zero result. We don't care about the
		// specific sign, just that it's not zero (meaning "not equal").
		{"string_vs_bool", "hello", true, 0},      // placeholder; checked below
		{"int64_vs_string", int64(1), "hello", 0}, // placeholder; checked below
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CompareValues(tt.a, tt.b)

			// For cross-type tests we only assert non-zero.
			switch tt.name {
			case "string_vs_bool", "int64_vs_string":
				if got == 0 {
					t.Errorf("CompareValues(%v, %v) = 0, want non-zero for cross-type", tt.a, tt.b)
				}
			default:
				if got != tt.want {
					t.Errorf("CompareValues(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
				}
			}
		})
	}
}

func TestCompareValues_CrossTypeStable(t *testing.T) {
	t.Parallel()
	// The cross-type result must be consistent across calls (stable ordering).
	r1 := CompareValues(int64(1), "hello")
	r2 := CompareValues(int64(1), "hello")
	if r1 != r2 {
		t.Errorf("cross-type compare not stable: %d vs %d", r1, r2)
	}
	if r1 == 0 {
		t.Error("cross-type compare returned 0, expected non-zero")
	}
}

// ---------------------------------------------------------------------------
// StripStringLiteralQuotes
// ---------------------------------------------------------------------------

func TestStripStringLiteralQuotes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"quoted", "'hello'", "hello"},
		{"escaped_quote", "'it''s'", "it's"},
		{"no_quotes", "hello", "hello"},
		{"empty_quoted", "''", ""},
		{"single_char_quoted", "'x'", "x"},
		{"multiple_escaped", "'a''b''c'", "a'b'c"},
		{"only_single_quote", "'", "'"},
		{"empty_string", "", ""},
		{"double_quotes_untouched", "\"hello\"", "\"hello\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := StripStringLiteralQuotes(tt.input)
			if got != tt.want {
				t.Errorf("StripStringLiteralQuotes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func assertEq(t *testing.T, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("got %v (%T), want %v (%T)", got, got, want, want)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkCompareValues_Int64(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(int64(42), int64(99))
	}
}

func BenchmarkCompareValues_Float64(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(float64(3.14), float64(2.71))
	}
}

func BenchmarkCompareValues_MixedNumeric(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(int64(42), float64(42.0))
	}
}

func BenchmarkCompareValues_String(b *testing.B) {
	for b.Loop() {
		_ = CompareValues("hello world", "hello xorld")
	}
}

func BenchmarkCompareValues_Bytes(b *testing.B) {
	a := []byte("hello world this is test data")
	c := []byte("hello world this is test datb")
	for b.Loop() {
		_ = CompareValues(a, c)
	}
}

func BenchmarkCompareValues_NilLeft(b *testing.B) {
	for b.Loop() {
		_ = CompareValues(nil, int64(1))
	}
}

func BenchmarkStripStringLiteralQuotes(b *testing.B) {
	for b.Loop() {
		_ = StripStringLiteralQuotes("'it''s a test'")
	}
}
