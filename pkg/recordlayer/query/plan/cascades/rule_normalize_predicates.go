package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// NormalizePredicatesRule converts the predicates of a SelectExpression
// into conjunctive normal form (CNF) — AND of ORs. The normalised
// predicates are set as the new predicate list on a freshly yielded
// SelectExpression carrying the SAME quantifier values, passed through
// verbatim (OnMatch's sel.GetQuantifiers()).
//
// That verbatim pass-through is load-bearing, not incidental, and this comment
// used to say the opposite — "quantifiers are rebuilt with new aliases". It is
// what makes Reference.Insert's pointer-identity fast path hit on a re-fire,
// which is half of why this rule needs no memory of what it has already seen.
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
// TERMINATION is algebraic, not bookkept. The rule used to carry an
// identity-keyed set of SelectExpressions it had already fired on; Java has no
// counterpart, because its termination falls out of isInNormalForm accepting
// the rule's OWN output, so a re-fire returns Optional.empty(). RFC-240's
// strict normal-form test gives Go the same property —
// TestNormalizeCNF_IsStableOnItsOwnOutput is that assertion, and it is Java's
// (BooleanPredicateNormalizerTest.java:262-267, "Normalized form should be
// stable").
//
// Declining is only half of why the set was unnecessary; the other half is that
// a re-fire must COST nothing rather than accumulate a duplicate member. Two
// mechanisms, in order:
//
//   - Reference.Insert dedups on EqualsWithoutChildren plus sameChildReferences
//     (expressions/reference.go:654), with a SemanticEquals fallback below it
//     for the fresh-Reference case. The pointer-identity tier is the one that
//     hits here, and only because OnMatch passes sel.GetQuantifiers() verbatim.
//   - A deduped yield then schedules NOTHING: unified_tasks.go:492's
//     `if !inserted[i] { continue }` skips the follow-on exploration task. That
//     is where the cost actually goes to zero; the dedup alone would still
//     re-walk.
//
// Together they are the Go analogue of Java's memo dedup, which is what the set
// was standing in for.
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

	// Step 4: Yield with original quantifiers and metadata preserved.
	result, err := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		sel.GetQuantifiers(),
		cnfConjuncts,
		sel.GetSourceAliases(),
		sel.GetJoinType(),
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(result)
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
			// ci is absorbed if cj is a subset of ci and ci is strictly
			// larger — or the two are the same size, where the tie-break
			// decides which of two IDENTICAL clauses survives (equal size
			// plus containsAll means equal sets, so this arm can mean
			// nothing else).
			//
			// The tie-break is `i < j`, Java's (:461), and it is not
			// arbitrary: it decides the surviving clause's POSITION, and
			// position is the emitted child order. On `[A, X, A]`, `i < j`
			// drops the first A and yields `[X, A]`; `i > j` drops the last
			// and yields `[A, X]`. Go had the second.
			if len(ci) > len(cj) || (len(ci) == len(cj) && i < j) {
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
