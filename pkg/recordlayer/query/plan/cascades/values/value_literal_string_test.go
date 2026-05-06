package values

// Targeted tests for valueLiteralString — the literal-rendering helper
// behind ExplainValue + formatCompareOperand. Indirectly tested via
// ExplainValue today, but the int-narrow types (int, int32, int16,
// int8), float32, []byte, []any-with-NULL, and []any-with-string
// branches were 0% reached. ExplainValue is called from many plans
// in the planner, so coverage drift here would surface as
// inconsistent EXPLAIN output and silently break plandiff hashes.

import "testing"

// TestExplainValue_NarrowIntTypes pins int/int32/int16/int8 → decimal
// rendering. ConstantValue.Value is `any`, so callers can pass any
// of these even if the simplifier normalises to int64.
func TestExplainValue_NarrowIntTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"int positive", int(42), "42"},
		{"int negative", int(-7), "-7"},
		{"int zero", int(0), "0"},
		{"int32", int32(1234), "1234"},
		{"int16", int16(-9), "-9"},
		{"int8", int8(127), "127"},
		// MIN_INT64 boundary — pre-fix intToDec used `n = -n` which
		// overflows for math.MinInt64 (|MinInt64| > MaxInt64) and
		// produced just "-". Pin the correct full decimal so a
		// future intToDec rewrite can't regress this — ExplainValue
		// is the plan-cache key seam and a wrong rendering would
		// hash-collide distinct queries.
		{"int64 MIN", int64(-9223372036854775808), "-9223372036854775808"},
		{"int64 MAX", int64(9223372036854775807), "9223372036854775807"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ExplainValue(&ConstantValue{Value: tc.v, Typ: TypeInt})
			if got != tc.want {
				t.Fatalf("ExplainValue(%v): got %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestExplainValue_FloatTypes pins float32 + float64 rendering.
// strconv.FormatFloat with 'g'/-1 and bitSize 32 vs 64 produces
// subtly different shortest-round-trip output.
func TestExplainValue_FloatTypes(t *testing.T) {
	t.Parallel()
	got := ExplainValue(&ConstantValue{Value: float64(1.5), Typ: TypeFloat})
	if got != "1.5" {
		t.Fatalf("float64 1.5: got %q", got)
	}
	got = ExplainValue(&ConstantValue{Value: float32(2.5), Typ: TypeFloat})
	if got != "2.5" {
		t.Fatalf("float32 2.5: got %q", got)
	}
}

// TestExplainValue_BoolLiteral pins TRUE/FALSE rendering — bool
// shows up in BooleanValue today but ConstantValue with a bool also
// flows through the same renderer.
func TestExplainValue_BoolLiteral(t *testing.T) {
	t.Parallel()
	if got := ExplainValue(&ConstantValue{Value: true, Typ: TypeBool}); got != "TRUE" {
		t.Fatalf("true: got %q, want TRUE", got)
	}
	if got := ExplainValue(&ConstantValue{Value: false, Typ: TypeBool}); got != "FALSE" {
		t.Fatalf("false: got %q, want FALSE", got)
	}
}

// TestExplainValue_BytesAsHexLiteral pins the SQL hex-literal form
// `X'0102'`. Required for plandiff hash injectivity over byte slices —
// `X'0102'` ≠ `X'0103'`.
func TestExplainValue_BytesAsHexLiteral(t *testing.T) {
	t.Parallel()
	got := ExplainValue(&ConstantValue{Value: []byte{0x01, 0x02, 0xab, 0xcd}, Typ: TypeUnknown})
	if got != "X'0102abcd'" {
		t.Fatalf("got %q, want X'0102abcd'", got)
	}
	got = ExplainValue(&ConstantValue{Value: []byte{}, Typ: TypeUnknown})
	if got != "X''" {
		t.Fatalf("empty bytes: got %q, want X''", got)
	}
}

// TestExplainValue_AnySlice_INListRendering pins the IN-list shape:
// (1, 2, 3). Comma-separated with parens, NULL elements become NULL,
// strings get single-quoted. Required for IN-list plan equality
// downstream.
func TestExplainValue_AnySlice_INListRendering(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    []any
		want string
	}{
		{"int list", []any{int64(1), int64(2), int64(3)}, "(1, 2, 3)"},
		{"string list", []any{"a", "b"}, "('a', 'b')"},
		{"with NULL", []any{int64(1), nil, int64(3)}, "(1, NULL, 3)"},
		{"empty", []any{}, "()"},
		{"single", []any{int64(42)}, "(42)"},
		{"mixed", []any{int64(1), "two", true}, "(1, 'two', TRUE)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ExplainValue(&ConstantValue{Value: tc.v, Typ: TypeUnknown})
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExplainValue_UnknownType pins the fall-through "?" sentinel.
// Critical: NEVER panic on an unrecognised literal Go type — the
// explain path must stay total because plandiff renders any plan it
// receives.
func TestExplainValue_UnknownType(t *testing.T) {
	t.Parallel()
	type weird struct{ x int }
	got := ExplainValue(&ConstantValue{Value: weird{x: 1}, Typ: TypeUnknown})
	if got != "?" {
		t.Fatalf("got %q, want ?", got)
	}
}
