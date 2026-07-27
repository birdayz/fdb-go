package values

import (
	"math"
	"testing"
)

// FLOAT and DOUBLE must render as DIFFERENT type names. The rendering is not
// only cosmetic: dedupSortKeys (rule_sort_dedup_keys.go) keys sort-key identity
// on this exact text, so two ORDER BY keys differing only in width would read
// as duplicates and one would be dropped. Collapsing the two was harmless only
// while the walker mapped both CAST targets onto a single type, which is
// exactly the defect that made CAST(x AS FLOAT) never round.
func TestExplainRendersFloatAndDoubleDistinctly(t *testing.T) {
	t.Parallel()
	child := &ConstantValue{Value: float64(1.5), Typ: NullableDouble}
	asFloat := ExplainValue(NewCastValue(child, NullableFloat))
	asDouble := ExplainValue(NewCastValue(child, NullableDouble))
	if asFloat == asDouble {
		t.Fatalf("CAST to FLOAT and to DOUBLE both render %q — a sort-key dedup that keys on this "+
			"text cannot tell them apart", asFloat)
	}
	if asFloat != "CAST(1.5 AS FLOAT)" {
		t.Errorf("FLOAT cast renders %q, want CAST(1.5 AS FLOAT)", asFloat)
	}
	if asDouble != "CAST(1.5 AS DOUBLE)" {
		t.Errorf("DOUBLE cast renders %q, want CAST(1.5 AS DOUBLE)", asDouble)
	}
}

// Java short-circuits a cast whose source and target types are equal before any
// operator runs ("If the types are the same, no cast is needed" —
// CastValue.inject, CastValue.java:441-446). Re-applying DOUBLE_TO_FLOAT's
// NaN/Infinite rejection to an already-FLOAT value would reject a value for
// being cast to its own type.
func TestCastFloatToFloatIsIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   float64
	}{
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"NaN", math.NaN()},
		{"ordinary value", 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A FLOAT-typed child: the static type is what selects identity,
			// since every FLOAT value is carried as a float64 at runtime.
			c := NewCastValue(&ConstantValue{Value: tc.in, Typ: NullableFloat}, NullableFloat)
			got, err := c.Evaluate(nil)
			if err != nil {
				t.Fatalf("CAST(<FLOAT> AS FLOAT) on %v: %v — an identity cast must not re-apply "+
					"the narrowing checks", tc.in, err)
			}
			g, ok := got.(float64)
			if !ok {
				t.Fatalf("got %T, want float64", got)
			}
			if math.IsNaN(tc.in) {
				if !math.IsNaN(g) {
					t.Fatalf("got %v, want NaN", g)
				}
				return
			}
			if g != tc.in {
				t.Fatalf("got %v, want %v unchanged", g, tc.in)
			}
		})
	}

	// The narrowing direction still rejects, matching Java's DOUBLE_TO_FLOAT.
	for _, in := range []float64{math.Inf(1), math.NaN()} {
		c := NewCastValue(&ConstantValue{Value: in, Typ: NullableDouble}, NullableFloat)
		if _, err := c.Evaluate(nil); err == nil {
			t.Fatalf("CAST(<DOUBLE %v> AS FLOAT) = nil error, want the DOUBLE_TO_FLOAT rejection", in)
		}
	}
}

// Float.parseFloat returns Infinity on numeric overflow and throws only for
// malformed text. Go's ParseFloat reports overflow by returning ±Inf WITH
// ErrRange, so treating that error as a failed cast would reject input Java
// accepts.
func TestCastStringToFloatOverflowsToInfinity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"1e39", math.Inf(1)},
		{"-1e39", math.Inf(-1)},
		{"Infinity", math.Inf(1)},
	} {
		c := NewCastValue(&ConstantValue{Value: tc.in, Typ: NullableString}, NullableFloat)
		got, err := c.Evaluate(nil)
		if err != nil {
			t.Fatalf("CAST('%s' AS FLOAT): %v, want %v — binary32 overflow is Infinity, not an error",
				tc.in, err, tc.want)
		}
		if got != any(tc.want) {
			t.Fatalf("CAST('%s' AS FLOAT) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// Malformed text must still fail.
	c := NewCastValue(&ConstantValue{Value: "notanumber", Typ: NullableString}, NullableFloat)
	if _, err := c.Evaluate(nil); err == nil {
		t.Fatal("CAST('notanumber' AS FLOAT) = nil error, want a malformed-text rejection")
	}
}
