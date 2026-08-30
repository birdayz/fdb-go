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

// compensationCorpus is the population every law below is a statement about, so
// its membership is the scope of those laws and each entry is here because some
// arm of the folds can only be reached by that shape. In order: the two
// identities; a plain residual compensation; one impossible-by-child; one
// carrying a primary-key-distinct obligation, for intersectTwo's dedicated arm
// (primaryKeyDistinctOnlyCompensation) that fires only when the OTHER side is
// not needed; one needing only its OWN result compensation; and one needing only
// its CHILD's.
//
// The last two were each added after a defect the corpus could not see, and each
// found one immediately. Growth is the expected direction here: a shape absent
// from this list is an arm the laws do not cover, whatever they otherwise
// report.
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
	// A leg needing ONLY a result compensation: predicates fully matched, no
	// unmatched ForEach, but the result value must be re-projected. This is the
	// shape whose ABSENCE let a regression through — every other entry here has
	// IsNeededForFiltering true or IsFinalNeeded false, so none could catch an
	// absorbing rule that swallowed the result function. It is also the
	// canonical intersection case rather than an exotic one: two covering
	// indexes intersected, where the pulled-up value is not the bare
	// QuantifiedObjectValue.
	resultOnly := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{q1}, nil,
		aliases, NewResultCompensationFunction(true), EmptyGroupByMappings(),
	)

	// A leg needing nothing AT THIS LEVEL whose CHILD needs a result
	// compensation. Distinct from resultOnly, and the distinction is the whole
	// point: IsNeededForFiltering recurses on the child's FILTERING need, so
	// this leg reports false for both IsNeededForFiltering and IsFinalNeeded
	// while still owing a re-projection one level down.
	//
	// It is not a contrived shape. NewForMatchCompensation's base-alias
	// invariant names it explicitly — "a compensation needed only for a CHILD's
	// final shape, where this level never builds a base quantifier" — as a case
	// it must NOT fail closed, and partial_match.go passes a child partial
	// match's compensation straight through as childCompensation.
	nestedResultOnly := NewForMatchCompensation(
		false, resultOnly, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{q1}, nil,
		aliases, NoResultCompensation(), EmptyGroupByMappings(),
	)

	// A TWO-LEVEL primary-key-distinct chain. One level is not enough to see the
	// absorbing arm's child slot: with a single level the nested position is
	// NoCompensation either way, so dropping it is invisible. This is the shape
	// that caught the arm rebuilding with a NoCompensation child and discarding
	// both operands' nested obligations.
	nestedPkDistinct := NewForMatchCompensationWithPrimaryKeyDistinct(
		false, pkDistinct, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{q1}, nil,
		aliases, NoResultCompensation(), EmptyGroupByMappings(), true,
	)

	return []Compensation{
		NoCompensation,
		ImpossibleCompensation,
		plain,
		impossibleChild,
		pkDistinct,
		resultOnly,
		nestedResultOnly,
		nestedPkDistinct,
	}
}

// primaryKeyDistinctChainDepth counts the primary-key-distinct obligations along
// a compensation chain. Depth, not presence: the defect this measures kept the
// TOP obligation and dropped the nested one, so a boolean cannot see it.
func primaryKeyDistinctChainDepth(c Compensation) int {
	depth := 0
	for {
		forMatch, ok := c.(*ForMatchCompensation)
		if !ok {
			return depth
		}
		if forMatch.requiresPrimaryKeyDistinct {
			depth++
		}
		c = forMatch.childCompensation
	}
}

// TestIntersectCompensations_KeepsNestedPrimaryKeyDistinctObligations pins the
// child slot of intersectTwo's absorbing arm.
//
// That arm reduces both operands to their cardinality obligations and unions
// them. It rebuilt with NoCompensation in the child position, which keeps the
// TOP obligation and silently discards every nested one — and a nested
// primary-key-distinct obligation is reachable: Apply recurses through the
// chain, which is why IsNeeded counts it (see IsNeeded's doc, and
// TestForMatchCompensation_NestedPrimaryKeyDistinct, which applies one end to
// end and requires the Unique to appear).
//
// Losing a primary-key-distinct obligation returns DUPLICATE ROWS. Nothing else
// in this file could see it: every other corpus shape is at most one level deep,
// where the nested position is NoCompensation either way.
func TestIntersectCompensations_KeepsNestedPrimaryKeyDistinctObligations(t *testing.T) {
	t.Parallel()

	corpus := compensationCorpus(t)
	deep := corpus[len(corpus)-1]
	if got := primaryKeyDistinctChainDepth(deep); got != 2 {
		t.Fatalf("the deep corpus shape has a chain of depth %d, want 2 — it no longer "+
			"nests and every check below is vacuous", got)
	}
	shallow := corpus[4]
	if got := primaryKeyDistinctChainDepth(shallow); got != 1 {
		t.Fatalf("the shallow corpus shape has depth %d, want 1", got)
	}

	// The fold must not shorten the chain, in either leg order.
	for _, legs := range [][]Compensation{{deep, shallow}, {shallow, deep}} {
		got := IntersectCompensations(legs)
		if got.IsImpossible() {
			t.Fatal("intersecting two cardinality-only legs must not be impossible")
		}
		if depth := primaryKeyDistinctChainDepth(got); depth < 2 {
			t.Errorf("folding a depth-2 obligation chain with a depth-1 one produced depth "+
				"%d — a nested primary-key-distinct obligation was dropped, and losing one "+
				"returns duplicate rows", depth)
		}
	}

	// Control: the arm still REDUCES. A leg carrying filtering residuals must
	// come back carrying only its obligations, or this test would pass equally
	// well against an arm that had stopped absorbing at all.
	reduced := IntersectCompensations([]Compensation{deep, corpus[2]})
	if reduced.IsNeededForFiltering() {
		t.Error("the absorbing arm must strip the other leg's filtering residual; it is " +
			"absorbing precisely because that leg selects exactly its rows")
	}
}

// TestForMatchCompensation_AChildsResultCompensationIsUnreachable is the
// negative result IsNeeded's narrowing rests on, pinned so that undoing the
// premise re-arms the bug.
//
// IsNeeded now recurses with the child's PRE-FINAL need rather than the child's
// full IsNeeded, dropping the child's result compensation from the answer. That
// is only sound because a child's result compensation can never be applied:
// ApplyFinal reads its OWN resultCompensationFn and does not recurse, and
// ApplyAllNeeded's only recursion into a child is through Apply, which is the
// pre-final half. Java is the same (Compensation.java:1051-1074).
//
// So this test does not assert the narrowing — it asserts the FACT. Teach
// ApplyFinal to recurse and this reddens, which is the signal that IsNeeded must
// widen again or the child's projection will be silently dropped.
func TestForMatchCompensation_AChildsResultCompensationIsUnreachable(t *testing.T) {
	t.Parallel()

	base := compensationNamedForEachQuantifier(t, "unreachable_final_base")
	child := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base}, nil, aliasesOf(base),
		ResultCompensationOfValue(mustCompensationQOV(t, base.GetAlias(), compensationRFC232RowType())),
		EmptyGroupByMappings(),
	)
	parent := NewForMatchCompensation(
		false, child, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)

	// The premise: the child really does have an APPLIABLE result compensation.
	// Without this the parent applies nothing for a trivial reason and every
	// check below passes for the wrong one.
	if !child.IsFinalNeeded() {
		t.Fatal("the child's result compensation is not needed — the fixture no longer " +
			"builds the shape this test is about")
	}
	scan := mustCompensationScan(t)
	appliedChild, ok := child.ApplyFinal(scan, nil)
	if !ok || appliedChild == scan {
		t.Fatalf("the child's own ApplyFinal must rewrite the expression (ok=%v, changed=%v); "+
			"if it cannot, the parent applying nothing says nothing about reachability",
			ok, appliedChild != scan)
	}

	// The fact. The parent owes nothing of its own, and the child's result
	// compensation is out of reach from here.
	appliedParent, ok := parent.ApplyAllNeeded(scan, nil)
	if !ok {
		t.Fatal("applying a parent that needs nothing must succeed")
	}
	if appliedParent != scan {
		t.Error("ApplyAllNeeded now REACHES a child's result compensation. That is the " +
			"premise IsNeeded's narrowing rests on: it stops counting a child's result " +
			"function precisely because nothing can apply it. Widen IsNeeded back, or the " +
			"child's projection is dropped while the fold reports nothing to do.")
	}
	if parent.IsNeeded() {
		t.Error("a compensation whose only need is an unreachable child result function " +
			"must report NOT needed — reporting needed is what made the intersection and " +
			"union folds order-dependent over this shape")
	}

	// Control, and it is the one that stops this becoming a licence to drop
	// every nested need: a child's PRIMARY-KEY-DISTINCT obligation IS reachable
	// through Apply, so it must still count.
	// TestForMatchCompensation_NestedPrimaryKeyDistinct applies one end to end;
	// here it is enough that the parent reports it.
	pkChild := NewForMatchCompensationWithPrimaryKeyDistinct(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base}, nil, aliasesOf(base),
		NoResultCompensation(), EmptyGroupByMappings(), true,
	)
	pkParent := NewForMatchCompensation(
		false, pkChild, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	if !pkParent.IsNeeded() {
		t.Error("a nested primary-key-distinct obligation IS applied through Apply and must " +
			"still make the parent needed — the narrowing drops the child's RESULT term and " +
			"nothing else")
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
	// The population every count below is a function of. Mutation figures for
	// this file are quoted nowhere in prose precisely because they move with
	// this number: a "151 violations" written against a five-shape corpus was
	// stale the moment a sixth was added, and had been wrong before that. Anyone
	// verifying a mutation reads the count from the run, against this size.
	t.Logf("compensation corpus: %d shapes — %d pairs, %d triples, %d permutation checks",
		len(corpus), len(corpus)*len(corpus), len(corpus)*len(corpus)*len(corpus),
		len(corpus)*len(corpus)*len(corpus)*5)

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
