package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// NormalizerDefaultSizeLimit is BooleanPredicateNormalizer.DEFAULT_SIZE_LIMIT
// (BooleanPredicateNormalizer.java:117) — the limit the default DNF/CNF
// normalizer instances carry.
const NormalizerDefaultSizeLimit = 1_000_000

// NormalizeDNFWithoutSimplification is the Go port of
// BooleanPredicateNormalizer.getDefaultInstanceForDnf().normalize(pred, false)
// (BooleanPredicateNormalizer.java:227-230): pure distribution into
// disjunctive normal form with NOT pushed down over AND/OR via De Morgan
// (toNormalized's negate flag, :370-384), but — unlike NormalizeDNF above,
// which matches normalizeAndSimplify (:209-215) — WITHOUT absorption and
// WITHOUT dedup. The distinction is wire-visible for the index-predicate
// producer (MaterializedViewIndexGenerator.java:675 calls normalize, not
// normalizeAndSimplify): an absorbed clause would change the stored
// RecordMetaDataProto.Predicate bytes relative to Java.
//
// Returns (pred, false) when the predicate is already in normal form
// (isInNormalForm, :255-293) or the normalized size would exceed sizeLimit
// (shouldNormalize with failIfTooLarge=false, :295-303) — Java's
// Optional.empty(), which the caller resolves with orElse(conjunction).
func NormalizeDNFWithoutSimplification(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	if isInDNFStrict(pred) {
		return pred, false
	}
	if dnfSizeNegated(pred, false) > int64(sizeLimit) {
		return pred, false
	}
	majorOfMinor := toDNFNegated(pred, false)
	orChildren := make([]predicates.QueryPredicate, 0, len(majorOfMinor))
	for _, andList := range majorOfMinor {
		orChildren = append(orChildren, buildAnd(andList))
	}
	return buildOr(orChildren), true
}

// dnfVariable is Java's isNormalFormVariable
// (BooleanPredicateNormalizer.java:490-492): anything that is not a boolean
// connective. Go's predicates carry no atomic marking, so the structural
// test is exact.
func dnfVariable(p predicates.QueryPredicate) bool {
	switch p.(type) {
	case *predicates.AndPredicate, *predicates.OrPredicate, *predicates.NotPredicate:
		return false
	default:
		return true
	}
}

// dnfVariableOrNot is isNormalFormVariableOrNotPredicate (:494-504): a
// variable, or a NOT over a variable.
func dnfVariableOrNot(p predicates.QueryPredicate) bool {
	if dnfVariable(p) {
		return true
	}
	if n, ok := p.(*predicates.NotPredicate); ok {
		return dnfVariable(n.Child)
	}
	return false
}

// isInDNFStrict is isInNormalForm for Mode.DNF (:255-293): an OR (major) of
// variables-or-NOTs or ANDs (minor) of variables-or-NOTs; a bare AND of
// variables-or-NOTs; a variable-or-NOT. A NOT over a connective is NOT in
// normal form (:287-289) — unlike isInDNF above, which treats every NOT as a
// leaf.
func isInDNFStrict(p predicates.QueryPredicate) bool {
	if dnfVariableOrNot(p) {
		return true
	}
	switch q := p.(type) {
	case *predicates.OrPredicate:
		for _, child := range q.SubPredicates {
			if dnfVariableOrNot(child) {
				continue
			}
			and, ok := child.(*predicates.AndPredicate)
			if !ok {
				return false
			}
			for _, andChild := range and.SubPredicates {
				if !dnfVariableOrNot(andChild) {
					return false
				}
			}
		}
		return true
	case *predicates.AndPredicate:
		for _, child := range q.SubPredicates {
			if !dnfVariableOrNot(child) {
				return false
			}
		}
		return true
	default:
		// A NOT over a connective (dnfVariableOrNot already said no).
		return false
	}
}

// normalFormSizeSaturated is the value a normal-form size computation reports
// when it overflows int64. Java uses Math.addExact/Math.multiplyExact
// (BooleanPredicateNormalizer.java:342-360) and shouldNormalize catches the
// ArithmeticException (:296-301) with "our computation caused an integer
// overflow so the normal form would _definitely_ be too big" — i.e. overflow
// means reject, never admit. Go has no checked arithmetic, so the size
// computation saturates here instead; MaxInt64 exceeds every int size limit a
// caller can pass, so every `size > limit` guard rejects.
//
// Testing the accumulator for a negative result AFTER the multiply does not
// work: an AND of 32 four-way ORs has a true DNF size of 4^32 == 2^64, which
// wraps to exactly zero — smaller than every limit — so the guard admits the
// predicate and the distribution then materializes the full cross product.
const normalFormSizeSaturated = math.MaxInt64

// saturatingAddSize adds two non-negative normal-form sizes, clamping at
// normalFormSizeSaturated instead of wrapping.
func saturatingAddSize(a, b int64) int64 {
	if a > normalFormSizeSaturated-b {
		return normalFormSizeSaturated
	}
	return a + b
}

// saturatingMulSize multiplies two non-negative normal-form sizes, clamping at
// normalFormSizeSaturated instead of wrapping.
func saturatingMulSize(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > normalFormSizeSaturated/b {
		return normalFormSizeSaturated
	}
	return a * b
}

// dnfSizeNegated is getMetrics' normal-form size with the negate flag
// (:319-334): under negation the connectives swap roles (De Morgan), so a
// negated AND counts like an OR and vice versa.
func dnfSizeNegated(p predicates.QueryPredicate, negate bool) int64 {
	switch q := p.(type) {
	case *predicates.AndPredicate:
		if negate {
			return dnfSumNegated(q.SubPredicates, true)
		}
		return dnfProductNegated(q.SubPredicates, false)
	case *predicates.OrPredicate:
		if negate {
			return dnfProductNegated(q.SubPredicates, true)
		}
		return dnfSumNegated(q.SubPredicates, false)
	case *predicates.NotPredicate:
		return dnfSizeNegated(q.Child, !negate)
	default:
		return 1
	}
}

func dnfSumNegated(children []predicates.QueryPredicate, negate bool) int64 {
	var sum int64
	for _, c := range children {
		sum = saturatingAddSize(sum, dnfSizeNegated(c, negate))
	}
	return sum
}

func dnfProductNegated(children []predicates.QueryPredicate, negate bool) int64 {
	var product int64 = 1
	for _, c := range children {
		product = saturatingMulSize(product, dnfSizeNegated(c, negate))
	}
	return product
}

// toDNFNegated is toNormalized for Mode.DNF (:370-384): OR is the major, AND
// the minor; a NOT recurses with the negation flipped; a negated variable
// wraps in NOT. Returns the major-of-minor list-of-lists.
func toDNFNegated(p predicates.QueryPredicate, negate bool) [][]predicates.QueryPredicate {
	switch q := p.(type) {
	case *predicates.AndPredicate: // minor
		if negate {
			return dnfMajorNormalized(q.SubPredicates, true)
		}
		return dnfMinorNormalized(q.SubPredicates, false)
	case *predicates.OrPredicate: // major
		if negate {
			return dnfMinorNormalized(q.SubPredicates, true)
		}
		return dnfMajorNormalized(q.SubPredicates, false)
	case *predicates.NotPredicate:
		return toDNFNegated(q.Child, !negate)
	default:
		if negate {
			return [][]predicates.QueryPredicate{{predicates.NewNot(p)}}
		}
		return [][]predicates.QueryPredicate{{p}}
	}
}

// dnfMajorNormalized is majorToNormalized (:391-396): flatten all children's
// normalized majors.
func dnfMajorNormalized(children []predicates.QueryPredicate, negate bool) [][]predicates.QueryPredicate {
	var result [][]predicates.QueryPredicate
	for _, c := range children {
		result = append(result, toDNFNegated(c, negate)...)
	}
	return result
}

// dnfMinorNormalized is minorToNormalized (:404-424): the cross product,
// combining one alternative from each child. The iteration order — each new
// child alternative (right) appended to every element of the cross product so
// far (left) — matches Java's flatMap so the emitted clause order, and hence
// the serialized predicate bytes, agree.
func dnfMinorNormalized(children []predicates.QueryPredicate, negate bool) [][]predicates.QueryPredicate {
	cross := [][]predicates.QueryPredicate{{}}
	for _, child := range children {
		alternatives := toDNFNegated(child, negate)
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
