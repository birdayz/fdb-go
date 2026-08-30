package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// The normal-form machinery, ported from Java's BooleanPredicateNormalizer as
// ONE implementation parameterized over the target form — which is what Java
// has, and what Go had two of (RFC-240).
//
// Java's own framing, kept because it is what makes the CNF and DNF code
// identical: "the logic to obtain these normal forms is identical except that
// the roles of Ors and Ands are reversed. In order to name identifiers in the
// code in a meaningful way, we talk about MAJOR and MINOR where CNF uses a
// major of And and a minor of Or and a DNF uses a major of Or and a minor of
// And" (BooleanPredicateNormalizer.java:60-66).
//
//	           major (outer)   minor (inner)
//	  DNF      Or              And
//	  CNF      And             Or
//
// ATOMICITY, and why there is one size function here where Java has two
// fields. Java's PredicateMetrics carries normalFormSize (what the size GUARD
// reads) and normalFormFullSize (what the COST MODEL reads via
// NormalizedResidualPredicateProperty). They differ only at nodes where
// QueryPredicate.isAtomic() is true (:323, :327, :330), which suppresses
// multiplying a subtree out. Go has no atomicity concept at all — there is no
// counterpart to withAtomicity — so the two metrics coincide and one function
// serves both readers. If atomicity ever lands, THIS is the function that has
// to split in two, and the two readers are normalFormSize's callers: the guard
// in normalizeInternal and the cost sites in designated_final.go and
// planning_cost_model.go.

// normalFormMode selects which connective is the outer one. It is an enum with
// a switch, as Java's Mode is (:72-112) — deliberately not a struct of
// closures, which would allocate on a path the cost model calls per plan
// comparison and would be a Go-only shape where Java has an enum.
type normalFormMode int

const (
	// normalFormDNF is a disjunction of conjunctions: major Or, minor And.
	normalFormDNF normalFormMode = iota
	// normalFormCNF is a conjunction of disjunctions: major And, minor Or.
	normalFormCNF
)

// isMajor reports whether p is this mode's OUTER connective — Java's
// Mode.instanceOfMajorClass.
func (m normalFormMode) isMajor(p predicates.QueryPredicate) bool {
	if m == normalFormCNF {
		_, ok := p.(*predicates.AndPredicate)
		return ok
	}
	_, ok := p.(*predicates.OrPredicate)
	return ok
}

// isMinor reports whether p is this mode's INNER connective — Java's
// Mode.instanceOfMinorClass.
func (m normalFormMode) isMinor(p predicates.QueryPredicate) bool {
	if m == normalFormCNF {
		_, ok := p.(*predicates.OrPredicate)
		return ok
	}
	_, ok := p.(*predicates.AndPredicate)
	return ok
}

// majorWithChildren builds the outer connective — Java's Mode.majorWithChildren.
func (m normalFormMode) majorWithChildren(children []predicates.QueryPredicate) predicates.QueryPredicate {
	if m == normalFormCNF {
		return buildAnd(children)
	}
	return buildOr(children)
}

// minorWithChildren builds the inner connective — Java's Mode.minorWithChildren.
func (m normalFormMode) minorWithChildren(children []predicates.QueryPredicate) predicates.QueryPredicate {
	if m == normalFormCNF {
		return buildOr(children)
	}
	return buildAnd(children)
}

// connectiveChildren returns the sub-predicates of an AND or OR, or nil for
// anything else. One accessor rather than a type switch repeated at each of the
// three walks below, which is how two of them would eventually disagree.
func connectiveChildren(p predicates.QueryPredicate) []predicates.QueryPredicate {
	switch q := p.(type) {
	case *predicates.AndPredicate:
		return q.SubPredicates
	case *predicates.OrPredicate:
		return q.SubPredicates
	default:
		return nil
	}
}

// normalFormVariable is Java's isNormalFormVariable (:490-492): anything that
// is not a boolean connective.
//
// Java's test is `isAtomic() || instanceof LeafQueryPredicate`. Go's structural
// test is the exact equivalent for every predicate Go can build — among the
// non-test QueryPredicate implementations, only And/Or/Not return a non-empty
// Children() — and TestNormalFormVariable_MatchesTheConcreteTypes pins that
// rather than leaving it asserted.
//
// It is MODE-INDEPENDENT, as Java's is: neither half of the question consults
// which connective is outer.
//
// One divergence, in the FAILURE direction and recorded here because it is
// silent: Java's isInNormalForm throws "unknown boolean expression" (:292) for
// a predicate that is neither variable, connective, nor NOT. Go answers
// "variable" instead. The sets coincide today so nothing differs; if a
// connective-shaped predicate is ever added without an arm here, Java would
// raise and Go will quietly treat it as an atom.
func normalFormVariable(p predicates.QueryPredicate) bool {
	switch p.(type) {
	case *predicates.AndPredicate, *predicates.OrPredicate, *predicates.NotPredicate:
		return false
	default:
		return true
	}
}

// normalFormVariableOrNot is Java's isNormalFormVariableOrNotPredicate
// (:494-504): a variable, or a NOT over a variable. Static in Java and
// mode-independent here for the same reason.
func normalFormVariableOrNot(p predicates.QueryPredicate) bool {
	if normalFormVariable(p) {
		return true
	}
	if n, ok := p.(*predicates.NotPredicate); ok {
		return normalFormVariable(n.Child)
	}
	return false
}

// isInNormalForm is Java's isInNormalForm (:255-293): a major of
// variables-or-NOTs and minors-of-variables-or-NOTs; a bare minor of
// variables-or-NOTs; a bare variable-or-NOT.
//
// A NOT over anything that is NOT a variable is not in normal form. That single
// line is the whole of RFC-240's read-side defect: Go's retired isInCNF
// treated every NotPredicate as a leaf, so `AND(NOT(AND(a,b)), c)` reported
// already-normalized and the planner's normalization rule declined a predicate
// Java rewrites to `(NOT a OR NOT b) AND c`.
func isInNormalForm(p predicates.QueryPredicate, mode normalFormMode) bool {
	if p == nil {
		return true
	}
	if normalFormVariableOrNot(p) {
		return true
	}
	if mode.isMajor(p) {
		for _, child := range connectiveChildren(p) {
			if normalFormVariableOrNot(child) {
				continue
			}
			if !mode.isMinor(child) {
				return false
			}
			for _, grandChild := range connectiveChildren(child) {
				if !normalFormVariableOrNot(grandChild) {
					return false
				}
			}
		}
		return true
	}
	if mode.isMinor(p) {
		for _, child := range connectiveChildren(p) {
			if !normalFormVariableOrNot(child) {
				return false
			}
		}
		return true
	}
	// A NOT over a non-variable — normalFormVariableOrNot already declined it.
	// Java reaches its throw here for anything else; see normalFormVariable.
	return false
}

// normalFormSize is Java's getMetrics (:319-334) — the number of MAJORS the
// normal form would have, carrying the negate flag.
//
// Under negation the major and minor arms SWAP, which is De Morgan expressed as
// a role swap rather than as a rewrite. That swap is the difference between
// this and the negate-blind size Go's cost model used to read: for CNF,
// `normalFormSize(NOT(a OR b), false, CNF)` is 2 — the negated Or is sized as a
// major, so its children SUM — where a walk that just recursed through the NOT
// answers 1.
//
// Overflow SATURATES rather than wrapping; see normalFormSizeSaturated for why
// testing for a negative product afterwards does not work.
func normalFormSize(p predicates.QueryPredicate, negate bool, mode normalFormMode) int64 {
	if p == nil {
		return 0
	}
	if n, ok := p.(*predicates.NotPredicate); ok {
		return normalFormSize(n.Child, !negate, mode)
	}
	children := connectiveChildren(p)
	switch {
	case mode.isMinor(p):
		if negate {
			return normalFormSizeSum(children, true, mode)
		}
		return normalFormSizeProduct(children, false, mode)
	case mode.isMajor(p):
		if negate {
			return normalFormSizeProduct(children, true, mode)
		}
		return normalFormSizeSum(children, false, mode)
	default:
		return 1
	}
}

// normalFormSizeSum is Java's getMetricsForMajor (:336-347): majors add.
func normalFormSizeSum(children []predicates.QueryPredicate, negate bool, mode normalFormMode) int64 {
	var sum int64
	for _, c := range children {
		sum = saturatingAddSize(sum, normalFormSize(c, negate, mode))
	}
	return sum
}

// normalFormSizeProduct is Java's getMetricsForMinor (:349-361): minors
// multiply, because the minor is where the cross product happens.
func normalFormSizeProduct(children []predicates.QueryPredicate, negate bool, mode normalFormMode) int64 {
	var product int64 = 1
	for _, c := range children {
		product = saturatingMulSize(product, normalFormSize(c, negate, mode))
	}
	return product
}

// toNormalized is Java's toNormalized (:370-384): the major-of-minor
// list-of-lists, carrying the negate flag through the same role swap
// normalFormSize uses. A NOT recurses with the flag flipped; a negated variable
// is wrapped in NOT.
//
// Java opens with `if (!predicate.isAtomic())`, which suppresses descent into
// an atomic subtree. Go has no atomicity, so the dispatch is unconditional —
// see this file's header for where that has to change if atomicity lands.
func toNormalized(p predicates.QueryPredicate, negate bool, mode normalFormMode) [][]predicates.QueryPredicate {
	if n, ok := p.(*predicates.NotPredicate); ok {
		return toNormalized(n.Child, !negate, mode)
	}
	children := connectiveChildren(p)
	switch {
	case mode.isMinor(p):
		if negate {
			return majorToNormalized(children, true, mode)
		}
		return minorToNormalized(children, false, mode)
	case mode.isMajor(p):
		if negate {
			return minorToNormalized(children, true, mode)
		}
		return majorToNormalized(children, false, mode)
	default:
		if negate {
			return [][]predicates.QueryPredicate{{predicates.NewNot(p)}}
		}
		return [][]predicates.QueryPredicate{{p}}
	}
}

// majorToNormalized is Java's majorToNormalized (:391-396): flatten every
// child's normalized majors.
func majorToNormalized(children []predicates.QueryPredicate, negate bool, mode normalFormMode) [][]predicates.QueryPredicate {
	var result [][]predicates.QueryPredicate
	for _, c := range children {
		result = append(result, toNormalized(c, negate, mode)...)
	}
	return result
}

// minorToNormalized is Java's minorToNormalized (:404-424): the cross product,
// combining one alternative from each child.
//
// The iteration order — each new child alternative (right) appended to every
// element of the cross product so far (left) — matches Java's flatMap, so the
// emitted clause order agrees. That is not cosmetic on the DNF path: its output
// becomes stored index predicate bytes.
func minorToNormalized(children []predicates.QueryPredicate, negate bool, mode normalFormMode) [][]predicates.QueryPredicate {
	cross := [][]predicates.QueryPredicate{{}}
	for _, child := range children {
		alternatives := toNormalized(child, negate, mode)
		newCross := make([][]predicates.QueryPredicate, 0, len(cross)*len(alternatives))
		for _, right := range alternatives {
			for _, left := range cross {
				combined := make([]predicates.QueryPredicate, 0, len(left)+len(right))
				combined = append(combined, left...)
				combined = append(combined, right...)
				newCross = append(newCross, combined)
			}
		}
		cross = newCross
	}
	return cross
}
