package javacorpus

import (
	"math"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// TestFloatMatchingUsesJavaBitEquality pins the numeric-equality semantics of
// the float matchers to Java's.
//
// Matchers.matchField compares a boxed Double with Objects.equals
// (Matchers.java:691) and a Float with Objects.equals at either width
// (Matchers.java:728/733). Boxed equality is doubleToLongBits /
// floatToIntBits, which differs from `==` in exactly two places, in opposite
// directions: NaN EQUALS NaN, and +0.0 DOES NOT equal -0.0. The signed-zero
// half is the load-bearing one here — the factory corpus freezes -0.0
// expectations (the generator seeds it deliberately; this repo shipped a
// signed-zero wrong-rows bug once already), and a `==` comparison would let a
// +0.0 actual satisfy a frozen -0.0.
func TestFloatMatchingUsesJavaBitEquality(t *testing.T) {
	t.Parallel()
	fail := func(string, ...any) error { return errFloatMismatch }

	negZero := math.Copysign(0, -1)

	// Double expectation vs Double actual.
	if err := matchDouble(negZero, float64(0), "DOUBLE", fail); err == nil {
		t.Error("a -0.0 (Double) expectation accepted a +0.0 actual; Java's Double.equals distinguishes the sign bit")
	}
	if err := matchDouble(negZero, negZero, "DOUBLE", fail); err != nil {
		t.Error("a -0.0 (Double) expectation rejected a -0.0 actual")
	}
	if err := matchDouble(math.NaN(), math.NaN(), "DOUBLE", fail); err != nil {
		t.Error("a NaN (Double) expectation rejected a NaN actual; Java's Double.equals treats NaN as equal to itself")
	}

	// Float expectation (!f) vs Float and Double actuals.
	negZero32 := float32(negZero)
	if err := matchFloat(negZero32, float32(0), "FLOAT", fail); err == nil {
		t.Error("a -0.0 (Float) expectation accepted a +0.0 Float actual")
	}
	if err := matchFloat(negZero32, negZero32, "FLOAT", fail); err != nil {
		t.Error("a -0.0 (Float) expectation rejected a -0.0 Float actual")
	}
	if err := matchFloat(float32(math.NaN()), float32(math.NaN()), "FLOAT", fail); err != nil {
		t.Error("a NaN (Float) expectation rejected a NaN Float actual")
	}
	if err := matchFloat(negZero32, float64(0), "DOUBLE", fail); err == nil {
		t.Error("a -0.0 (Float) expectation accepted a +0.0 Double actual")
	}

	// The same semantics through the full cell path, as a corpus row would
	// exercise them: a plain YAML float cell is a Double expectation.
	cell := &javayamsql.Value{Kind: javayamsql.KindFloat, Float: negZero}
	if err := matchField(cell, float64(0), "DOUBLE", 1, "D"); err == nil {
		t.Error("matchField accepted +0.0 for a frozen -0.0 cell")
	}
	if err := matchField(cell, negZero, "DOUBLE", 1, "D"); err != nil {
		t.Errorf("matchField rejected -0.0 for a frozen -0.0 cell: %v", err)
	}
}

var errFloatMismatch = &floatMismatchError{}

type floatMismatchError struct{}

func (*floatMismatchError) Error() string { return "float mismatch" }
