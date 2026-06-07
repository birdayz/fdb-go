package values

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
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
		{[]any{"oops"}, nil}, // wrong type → decline
		// ABS(MinInt64) → ArithmeticOverflowError is pinned on the error
		// channel in TestEvalScalarFunction_ErrorEdges.
	}
	for _, tc := range cases {
		got, errEv0 := evalScalarFunction("ABS", tc.args)
		require.NoError(t, errEv0)
		if got != tc.want {
			t.Fatalf("ABS(%v): got %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestEvalScalarFunction_FloorCeilRound(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("FLOOR", []any{float64(2.7)})
	require.NoError(t, errEv0)
	if got != int64(2) {
		t.Fatalf("FLOOR(2.7): got %v", got)
	}
	got, errEv1 := evalScalarFunction("CEIL", []any{float64(2.1)})
	require.NoError(t, errEv1)
	if got != int64(3) {
		t.Fatalf("CEIL(2.1): got %v", got)
	}
	got, errEv2 := evalScalarFunction("CEILING", []any{float64(-1.2)})
	require.NoError(t, errEv2)
	if got != int64(-1) {
		t.Fatalf("CEILING(-1.2): got %v", got)
	}
	got, errEv3 := evalScalarFunction("ROUND", []any{float64(2.5)})
	require.NoError(t, errEv3)
	if got != int64(3) {
		t.Fatalf("ROUND(2.5): got %v", got)
	}
	got, errEv4 := evalScalarFunction("ROUND", []any{float64(2.49)})
	require.NoError(t, errEv4)
	if got != int64(2) {
		t.Fatalf("ROUND(2.49): got %v", got)
	}
	// ROUND(x, decimals) — float result when fractional part remains.
	got, errEv5 := evalScalarFunction("ROUND", []any{float64(3.14159), int64(2)})
	require.NoError(t, errEv5)
	if g, ok := got.(float64); !ok || math.Abs(g-3.14) > 1e-9 {
		t.Fatalf("ROUND(3.14159, 2): got %v (%T)", got, got)
	}
	// Integer args short-circuit (no float coercion).
	got, errEv6 := evalScalarFunction("FLOOR", []any{int64(7)})
	require.NoError(t, errEv6)
	if got != int64(7) {
		t.Fatalf("FLOOR(7): got %v", got)
	}
}

func TestEvalScalarFunction_SqrtPower(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("SQRT", []any{float64(16)})
	require.NoError(t, errEv0)
	if got != float64(4) {
		t.Fatalf("SQRT(16): got %v", got)
	}
	got, errEv1 := evalScalarFunction("SQRT", []any{int64(9)})
	require.NoError(t, errEv1)
	if got != float64(3) {
		t.Fatalf("SQRT(9): got %v", got)
	}
	// SQRT(-1) → InvalidArgumentError is pinned on the error channel in
	// TestEvalScalarFunction_ErrorEdges.
	// POWER with int result.
	got, errEv2 := evalScalarFunction("POWER", []any{int64(2), int64(3)})
	require.NoError(t, errEv2)
	if got != int64(8) {
		t.Fatalf("POWER(2,3): got %v", got)
	}
	// POW alias.
	got, errEv3 := evalScalarFunction("POW", []any{int64(2), int64(10)})
	require.NoError(t, errEv3)
	if got != int64(1024) {
		t.Fatalf("POW(2,10): got %v", got)
	}
	// POWER with float result.
	got, errEv4 := evalScalarFunction("POWER", []any{float64(2), float64(0.5)})
	require.NoError(t, errEv4)
	if got != math.Sqrt2 {
		t.Fatalf("POWER(2,0.5): got %v", got)
	}
	// Domain error → decline.
	got, errEv5 := evalScalarFunction("POWER", []any{int64(0), int64(-1)})
	require.NoError(t, errEv5)
	if got != nil {
		t.Fatalf("POWER(0,-1): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_CoalesceNullif(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("COALESCE", []any{nil, nil, "third", "ignored"})
	require.NoError(t, errEv0)
	if got != "third" {
		t.Fatalf("COALESCE: got %v", got)
	}
	got, errEv1 := evalScalarFunction("COALESCE", []any{nil, nil})
	require.NoError(t, errEv1)
	if got != nil {
		t.Fatalf("COALESCE all-null: got %v", got)
	}
	got, errEv2 := evalScalarFunction("COALESCE", []any{int64(5)})
	require.NoError(t, errEv2)
	if got != int64(5) {
		t.Fatalf("COALESCE single: got %v", got)
	}
	// NULLIF: equal → NULL; unequal → first arg.
	got, errEv3 := evalScalarFunction("NULLIF", []any{int64(5), int64(5)})
	require.NoError(t, errEv3)
	if got != nil {
		t.Fatalf("NULLIF(5,5): got %v, want nil", got)
	}
	got, errEv4 := evalScalarFunction("NULLIF", []any{int64(5), int64(6)})
	require.NoError(t, errEv4)
	if got != int64(5) {
		t.Fatalf("NULLIF(5,6): got %v", got)
	}
	// Cross-numeric promotion mirrors embedded.
	got, errEv5 := evalScalarFunction("NULLIF", []any{int64(5), float64(5)})
	require.NoError(t, errEv5)
	if got != nil {
		t.Fatalf("NULLIF(5,5.0): got %v, want nil", got)
	}
	// Incomparable types → not equal.
	got, errEv6 := evalScalarFunction("NULLIF", []any{"x", int64(5)})
	require.NoError(t, errEv6)
	if got != "x" {
		t.Fatalf(`NULLIF("x",5): got %v`, got)
	}
	// Null first arg → NULL.
	got, errEv7 := evalScalarFunction("NULLIF", []any{nil, int64(5)})
	require.NoError(t, errEv7)
	if got != nil {
		t.Fatalf("NULLIF(nil,5): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_TrimFamily(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("TRIM", []any{"  hello  "})
	require.NoError(t, errEv0)
	if got != "hello" {
		t.Fatalf("TRIM: got %v", got)
	}
	got, errEv1 := evalScalarFunction("LTRIM", []any{"  hello  "})
	require.NoError(t, errEv1)
	if got != "hello  " {
		t.Fatalf("LTRIM: got %v", got)
	}
	got, errEv2 := evalScalarFunction("RTRIM", []any{"  hello  "})
	require.NoError(t, errEv2)
	if got != "  hello" {
		t.Fatalf("RTRIM: got %v", got)
	}
	got, errEv3 := evalScalarFunction("TRIM", []any{nil})
	require.NoError(t, errEv3)
	if got != nil {
		t.Fatalf("TRIM(NULL): got %v, want nil", got)
	}
	got, errEv4 := evalScalarFunction("TRIM", []any{int64(5)})
	require.NoError(t, errEv4)
	if got != nil {
		t.Fatalf("TRIM(5): got %v, want nil (non-string declines)", got)
	}
}

func TestEvalScalarFunction_Concat(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("CONCAT", []any{"a", "b", "c"})
	require.NoError(t, errEv0)
	if got != "abc" {
		t.Fatalf("CONCAT abc: got %v", got)
	}
	// NULL skip — MySQL/Postgres semantics, mirrors embedded.
	got, errEv1 := evalScalarFunction("CONCAT", []any{"a", nil, "c"})
	require.NoError(t, errEv1)
	if got != "ac" {
		t.Fatalf("CONCAT NULL skip: got %v", got)
	}
	// Numeric coercion via fmt.Sprintf.
	got, errEv2 := evalScalarFunction("CONCAT", []any{"id=", int64(42)})
	require.NoError(t, errEv2)
	if got != "id=42" {
		t.Fatalf("CONCAT mixed: got %v", got)
	}
	got, errEv3 := evalScalarFunction("CONCAT", []any{nil, nil})
	require.NoError(t, errEv3)
	if got != "" {
		t.Fatalf("CONCAT all-nil: got %v, want empty string", got)
	}
}

func TestEvalScalarFunction_Substring(t *testing.T) {
	t.Parallel()
	// 1-based position.
	got, errEv0 := evalScalarFunction("SUBSTRING", []any{"hello", int64(2)})
	require.NoError(t, errEv0)
	if got != "ello" {
		t.Fatalf("SUBSTRING(hello, 2): got %v", got)
	}
	got, errEv1 := evalScalarFunction("SUBSTRING", []any{"hello", int64(2), int64(3)})
	require.NoError(t, errEv1)
	if got != "ell" {
		t.Fatalf("SUBSTRING(hello, 2, 3): got %v", got)
	}
	// pos < 1 normalises to 1.
	got, errEv2 := evalScalarFunction("SUBSTRING", []any{"hello", int64(-1)})
	require.NoError(t, errEv2)
	if got != "hello" {
		t.Fatalf("SUBSTRING(hello, -1): got %v", got)
	}
	// pos past end → empty string.
	got, errEv3 := evalScalarFunction("SUBSTRING", []any{"hi", int64(10)})
	require.NoError(t, errEv3)
	if got != "" {
		t.Fatalf("SUBSTRING(hi, 10): got %v, want empty", got)
	}
	// Multibyte rune handling — `é` is one rune, two bytes.
	got, errEv4 := evalScalarFunction("SUBSTRING", []any{"café", int64(4)})
	require.NoError(t, errEv4)
	if got != "é" {
		t.Fatalf("SUBSTRING(café, 4): got %v, want é", got)
	}
	// SUBSTR alias.
	got, errEv5 := evalScalarFunction("SUBSTR", []any{"world", int64(1), int64(3)})
	require.NoError(t, errEv5)
	if got != "wor" {
		t.Fatalf("SUBSTR(world, 1, 3): got %v", got)
	}
}

func TestEvalScalarFunction_Replace(t *testing.T) {
	t.Parallel()
	got, errEv0 := evalScalarFunction("REPLACE", []any{"hello world", "world", "Go"})
	require.NoError(t, errEv0)
	if got != "hello Go" {
		t.Fatalf("REPLACE: got %v", got)
	}
	// Repeat replacement.
	got, errEv1 := evalScalarFunction("REPLACE", []any{"aaa", "a", "bb"})
	require.NoError(t, errEv1)
	if got != "bbbbbb" {
		t.Fatalf("REPLACE repeat: got %v", got)
	}
	// NULL `to` → empty (drop matches), mirrors embedded.
	got, errEv2 := evalScalarFunction("REPLACE", []any{"abc", "b", nil})
	require.NoError(t, errEv2)
	if got != "ac" {
		t.Fatalf("REPLACE NULL to: got %v", got)
	}
	// NULL string / NULL from → NULL.
	got, errEv3 := evalScalarFunction("REPLACE", []any{nil, "x", "y"})
	require.NoError(t, errEv3)
	if got != nil {
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
