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

// TestNormalizeCNF_GateIsJavasComplexityThreshold pins the VALUE of the CNF
// gate, and it exists because the change that set it shipped without a test —
// reverting `cnfSizeLimit` to 1,000,000 reddened nothing, which meant the whole
// suite could not detect the change at all.
//
// The gate is Java's, not the normalizer's default. NormalizePredicatesRule
// calls forConfiguration(Mode.CNF, plannerConfiguration)
// (NormalizePredicatesRule.java:75-77), forConfiguration returns the DEFAULT
// instance only when getComplexityThreshold() equals DEFAULT_SIZE_LIMIT
// (:175-180), and that threshold falls back to
// RecordQueryPlanner.DEFAULT_COMPLEXITY_THRESHOLD = 3000
// (RecordQueryPlannerConfiguration.java:154-156, RecordQueryPlanner.java:150).
// Go gated at 1,000,000 — 333x — while RFC-240 was busy pushing MORE predicates
// into that gate.
//
// The fixture sits in the WINDOW between the two limits, which is the only
// place the two behaviours differ: an OR of six four-way ANDs has a CNF size of
// 4^6 = 4096, above 3000 and far below 1,000,000. At Java's gate it declines;
// at Go's old one it distributed. Both bounds are asserted, so a fixture that
// drifts out of the window fails as an inert fixture rather than passing for
// the wrong reason.
func TestNormalizeCNF_GateIsJavasComplexityThreshold(t *testing.T) {
	t.Parallel()

	// OR of six four-way ANDs: CNF size = 4^6 = 4096.
	conj := make([]predicates.QueryPredicate, 0, 6)
	for i := 0; i < 6; i++ {
		conj = append(conj, predicates.NewAnd(
			pred(string(rune('a'+i))+"0"), pred(string(rune('a'+i))+"1"),
			pred(string(rune('a'+i))+"2"), pred(string(rune('a'+i))+"3")))
	}
	p := predicates.NewOr(conj...)

	size := normalFormSize(p, false, normalFormCNF)
	if size != 4096 {
		t.Fatalf("fixture drifted: CNF size is %d, want 4096", size)
	}
	// The window. Without both bounds this test would pass at any limit above
	// the size, which is the failure it was written to prevent.
	if size <= int64(cnfSizeLimit) {
		t.Fatalf("fixture is inert: CNF size %d is at or below the gate %d, so the "+
			"decline below proves nothing about the gate's VALUE", size, cnfSizeLimit)
	}
	if size > int64(NormalizerDefaultSizeLimit) {
		t.Fatalf("fixture is outside the window: CNF size %d exceeds the OLD limit %d "+
			"too, so it would have declined either way", size, NormalizerDefaultSizeLimit)
	}

	if _, changed := normalizeCNF(p, cnfSizeLimit); changed {
		t.Fatalf("normalizeCNF distributed a %d-clause CNF. Java's planner gate is "+
			"DEFAULT_COMPLEXITY_THRESHOLD = 3000 (RecordQueryPlanner.java:150), reached "+
			"through forConfiguration; cnfSizeLimit is %d and must match it.",
			size, cnfSizeLimit)
	}

	// The SAME predicate normalizes when the limit is raised — which shows the
	// decline above is attributable to the LIMIT rather than to the shape.
	//
	// It constrains nothing about cnfSizeLimit, because it passes a different
	// constant. Saying otherwise is what the previous revision of this comment
	// did, and it was measured false: with only these two assertions, gates of
	// 1 and of 4095 both pass. The LOW direction is the one that matters —
	// a gate too low declines predicates Java normalizes, which is RFC-240's
	// original defect wearing the other hat — so the low end is closed by the
	// second fixture below, against cnfSizeLimit itself.
	//
	// An earlier draft asserted the WRITE path here and was wrong for a reason
	// worth keeping: this predicate is an OR of ANDs, ALREADY in DNF, so
	// NormalizeDNFWithoutSimplification declines it on SHAPE and never consults
	// its limit. A decline that proves nothing about the limit reads exactly
	// like one that does.
	if _, changed := normalizeCNF(p, NormalizerDefaultSizeLimit); !changed {
		t.Fatalf("the fixture does not normalize even at %d, so the decline above "+
			"is not attributable to the gate's value", NormalizerDefaultSizeLimit)
	}

	// THE LOW END, closed against cnfSizeLimit itself. An OR of FIVE four-way
	// ANDs has CNF size 4^5 = 1024, under the gate, and must normalize.
	small := make([]predicates.QueryPredicate, 0, 5)
	for i := 0; i < 5; i++ {
		small = append(small, predicates.NewAnd(
			pred(string(rune('a'+i))+"0"), pred(string(rune('a'+i))+"1"),
			pred(string(rune('a'+i))+"2"), pred(string(rune('a'+i))+"3")))
	}
	sp := predicates.NewOr(small...)
	smallSize := normalFormSize(sp, false, normalFormCNF)
	if smallSize != 1024 {
		t.Fatalf("low-end fixture drifted: CNF size is %d, want 1024", smallSize)
	}
	if smallSize >= int64(cnfSizeLimit) {
		t.Fatalf("low-end fixture is inert: CNF size %d is at or above the gate %d",
			smallSize, cnfSizeLimit)
	}
	if _, changed := normalizeCNF(sp, cnfSizeLimit); !changed {
		t.Fatalf("normalizeCNF declined a %d-clause CNF at a gate of %d — the gate is "+
			"too LOW, which makes Go refuse a normalization Java performs",
			smallSize, cnfSizeLimit)
	}

	// What the two fixtures actually constrain, stated as the arithmetic rather
	// than as a claim: the high one requires cnfSizeLimit < 4096, the low one
	// requires cnfSizeLimit >= 1024. That is a bracket of [1024, 4095] — real,
	// but not a pin. The exact value is pinned separately, below, because a
	// bracket is not what Java says.
	if cnfSizeLimit != 3000 {
		t.Fatalf("cnfSizeLimit is %d, want 3000 — Java's "+
			"RecordQueryPlanner.DEFAULT_COMPLEXITY_THRESHOLD (:150), which "+
			"RecordQueryPlannerConfiguration.getComplexityThreshold (:154-156) falls "+
			"back to and NormalizePredicatesRule reads through forConfiguration "+
			"(:75-77). The fixtures above only bracket it to [1024, 4095]; this is "+
			"the assertion that says which value in that range is Java's.",
			cnfSizeLimit)
	}
}
