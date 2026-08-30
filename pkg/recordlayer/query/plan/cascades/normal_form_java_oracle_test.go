package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Java's BooleanPredicateNormalizerTest, ported.
//
// This is RFC-240's PRIMARY verification, and it is primary because of what it
// checks against: Java's ASSERTED outputs, written by the authors of the
// algorithm, rather than anything derived from reading the Go port. Every other
// test in this change checks the port against itself in some form — a golden
// captured from it, a property it satisfies, a mutation of it. This one does
// not.
//
// Cases and line numbers are BooleanPredicateNormalizerTest.java:
//
//	atomic          :69-75    a bare leaf and a bare NOT stay put
//	flattenDnf      :77-91    nested majors/minors flatten
//	flattenCnf      :93-107
//	distributeDnf   :109-119  the cross product, INCLUDING its clause order
//	distributeCnf   :121-128
//	deMorgan        :130-139  the four cases RFC-240 exists to make pass
//	complexDnf      :141-145
//	complexCnf      :147-151
//	complexRoundTrip:153-165  DNF -> CNF -> DNF round trips
//
// assertExpectedNormalization (:262-267) also asserts STABILITY, which is
// ported alongside every case rather than as a separate test, exactly as Java
// does it: a normal form re-normalizes to itself.

func TestJavaOracle_Atomic(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	assertDNF(t, p[1], p[1])
	assertDNF(t, predicates.NewNot(p[1]), predicates.NewNot(p[1]))
	assertCNF(t, p[1], p[1])
	assertCNF(t, predicates.NewNot(p[1]), predicates.NewNot(p[1]))
}

func TestJavaOracle_Flatten(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or := predicates.NewAnd, predicates.NewOr

	for _, assert := range []func(*testing.T, predicates.QueryPredicate, predicates.QueryPredicate){assertDNF, assertCNF} {
		assert(t, and(p[1], p[2], p[3]), and(and(p[1], p[2]), p[3]))
		assert(t, and(p[1], p[2], p[3]), and(p[1], and(p[2], p[3])))
		assert(t, and(p[1], p[2], p[3], p[4], p[5]),
			and(p[1], and(p[2], and(p[3], and(p[4], p[5])))))
		assert(t, and(p[1], p[2], p[3], p[4], p[5]),
			and(and(and(and(p[1], p[2]), p[3]), p[4]), p[5]))
		assert(t, or(p[1], p[2], p[3]), or(or(p[1], p[2]), p[3]))
		assert(t, or(p[1], p[2], p[3]), or(p[1], or(p[2], p[3])))
	}
}

func TestJavaOracle_Distribute(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or := predicates.NewAnd, predicates.NewOr

	// distributeDnf. The expected CLAUSE ORDER is Java's and is asserted:
	// NormalizeDNFWithoutSimplification's order becomes stored index predicate
	// bytes, so an order-insensitive check here would not be checking the thing
	// that matters.
	assertDNF(t, or(p[1], and(p[2], p[3])), and(or(p[1], p[2]), or(p[1], p[3])))
	assertDNF(t, or(and(p[1], p[2]), and(p[3], p[4])), or(and(p[1], p[2]), and(p[3], p[4])))
	assertDNF(t, or(and(p[1], p[2]), and(p[1], p[3])), and(p[1], or(p[2], p[3])))
	assertDNF(t, or(and(p[1], p[3]), and(p[2], p[3]), and(p[1], p[4]), and(p[2], p[4])),
		and(or(p[1], p[2]), or(p[3], p[4])))

	// distributeCnf.
	assertCNF(t, and(or(p[1], p[2]), or(p[3], p[4])), and(or(p[1], p[2]), or(p[3], p[4])))
	assertCNF(t, and(p[1], or(p[2], p[3])), or(and(p[1], p[2]), and(p[1], p[3])))
	assertCNF(t, and(or(p[1], p[2]), or(p[3], p[4])),
		or(and(p[1], p[3]), and(p[2], p[3]), and(p[1], p[4]), and(p[2], p[4])))
}

// TestJavaOracle_DeMorgan is the reason RFC-240 exists. Before it, the CNF half
// of this test failed: normalizeCNF treated a NotPredicate as a leaf and
// returned the input unchanged.
func TestJavaOracle_DeMorgan(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or, not := predicates.NewAnd, predicates.NewOr, predicates.NewNot

	assertDNF(t, or(not(p[1]), not(p[2])), not(and(p[1], p[2])))
	assertDNF(t, and(not(p[1]), not(p[2])), not(or(p[1], p[2])))
	assertCNF(t, or(not(p[1]), not(p[2])), not(and(p[1], p[2])))
	assertCNF(t, and(not(p[1]), not(p[2])), not(or(p[1], p[2])))
}

func TestJavaOracle_Complex(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or, not := predicates.NewAnd, predicates.NewOr, predicates.NewNot

	// complexDnf.
	assertDNF(t, or(and(p[1], not(p[2])), and(p[1], not(p[3]), not(p[4]))),
		and(p[1], not(and(p[2], or(p[3], p[4])))))

	// complexCnf.
	assertCNF(t, and(p[1], or(not(p[2]), not(p[3])), or(not(p[2]), not(p[4]))),
		or(and(p[1], not(p[2])), and(p[1], not(p[3]), not(p[4]))))
}

func TestJavaOracle_ComplexRoundTrip(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or := predicates.NewAnd, predicates.NewOr

	assertDNF(t, or(and(p[1], p[2]), and(p[1], p[3]), and(p[1], p[4]), and(p[1], p[5])),
		and(p[1], or(p[2], p[3], p[4], p[5])))
	assertCNF(t, and(p[1], or(p[2], p[3], p[4], p[5])),
		or(and(p[1], p[2]), and(p[1], p[3]), and(p[1], p[4]), and(p[1], p[5])))

	assertDNF(t, or(and(p[1], p[2], p[3]), and(p[1], p[4], p[5])),
		and(p[1], or(and(p[2], p[3]), and(p[4], p[5]))))
	assertCNF(t, and(p[1], or(p[2], p[4]), or(p[3], p[4]), or(p[2], p[5]), or(p[3], p[5])),
		or(and(p[1], p[2], p[3]), and(p[1], p[4], p[5])))
	assertDNF(t, or(and(p[1], p[2], p[3]), and(p[1], p[4], p[5])),
		and(p[1], or(p[2], p[4]), or(p[3], p[4]), or(p[2], p[5]), or(p[3], p[5])))
}

// assertDNF / assertCNF are Java's assertExpectedDnf / assertExpectedCnf, which
// route through assertExpectedNormalization (:262-267) — expected output AND
// stability.
func assertDNF(t *testing.T, expected, given predicates.QueryPredicate) {
	t.Helper()
	assertNormalization(t, "DNF", expected, given, func(p predicates.QueryPredicate) predicates.QueryPredicate {
		out, changed := NormalizeDNF(p, NormalizerDefaultSizeLimit)
		if !changed {
			return p // Java's .orElse(given)
		}
		return out
	})
}

func assertCNF(t *testing.T, expected, given predicates.QueryPredicate) {
	t.Helper()
	assertNormalization(t, "CNF", expected, given, func(p predicates.QueryPredicate) predicates.QueryPredicate {
		out, changed := normalizeCNF(p, cnfSizeLimit)
		if !changed {
			return p
		}
		return out
	})
}

func assertNormalization(
	t *testing.T, mode string, expected, given predicates.QueryPredicate,
	normalize func(predicates.QueryPredicate) predicates.QueryPredicate,
) {
	t.Helper()
	got := normalize(given)
	if !predicates.PredicateEquals(got, expected) {
		t.Fatalf("%s: normalize(%s)\n  got:  %s\n  want: %s",
			mode, given.Explain(), got.Explain(), expected.Explain())
	}
	// Java: "Normalized form should be stable".
	stable := normalize(got)
	if !predicates.PredicateEquals(stable, got) {
		t.Fatalf("%s: normal form is not stable — normalize(%s) = %s",
			mode, got.Explain(), stable.Explain())
	}
}

// javaOracleLeaves returns P1..P7, Java's fixture predicates (:61-67) — the
// same column compared against seven distinct constants, so two leaves are
// never accidentally equal.
//
// Indexed from 1 so the cases above read exactly as Java's do; index 0 is unused.
func javaOracleLeaves(t *testing.T) [8]predicates.QueryPredicate {
	t.Helper()
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("java_oracle"), predicateSemanticsRowType())
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve field: %v", err)
	}
	var out [8]predicates.QueryPredicate
	for i := 1; i <= 7; i++ {
		out[i] = predicates.NewComparisonPredicate(field,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)))
	}
	return out
}

// TestJavaOracle_Redundant is BooleanPredicateNormalizerTest.redundant
// (:180-186), and it is the ONLY case in Java's suite that exercises the
// absorption law — RFC-240 commit A changed absorption's tie-break and had no
// Java-side case covering it until now.
//
// `and(P1, or(and(P2, P3), and(P2, P4, P3)))` distributes to
// `or(and(P1,P2,P3), and(P1,P2,P4,P3))`, and the second clause is a superset of
// the first, so absorption removes it and the answer collapses to
// `and(P1, P2, P3)`.
func TestJavaOracle_Redundant(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or := predicates.NewAnd, predicates.NewOr

	assertDNF(t, and(p[1], p[2], p[3]),
		and(p[1], or(and(p[2], p[3]), and(p[2], p[4], p[3]))))
}

// TestJavaOracle_ComplexRoundTripThreeWay is the second half of Java's
// complexRoundTrip (:166-178), which the first port stopped short of — and the
// tell was that javaOracleLeaves builds P6 and P7 and nothing used them.
//
// It round-trips a three-way disjunction of conjunctions through CNF and back,
// asserting the DNF is reached from the original, from its CNF, and from its
// own expected form. Java asserts all three because a normalizer can be right
// on the first and wrong on a re-entry.
func TestJavaOracle_ComplexRoundTripThreeWay(t *testing.T) {
	t.Parallel()
	p := javaOracleLeaves(t)
	and, or := predicates.NewAnd, predicates.NewOr

	original := and(p[1], or(and(p[2], p[3]), and(p[4], p[5]), and(p[6], p[7])))
	expectedDNF := or(and(p[1], p[2], p[3]), and(p[1], p[4], p[5]), and(p[1], p[6], p[7]))

	assertDNF(t, expectedDNF, original)

	// original -> cnf -> dnf
	cnfOfOriginal := normalizeOrSelf(func(q predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
		return normalizeCNF(q, cnfSizeLimit)
	}, original)
	assertDNF(t, expectedDNF, cnfOfOriginal)

	// expected dnf -> cnf -> dnf
	cnfOfExpected := normalizeOrSelf(func(q predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
		return normalizeCNF(q, cnfSizeLimit)
	}, expectedDNF)
	assertDNF(t, expectedDNF, cnfOfExpected)
}

// TestJavaOracle_SizeIsExactNotJustOverTheLimit ports the assertion at
// BooleanPredicateNormalizerTest.java:249, which pins the normalized size to an
// EXACT value — 2^62 for 62 two-way ORs — rather than merely "above the limit".
//
// The distinction is the point. A saturating multiply that clamped too early,
// or a sum where a product belongs, still lands above any limit and still
// refuses the predicate; only the exact value shows the arithmetic is the
// arithmetic. Go's other overflow test asserts the refusal; this one asserts
// the number.
func TestJavaOracle_SizeIsExactNotJustOverTheLimit(t *testing.T) {
	t.Parallel()
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("java_oracle_size"), predicateSemanticsRowType())
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve field: %v", err)
	}

	conjuncts := make([]predicates.QueryPredicate, 0, 62)
	for i := 0; i < 62; i++ {
		disjuncts := make([]predicates.QueryPredicate, 0, 2)
		for j := 0; j < 2; j++ {
			disjuncts = append(disjuncts, predicates.NewComparisonPredicate(field,
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i*100+j))))
		}
		conjuncts = append(conjuncts, predicates.NewOr(disjuncts...))
	}
	cnf := predicates.NewAnd(conjuncts...)

	// Java: assertEquals(4611686018427387904L, normalizer.getNormalizedSize(cnf))
	const want = int64(4611686018427387904) // 2^62
	if got := normalFormSize(cnf, false, normalFormDNF); got != want {
		t.Fatalf("normalFormSize(62 two-way ORs, DNF) = %d, want exactly %d (2^62).\n"+
			"An over-the-limit answer is not enough here: a saturation that clamps "+
			"early, or a sum where a product belongs, also lands above every limit.",
			got, want)
	}
	// And it still refuses, which is the behaviour the size feeds.
	if _, changed := NormalizeDNFWithoutSimplification(cnf, NormalizerDefaultSizeLimit); changed {
		t.Fatal("a 2^62-clause normal form was not declined")
	}
}

// normalizeOrSelf is Java's `.orElse(given)` — a declined normalization yields
// the input unchanged.
func normalizeOrSelf(
	normalize func(predicates.QueryPredicate) (predicates.QueryPredicate, bool),
	given predicates.QueryPredicate,
) predicates.QueryPredicate {
	out, changed := normalize(given)
	if !changed {
		return given
	}
	return out
}
