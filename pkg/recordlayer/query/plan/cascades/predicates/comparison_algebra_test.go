package predicates

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// algebraOperands is the operand corpus the two algebraic laws below are
// checked over. It is deliberately weighted towards the values that break
// order-based reasoning rather than towards ordinary ones: NULL (which makes
// every binary comparison UNKNOWN), NaN (which is unordered against everything
// INCLUDING itself under IEEE 754, so `x < y` and `x >= y` can both be false at
// once), the two signed zeros (equal under IEEE but distinct bit patterns), and
// both infinities.
//
// Type-crossing pairs are in scope on purpose: a comparison between a string
// and an int has to reach the same verdict through both laws or the planner can
// rewrite one into the other and change the answer.
var algebraOperands = []any{
	nil,
	int64(-1), int64(0), int64(1), int64(5),
	math.NaN(),
	math.Inf(1), math.Inf(-1),
	float64(0), math.Copysign(0, -1),
	float64(1), float64(5.5),
	"", "abc", "abd",
	true, false,
	[]byte("abc"),
}

// triName renders a TriBool for a failure message. It is needed because
// TriBool is `*bool`, so %v prints a POINTER — a mutation of Negate() was
// diagnosed here against messages reading "< gave 0x105d7ad, > gave 0x105d7ad",
// which is the right verdict rendered unusably. Anything asserting on a TriBool
// wants this rather than %v.
func triName(t TriBool) string {
	switch t {
	case TriTrue:
		return "TRUE"
	case TriFalse:
		return "FALSE"
	case TriUnknown:
		return "UNKNOWN"
	}
	// A TriBool that is none of the three singletons is a fresh pointer, which
	// every `== TriTrue` comparison in the tree silently reads as not-true.
	return "NON-CANONICAL(fresh *bool, not one of the three singletons)"
}

// negateTriBool is Kleene negation: the ONLY correct reading of NOT over a
// three-valued logic. UNKNOWN negates to itself, which is what makes the law
// below non-trivial — a comparison pair that both answer FALSE on some operand
// is a violation even though neither answer is individually surprising.
func negateTriBool(t TriBool) TriBool {
	switch t {
	case TriTrue:
		return TriFalse
	case TriFalse:
		return TriTrue
	default:
		return TriUnknown
	}
}

// TestTriBoolSingletonsAreIntactAndDistinct guards the substrate the two laws
// below rest on. TriBool is `*bool`, and the three constants are package-level
// vars pointing at package-level bools — so they are MUTABLE. Anything that
// dereferences a TriBool it was handed and assigns through it (`*result = false`)
// corrupts the constant for the whole process, and every `== TriTrue` in the
// tree keeps comparing pointers happily while the value behind one of them is
// wrong. Nothing in the tree does that today; this is what notices if something
// starts.
//
// The distinctness half matters for a different reason: `==` on TriBool compares
// POINTERS, so the singleton discipline the type's doc comment describes
// ("matchers compare against these rather than constructing fresh pointers") is
// load-bearing and unenforced by the compiler. A fresh `&someBool` holding true
// is not TriTrue and never will be.
func TestTriBoolSingletonsAreIntactAndDistinct(t *testing.T) {
	t.Parallel()

	if TriTrue == nil || *TriTrue != true {
		t.Fatalf("TriTrue no longer points at true (%v) — something assigned through a "+
			"TriBool it was handed, and every three-valued verdict in this process is "+
			"now suspect", TriTrue)
	}
	if TriFalse == nil || *TriFalse != false {
		t.Fatalf("TriFalse no longer points at false (%v) — same corruption", TriFalse)
	}
	if TriUnknown != nil {
		t.Errorf("TriUnknown must be the nil pointer; the `result != nil && *result` idiom " +
			"the type's doc comment describes depends on it")
	}
	if TriTrue == TriFalse {
		t.Error("TriTrue and TriFalse must be distinct pointers")
	}

	// The fresh-pointer trap, stated as a test so it is a known property rather
	// than a surprise: a *bool holding true is NOT TriTrue.
	fresh := true
	if TriBool(&fresh) == TriTrue {
		t.Error("a fresh *bool compared equal to TriTrue — TriBool equality is no longer " +
			"pointer identity, which changes what every matcher in the tree means")
	}
	if got := triName(TriBool(&fresh)); got == "TRUE" {
		t.Errorf("triName reported %q for a non-canonical pointer; it must say so, because "+
			"that is exactly the case a reader needs named", got)
	}
}

// TestComparisonType_NegateComplementsEval is the law that makes Negate() safe
// to use: for every type Negate() claims to handle, evaluating the negated
// comparison must equal the Kleene negation of evaluating the original, for
// EVERY operand pair.
//
// Negate()'s result feeds a predicate rewrite — it is how a NOT gets pushed
// through a comparison — so a pair that disagrees on any operand is a rewrite
// that changes which rows come back. Nothing checked this: the existing
// TestComparisonType_Negate asserts the TABLE (that < maps to >=) and never
// evaluates either side, so it cannot see a mapping that is correct as algebra
// on totally-ordered values and wrong on the values that are not.
func TestComparisonType_NegateComplementsEval(t *testing.T) {
	t.Parallel()

	negatable, pairs := 0, 0
	for c := ComparisonEquals; c <= ComparisonDistanceRankLessThanOrEq; c++ {
		n, ok := c.Negate()
		if !ok {
			continue
		}
		negatable++
		for _, left := range algebraOperands {
			for _, right := range algebraOperands {
				got, gotErr := Comparison{Type: c, Operand: values.LiteralValue(right)}.EvalAgainst(left, right)
				neg, negErr := Comparison{Type: n, Operand: values.LiteralValue(right)}.EvalAgainst(left, right)
				// A comparison and its negation must be comparable on exactly
				// the same operands. If one errors and the other answers, the
				// rewrite changes a query from "type mismatch" into a verdict,
				// which is a divergence in its own right and not a pair to skip.
				if (gotErr != nil) != (negErr != nil) {
					t.Errorf("%s vs %s on (%#v, %#v): comparability disagrees — %s err=%v, "+
						"%s err=%v. A NOT pushed through this comparison turns an error into "+
						"an answer or the reverse.",
						c.Symbol(), n.Symbol(), left, right, c.Symbol(), gotErr, n.Symbol(), negErr)
					continue
				}
				if gotErr != nil {
					continue
				}
				pairs++
				if want := negateTriBool(got); neg != want {
					t.Errorf("%s vs %s on (%#v, %#v): %s gave %v, %s gave %v, want %v "+
						"(Kleene negation of %v). Negate() claims these are complements, so "+
						"a NOT pushed through this comparison changes the result here.",
						c.Symbol(), n.Symbol(), left, right, c.Symbol(), triName(got), n.Symbol(), triName(neg), triName(want), triName(got))
				}
			}
		}
	}

	// Population floor. Java's switch handles five types (=, <, <=, >, >=); a
	// zero here would mean Negate() stopped claiming anything and every pair
	// above went unchecked, which reads exactly like a clean pass.
	if negatable != 5 {
		t.Errorf("Negate() claims %d types, want 5 — the law above was checked over a "+
			"different population than the one this floor describes", negatable)
	}
	// The floor that matters more. Every pair reaches the assertion past a
	// `continue`, so a change making EvalAgainst error on most inputs would skip
	// the law entirely and report a clean pass over an empty set. 5 types x 19
	// operands squared is 1805 candidates and 1290 are actually compared —
	// the 515 skipped are cross-type pairs EvalAgainst declines on both sides.
	// Floored at 1200: under the measured 1290 so corpus edits do not trip it,
	// far above the collapse this is watching for.
	if pairs < 1200 {
		t.Fatalf("the negation law was checked on only %d operand pairs — EvalAgainst is "+
			"erroring on inputs it used to answer, so the law above is passing vacuously",
			pairs)
	}
}

// TestComparisonType_CommutePreservesEval is the matching law for Commute():
// `a OP b` must equal `b Commute(OP) a` on every operand pair. Commute() feeds
// index and primary-key matching, where a join predicate is read from either
// side, so a pair that disagrees lets the planner match a predicate to a key
// column it does not actually constrain.
func TestComparisonType_CommutePreservesEval(t *testing.T) {
	t.Parallel()

	commutable, pairs := 0, 0
	for c := ComparisonEquals; c <= ComparisonDistanceRankLessThanOrEq; c++ {
		flipped, ok := c.Commute()
		if !ok {
			continue
		}
		commutable++
		for _, left := range algebraOperands {
			for _, right := range algebraOperands {
				forward, fErr := Comparison{Type: c, Operand: values.LiteralValue(right)}.EvalAgainst(left, right)
				backward, bErr := Comparison{Type: flipped, Operand: values.LiteralValue(left)}.EvalAgainst(right, left)
				if (fErr != nil) != (bErr != nil) {
					t.Errorf("(%#v %s %#v) and (%#v %s %#v) disagree on comparability — "+
						"err=%v vs err=%v. Index matching reads a join predicate from either "+
						"side, so one direction erroring and the other answering is a real "+
						"difference, not a pair to skip.",
						left, c.Symbol(), right, right, flipped.Symbol(), left, fErr, bErr)
					continue
				}
				if fErr != nil {
					continue
				}
				pairs++
				if forward != backward {
					t.Errorf("(%#v %s %#v) = %v but (%#v %s %#v) = %v — Commute() claims these "+
						"are the same predicate, and index matching reads join predicates from "+
						"either side on that basis.",
						left, c.Symbol(), right, triName(forward), right, flipped.Symbol(), left, triName(backward))
				}
			}
		}
	}

	// Six: =, <>, <, <=, >, >=.
	if commutable != 6 {
		t.Errorf("Commute() claims %d types, want 6 — the law was checked over a different "+
			"population than this floor describes", commutable)
	}
	// 6 types x 19 operands squared is 2166 candidates, 1548 actually compared.
	// Floored at 1400 on the same reasoning as the negation law's floor.
	if pairs < 1400 {
		t.Fatalf("the commutation law was checked on only %d operand pairs — EvalAgainst is "+
			"erroring on inputs it used to answer, so the law above is passing vacuously",
			pairs)
	}
}

// TestComparisonEval_NaNIsOrderedNotIEEE pins the fact that makes the negation
// law above hold, rather than leaving it to fall out of a sweep whose failure
// would look like a corpus problem.
//
// Under IEEE 754 NaN is UNORDERED: `NaN < 5`, `NaN >= 5` and even `NaN == NaN`
// are all false. A comparison layer with those semantics CANNOT have a sound
// Negate(), because `<` and `>=` would both answer FALSE and the planner would
// be free to rewrite a NOT into a predicate that drops rows.
//
// This layer does not have those semantics: it orders NaN, so `<` and `>=`
// partition every pair and negation is total. That is the property Negate()
// rests on, and nothing else states it. If a change ever makes float comparison
// IEEE-faithful, this test fails FIRST and names the consequence — before
// TestComparisonType_NegateComplementsEval fails with a pair that looks
// incidental.
func TestComparisonEval_NaNIsOrderedNotIEEE(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	for _, tc := range []struct {
		name        string
		left, right any
	}{
		{"NaN against an int", nan, int64(5)},
		{"NaN against a float", nan, float64(5.5)},
		{"NaN against itself", nan, nan},
		{"NaN against +Inf", nan, math.Inf(1)},
		{"int against NaN", int64(5), nan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lt, ltErr := Comparison{Type: ComparisonLessThan, Operand: values.LiteralValue(tc.right)}.
				EvalAgainst(tc.left, tc.right)
			ge, geErr := Comparison{Type: ComparisonGreaterThanEq, Operand: values.LiteralValue(tc.right)}.
				EvalAgainst(tc.left, tc.right)
			if ltErr != nil || geErr != nil {
				t.Fatalf("NaN comparison errored (< : %v, >= : %v); if that becomes the "+
					"intended behaviour the negation law's skip path swallows this case "+
					"and stops testing it", ltErr, geErr)
			}
			// The whole point: exactly one of the two holds. Under IEEE both are
			// FALSE, which is the state that would break Negate().
			if lt == TriFalse && ge == TriFalse {
				t.Fatalf("both `< ` and `>=` answered FALSE for (%v, %v) — that is IEEE "+
					"unordered semantics, and it makes ComparisonType.Negate() UNSOUND: a "+
					"NOT pushed through `<` becomes `>=` and drops this row from both sides",
					tc.left, tc.right)
			}
			if lt == ge {
				t.Errorf("`<` and `>=` both answered %v for (%v, %v); they must partition",
					triName(lt), tc.left, tc.right)
			}
		})
	}
}
