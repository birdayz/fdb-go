package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestNormalizeCNF_PushesNotThroughAConnective is the POSITIVE form of the test
// that measured this divergence before it was closed (RFC-240 §8.4).
//
// It replaces an assertion that Go had TWO normal-form implementations which
// disagreed — `normalize_dnf_exact.go` pushed a NOT through De Morgan while the
// planner-wired `normalizeCNF` treated every NotPredicate as a leaf and
// declined. There is one implementation now, so the thing to assert is the
// answer rather than the disagreement.
//
// The shape is what a `WHERE NOT (x = 1 AND y = 2) AND x > 0` reaches the
// planner as: `expr.ResolveNot` builds a plain NotPredicate and applies no De
// Morgan of its own, so the NOT arrives intact. Java's NormalizePredicatesRule
// calls normalizeAndSimplify in CNF mode and produces `(NOT a OR NOT b) AND c`;
// that shape is the precursor PredicateToLogicalUnionRule consumes to split a
// disjunction across index accesses, which is why a declined normalization cost
// plan quality rather than rows.
func TestNormalizeCNF_PushesNotThroughAConnective(t *testing.T) {
	t.Parallel()

	eqA, eqB, gtA := normalFormNotHandlingLeaves(t)
	input := predicates.NewAnd(predicates.NewNot(predicates.NewAnd(eqA, eqB)), gtA)

	got, changed := normalizeCNF(input, cnfSizeLimit)
	if !changed {
		t.Fatalf("normalizeCNF declined a NOT over a connective — the lax "+
			"already-in-normal-form test is back: %s", input.Explain())
	}

	// Java: (NOT a OR NOT b) AND c. Asserted structurally rather than on
	// Explain text so a rendering change does not read as a behaviour change.
	and, isAnd := got.(*predicates.AndPredicate)
	if !isAnd || len(and.SubPredicates) != 2 {
		t.Fatalf("expected a 2-conjunct AND, got %s", got.Explain())
	}
	or, isOr := and.SubPredicates[0].(*predicates.OrPredicate)
	if !isOr || len(or.SubPredicates) != 2 {
		t.Fatalf("expected the first conjunct to be a 2-way OR of NOTs, got %s",
			and.SubPredicates[0].Explain())
	}
	for i, want := range []predicates.QueryPredicate{eqA, eqB} {
		not, isNot := or.SubPredicates[i].(*predicates.NotPredicate)
		if !isNot {
			t.Fatalf("OR child %d is %s, want a NOT", i, or.SubPredicates[i].Explain())
		}
		if !predicates.PredicateEquals(not.Child, want) {
			t.Fatalf("OR child %d negates %s, want %s", i, not.Child.Explain(), want.Explain())
		}
	}
	if !predicates.PredicateEquals(and.SubPredicates[1], gtA) {
		t.Fatalf("second conjunct is %s, want %s", and.SubPredicates[1].Explain(), gtA.Explain())
	}
}

// TestNormalizeCNF_IsStableOnItsOwnOutput ports Java's stability assertion
// (BooleanPredicateNormalizerTest.java:262-267, "Normalized form should be
// stable").
//
// It is not decoration: it is the property that lets NormalizePredicatesRule
// drop the identity-keyed `normalized` set it used to carry. Java has no such
// set — its termination is exactly this, `isInNormalForm` accepting the rule's
// own output so a re-fire yields Optional.empty(). Asserting it here is what
// makes that deletion a consequence rather than a hope.
func TestNormalizeCNF_IsStableOnItsOwnOutput(t *testing.T) {
	t.Parallel()

	eqA, eqB, gtA := normalFormNotHandlingLeaves(t)
	for _, input := range []predicates.QueryPredicate{
		predicates.NewAnd(predicates.NewNot(predicates.NewAnd(eqA, eqB)), gtA),
		predicates.NewNot(predicates.NewOr(eqA, eqB)),
		predicates.NewOr(predicates.NewAnd(eqA, eqB), predicates.NewAnd(eqB, gtA)),
	} {
		normalized, changed := normalizeCNF(input, cnfSizeLimit)
		if !changed {
			t.Fatalf("fixture is inert: %s was already in CNF, so stability is untested here",
				input.Explain())
		}
		again, changedAgain := normalizeCNF(normalized, cnfSizeLimit)
		if changedAgain {
			t.Fatalf("normalizeCNF is not stable on its own output — %s became %s.\n"+
				"NormalizePredicatesRule relies on this to terminate without keeping "+
				"a set of expressions it has already fired on.",
				normalized.Explain(), again.Explain())
		}
	}
}

func normalFormNotHandlingLeaves(t *testing.T) (eqA, eqB, gtA predicates.QueryPredicate) {
	t.Helper()
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
	return predicates.NewComparisonPredicate(a,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
		predicates.NewComparisonPredicate(b,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
		predicates.NewComparisonPredicate(a,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
}

// TestNormalizePredicatesRule_DeclinesOnItsOwnOutput is the RULE-level form of
// the stability property, and it is what replaced the identity-keyed set the
// rule used to carry (RFC-240 §9).
//
// TestNormalizeCNF_IsStableOnItsOwnOutput asserts it of the FUNCTION. That is
// necessary and not sufficient: the rule wraps the function in a conjunction of
// the select's predicate list and then re-splits the result with andConjuncts,
// and a round trip through that wrapper is where a shape could come back
// different. This drives the rule's own decline condition over the round trip.
//
// Without it, deleting the set would rest on an argument rather than a
// measurement, and the failure mode of a wrong argument here is a planner that
// does not terminate.
func TestNormalizePredicatesRule_DeclinesOnItsOwnOutput(t *testing.T) {
	t.Parallel()

	eqA, eqB, gtA := normalFormNotHandlingLeaves(t)
	for _, input := range []predicates.QueryPredicate{
		predicates.NewAnd(predicates.NewNot(predicates.NewAnd(eqA, eqB)), gtA),
		predicates.NewNot(predicates.NewOr(eqA, eqB)),
		predicates.NewOr(predicates.NewAnd(eqA, eqB), predicates.NewAnd(eqB, gtA)),
		// A NOT over an OR of an AND: normalizes to TWO conjuncts
		// ((NOT a OR NOT b) AND NOT c), which is what exercises the
		// andConjuncts split-and-rejoin rather than the single-conjunct
		// shortcut. An earlier fourth fixture here was already in CNF and the
		// inert-fixture guard below caught it.
		predicates.NewNot(predicates.NewOr(predicates.NewAnd(eqA, eqB), gtA)),
	} {
		normalized, changed := normalizeCNF(input, cnfSizeLimit)
		if !changed {
			t.Fatalf("fixture is inert: %s was already in CNF", input.Explain())
		}
		if len(andConjuncts(normalized)) < 2 && len(input.Children()) > 1 {
			t.Logf("note: %s normalized to a single conjunct", input.Explain())
		}

		// The rule's own round trip: split into conjuncts, re-conjunct, and ask
		// whether normalization would fire a second time.
		conjuncts := andConjuncts(normalized)
		if len(conjuncts) == 0 {
			t.Fatalf("andConjuncts emptied %s", normalized.Explain())
		}
		var reconjuncted predicates.QueryPredicate
		if len(conjuncts) == 1 {
			reconjuncted = conjuncts[0]
		} else {
			reconjuncted = &predicates.AndPredicate{SubPredicates: conjuncts}
		}
		if _, changedAgain := normalizeCNF(reconjuncted, cnfSizeLimit); changedAgain {
			t.Fatalf("the rule would fire again on its own output.\n"+
				"  input:        %s\n  normalized:   %s\n  reconjuncted: %s\n"+
				"NormalizePredicatesRule no longer keeps a set of expressions it has "+
				"already visited; it relies on this decline instead.",
				input.Explain(), normalized.Explain(), reconjuncted.Explain())
		}
	}
}
