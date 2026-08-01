package expr_test

// WIDTH-SUFFIXED numeric literals — the walker-level pins for Java's
// ParseHelpers.parseDecimal (ParseHelpers.java:68-101). The suffix decides
// the literal's STATIC TYPE: `2L` is LONG even though 2 fits int32 (no
// re-narrowing), `1I` is INT, `1.0f` is the binary32 FLOAT, `2.0d` DOUBLE.
// Before the port the whole token went to strconv and every suffixed
// literal failed to parse (the typed-integer/typed-float literal gaps).

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
)

func TestWalkExpression_TypedNumericSuffixes(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)

	cases := []struct {
		lit      string
		wantVal  any
		wantType values.Type
	}{
		{"1I", int64(1), values.NullableInt},
		{"3i", int64(3), values.NullableInt},
		// The L suffix PINS the width — no fits-int32 re-narrowing.
		{"2L", int64(2), values.NullableLong},
		{"4l", int64(4), values.NullableLong},
		{"-7I", int64(-7), values.NullableInt},
		{"-8L", int64(-8), values.NullableLong},
		// f/F parses the ROUNDED binary32 value (Float.parseFloat).
		{"0.5f", float64(0.5), values.NullableFloat},
		{"1.0F", float64(1), values.NullableFloat},
		{"-0.5f", float64(-0.5), values.NullableFloat},
		{"2.0d", float64(2), values.NullableDouble},
		{"3.5D", float64(3.5), values.NullableDouble},
		// Unsuffixed keeps the existing rules.
		{"42", int64(42), values.NullableInt},
		{"3000000000", int64(3000000000), values.NullableLong},
		{"1.5", float64(1.5), values.NullableDouble},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.lit, func(t *testing.T) {
			t.Parallel()
			ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE "+tc.lit)
			v, err := r.WalkExpression(ctx)
			if err != nil {
				t.Fatalf("walk %s: %v", tc.lit, err)
			}
			cv, ok := v.(*values.ConstantValue)
			if !ok {
				t.Fatalf("%s: expected *ConstantValue, got %T", tc.lit, v)
			}
			if cv.Value != tc.wantVal {
				t.Fatalf("%s: Value = %v (%T), want %v (%T)", tc.lit, cv.Value, cv.Value, tc.wantVal, tc.wantVal)
			}
			if cv.Typ != tc.wantType {
				t.Fatalf("%s: Typ = %v, want %v", tc.lit, cv.Typ, tc.wantType)
			}
		})
	}

	t.Run("0.1f is the rounded binary32, not 0.1", func(t *testing.T) {
		t.Parallel()
		ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 0.1f")
		v, err := r.WalkExpression(ctx)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		cv := v.(*values.ConstantValue)
		if cv.Value == float64(0.1) {
			t.Fatalf("0.1f parsed as exact binary64 0.1 — must be the float32-rounded value")
		}
		if cv.Value != float64(float32(0.1)) {
			t.Fatalf("0.1f = %v, want %v (float64(float32(0.1)))", cv.Value, float64(float32(0.1)))
		}
	})

	t.Run("out-of-int32-range I suffix errors", func(t *testing.T) {
		t.Parallel()
		// Java: Integer.parseInt throws — the suffix pins the width, it
		// does not clamp or widen.
		ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 99999999999I")
		_, err := r.WalkExpression(ctx)
		var overflow *expr.NumericOverflowLiteralError
		if err == nil || !errors.As(err, &overflow) {
			t.Fatalf("99999999999I: expected NumericOverflowLiteralError, got %v", err)
		}
	})

	t.Run("exponent-only float suffix fails like Java", func(t *testing.T) {
		t.Parallel()
		// Java's contains(".") gate: `1e5f` never reaches the suffix arm
		// and Long.parseLong("1e5f") throws; Go's ParseFloat("1e5f") fails
		// the same way — both engines reject.
		ctx := parseFirstWhereExpr(t, "SELECT * FROM users WHERE 1e5f")
		if _, err := r.WalkExpression(ctx); err == nil {
			t.Fatalf("1e5f: expected an error, parsed")
		}
		_, err := r.WalkExpression(ctx)
		if err == nil || !strings.Contains(err.Error(), "1e5f") {
			t.Fatalf("1e5f: error should carry the literal text, got %v", err)
		}
	})
}
