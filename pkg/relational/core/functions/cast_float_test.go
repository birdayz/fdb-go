package functions

import (
	"math"
	"testing"
)

// CastValue here serves the MAP-path evaluator (system-table filtering); the
// SQL query path folds CAST through the Cascades value layer's own
// values.CastValue instead. Two implementations of one operator is a standing
// hazard: they have already drifted once, both sharing a single FLOAT/DOUBLE
// arm that returned the operand at binary64 precision so that CAST(x AS FLOAT)
// never rounded at all. This pins the FLOAT contract on THIS copy directly,
// because no SQL query reaches it — a test routed through the query engine
// would exercise the other implementation and pass no matter what this one
// does.
//
// The contract is Java's, via CastValue.java:126-205: DOUBLE_TO_FLOAT rounds
// with value.floatValue() and rejects NaN/Infinite and out-of-range;
// INT/LONG_TO_FLOAT use Float.valueOf; STRING_TO_FLOAT uses Float.parseFloat.
func TestCastValueFloatRoundsToBinary32(t *testing.T) {
	t.Parallel()

	// 0.1 is not representable in binary32, so it separates a real rounding
	// implementation from one that returns the operand untouched. A value like
	// 1.5, exact in both widths, cannot tell the two apart.
	const wantTenth = float64(float32(0.1))
	if wantTenth == 0.1 {
		t.Fatal("precondition: float64(float32(0.1)) must differ from 0.1, else this test proves nothing")
	}

	t.Run("double rounds to binary32", func(t *testing.T) {
		t.Parallel()
		got, err := CastValue(0.1, "FLOAT")
		if err != nil {
			t.Fatalf("CastValue(0.1, FLOAT): %v", err)
		}
		if got != any(wantTenth) {
			t.Fatalf("CastValue(0.1, FLOAT) = %v (%T), want %v — the cast did not round", got, got, wantTenth)
		}
	})

	t.Run("double stays binary64", func(t *testing.T) {
		t.Parallel()
		got, err := CastValue(0.1, "DOUBLE")
		if err != nil {
			t.Fatalf("CastValue(0.1, DOUBLE): %v", err)
		}
		if got != any(0.1) {
			t.Fatalf("CastValue(0.1, DOUBLE) = %v, want 0.1 — DOUBLE must not round", got)
		}
	})

	t.Run("string parses at binary32", func(t *testing.T) {
		t.Parallel()
		got, err := CastValue("0.1", "FLOAT")
		if err != nil {
			t.Fatalf("CastValue(\"0.1\", FLOAT): %v", err)
		}
		if got != any(wantTenth) {
			t.Fatalf("CastValue(\"0.1\", FLOAT) = %v, want %v — parsed at the wrong width", got, wantTenth)
		}
	})

	t.Run("integer beyond the binary32 mantissa rounds", func(t *testing.T) {
		t.Parallel()
		// 2^24+1 is the smallest positive integer binary32 cannot hold.
		const n = int64(1<<24) + 1
		got, err := CastValue(n, "FLOAT")
		if err != nil {
			t.Fatalf("CastValue(%d, FLOAT): %v", n, err)
		}
		if got != any(float64(float32(n))) {
			t.Fatalf("CastValue(%d, FLOAT) = %v, want %v", n, got, float64(float32(n)))
		}
	})

	t.Run("exact values are unchanged", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{1.5, 2.5, 0, -0.5, 1 << 24} {
			got, err := CastValue(v, "FLOAT")
			if err != nil {
				t.Fatalf("CastValue(%v, FLOAT): %v", v, err)
			}
			if got != any(v) {
				t.Fatalf("CastValue(%v, FLOAT) = %v, want it unchanged", v, got)
			}
		}
	})

	t.Run("NaN and Infinity are rejected", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			if _, err := CastValue(v, "FLOAT"); err == nil {
				t.Fatalf("CastValue(%v, FLOAT) = nil error, want the Java DOUBLE_TO_FLOAT rejection", v)
			}
		}
	})

	t.Run("out of binary32 range is rejected", func(t *testing.T) {
		t.Parallel()
		for _, v := range []float64{1e39, -1e39} {
			if _, err := CastValue(v, "FLOAT"); err == nil {
				t.Fatalf("CastValue(%v, FLOAT) = nil error, want an out-of-range rejection", v)
			}
		}
		// Still inside the range, so it must NOT be rejected.
		if _, err := CastValue(float64(math.MaxFloat32), "FLOAT"); err != nil {
			t.Fatalf("CastValue(MaxFloat32, FLOAT): %v, want it accepted", err)
		}
	})
}
