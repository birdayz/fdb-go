package values

import (
	"math"
	"testing"
)

// Extended scalar-function fold tests for the dayshift-49 → swingshift-50
// growth: ABS, FLOOR, CEILING, ROUND, SQRT, POWER, COALESCE, NULLIF,
// TRIM family, CONCAT, SUBSTRING, REPLACE. Each row pins one Go-native
// (input → output) pair through evalScalarFunction. The walker hooks
// these names up to ScalarFunctionValue at parse time and SimplifyValue
// folds the constant cases at plan time, so the runtime executor never
// re-evaluates a fully-constant arithmetic / string sub-tree.

func TestEvalScalarFunction_ABS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []any
		want any
	}{
		{[]any{int64(5)}, int64(5)},
		{[]any{int64(-5)}, int64(5)},
		{[]any{float64(-3.5)}, float64(3.5)},
		{[]any{nil}, nil},
		{[]any{math.MinInt64}, nil}, // overflow → decline (runtime errors instead)
		{[]any{"oops"}, nil},        // wrong type → decline
	}
	for _, tc := range cases {
		got := evalScalarFunction("ABS", tc.args)
		if got != tc.want {
			t.Fatalf("ABS(%v): got %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestEvalScalarFunction_FloorCeilRound(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("FLOOR", []any{float64(2.7)}); got != int64(2) {
		t.Fatalf("FLOOR(2.7): got %v", got)
	}
	if got := evalScalarFunction("CEIL", []any{float64(2.1)}); got != int64(3) {
		t.Fatalf("CEIL(2.1): got %v", got)
	}
	if got := evalScalarFunction("CEILING", []any{float64(-1.2)}); got != int64(-1) {
		t.Fatalf("CEILING(-1.2): got %v", got)
	}
	if got := evalScalarFunction("ROUND", []any{float64(2.5)}); got != int64(3) {
		t.Fatalf("ROUND(2.5): got %v", got)
	}
	if got := evalScalarFunction("ROUND", []any{float64(2.49)}); got != int64(2) {
		t.Fatalf("ROUND(2.49): got %v", got)
	}
	// ROUND(x, decimals) — float result when fractional part remains.
	got := evalScalarFunction("ROUND", []any{float64(3.14159), int64(2)})
	if g, ok := got.(float64); !ok || math.Abs(g-3.14) > 1e-9 {
		t.Fatalf("ROUND(3.14159, 2): got %v (%T)", got, got)
	}
	// Integer args short-circuit (no float coercion).
	if got := evalScalarFunction("FLOOR", []any{int64(7)}); got != int64(7) {
		t.Fatalf("FLOOR(7): got %v", got)
	}
}

func TestEvalScalarFunction_SqrtPower(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("SQRT", []any{float64(16)}); got != float64(4) {
		t.Fatalf("SQRT(16): got %v", got)
	}
	if got := evalScalarFunction("SQRT", []any{int64(9)}); got != float64(3) {
		t.Fatalf("SQRT(9): got %v", got)
	}
	// Negative SQRT declines (NaN avoidance).
	if got := evalScalarFunction("SQRT", []any{float64(-1)}); got != nil {
		t.Fatalf("SQRT(-1): got %v, want nil", got)
	}
	// POWER with int result.
	if got := evalScalarFunction("POWER", []any{int64(2), int64(3)}); got != int64(8) {
		t.Fatalf("POWER(2,3): got %v", got)
	}
	// POW alias.
	if got := evalScalarFunction("POW", []any{int64(2), int64(10)}); got != int64(1024) {
		t.Fatalf("POW(2,10): got %v", got)
	}
	// POWER with float result.
	if got := evalScalarFunction("POWER", []any{float64(2), float64(0.5)}); got != math.Sqrt2 {
		t.Fatalf("POWER(2,0.5): got %v", got)
	}
	// Domain error → decline.
	if got := evalScalarFunction("POWER", []any{int64(0), int64(-1)}); got != nil {
		t.Fatalf("POWER(0,-1): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_CoalesceNullif(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("COALESCE", []any{nil, nil, "third", "ignored"}); got != "third" {
		t.Fatalf("COALESCE: got %v", got)
	}
	if got := evalScalarFunction("COALESCE", []any{nil, nil}); got != nil {
		t.Fatalf("COALESCE all-null: got %v", got)
	}
	if got := evalScalarFunction("COALESCE", []any{int64(5)}); got != int64(5) {
		t.Fatalf("COALESCE single: got %v", got)
	}
	// NULLIF: equal → NULL; unequal → first arg.
	if got := evalScalarFunction("NULLIF", []any{int64(5), int64(5)}); got != nil {
		t.Fatalf("NULLIF(5,5): got %v, want nil", got)
	}
	if got := evalScalarFunction("NULLIF", []any{int64(5), int64(6)}); got != int64(5) {
		t.Fatalf("NULLIF(5,6): got %v", got)
	}
	// Cross-numeric promotion mirrors embedded.
	if got := evalScalarFunction("NULLIF", []any{int64(5), float64(5)}); got != nil {
		t.Fatalf("NULLIF(5,5.0): got %v, want nil", got)
	}
	// Incomparable types → not equal.
	if got := evalScalarFunction("NULLIF", []any{"x", int64(5)}); got != "x" {
		t.Fatalf(`NULLIF("x",5): got %v`, got)
	}
	// Null first arg → NULL.
	if got := evalScalarFunction("NULLIF", []any{nil, int64(5)}); got != nil {
		t.Fatalf("NULLIF(nil,5): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_TrimFamily(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("TRIM", []any{"  hello  "}); got != "hello" {
		t.Fatalf("TRIM: got %v", got)
	}
	if got := evalScalarFunction("LTRIM", []any{"  hello  "}); got != "hello  " {
		t.Fatalf("LTRIM: got %v", got)
	}
	if got := evalScalarFunction("RTRIM", []any{"  hello  "}); got != "  hello" {
		t.Fatalf("RTRIM: got %v", got)
	}
	if got := evalScalarFunction("TRIM", []any{nil}); got != nil {
		t.Fatalf("TRIM(NULL): got %v, want nil", got)
	}
	if got := evalScalarFunction("TRIM", []any{int64(5)}); got != nil {
		t.Fatalf("TRIM(5): got %v, want nil (non-string declines)", got)
	}
}

func TestEvalScalarFunction_Concat(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("CONCAT", []any{"a", "b", "c"}); got != "abc" {
		t.Fatalf("CONCAT abc: got %v", got)
	}
	// NULL skip — MySQL/Postgres semantics, mirrors embedded.
	if got := evalScalarFunction("CONCAT", []any{"a", nil, "c"}); got != "ac" {
		t.Fatalf("CONCAT NULL skip: got %v", got)
	}
	// Numeric coercion via fmt.Sprintf.
	if got := evalScalarFunction("CONCAT", []any{"id=", int64(42)}); got != "id=42" {
		t.Fatalf("CONCAT mixed: got %v", got)
	}
	if got := evalScalarFunction("CONCAT", []any{nil, nil}); got != "" {
		t.Fatalf("CONCAT all-nil: got %v, want empty string", got)
	}
}

func TestEvalScalarFunction_Substring(t *testing.T) {
	t.Parallel()
	// 1-based position.
	if got := evalScalarFunction("SUBSTRING", []any{"hello", int64(2)}); got != "ello" {
		t.Fatalf("SUBSTRING(hello, 2): got %v", got)
	}
	if got := evalScalarFunction("SUBSTRING", []any{"hello", int64(2), int64(3)}); got != "ell" {
		t.Fatalf("SUBSTRING(hello, 2, 3): got %v", got)
	}
	// pos < 1 normalises to 1.
	if got := evalScalarFunction("SUBSTRING", []any{"hello", int64(-1)}); got != "hello" {
		t.Fatalf("SUBSTRING(hello, -1): got %v", got)
	}
	// pos past end → empty string.
	if got := evalScalarFunction("SUBSTRING", []any{"hi", int64(10)}); got != "" {
		t.Fatalf("SUBSTRING(hi, 10): got %v, want empty", got)
	}
	// Multibyte rune handling — `é` is one rune, two bytes.
	if got := evalScalarFunction("SUBSTRING", []any{"café", int64(4)}); got != "é" {
		t.Fatalf("SUBSTRING(café, 4): got %v, want é", got)
	}
	// SUBSTR alias.
	if got := evalScalarFunction("SUBSTR", []any{"world", int64(1), int64(3)}); got != "wor" {
		t.Fatalf("SUBSTR(world, 1, 3): got %v", got)
	}
}

func TestEvalScalarFunction_Replace(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("REPLACE", []any{"hello world", "world", "Go"}); got != "hello Go" {
		t.Fatalf("REPLACE: got %v", got)
	}
	// Repeat replacement.
	if got := evalScalarFunction("REPLACE", []any{"aaa", "a", "bb"}); got != "bbbbbb" {
		t.Fatalf("REPLACE repeat: got %v", got)
	}
	// NULL `to` → empty (drop matches), mirrors embedded.
	if got := evalScalarFunction("REPLACE", []any{"abc", "b", nil}); got != "ac" {
		t.Fatalf("REPLACE NULL to: got %v", got)
	}
	// NULL string / NULL from → NULL.
	if got := evalScalarFunction("REPLACE", []any{nil, "x", "y"}); got != nil {
		t.Fatalf("REPLACE NULL str: got %v, want nil", got)
	}
}

// TestSimplifyValue_FoldsExtendedScalars composes everything at the
// SimplifyValue layer: a literal-only ScalarFunctionValue tree folds
// straight to a ConstantValue at plan time.
func TestSimplifyValue_FoldsExtendedScalars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Value
		want any
	}{
		{
			"ABS",
			NewScalarFunctionValue("ABS", TypeUnknown,
				&ConstantValue{Value: int64(-7), Typ: TypeInt}),
			int64(7),
		},
		{
			"FLOOR",
			NewScalarFunctionValue("FLOOR", TypeUnknown,
				&ConstantValue{Value: float64(3.9), Typ: TypeFloat}),
			int64(3),
		},
		{
			"COALESCE picks first non-null",
			NewScalarFunctionValue("COALESCE", TypeUnknown,
				&NullValue{Typ: TypeUnknown},
				&ConstantValue{Value: "x", Typ: TypeString}),
			"x",
		},
		{
			"CONCAT NULL skip",
			NewScalarFunctionValue("CONCAT", TypeString,
				&ConstantValue{Value: "a", Typ: TypeString},
				&NullValue{Typ: TypeUnknown},
				&ConstantValue{Value: "c", Typ: TypeString}),
			"ac",
		},
	}
	for _, tc := range cases {
		out := SimplifyValue(tc.v)
		cv, ok := out.(*ConstantValue)
		if !ok {
			t.Fatalf("%s: expected *ConstantValue, got %T", tc.name, out)
		}
		if cv.Value != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, cv.Value, tc.want)
		}
	}
}
