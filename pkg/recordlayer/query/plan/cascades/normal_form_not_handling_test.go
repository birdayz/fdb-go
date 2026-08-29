package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestNormalForm_NotOverConnective_TwoImplementationsDisagree measures, and
// keeps measured, a divergence this package cannot fix under its own review
// gate: Go has TWO normal-form implementations where Java has ONE, and the one
// wired into the planner is the one that does not match Java.
//
// Java's BooleanPredicateNormalizer has a single `isInNormalForm` — a NOT over
// anything that is not a variable is NOT in normal form
// (BooleanPredicateNormalizer.java:284-289) — and a single
// `toNormalized(predicate, negate)` that carries a negate flag down through
// AND/OR, which is De Morgan. Both `normalize` and `normalizeAndSimplify`, and
// both CNF and DNF modes, go through them.
//
// Go split that into two:
//
//	normalize_dnf_exact.go     isInDNFStrict + toDNFNegated   MATCHES Java
//	rule_normalize_predicates.go  isInCNF/isInDNF + toCNFNormalized/toDNFNormalized
//	                                                          treats NOT as a LEAF
//
// `isLeafPredicate` (rule_normalize_predicates.go) returns true for a
// NotPredicate, so `AND(NOT(AND(a, b)), c)` reads as already-CNF and
// normalizeCNF declines. NormalizePredicatesRule — registered in the default
// planner pipeline, and a port of the Java rule that calls
// normalizeAndSimplify in CNF mode — therefore leaves the NOT standing, where
// Java produces `(NOT a OR NOT b) AND c`. That shape is what
// PredicateToLogicalUnionRule needs to split a disjunction across index
// accesses, so `WHERE NOT (x = 1 AND y = 2)` stays one opaque residual in Go.
//
// The exact port is reachable and correct, which is what makes this a wiring
// question rather than an algorithm one: NormalizeDNFWithoutSimplification
// pushes the same NOT through on the same input, asserted below.
//
// This test asserts the CURRENT state, deliberately. Closing the divergence
// changes the planner's canonical predicate shape for every query containing a
// NOT over a connective — plans, goldens and the explaindiff corpus — so it is
// a query-engine change carrying a plan-shape diff, not a local edit. When it
// lands, this test fails, and it fails holding the whole measurement rather
// than leaving the next reader to re-derive it. TODO.md's entry of the same
// name carries the rest; the two point at each other.
func TestNormalForm_NotOverConnective_TwoImplementationsDisagree(t *testing.T) {
	t.Parallel()

	rowType := predicateSemanticsRowType()
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("normal_form"), rowType)
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	a, err := values.ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	b, err := values.ResolveFieldOrdinals(root, []int{1})
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	eqA := predicates.NewComparisonPredicate(a,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	eqB := predicates.NewComparisonPredicate(b,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)))
	gtA := predicates.NewComparisonPredicate(a,
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))

	// The shape a `WHERE NOT (x = 1 AND y = 2) AND x > 0` reaches the planner
	// as: the SQL resolver builds a plain NotPredicate (expr.ResolveNot) and
	// applies no De Morgan of its own, so the NOT arrives intact.
	input := predicates.NewAnd(predicates.NewNot(predicates.NewAnd(eqA, eqB)), gtA)

	// The planner-wired path declines.
	cnf, cnfChanged := normalizeCNF(input, cnfSizeLimit)
	if cnfChanged {
		t.Fatalf("normalizeCNF now normalizes through a NOT over a connective — the divergence "+
			"this test measures is CLOSED. Confirm it matches Java's "+
			"(NOT a OR NOT b) AND c and replace this test with that assertion.\n  got: %s",
			cnf.Explain())
	}
	if _, stillNot := firstConjunctIsNot(cnf); !stillNot {
		t.Fatalf("normalizeCNF returned something other than its unchanged input: %s", cnf.Explain())
	}

	// Same for the DNF twin in the same file, which shares isLeafPredicate.
	if _, dnfChanged := NormalizeDNF(input, cnfSizeLimit); dnfChanged {
		t.Fatal("NormalizeDNF now normalizes through a NOT over a connective while " +
			"normalizeCNF still does not — the two halves of one file have drifted apart")
	}

	// The exact Java port, right beside them, DOES push it. This is the half
	// that makes the finding a wiring question: nothing has to be invented.
	strict, strictChanged := NormalizeDNFWithoutSimplification(input, NormalizerDefaultSizeLimit)
	if !strictChanged {
		t.Fatal("NormalizeDNFWithoutSimplification stopped pushing NOT through a connective — " +
			"the one implementation that matches Java's toNormalized(negate) has regressed")
	}
	explained := strict.Explain()
	for _, want := range []string{"NOT (normal_form.A#0 = 1)", "NOT (normal_form.B#1 = 2)"} {
		if !strings.Contains(explained, want) {
			t.Fatalf("expected the De Morgan split to surface %q, got: %s", want, explained)
		}
	}
	if strings.Contains(explained, "NOT (normal_form.A#0 = 1 AND normal_form.B#1 = 2)") {
		t.Fatalf("the NOT was not pushed through the AND: %s", explained)
	}
}

// firstConjunctIsNot reports whether p is an AND whose first conjunct is a NOT
// — the shape normalizeCNF hands back unchanged. Used instead of comparing
// Explain text so the assertion survives a rendering change.
func firstConjunctIsNot(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
	and, isAnd := p.(*predicates.AndPredicate)
	if !isAnd || len(and.SubPredicates) == 0 {
		return nil, false
	}
	not, isNot := and.SubPredicates[0].(*predicates.NotPredicate)
	return not, isNot
}
