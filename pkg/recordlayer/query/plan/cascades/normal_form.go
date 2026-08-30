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
