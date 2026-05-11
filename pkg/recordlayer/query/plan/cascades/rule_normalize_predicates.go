package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// NormalizePredicatesRule converts the predicates of a SelectExpression
// into conjunctive normal form (CNF) — AND of ORs. The normalised
// predicates are set as the new predicate list on a freshly yielded
// SelectExpression (quantifiers are rebuilt with new aliases pointing
// at the same References).
//
// Ports Java's NormalizePredicatesRule which is the precursor to
// PredicateToLogicalUnionRule. The CNF form makes each OR clause
// independently matchable by index-pushdown rules, enabling OR-to-
// UNION transformations.
//
// Algorithm:
//  1. AND all predicates together.
//  2. Run CNF normalization (distribute OR over AND).
//  3. If the result is already in CNF, bail (nothing to do).
//  4. Extract the top-level AND conjuncts as the new predicate list.
//  5. Yield a new SelectExpression with rebuilt quantifiers.
//
// The normalizer respects a complexity threshold (cnfSizeLimit) to
// avoid exponential blow-up from deeply nested OR/AND trees. If the
// normalised form would exceed the limit, the rule produces no yield.
//
// Mirrors Java's BooleanPredicateNormalizer in CNF mode with a
// default size limit of 1,000,000.
type NormalizePredicatesRule struct {
	matcher matching.BindingMatcher
}

func NewNormalizePredicatesRule() *NormalizePredicatesRule {
	return &NormalizePredicatesRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("normalize_predicates"),
	}
}

func (r *NormalizePredicatesRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *NormalizePredicatesRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	preds := sel.GetPredicates()
	if len(preds) == 0 {
		return
	}

	for _, q := range sel.GetQuantifiers() {
		if q.Kind() == expressions.QuantifierExistential {
			return
		}
	}

	// Step 1: AND all predicates together.
	var conjuncted predicates.QueryPredicate
	if len(preds) == 1 {
		conjuncted = preds[0]
	} else {
		conjuncted = &predicates.AndPredicate{SubPredicates: preds}
	}

	// Step 2: Normalise to CNF.
	cnf, changed := normalizeCNF(conjuncted, cnfSizeLimit)
	if !changed {
		return
	}

	// Step 3: Extract conjuncts from the CNF result.
	cnfConjuncts := andConjuncts(cnf)

	// Step 4: Yield with original quantifiers (not rebuilt). Java
	// rebuilds via toBuilder().build() but Go's Quantifier is a value
	// type — rebuilding creates new values that break downstream
	// pattern matching for Existential quantifiers. Reusing the
	// originals preserves structural identity.
	call.Yield(expressions.NewSelectExpression(
		sel.GetResultValue(),
		sel.GetQuantifiers(),
		cnfConjuncts,
	))
}

// cnfSizeLimit is the maximum number of terms in the outer AND of the
// CNF before the normalizer gives up. Mirrors Java's
// BooleanPredicateNormalizer.DEFAULT_SIZE_LIMIT.
const cnfSizeLimit = 1_000_000

// normalizeCNF converts a predicate to conjunctive normal form (CNF).
// Returns (result, true) if a transformation was applied, or
// (original, false) if the predicate is already in CNF or the
// normalised form would exceed sizeLimit.
//
// CNF: the outer connective is AND, each child is either a leaf or an
// OR of leaves. The transformation distributes OR over AND:
//
//	A OR (B AND C) -> (A OR B) AND (A OR C)
//
// The implementation uses the list-of-lists intermediate form from
// Java's BooleanPredicateNormalizer: a list (to be AND'd) of lists
// (to be OR'd).
func normalizeCNF(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	if isInCNF(pred) {
		return pred, false
	}
	size := cnfSize(pred)
	if size > int64(sizeLimit) {
		return pred, false
	}

	normalized := toCNFNormalized(pred)
	absorbed := applyAbsorption(normalized)

	// Reconstruct: AND of ORs.
	andChildren := make([]predicates.QueryPredicate, 0, len(absorbed))
	for _, orList := range absorbed {
		andChildren = append(andChildren, buildOr(orList))
	}
	result := buildAnd(andChildren)
	return result, true
}

// isInCNF checks whether a predicate is already in conjunctive normal
// form. CNF means: the top is an AND (or a leaf), each AND-child is
// an OR of leaves (or a leaf). "Leaf" means not AND or OR.
func isInCNF(pred predicates.QueryPredicate) bool {
	if isLeafPredicate(pred) {
		return true
	}
	switch p := pred.(type) {
	case *predicates.AndPredicate:
		for _, child := range p.SubPredicates {
			if isLeafPredicate(child) {
				continue
			}
			or, ok := child.(*predicates.OrPredicate)
			if !ok {
				return false
			}
			for _, orChild := range or.SubPredicates {
				if !isLeafPredicate(orChild) {
					return false
				}
			}
		}
		return true
	case *predicates.OrPredicate:
		for _, child := range p.SubPredicates {
			if !isLeafPredicate(child) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// isLeafPredicate returns true for predicates that are not AND/OR.
func isLeafPredicate(pred predicates.QueryPredicate) bool {
	switch pred.(type) {
	case *predicates.AndPredicate, *predicates.OrPredicate:
		return false
	default:
		return true
	}
}

// cnfSize estimates the size of the CNF form. Size is the number of
// terms in the outer AND. For an OR of N AND-children with sizes
// s1..sN, the CNF size is s1 * s2 * ... * sN (cross-product).
// For an AND, it's the sum of children's sizes.
func cnfSize(pred predicates.QueryPredicate) int64 {
	switch p := pred.(type) {
	case *predicates.AndPredicate:
		var sum int64
		for _, child := range p.SubPredicates {
			sum += cnfSize(child)
			if sum < 0 { // overflow
				return int64(cnfSizeLimit) + 1
			}
		}
		return sum
	case *predicates.OrPredicate:
		var product int64 = 1
		for _, child := range p.SubPredicates {
			product *= cnfSize(child)
			if product < 0 { // overflow
				return int64(cnfSizeLimit) + 1
			}
		}
		return product
	case *predicates.NotPredicate:
		return cnfSize(p.Child)
	default:
		return 1
	}
}

// toCNFNormalized converts a predicate to the list-of-lists form
// for CNF: list (AND) of lists (OR) of leaf predicates.
func toCNFNormalized(pred predicates.QueryPredicate) [][]predicates.QueryPredicate {
	switch p := pred.(type) {
	case *predicates.AndPredicate:
		// AND flattens: AND(A, B) -> concat of normalised children.
		var result [][]predicates.QueryPredicate
		for _, child := range p.SubPredicates {
			result = append(result, toCNFNormalized(child)...)
		}
		return result
	case *predicates.OrPredicate:
		// OR distributes: cross-product of children's CNF forms.
		return orToCNF(p.SubPredicates)
	default:
		// Leaf or NOT: single-element outer, single-element inner.
		return [][]predicates.QueryPredicate{{pred}}
	}
}

// orToCNF computes the cross-product distribution of OR-children's
// CNF forms. OR(A AND B, C) with CNF(A AND B) = [[A],[B]] and
// CNF(C) = [[C]] produces [[A,C],[B,C]].
func orToCNF(children []predicates.QueryPredicate) [][]predicates.QueryPredicate {
	// Start with a single empty clause.
	cross := [][]predicates.QueryPredicate{{}}

	for _, child := range children {
		childNorm := toCNFNormalized(child)
		var newCross [][]predicates.QueryPredicate
		for _, right := range childNorm {
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

// applyAbsorption implements the absorption law on the CNF
// list-of-lists: removes clauses that are supersets of other clauses.
// Also deduplicates atoms within each OR-clause.
//
// Mirrors Java's BooleanPredicateNormalizer.applyAbsorptionLaw().
func applyAbsorption(clauses [][]predicates.QueryPredicate) [][]predicates.QueryPredicate {
	if len(clauses) < 2 {
		return clauses
	}

	// Step 1: Deduplicate atoms within each clause.
	deduped := make([][]predicates.QueryPredicate, len(clauses))
	for i, clause := range clauses {
		deduped[i] = dedupPredicateSlice(clause)
	}

	// Step 2: Remove clauses absorbed by shorter/equal clauses.
	// A clause C_i is absorbed if some C_j (j != i) is a subset of C_i.
	result := make([][]predicates.QueryPredicate, 0, len(deduped))
	for i, ci := range deduped {
		absorbed := false
		for j, cj := range deduped {
			if i == j {
				continue
			}
			// ci is absorbed if cj is a subset of ci (and ci is strictly larger,
			// or same size with i > j to break ties).
			if len(ci) > len(cj) || (len(ci) == len(cj) && i > j) {
				if predicateSliceContainsAll(ci, cj) {
					absorbed = true
					break
				}
			}
		}
		if !absorbed {
			result = append(result, ci)
		}
	}
	return result
}

// dedupPredicateSlice removes duplicate predicates from a slice,
// preserving first-occurrence order.
func dedupPredicateSlice(in []predicates.QueryPredicate) []predicates.QueryPredicate {
	out := make([]predicates.QueryPredicate, 0, len(in))
	for _, p := range in {
		dup := false
		for _, o := range out {
			if predicates.PredicateEquals(p, o) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// predicateSliceContainsAll returns true if `haystack` contains every
// predicate in `needles` (by structural equality).
func predicateSliceContainsAll(haystack, needles []predicates.QueryPredicate) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if predicates.PredicateEquals(n, h) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// buildAnd constructs an AND predicate from a list, collapsing
// single-element lists and empty lists.
func buildAnd(preds []predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		return preds[0]
	default:
		return &predicates.AndPredicate{SubPredicates: preds}
	}
}

// buildOr constructs an OR predicate from a list, collapsing
// single-element lists and empty lists.
func buildOr(preds []predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriFalse)
	case 1:
		return preds[0]
	default:
		return &predicates.OrPredicate{SubPredicates: preds}
	}
}

// NormalizeDNF converts a predicate to disjunctive normal form (DNF).
// Returns (result, true) if a transformation was applied, or
// (original, false) if the predicate is already in DNF or the
// normalised form would exceed sizeLimit.
//
// DNF: the outer connective is OR, each child is either a leaf or an
// AND of leaves. The transformation distributes AND over OR:
//
//	A AND (B OR C) -> (A AND B) OR (A AND C)
//
// Mirrors Java's BooleanPredicateNormalizer with Mode.DNF.
func NormalizeDNF(pred predicates.QueryPredicate, sizeLimit int) (predicates.QueryPredicate, bool) {
	if isInDNF(pred) {
		return pred, false
	}
	size := dnfSize(pred)
	if size > int64(sizeLimit) {
		return pred, false
	}

	normalized := toDNFNormalized(pred)
	absorbed := applyAbsorption(normalized)

	orChildren := make([]predicates.QueryPredicate, 0, len(absorbed))
	for _, andList := range absorbed {
		orChildren = append(orChildren, buildAnd(andList))
	}
	result := buildOr(orChildren)
	return result, true
}

func isInDNF(pred predicates.QueryPredicate) bool {
	if isLeafPredicate(pred) {
		return true
	}
	switch p := pred.(type) {
	case *predicates.OrPredicate:
		for _, child := range p.SubPredicates {
			if isLeafPredicate(child) {
				continue
			}
			and, ok := child.(*predicates.AndPredicate)
			if !ok {
				return false
			}
			for _, andChild := range and.SubPredicates {
				if !isLeafPredicate(andChild) {
					return false
				}
			}
		}
		return true
	case *predicates.AndPredicate:
		for _, child := range p.SubPredicates {
			if !isLeafPredicate(child) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// dnfSize estimates the size of the DNF form. For an AND of OR-children
// with sizes s1..sN, the DNF size is s1 * s2 * ... * sN (cross-product).
// For an OR, it's the sum of children's sizes.
func dnfSize(pred predicates.QueryPredicate) int64 {
	switch p := pred.(type) {
	case *predicates.OrPredicate:
		var sum int64
		for _, child := range p.SubPredicates {
			sum += dnfSize(child)
			if sum < 0 {
				return int64(cnfSizeLimit) + 1
			}
		}
		return sum
	case *predicates.AndPredicate:
		var product int64 = 1
		for _, child := range p.SubPredicates {
			product *= dnfSize(child)
			if product < 0 {
				return int64(cnfSizeLimit) + 1
			}
		}
		return product
	case *predicates.NotPredicate:
		return dnfSize(p.Child)
	default:
		return 1
	}
}

func toDNFNormalized(pred predicates.QueryPredicate) [][]predicates.QueryPredicate {
	switch p := pred.(type) {
	case *predicates.OrPredicate:
		var result [][]predicates.QueryPredicate
		for _, child := range p.SubPredicates {
			result = append(result, toDNFNormalized(child)...)
		}
		return result
	case *predicates.AndPredicate:
		return andToDNF(p.SubPredicates)
	default:
		return [][]predicates.QueryPredicate{{pred}}
	}
}

func andToDNF(children []predicates.QueryPredicate) [][]predicates.QueryPredicate {
	cross := [][]predicates.QueryPredicate{{}}
	for _, child := range children {
		childNorm := toDNFNormalized(child)
		var newCross [][]predicates.QueryPredicate
		for _, right := range childNorm {
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

// andConjuncts extracts the top-level AND children from a predicate.
// If the predicate is an AndPredicate, returns its children. If it's
// a tautology (TRUE constant), returns empty. Otherwise wraps it.
// Mirrors Java's AndPredicate.conjuncts().
func andConjuncts(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if cp, ok := pred.(*predicates.ConstantPredicate); ok && cp.Value == predicates.TriTrue {
		return nil
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		return and.SubPredicates
	}
	return []predicates.QueryPredicate{pred}
}

var _ ExpressionRule = (*NormalizePredicatesRule)(nil)
