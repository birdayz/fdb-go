package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// IntersectCompensations and UnionCompensations are FOLDS over what their doc
// comments call a monoid — "the identity element is ImpossibleCompensation",
// "the identity element is NoCompensation". A fold is only well defined if the
// binary operation is order-independent, and the compensations being folded are
// the legs of an intersection or union, which arrive in whatever order the
// planner enumerated them.
//
// If the fold is order-dependent, the SAME query yields different compensation
// depending on leg order, and compensation is what re-applies the predicates an
// index match did not cover. That is either a plan that varies run to run, or —
// where the orders disagree about IsNeeded — rows that are filtered in one
// ordering and not the other.
//
// The existing compensation tests are per-property unit tests on single values
// (IsNeeded, IsImpossible, CanBeDeferred, the constructors). Nothing exercises
// the combinators as an algebra.

// compensationCorpus is a spread of shapes the folds actually meet: the two
// identities, a plain residual compensation, one that is impossible-by-child,
// and one carrying a primary-key-distinct obligation — the last because
// intersectTwo has a dedicated arm for it (primaryKeyDistinctOnlyCompensation)
// that fires only when the OTHER side is not needed.
func compensationCorpus(t *testing.T) []Compensation {
	t.Helper()
	ref := expressions.InitialOf(nil)
	q1 := expressions.ForEachQuantifier(ref)
	q2 := expressions.ForEachQuantifier(ref)
	aliases := map[values.CorrelationIdentifier]struct{}{q1.GetAlias(): {}}

	plain := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q1}, []expressions.Quantifier{q2},
		aliases, NewResultCompensationFunction(true), EmptyGroupByMappings(),
	)
	impossibleChild := NewForMatchCompensation(
		true, ImpossibleCompensation, StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q1}, []expressions.Quantifier{q2},
		aliases, NewResultCompensationFunction(true), EmptyGroupByMappings(),
	)
	pkDistinct := NewForMatchCompensationWithPrimaryKeyDistinct(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{q1}, nil,
		aliases, NoResultCompensation(), EmptyGroupByMappings(), true,
	)

	return []Compensation{
		NoCompensation,
		ImpossibleCompensation,
		plain,
		impossibleChild,
		pkDistinct,
	}
}

// compensationShape is the observable behaviour of a Compensation — the five
// questions every caller asks it. Comparing shapes rather than pointers is the
// point: two foldings may legitimately build different objects, but they must
// not answer these differently, because these are what the planner acts on.
type compensationShape struct {
	needed, impossible, forFiltering, finalNeeded, deferrable bool
}

func shapeOf(c Compensation) compensationShape {
	return compensationShape{
		needed:       c.IsNeeded(),
		impossible:   c.IsImpossible(),
		forFiltering: c.IsNeededForFiltering(),
		finalNeeded:  c.IsFinalNeeded(),
		deferrable:   c.CanBeDeferred(),
	}
}

func TestCompensationFolds_AreOrderIndependent(t *testing.T) {
	t.Parallel()

	corpus := compensationCorpus(t)

	for _, fold := range []struct {
		name string
		fn   func([]Compensation) Compensation
	}{
		{"IntersectCompensations", IntersectCompensations},
		{"UnionCompensations", UnionCompensations},
	} {
		t.Run(fold.name+" is commutative", func(t *testing.T) {
			t.Parallel()
			pairs := 0
			for _, a := range corpus {
				for _, b := range corpus {
					pairs++
					ab := shapeOf(fold.fn([]Compensation{a, b}))
					ba := shapeOf(fold.fn([]Compensation{b, a}))
					if ab != ba {
						t.Errorf("%s(%v, %v) = %+v but reversed = %+v — the fold depends on "+
							"leg order, so the same query compensates differently depending on "+
							"how the planner enumerated its legs",
							fold.name, a, b, ab, ba)
					}
				}
			}
			if pairs != len(corpus)*len(corpus) {
				t.Fatalf("checked %d pairs, want %d", pairs, len(corpus)*len(corpus))
			}
		})

		// Associativity is asserted for UNION only. IntersectCompensations is
		// NOT associative today — see
		// TestIntersectCompensations_OrderDependenceReproducer below, which pins
		// the exact disagreement. Asserting it here would be a failing test; the
		// reproducer states the same fact in a form that stays green while the
		// defect is open and fails the moment it is fixed.
		if fold.name != "UnionCompensations" {
			continue
		}
		t.Run(fold.name+" is associative", func(t *testing.T) {
			t.Parallel()
			triples := 0
			for _, a := range corpus {
				for _, b := range corpus {
					for _, c := range corpus {
						triples++
						left := fold.fn([]Compensation{fold.fn([]Compensation{a, b}), c})
						right := fold.fn([]Compensation{a, fold.fn([]Compensation{b, c})})
						if shapeOf(left) != shapeOf(right) {
							t.Errorf("%s is not associative at (%v, %v, %v): %+v vs %+v",
								fold.name, a, b, c, shapeOf(left), shapeOf(right))
						}
					}
				}
			}
			if triples != len(corpus)*len(corpus)*len(corpus) {
				t.Fatalf("checked %d triples, want %d", triples, len(corpus)*len(corpus)*len(corpus))
			}
		})

		t.Run(fold.name+" folds permuted legs identically", func(t *testing.T) {
			t.Parallel()
			checked := 0
			for _, a := range corpus {
				for _, b := range corpus {
					for _, c := range corpus {
						want := shapeOf(fold.fn([]Compensation{a, b, c}))
						for _, perm := range [][]Compensation{
							{a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a},
						} {
							checked++
							if got := shapeOf(fold.fn(perm)); got != want {
								t.Errorf("%s over three legs depends on their ORDER: %+v vs %+v",
									fold.name, want, got)
							}
						}
					}
				}
			}
			if want := len(corpus) * len(corpus) * len(corpus) * 5; checked != want {
				t.Fatalf("checked %d permutations, want %d", checked, want)
			}
		})
	}
}

// TestIntersectCompensations_OrderDependenceReproducer pins an OPEN DEFECT.
//
// IntersectCompensations is a LEFT fold, so three legs fold as ((I·a)·b)·c and
// a reordering folds differently unless the operation is associative. It is
// commutative (checked above) and NOT associative, so the fold's result depends
// on the order the planner enumerated the intersection legs.
//
// Three orders of the same three legs give three incompatible answers:
//
//	[NoCompensation, plain, pkDistinct] -> needed, possible, not-for-filtering
//	[NoCompensation, pkDistinct, plain] -> needed, IMPOSSIBLE, for-filtering
//	[plain, pkDistinct, NoCompensation] -> NOT NEEDED
//
// "impossible" discards a usable intersection; "not needed" drops the
// primary-key-distinct obligation, which is a cardinality correction — losing
// it returns duplicate rows. intersectTwo's own comment states the invariant
// being violated: a leg that needs it "cannot lose it merely because the other
// leg has no filter or result residual".
//
// MEASURED SCOPE: over the five-shape corpus there are 96 disagreeing
// permutation pairs, and EVERY ONE involves requiresPrimaryKeyDistinct — zero
// triples disagree without it. That field is a Go-only extension;
// Compensation.java has no equivalent, so Java's fold is unaffected. The
// mechanism is the interaction: ForMatchCompensation.Intersect ORs the flag,
// then returns the bare ImpossibleCompensation singleton when the intersected
// child is impossible, discarding it — and intersectTwo treats Impossible as
// the intersection IDENTITY (Java reduces from impossibleCompensation), so that
// result is absorbed rather than poisoning the fold, and the obligation is gone.
//
// Reachable: requiresPrimaryKeyDistinct is set by PartialMatchImpl.GetCompensation,
// and intersector_primary_key.go folds one compensation per intersection leg.
//
// WHEN FIXED, DELETE THIS TEST and drop the UnionCompensations-only guard on
// the associativity and permutation subtests above, so both folds are held to
// the laws. This asserts the broken behaviour on purpose, so that the defect is
// visible in CI rather than latent, and so the fix cannot land silently.
func TestIntersectCompensations_OrderDependenceReproducer(t *testing.T) {
	t.Parallel()

	corpus := compensationCorpus(t)
	noComp, plain, pkDistinct := corpus[0], corpus[2], corpus[4]

	first := shapeOf(IntersectCompensations([]Compensation{noComp, plain, pkDistinct}))
	second := shapeOf(IntersectCompensations([]Compensation{noComp, pkDistinct, plain}))
	third := shapeOf(IntersectCompensations([]Compensation{plain, pkDistinct, noComp}))

	if first == second && second == third {
		t.Fatal("IntersectCompensations now folds these three legs identically in every " +
			"order. That is the FIX: delete this test and remove the UnionCompensations-only " +
			"guard on the associativity and permutation subtests above.")
	}

	// Pin the three specific answers, so a partial change that shuffles the
	// disagreement without removing it does not read as progress.
	if first.impossible || !first.needed || first.forFiltering {
		t.Errorf("[NoCompensation, plain, pkDistinct] = %+v; the reproducer expected a "+
			"needed, possible, non-filtering compensation", first)
	}
	if !second.impossible {
		t.Errorf("[NoCompensation, pkDistinct, plain] = %+v; the reproducer expected "+
			"IMPOSSIBLE — a usable intersection discarded", second)
	}
	if third.needed {
		t.Errorf("[plain, pkDistinct, NoCompensation] = %+v; the reproducer expected NOT "+
			"NEEDED — the primary-key-distinct obligation dropped, which returns duplicate rows",
			third)
	}
}

// TestCompensationFolds_ImpossiblePropagatesThroughUnionOnly pins the asymmetry
// between the two folds, which is the part a reader is most likely to get
// backwards.
//
// For a UNION, every leg's rows reach the output, so a leg whose residual
// cannot be applied poisons the whole union — impossible must propagate. For an
// INTERSECTION, a row must survive every leg, so compensating through any one
// leg suffices and impossible is the IDENTITY (Java folds intersect from
// impossibleCompensation for exactly that reason).
//
// These read as contradictory and are not; without them stated, "fix" the
// intersection to propagate impossible and every multi-leg intersection loses
// its compensation.
func TestCompensationFolds_ImpossiblePropagatesThroughUnionOnly(t *testing.T) {
	t.Parallel()

	corpus := compensationCorpus(t)
	for _, other := range corpus {
		union := UnionCompensations([]Compensation{ImpossibleCompensation, other})
		if !union.IsImpossible() && other.IsNeeded() {
			t.Errorf("union of impossible with %v is not impossible — a union leg whose "+
				"residual cannot be applied must poison the whole union, since its rows "+
				"reach the output uncompensated", other)
		}

		intersect := IntersectCompensations([]Compensation{ImpossibleCompensation, other})
		if shapeOf(intersect) != shapeOf(IntersectCompensations([]Compensation{other})) {
			t.Errorf("impossible is not the identity for intersection at %v — folding it in "+
				"changed the result, and Java reduces intersect FROM impossibleCompensation",
				other)
		}
	}
}
