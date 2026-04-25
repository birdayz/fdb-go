package cascades

// Second batch of Java-test-suite-inspired scalar function tests
// (RFC-025 §"Strong unit-test coverage per package"). Adds direct
// coverage for the swingshift-50 second-batch additions: SIGN, MOD,
// IFNULL, IF/IIF, GREATEST/LEAST, EXP, LN, LOG, REVERSE, POSITION,
// LEFT, RIGHT.
//
// Same parameterised-table style as scalar_functions_extra_test.go
// (the first batch): each row pins one Go-native (input → output)
// pair through evalScalarFunction. Walker hooks these names to
// ScalarFunctionValue at parse time and SimplifyValue folds the
// constant cases at plan time, so the runtime executor never re-
// evaluates a fully-constant arithmetic / string sub-tree.

import (
	"math"
	"testing"
)

// ----- SIGN -------------------------------------------------------------

func TestEvalScalarFunction_SIGN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []any
		want any
	}{
		// int64 — preserves int64 result type
		{[]any{int64(0)}, int64(0)},
		{[]any{int64(5)}, int64(1)},
		{[]any{int64(-3)}, int64(-1)},
		{[]any{int64(math.MaxInt64)}, int64(1)},
		// float64 — preserves float64 result type
		{[]any{float64(0)}, float64(0)},
		{[]any{float64(2.5)}, float64(1)},
		{[]any{float64(-1.7)}, float64(-1)},
		// declines
		{[]any{nil}, nil},
		{[]any{"abc"}, nil},
		{[]any{}, nil},
	}
	for _, tc := range cases {
		got := evalScalarFunction("SIGN", tc.args)
		if got != tc.want {
			t.Fatalf("SIGN(%v): got %v (%T), want %v (%T)", tc.args, got, got, tc.want, tc.want)
		}
	}
}

// ----- MOD --------------------------------------------------------------

func TestEvalScalarFunction_MOD(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []any
		want any
	}{
		{"int %% int", []any{int64(20), int64(7)}, int64(6)},
		{"int neg dividend", []any{int64(-20), int64(7)}, int64(-6)}, // Go truncates toward zero
		{"int by 1", []any{int64(42), int64(1)}, int64(0)},
		{"div by zero declines", []any{int64(5), int64(0)}, nil},
		{"float %% float", []any{float64(7.5), float64(2)}, float64(1.5)},
		{"mixed promotes to float", []any{int64(7), float64(2.5)}, float64(2)},
		{"float div by zero", []any{float64(5), float64(0)}, nil},
		{"nil declines", []any{nil, int64(1)}, nil},
		{"non-numeric declines", []any{"a", int64(1)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evalScalarFunction("MOD", tc.args)
			if got != tc.want {
				t.Fatalf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

// ----- IFNULL -----------------------------------------------------------

func TestEvalScalarFunction_IFNULL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []any
		want any
	}{
		{"first non-null", []any{int64(1), int64(2)}, int64(1)},
		{"first null falls back", []any{nil, int64(2)}, int64(2)},
		{"both null", []any{nil, nil}, nil},
		{"first non-zero false stays", []any{false, int64(99)}, false},
		{"wrong arity declines", []any{int64(1)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evalScalarFunction("IFNULL", tc.args)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ----- IF / IIF ---------------------------------------------------------

func TestEvalScalarFunction_IF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []any
		want any
	}{
		{"true bool", []any{true, "yes", "no"}, "yes"},
		{"false bool", []any{false, "yes", "no"}, "no"},
		{"non-zero int truthy", []any{int64(1), int64(10), int64(20)}, int64(10)},
		{"zero int falsy", []any{int64(0), int64(10), int64(20)}, int64(20)},
		{"non-empty string truthy", []any{"x", "yes", "no"}, "yes"},
		{"empty string falsy", []any{"", "yes", "no"}, "no"},
		{"NULL takes else branch", []any{nil, "yes", "no"}, "no"},
		{"non-zero float truthy", []any{float64(0.001), int64(1), int64(2)}, int64(1)},
		{"unsupported cond declines", []any{[]int{1, 2}, "y", "n"}, nil},
		{"wrong arity declines", []any{true, "y"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evalScalarFunction("IF", tc.args)
			if got != tc.want {
				t.Fatalf("IF: got %v, want %v", got, tc.want)
			}
			gotIIF := evalScalarFunction("IIF", tc.args)
			if gotIIF != tc.want {
				t.Fatalf("IIF: got %v, want %v", gotIIF, tc.want)
			}
		})
	}
}

// ----- GREATEST / LEAST -------------------------------------------------

func TestEvalScalarFunction_GREATEST_LEAST(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		args         []any
		wantGreatest any
		wantLeast    any
	}{
		{"all int positive", []any{int64(1), int64(5), int64(3)}, int64(5), int64(1)},
		{"with negative", []any{int64(-3), int64(0), int64(-7)}, int64(0), int64(-7)},
		{"mixed int float", []any{int64(1), float64(2.5), int64(2)}, float64(2.5), int64(1)},
		{"strings", []any{"b", "a", "c"}, "c", "a"},
		{"any NULL → NULL", []any{int64(1), nil, int64(2)}, nil, nil},
		{"first NULL → NULL", []any{nil, int64(1)}, nil, nil},
		{"single arg", []any{int64(42)}, int64(42), int64(42)},
		{"empty args", []any{}, nil, nil},
		{"cross-type incomparable declines", []any{"a", int64(1)}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotG := evalScalarFunction("GREATEST", tc.args)
			if gotG != tc.wantGreatest {
				t.Errorf("GREATEST(%v): got %v, want %v", tc.args, gotG, tc.wantGreatest)
			}
			gotL := evalScalarFunction("LEAST", tc.args)
			if gotL != tc.wantLeast {
				t.Errorf("LEAST(%v): got %v, want %v", tc.args, gotL, tc.wantLeast)
			}
		})
	}
}

// TestEvalScalarFunction_GREATEST_LEAST_AdditionalTypes drives the
// compareScalar branches the happy-path test misses: bool-vs-bool
// ordering (false < true), all-float64 path (no int promotion), and
// cross-type bool-vs-int decline. Pinning these closes the
// compareScalar coverage gap (was 48.5% before — bool + cross-type
// branches weren't reached).
func TestEvalScalarFunction_GREATEST_LEAST_AdditionalTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		args         []any
		wantGreatest any
		wantLeast    any
	}{
		// bool: false < true.
		{"bool-only", []any{true, false, true}, true, false},
		{"bool-only false-only", []any{false, false}, false, false},
		{"bool-only true-only", []any{true, true}, true, true},

		// all float64 (skip the int → float promotion arm).
		{"float-only", []any{float64(1.5), float64(2.5), float64(0.5)}, float64(2.5), float64(0.5)},
		{"float negatives", []any{float64(-1.5), float64(-2.5)}, float64(-1.5), float64(-2.5)},

		// Cross-type declines: bool vs int, float vs string.
		{"bool-int decline", []any{true, int64(1)}, nil, nil},
		{"float-string decline", []any{float64(1.5), "a"}, nil, nil},
		{"int-bool decline", []any{int64(0), false}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotG := evalScalarFunction("GREATEST", tc.args)
			if gotG != tc.wantGreatest {
				t.Errorf("GREATEST(%v): got %v, want %v", tc.args, gotG, tc.wantGreatest)
			}
			gotL := evalScalarFunction("LEAST", tc.args)
			if gotL != tc.wantLeast {
				t.Errorf("LEAST(%v): got %v, want %v", tc.args, gotL, tc.wantLeast)
			}
		})
	}
}

// TestEvalScalarFunction_NULLIF_AdditionalTypes drives nullifEqual
// branches: bool, float-only equality, and the cross-type decline
// (mixed types are NOT equal under NULLIF, so the surviving value is
// the LHS). The happy-path test only hits int64 + string.
func TestEvalScalarFunction_NULLIF_AdditionalTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
		want any
	}{
		// bool equality.
		{"bool-equal-true", true, true, nil},
		{"bool-equal-false", false, false, nil},
		{"bool-not-equal", true, false, true},

		// all-float (no int promotion).
		{"float-equal", float64(1.5), float64(1.5), nil},
		{"float-not-equal", float64(1.5), float64(2.5), float64(1.5)},

		// int64↔float64 promotion both ways.
		{"int-vs-float-equal", int64(2), float64(2), nil},
		{"float-vs-int-equal", float64(2), int64(2), nil},
		{"int-vs-float-different", int64(2), float64(2.5), int64(2)},

		// Cross-type — never equal, LHS survives.
		{"bool-vs-int", true, int64(1), true},
		{"int-vs-string", int64(1), "1", int64(1)},
		{"string-vs-bool", "true", true, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evalScalarFunction("NULLIF", []any{tc.a, tc.b})
			if got != tc.want {
				t.Errorf("NULLIF(%v, %v): got %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestScalarFnInt64Arg_AcceptsWholeFloat pins the scalarFnInt64Arg
// fold path: a whole-valued float64 (e.g. 3.0) coerces to int64.
// Reaches via SUBSTRING's pos arg — `SUBSTRING('hello', 2.0, 3.0)`
// → 'ell'. The non-integer + out-of-range branches are exercised
// by TestScalarFnInt64Arg_RejectsNonIntegerFloat below.
func TestScalarFnInt64Arg_AcceptsWholeFloat(t *testing.T) {
	t.Parallel()
	got := evalScalarFunction("SUBSTRING", []any{"hello", float64(2), float64(3)})
	if got != "ell" {
		t.Fatalf("SUBSTRING with float positions: got %v, want 'ell'", got)
	}
}

// TestScalarFnInt64Arg_RejectsNonIntegerFloat pins the strictness:
// non-whole floats decline (return nil from the scalar fn so the
// runtime can surface the conversion error). Mirrors
// embedded.functions.ToIntegerArg's strictness.
func TestScalarFnInt64Arg_RejectsNonIntegerFloat(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("SUBSTRING", []any{"hello", float64(2.5), int64(3)}); got != nil {
		t.Fatalf("SUBSTRING(_, 2.5, _) should decline: got %v", got)
	}
	if got := evalScalarFunction("LEFT", []any{"hello", float64(1.5)}); got != nil {
		t.Fatalf("LEFT(_, 1.5) should decline: got %v", got)
	}
}

// TestScalarFnInt64Arg_RejectsString pins the type-mismatch decline:
// a string argument where an int is expected returns nil.
func TestScalarFnInt64Arg_RejectsString(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("LEFT", []any{"hello", "two"}); got != nil {
		t.Fatalf("LEFT(_, 'two') should decline: got %v", got)
	}
}

// ----- EXP / LN / LOG ---------------------------------------------------

func TestEvalScalarFunction_EXP(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("EXP", []any{float64(0)}); got != float64(1) {
		t.Errorf("EXP(0): got %v, want 1", got)
	}
	if got := evalScalarFunction("EXP", []any{float64(1)}); got != math.E {
		t.Errorf("EXP(1): got %v, want e", got)
	}
	if got := evalScalarFunction("EXP", []any{int64(0)}); got != float64(1) {
		t.Errorf("EXP(int 0): got %v, want 1", got)
	}
	if got := evalScalarFunction("EXP", []any{nil}); got != nil {
		t.Errorf("EXP(NULL): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_LN(t *testing.T) {
	t.Parallel()
	if got := evalScalarFunction("LN", []any{float64(1)}); got != float64(0) {
		t.Errorf("LN(1): got %v, want 0", got)
	}
	if got := evalScalarFunction("LN", []any{math.E}); math.Abs(got.(float64)-1) > 1e-9 {
		t.Errorf("LN(e): got %v, want 1", got)
	}
	// Domain: x > 0
	if got := evalScalarFunction("LN", []any{float64(0)}); got != nil {
		t.Errorf("LN(0): got %v, want nil (out of domain)", got)
	}
	if got := evalScalarFunction("LN", []any{float64(-1)}); got != nil {
		t.Errorf("LN(-1): got %v, want nil", got)
	}
}

func TestEvalScalarFunction_LOG(t *testing.T) {
	t.Parallel()
	// 1-arg LOG is log10
	if got := evalScalarFunction("LOG", []any{float64(100)}); math.Abs(got.(float64)-2) > 1e-9 {
		t.Errorf("LOG(100): got %v, want 2", got)
	}
	if got := evalScalarFunction("LOG", []any{float64(1000)}); math.Abs(got.(float64)-3) > 1e-9 {
		t.Errorf("LOG(1000): got %v, want 3", got)
	}
	// 2-arg LOG(base, x) = log_base(x)
	if got := evalScalarFunction("LOG", []any{float64(2), float64(8)}); math.Abs(got.(float64)-3) > 1e-9 {
		t.Errorf("LOG(2, 8): got %v, want 3", got)
	}
	// Domain: base > 0, base != 1, x > 0
	if got := evalScalarFunction("LOG", []any{float64(1), float64(8)}); got != nil {
		t.Errorf("LOG(1, 8): got %v, want nil (base=1 forbidden)", got)
	}
	if got := evalScalarFunction("LOG", []any{float64(2), float64(-1)}); got != nil {
		t.Errorf("LOG(2, -1): got %v, want nil", got)
	}
}

// ----- REVERSE ----------------------------------------------------------

func TestEvalScalarFunction_REVERSE(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want any
	}{
		{"hello", "olleh"},
		{"", ""},
		{"a", "a"},
		{"héllo", "olléh"},  // multibyte rune-aware
		{int64(123), "321"}, // numeric coerces via fmt.Sprintf
		{nil, nil},
	}
	for _, tc := range cases {
		got := evalScalarFunction("REVERSE", []any{tc.in})
		if got != tc.want {
			t.Errorf("REVERSE(%v): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ----- POSITION ---------------------------------------------------------

func TestEvalScalarFunction_POSITION(t *testing.T) {
	t.Parallel()
	cases := []struct {
		needle, haystack any
		want             any
	}{
		{"world", "hello world", int64(7)},
		{"hello", "hello world", int64(1)},
		{"xyz", "hello world", int64(0)}, // not found
		{"", "hello", int64(1)},          // empty needle = position 1
		{"é", "café", int64(4)},          // multibyte aware
		{nil, "hello", nil},              // null arg declines
		{"x", nil, nil},
	}
	for _, tc := range cases {
		got := evalScalarFunction("POSITION", []any{tc.needle, tc.haystack})
		if got != tc.want {
			t.Errorf("POSITION(%v, %v): got %v, want %v", tc.needle, tc.haystack, got, tc.want)
		}
	}
}

// ----- LEFT / RIGHT -----------------------------------------------------

func TestEvalScalarFunction_LEFT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    any
		n    any
		want any
	}{
		{"hello", int64(3), "hel"},
		{"hello", int64(0), ""},
		{"hello", int64(99), "hello"}, // n exceeds length
		{"hello", int64(-2), ""},      // negative n clamped to 0
		{"héllo", int64(2), "hé"},     // rune-aware
		{"", int64(3), ""},
		{nil, int64(2), nil},
		{"x", float64(2.5), nil}, // non-int-valued float declines
	}
	for _, tc := range cases {
		got := evalScalarFunction("LEFT", []any{tc.s, tc.n})
		if got != tc.want {
			t.Errorf("LEFT(%v, %v): got %v, want %v", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestEvalScalarFunction_RIGHT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    any
		n    any
		want any
	}{
		{"hello", int64(3), "llo"},
		{"hello", int64(0), ""},
		{"hello", int64(99), "hello"},
		{"hello", int64(-2), ""},
		{"héllo", int64(2), "lo"},
		{"", int64(3), ""},
		{nil, int64(2), nil},
	}
	for _, tc := range cases {
		got := evalScalarFunction("RIGHT", []any{tc.s, tc.n})
		if got != tc.want {
			t.Errorf("RIGHT(%v, %v): got %v, want %v", tc.s, tc.n, got, tc.want)
		}
	}
}

// ----- SimplifyValue composition ----------------------------------------

// TestSimplifyValue_FoldsSecondBatchScalars composes the folding
// path: a fully-constant ScalarFunctionValue tree of the new
// functions folds straight to a ConstantValue at plan time.
func TestSimplifyValue_FoldsSecondBatchScalars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Value
		want any
	}{
		{
			"SIGN",
			NewScalarFunctionValue("SIGN", TypeUnknown,
				&ConstantValue{Value: int64(-7), Typ: TypeInt}),
			int64(-1),
		},
		{
			"MOD",
			NewScalarFunctionValue("MOD", TypeUnknown,
				&ConstantValue{Value: int64(20), Typ: TypeInt},
				&ConstantValue{Value: int64(7), Typ: TypeInt}),
			int64(6),
		},
		{
			"IFNULL non-null first",
			NewScalarFunctionValue("IFNULL", TypeUnknown,
				&ConstantValue{Value: int64(1), Typ: TypeInt},
				&ConstantValue{Value: int64(2), Typ: TypeInt}),
			int64(1),
		},
		{
			"GREATEST",
			NewScalarFunctionValue("GREATEST", TypeUnknown,
				&ConstantValue{Value: int64(1), Typ: TypeInt},
				&ConstantValue{Value: int64(5), Typ: TypeInt},
				&ConstantValue{Value: int64(3), Typ: TypeInt}),
			int64(5),
		},
		{
			"REVERSE",
			NewScalarFunctionValue("REVERSE", TypeString,
				&ConstantValue{Value: "abc", Typ: TypeString}),
			"cba",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := SimplifyValue(tc.v)
			cv, ok := out.(*ConstantValue)
			if !ok {
				t.Fatalf("expected *ConstantValue, got %T", out)
			}
			if cv.Value != tc.want {
				t.Fatalf("got %v, want %v", cv.Value, tc.want)
			}
		})
	}
}
