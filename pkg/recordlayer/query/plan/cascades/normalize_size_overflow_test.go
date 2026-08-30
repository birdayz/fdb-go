package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// fourWayOr builds OR(a=1, b=1, c=1, d=1) — a DNF size of exactly 4.
func fourWayOr(i int) predicates.QueryPredicate {
	return predicates.NewOr(
		pred(string(rune('a'+i%20))+"0"),
		pred(string(rune('a'+i%20))+"1"),
		pred(string(rune('a'+i%20))+"2"),
		pred(string(rune('a'+i%20))+"3"),
	)
}

// andOfFourWayOrs builds AND(OR4, OR4, ...) with n four-way OR groups. Its
// true DNF size is 4^n.
func andOfFourWayOrs(n int) predicates.QueryPredicate {
	children := make([]predicates.QueryPredicate, 0, n)
	for i := 0; i < n; i++ {
		children = append(children, fourWayOr(i))
	}
	return predicates.NewAnd(children...)
}

// TestNormalFormSize_OverflowSaturatesNotWraps pins the exact shape that made
// the size guard admit an exponential predicate: an AND of 32 four-way ORs has
// a true DNF size of 4^32 == 2^64, which wraps int64 to EXACTLY ZERO. A guard
// that only inspects the sign of the accumulator after the multiply sees 0,
// concludes the normal form is tiny, and lets the distribution materialize the
// full cross product.
//
// Java raises ArithmeticException from Math.multiplyExact and shouldNormalize
// (BooleanPredicateNormalizer.java:296-301) treats that as "definitely too
// big". Go must report a size ABOVE the limit for the same input.
//
// The assertion is on the size computation rather than on
// NormalizeDNFWithoutSimplification because with the defect present the latter
// does not fail — it builds 4^32 clauses and takes the machine with it.
func TestNormalFormSize_OverflowSaturatesNotWraps(t *testing.T) {
	t.Parallel()

	// 32 groups: 4^32 == 2^64 == 0 mod 2^64, the wrap-to-zero case.
	for _, groups := range []int{31, 32, 33, 64} {
		p := andOfFourWayOrs(groups)

		if got := normalFormSize(p, false, normalFormDNF); got <= int64(NormalizerDefaultSizeLimit) {
			t.Errorf("normalFormSize(AND of %d four-way ORs, DNF) = %d; true size is 4^%d, "+
				"which must report ABOVE the limit %d (int64 overflow must saturate, not wrap)",
				groups, got, groups, NormalizerDefaultSizeLimit)
		}
		// There used to be a second assertion here, on the OTHER of Go's two
		// DNF size functions over this same input — the two implementations had
		// to be checked to agree. RFC-240 left one function, so the second
		// assertion became a duplicate of the first and is gone rather than
		// repointed. Repointing it at the CNF mode instead was tried and is
		// WRONG: an AND of ORs is a major of minors in CNF, so its CNF size is
		// the SUM (31, 32, 33, 64) and not a product — the mirrored input
		// below is what exercises the CNF direction.

		// The CNF direction needs the mirrored shape: an OR of four-way ANDs.
		conj := make([]predicates.QueryPredicate, 0, groups)
		for i := 0; i < groups; i++ {
			conj = append(conj, predicates.NewAnd(
				pred("x0"), pred("x1"), pred("x2"), pred("x3")))
		}
		if got := normalFormSize(predicates.NewOr(conj...), false, normalFormCNF); got <= int64(cnfSizeLimit) {
			t.Errorf("normalFormSize(OR of %d four-way ANDs, CNF) = %d; true size is 4^%d, "+
				"which must report ABOVE the limit %d", groups, got, groups, cnfSizeLimit)
		}
	}
}

// TestNormalizeDNF_DeclinesOverflowingPredicate is the end-to-end half: the
// normalizer must decline the AND of 32 four-way ORs rather than distribute it.
//
// With the size guard wrapping instead of saturating this test does not report
// a failure — it consumes the machine. A 4^32-clause cross product is not a
// slow test, it is an OOM kill. That is the defect, stated as a test.
func TestNormalizeDNF_DeclinesOverflowingPredicate(t *testing.T) {
	t.Parallel()

	p := andOfFourWayOrs(32)
	if _, changed := NormalizeDNFWithoutSimplification(p, NormalizerDefaultSizeLimit); changed {
		t.Fatal("NormalizeDNFWithoutSimplification distributed an AND of 32 four-way ORs " +
			"(4^32 clauses); the size guard must decline it")
	}
	if _, changed := NormalizeDNF(p, cnfSizeLimit); changed {
		t.Fatal("NormalizeDNF distributed an AND of 32 four-way ORs (4^32 clauses); " +
			"the size guard must decline it")
	}
	// The CNF direction: an OR of 32 four-way ANDs.
	conj := make([]predicates.QueryPredicate, 0, 32)
	for i := 0; i < 32; i++ {
		conj = append(conj, predicates.NewAnd(pred("x0"), pred("x1"), pred("x2"), pred("x3")))
	}
	if _, changed := normalizeCNF(predicates.NewOr(conj...), cnfSizeLimit); changed {
		t.Fatal("normalizeCNF distributed an OR of 32 four-way ANDs (4^32 clauses); " +
			"the size guard must decline it")
	}
}

// TestSaturatingSize_Arithmetic pins the saturating helpers directly, including
// the boundaries where a wrapping implementation produces a SMALLER value than
// its inputs.
func TestSaturatingSize_Arithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  int64
		want int64
	}{
		{"mul small", saturatingMulSize(3, 7), 21},
		{"mul zero left", saturatingMulSize(0, math.MaxInt64), 0},
		{"mul zero right", saturatingMulSize(math.MaxInt64, 0), 0},
		{"mul at edge", saturatingMulSize(math.MaxInt64/2, 2), (math.MaxInt64 / 2) * 2},
		{"mul overflow", saturatingMulSize(math.MaxInt64, 2), math.MaxInt64},
		{"mul wrap-to-zero", saturatingMulSize(1<<32, 1<<32), math.MaxInt64},
		{"mul saturated is absorbing", saturatingMulSize(math.MaxInt64, math.MaxInt64), math.MaxInt64},
		{"add small", saturatingAddSize(3, 7), 10},
		{"add overflow", saturatingAddSize(math.MaxInt64, 1), math.MaxInt64},
		{"add saturated is absorbing", saturatingAddSize(math.MaxInt64, math.MaxInt64), math.MaxInt64},
		{"add zero", saturatingAddSize(math.MaxInt64, 0), math.MaxInt64},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
