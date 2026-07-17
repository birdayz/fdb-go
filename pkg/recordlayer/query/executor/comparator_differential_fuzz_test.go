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

	f.Fuzz(func(t *testing.T, kind uint8, aBits, bBits uint64, aStr, bStr string) {
		var a, b any
		var aNaN, bNaN bool
		switch kind % 5 {
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
		}

		got := sign(compareValues(a, b))
		rev := sign(compareValues(b, a))
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

	mk := func(kind uint8, bits uint64) any {
		switch kind % 4 {
		case 0:
			return int32(bits)
		case 1:
			return int64(bits)
		case 2:
			return math.Float32frombits(uint32(bits))
		default:
			return math.Float64frombits(bits)
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
		ss := sign(compareValues(a, b))
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
