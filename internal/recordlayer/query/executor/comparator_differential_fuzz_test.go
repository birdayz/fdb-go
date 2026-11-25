package executor

import (
	"bytes"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// sign normalizes a comparator result to -1/0/1.
func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	default:
		return 0
	}
}

// FuzzCompareValues_TupleOrderAgreement is the RFC-180 G3(a) net: the SORT
// comparator (compareValues, backing in-memory sort/merge/dedup) must agree
// with the FDB tuple byte order (what an ordered index scan yields) on every
// same-type slot pair — otherwise an in-memory plan and an indexed plan
// return the same rows in different orders. Every RFC-179 Theme-2 float bug
// (±0.0, NaN, float32 fmt-fallback) was an instance of this property
// breaking.
//
// NaN payloads are the one DOCUMENTED split, and it is Java parity, not a
// bug: both tuple codecs pack RAW float bits (Go math.Float64bits; Java
// TupleUtil uses Double.doubleToRawLongBits — verified against fdb-java
// 7.1.26 bytecode), so index byte order distinguishes NaN payloads and
// sorts sign-set NaNs below -Inf, while the in-memory comparator collapses
// all NaNs to one canonical greatest value exactly like Java's
// Double.compare. The property therefore asserts byte-order agreement for
// non-NaN pairs and Double.compare semantics (NaN greatest, NaN == NaN)
// for NaN pairs.
func FuzzCompareValues_TupleOrderAgreement(f *testing.F) {
	f.Add(uint8(0), uint64(0), uint64(0x8000000000000000), "", "") // +0.0 vs -0.0
	f.Add(uint8(0), math.Float64bits(math.NaN()), math.Float64bits(1.5), "", "")
	f.Add(uint8(0), math.Float64bits(math.Inf(1)), math.Float64bits(math.Inf(-1)), "", "")
	f.Add(uint8(1), uint64(0), uint64(0xFFFFFFFFFFFFFFFF), "", "") // 0 vs -1
	f.Add(uint8(1), uint64(math.MaxInt64), uint64(0x8000000000000000), "", "")
	f.Add(uint8(2), uint64(0), uint64(0), "a\x00b", "a")
	f.Add(uint8(3), uint64(0), uint64(0), "\x00\xff", "\x00")
	f.Add(uint8(4), uint64(math.Float32bits(float32(math.NaN()))), uint64(0x80000000), "", "") // f32 NaN vs -0.0
	// Deterministic kill for the float32 fmt-fallback bug class: lexically
	// "2.5" > "10.5" while the numeric/tuple order says 2.5 < 10.5 — a
	// reverted float32 arm fails on the SEED corpus, not just under fuzzing.
	f.Add(uint8(4), uint64(math.Float32bits(2.5)), uint64(math.Float32bits(10.5)), "", "")
	f.Add(uint8(0), math.Float64bits(math.NaN()), math.Float64bits(math.NaN()), "", "") // NaN==NaN branch
	// The unsigned integer half: tuple decoding returns uint64 for positive
	// values above math.MaxInt64, so uint64 slots ARE row-domain values.
	f.Add(uint8(5), uint64(math.MaxInt64)+1, uint64(math.MaxUint64), "", "")
	// Deterministic kill for a float-promotion revert of the integer arms:
	// float64(2^63) == float64(2^63-1) ties the ADJACENT pair the exact
	// integer comparison must order — on the seed corpus, not just under
	// fuzzing.
	f.Add(uint8(6), uint64(math.MaxInt64)+1, uint64(math.MaxInt64), "", "")
	f.Add(uint8(6), uint64(0), uint64(0xFFFFFFFFFFFFFFFF), "", "") // uint64(0) vs int64(-1)

	f.Fuzz(func(t *testing.T, kind uint8, aBits, bBits uint64, aStr, bStr string) {
		var a, b any
		var aNaN, bNaN bool
		switch kind % 7 {
		case 0:
			af, bf := math.Float64frombits(aBits), math.Float64frombits(bBits)
			a, b = af, bf
			aNaN, bNaN = math.IsNaN(af), math.IsNaN(bf)
		case 1:
			a, b = int64(aBits), int64(bBits)
		case 2:
			a, b = aStr, bStr
		case 3:
			a, b = []byte(aStr), []byte(bStr)
		case 4:
			af, bf := math.Float32frombits(uint32(aBits)), math.Float32frombits(uint32(bBits))
			a, b = af, bf
			aNaN = math.IsNaN(float64(af))
			bNaN = math.IsNaN(float64(bf))
		case 5:
			a, b = aBits, bBits
		case 6:
			// Mixed signed/unsigned integer pair. Both pack as the tuple
			// INTEGER type (ordered by true numeric value), so byte-order
			// agreement must hold across the signed/unsigned boundary too.
			a, b = aBits, int64(bBits)
		}

		cmpAB, errAB := compareValues(a, b)
		cmpBA, errBA := compareValues(b, a)
		if errAB != nil || errBA != nil {
			t.Fatalf("comparable fuzz domain must never hit the loud cross-type arm: %v / %v", errAB, errBA)
		}
		got := sign(cmpAB)
		rev := sign(cmpBA)
		if got != -rev {
			t.Fatalf("compareValues not antisymmetric: cmp(%v,%v)=%d cmp(%v,%v)=%d", a, b, got, b, a, rev)
		}

		if aNaN || bNaN {
			// Double.compare semantics: NaN sorts greatest, NaN == NaN.
			switch {
			case aNaN && bNaN:
				if got != 0 {
					t.Fatalf("NaN vs NaN must compare equal in-memory, got %d", got)
				}
			case aNaN:
				if got != 1 {
					t.Fatalf("NaN must sort greatest: cmp(NaN, %v)=%d", b, got)
				}
			default:
				if got != -1 {
					t.Fatalf("NaN must sort greatest: cmp(%v, NaN)=%d", a, got)
				}
			}
			return
		}

		aPacked := tuple.Tuple{a}.Pack()
		bPacked := tuple.Tuple{b}.Pack()
		want := sign(bytes.Compare(aPacked, bPacked))
		if got != want {
			t.Fatalf("sort comparator disagrees with index byte order: compareValues(%#v, %#v)=%d, tuple order=%d",
				a, b, got, want)
		}
	})
}

// FuzzCmpAnyVsCompareValues_PredicateSortSplit is the RFC-180 G3(b) net:
// the PREDICATE comparator (predicates cmpAny, reached through the exported
// Comparison.EvalAgainst) and the SORT comparator (compareValues) must agree
// on every numeric pair EXCEPT the one documented split — IEEE signed-zero
// equality. Predicates use SQL/IEEE numeric equality (-0.0 == +0.0 keeps
// the row; RFC-082 double_negative_zero_ge_predicate); the sort comparator
// uses the Double.compare total order (-0.0 < +0.0) to match index byte
// order. Any OTHER disagreement is a planner-invisible wrong-rows bug: the
// same value pair filters one way and orders another.
func FuzzCmpAnyVsCompareValues_PredicateSortSplit(f *testing.F) {
	f.Add(uint8(3), uint8(3), uint64(0), uint64(0x8000000000000000)) // +0.0 vs -0.0
	f.Add(uint8(3), uint8(3), math.Float64bits(math.NaN()), math.Float64bits(math.NaN()))
	f.Add(uint8(1), uint8(3), uint64(5), math.Float64bits(5.0)) // int64 5 vs 5.0
	f.Add(uint8(0), uint8(1), uint64(7), uint64(7))             // int32 vs int64
	f.Add(uint8(2), uint8(3), uint64(math.Float32bits(1.5)), math.Float64bits(1.5))
	f.Add(uint8(2), uint8(2), uint64(math.Float32bits(2.5)), uint64(math.Float32bits(10.5))) // lexical-vs-numeric split
	// Unsigned half of the integer domain (tuple decode above math.MaxInt64):
	// both comparators must order it identically — the adjacent boundary pair
	// deterministically kills a float-promotion revert on either side.
	f.Add(uint8(4), uint8(1), uint64(math.MaxInt64)+1, uint64(math.MaxInt64))
	f.Add(uint8(4), uint8(4), uint64(math.MaxInt64)+1, uint64(math.MaxUint64))
	f.Add(uint8(4), uint8(1), uint64(0), uint64(0xFFFFFFFFFFFFFFFF)) // uint64(0) vs int64(-1)

	mk := func(kind uint8, bits uint64) any {
		switch kind % 5 {
		case 0:
			return int32(bits)
		case 1:
			return int64(bits)
		case 2:
			return math.Float32frombits(uint32(bits))
		case 3:
			return math.Float64frombits(bits)
		default:
			return bits
		}
	}
	// predSign derives the predicate-layer ordering through the exported
	// eval surface: exactly one of a<b, a=b, a>b must hold.
	predSign := func(t *testing.T, a, b any) (int, bool) {
		t.Helper()
		lt, err1 := predicates.Comparison{Type: predicates.ComparisonLessThan}.EvalAgainst(a, b)
		eq, err2 := predicates.Comparison{Type: predicates.ComparisonEquals}.EvalAgainst(a, b)
		gt, err3 := predicates.Comparison{Type: predicates.ComparisonGreaterThan}.EvalAgainst(a, b)
		if err1 != nil || err2 != nil || err3 != nil {
			return 0, false
		}
		if lt == predicates.TriUnknown || eq == predicates.TriUnknown || gt == predicates.TriUnknown {
			return 0, false
		}
		trueCount := 0
		s := 0
		if lt == predicates.TriTrue {
			trueCount, s = trueCount+1, -1
		}
		if eq == predicates.TriTrue {
			trueCount, s = trueCount+1, 0
		}
		if gt == predicates.TriTrue {
			trueCount, s = trueCount+1, 1
		}
		if trueCount != 1 {
			t.Fatalf("predicate trichotomy violated for (%#v, %#v): lt=%v eq=%v gt=%v", a, b, lt, eq, gt)
		}
		return s, true
	}

	f.Fuzz(func(t *testing.T, aKind, bKind uint8, aBits, bBits uint64) {
		a, b := mk(aKind, aBits), mk(bKind, bBits)
		ps, ok := predSign(t, a, b)
		if !ok {
			return
		}
		cmpSS, errSS := compareValues(a, b)
		if errSS != nil {
			t.Fatalf("comparable fuzz domain must never hit the loud cross-type arm: %v", errSS)
		}
		ss := sign(cmpSS)
		if ps == ss {
			return
		}
		// The ONLY allowed split: an IEEE-equal pair whose canonical bit
		// patterns differ — i.e. ±0.0 (any float width): predicate says
		// equal, sort says -0.0 < +0.0.
		af, aok := toFloat64Scalar(a)
		bf, bok := toFloat64Scalar(b)
		if aok && bok && af == bf && ps == 0 && ss != 0 &&
			math.Signbit(af) != math.Signbit(bf) && af == 0 {
			return
		}
		t.Fatalf("predicate and sort comparators disagree beyond the documented signed-zero split: "+
			"pred(%#v, %#v)=%d sort=%d", a, b, ps, ss)
	})
}

// TestAggMinMax_NaNPropagatesOverInf pins the Java Math.min/Math.max corner
// the G4 exotic-float pin surfaced: Go's math.Min/math.Max resolve
// Min(-Inf, NaN) = -Inf and Max(+Inf, NaN) = +Inf (their special-case order
// checks infinities first), while Java propagates NaN unconditionally —
// evaluated MIN/MAX over a set holding both an infinity and a NaN must be
// NaN. RED before the javaMinF64/javaMaxF64 guard, in both operand orders
// and both float lanes.
func TestAggMinMax_NaNPropagatesOverInf(t *testing.T) {
	t.Parallel()
	nan, negInf, posInf := math.NaN(), math.Inf(-1), math.Inf(1)

	for _, tc := range []struct {
		name     string
		acc, val any
		isMin    bool
	}{
		{"min_negInf_then_NaN", negInf, nan, true},
		{"min_NaN_then_negInf", nan, negInf, true},
		{"max_posInf_then_NaN", posInf, nan, false},
		{"max_NaN_then_posInf", nan, posInf, false},
		{"min_f32_negInf_then_NaN", float32(math.Inf(-1)), float32(math.NaN()), true},
		{"max_f32_posInf_then_NaN", float32(math.Inf(1)), float32(math.NaN()), false},
	} {
		got := aggMinMax(tc.acc, tc.val, tc.isMin)
		isNaN := false
		switch g := got.(type) {
		case float64:
			isNaN = math.IsNaN(g)
		case float32:
			isNaN = math.IsNaN(float64(g))
		}
		if !isNaN {
			t.Errorf("%s: got %v, want NaN (Java Math.min/max propagates NaN over infinities)", tc.name, got)
		}
	}

	// The non-NaN corners keep Go/Java shared semantics: -0.0 below +0.0.
	if got := aggMinMax(math.Copysign(0, -1), 0.0, true); math.Signbit(got.(float64)) != true {
		t.Error("min(-0.0, +0.0) must be -0.0")
	}
	if got := aggMinMax(math.Copysign(0, -1), 0.0, false); math.Signbit(got.(float64)) != false {
		t.Error("max(-0.0, +0.0) must be +0.0")
	}
}
