package embedded

import (
	"math"
	"testing"
)

func TestParseDecimalLiteralValue_Int64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"zero", "0", 0},
		{"positive", "42", 42},
		{"negative", "-42", -42},
		{"max_int64", "9223372036854775807", math.MaxInt64},
		{"min_int64", "-9223372036854775808", math.MinInt64},
		{"one", "1", 1},
		{"large", "1000000000000", 1000000000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDecimalLiteralValue(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			v, ok := got.(int64)
			if !ok {
				t.Fatalf("got type %T, want int64", got)
			}
			if v != tc.want {
				t.Errorf("got %d, want %d", v, tc.want)
			}
		})
	}
}

func TestParseDecimalLiteralValue_Float64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"decimal_point", "3.14", 3.14},
		{"negative_decimal", "-2.5", -2.5},
		{"exponent_e", "1e10", 1e10},
		{"exponent_E", "1E10", 1e10},
		{"negative_exponent", "1e-3", 1e-3},
		{"decimal_and_exponent", "1.5e2", 150.0},
		{"zero_point_zero", "0.0", 0.0},
		{"leading_zero", "0.001", 0.001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDecimalLiteralValue(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			v, ok := got.(float64)
			if !ok {
				t.Fatalf("got type %T, want float64", got)
			}
			if v != tc.want {
				t.Errorf("got %g, want %g", v, tc.want)
			}
		})
	}
}

func TestParseDecimalLiteralValue_Int64Overflow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"above_max_int64", "9223372036854775808"},
		{"way_above_max", "99999999999999999999"},
		{"below_min_int64", "-9223372036854775809"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseDecimalLiteralValue(tc.in)
			if err == nil {
				t.Fatal("expected error for overflow, got nil")
			}
		})
	}
}

func TestParseDecimalLiteralValue_FloatOverflow(t *testing.T) {
	t.Parallel()
	_, err := parseDecimalLiteralValue("1e309")
	if err == nil {
		t.Fatal("expected error for float overflow, got nil")
	}
}

func TestParseDecimalLiteralValue_IntShapeNoDecimalPoint(t *testing.T) {
	t.Parallel()
	got, err := parseDecimalLiteralValue("100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got.(int64); !ok {
		t.Fatalf("integer-shaped literal should parse as int64, got %T", got)
	}
}

func TestParseDecimalLiteralValue_FloatShapeWithDecimalPoint(t *testing.T) {
	t.Parallel()
	got, err := parseDecimalLiteralValue("100.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got.(float64); !ok {
		t.Fatalf("float-shaped literal should parse as float64, got %T", got)
	}
}

func BenchmarkParseDecimalLiteralValue_Int(b *testing.B) {
	for b.Loop() {
		_, _ = parseDecimalLiteralValue("42")
	}
}

func BenchmarkParseDecimalLiteralValue_Float(b *testing.B) {
	for b.Loop() {
		_, _ = parseDecimalLiteralValue("3.14159")
	}
}
