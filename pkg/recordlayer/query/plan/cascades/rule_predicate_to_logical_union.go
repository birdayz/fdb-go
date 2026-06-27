package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// DefaultMaxNumConjuncts is the maximum number of OR-predicate conjuncts
// that PredicateToLogicalUnionRule will attempt to convert to DNF. Beyond
// this limit, the combinatorial explosion of DNF terms is too expensive.
// Java's default is 9 (510 combinations).
const DefaultMaxNumConjuncts = 9

// PredicateToLogicalUnionRule transforms a SelectExpression whose predicates
// (in CNF after NormalizePredicatesRule) contain OR terms into a
// DISTINCT(UNION(SELECT leg1, SELECT leg2, ...)) structure. Each union
// leg corresponds to one DNF term, enabling each leg to use a different
// index for evaluation.
//
// The core transformation:
//
//	SELECT WHERE A AND B AND (C1 OR C2) AND (D1 OR D2)
//
// becomes:
//
//	DISTINCT(UNION(
//	  UNIQUE(SELECT WHERE A AND B AND C1 AND D1),
//	  UNIQUE(SELECT WHERE A AND B AND C1 AND D2),
//	  UNIQUE(SELECT WHERE A AND B AND C2 AND D1),
//	  UNIQUE(SELECT WHERE A AND B AND C2 AND D2),
//	))
//
// Each UNIQUE deduplicates per-leg by primary key; DISTINCT deduplicates
// across legs. The fixed predicates (A, B) are repeated in every leg.
//
// Guards:
//   - Only fires on SelectExpressions with exactly 1 ForEach quantifier.
//   - Skips SelectExpressions with Existential quantifiers.
//   - Requires at least one non-leaf, non-atomic OR predicate.
//   - Respects DefaultMaxNumConjuncts to avoid combinatorial explosion.
//
// Convergence: the output is Distinct(Union(...)), not a SelectExpression,
// so the rule cannot re-fire on its own output.
//
// Ports Java's PredicateToLogicalUnionRule (a match-partition rule) as a
// Go ExpressionRule operating on SelectExpressions. The Go planner fires
// expression rules in the EXPLORE phase rather than as match-partition
// triggers — architecturally equivalent when combined with
// NormalizePredicatesRule.
type PredicateToLogicalUnionRule struct {
	matcher matching.BindingMatcher
}

// NewPredicateToLogicalUnionRule constructs the rule.
func NewPredicateToLogicalUnionRule() *PredicateToLogicalUnionRule {
	return &PredicateToLogicalUnionRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("predicate_to_logical_union"),
	}
}

func (r *PredicateToLogicalUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PredicateToLogicalUnionRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	preds := sel.GetPredicates()
	if len(preds) == 0 {
		return
	}

	quantifiers := sel.GetQuantifiers()

	// Guard: exactly 1 ForEach quantifier and no Existential quantifiers.
	// Mirrors Java's check on ownedForEachAliases.size() != 1.
	var forEachQuantifiers []expressions.Quantifier
	for _, q := range quantifiers {
		switch q.Kind() {
		case expressions.QuantifierForEach:
			forEachQuantifiers = append(forEachQuantifiers, q)
		case expressions.QuantifierExistential:
			// Java doesn't outright reject existential quantifiers —
			// it subsets them per leg. But the TODO in Java says
			// "for now we only allow exactly one for-each quantifier",
			// and the existential subsetting is complex. Match Java's
			// effective behaviour: skip if existentials are present.
			return
		}
	}
	if len(forEachQuantifiers) != 1 {
		return
	}

	// Partition predicates into OR predicates (non-trivial) and fixed predicates (leaves).
	// Mirrors Java's nonTrivialPredicates filter: keep predicates that are
	// not atomic and not LeafQueryPredicate. In Go, a non-trivial predicate
	// is an OrPredicate (after CNF normalization, the top-level predicates
	// are either leaves or ORs).
	var orPredicates []*predicates.OrPredicate
	var fixedPredicates []predicates.QueryPredicate
	for _, p := range preds {
		if op, ok := p.(*predicates.OrPredicate); ok {
			orPredicates = append(orPredicates, op)
		} else {
			fixedPredicates = append(fixedPredicates, p)
		}
	}

	// Need at least one OR predicate to split.
	if len(orPredicates) == 0 {
		return
	}

	// Guard: respect the max conjuncts limit to avoid combinatorial explosion.
	if len(orPredicates) > DefaultMaxNumConjuncts {
		return
	}

	// Convert the OR predicates to DNF. If there's one OR predicate,
	// its children are the DNF terms directly. If there are multiple,
	// AND them together and convert the result to DNF — the cross-product
	// of all OR children.
	var dnfTerms []predicates.QueryPredicate
	if len(orPredicates) == 1 {
		// Single OR: each child is a DNF term.
		dnfTerms = orPredicates[0].SubPredicates
	} else {
		// Multiple ORs: compute the cross-product (DNF of the conjunction).
		dnfTerms = orsToDNFTerms(orPredicates)
	}

	// If DNF produced nothing useful, bail.
	if len(dnfTerms) == 0 {
		return
	}

	// Build union legs.
	onlyForEachQ := forEachQuantifiers[0]
	lowerResultValue := onlyForEachQ.GetFlowedObjectValue()

	// Check if the result value is "simple" — a QuantifiedObjectValue
	// referencing the single ForEach alias. If so, the outer wrapping
	// SelectExpression is unnecessary.
	resultValue := sel.GetResultValue()
	isSimpleResultValue := false
	if qov, ok := resultValue.(*values.QuantifiedObjectValue); ok {
		if qov.Correlation == onlyForEachQ.GetAlias() {
			isSimpleResultValue = true
		}
	}

	var legRefs []*expressions.Reference
	for _, dnfTerm := range dnfTerms {
		// Build the predicate list for this leg: fixed predicates + the DNF term.
		legPreds := make([]predicates.QueryPredicate, 0, len(fixedPredicates)+1)
		legPreds = append(legPreds, fixedPredicates...)
		legPreds = append(legPreds, dnfTerm)

		// Rebuild the ForEach quantifier pointing at the same inner Reference.
		legForEach := expressions.NamedForEachQuantifier(
			onlyForEachQ.GetAlias(),
			onlyForEachQ.GetRangesOver(),
		)

		legSelect := expressions.NewSelectExpression(
			lowerResultValue,
			[]expressions.Quantifier{legForEach},
			legPreds,
		)
		legSelectRef := call.MemoizeExpression(legSelect)

		legUnique := expressions.NewLogicalUniqueExpression(
			expressions.ForEachQuantifier(legSelectRef),
		)
		legUniqueRef := call.MemoizeExpression(legUnique)

		legRefs = append(legRefs, legUniqueRef)
	}

	// Build the union over all legs.
	unionQuantifiers := make([]expressions.Quantifier, len(legRefs))
	for i, ref := range legRefs {
		unionQuantifiers[i] = expressions.ForEachQuantifier(ref)
	}
	unionExpr := expressions.NewLogicalUnionExpression(unionQuantifiers)
	unionRef := call.MemoizeExpression(unionExpr)

	// Wrap in LogicalDistinctExpression (dedup across legs).
	distinctExpr := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef),
	)

	if isSimpleResultValue {
		// Simple result value: Distinct is the final expression.
		call.Yield(distinctExpr)
	} else {
		// Non-simple result value: wrap in an outer SelectExpression
		// that projects the original result value.
		distinctRef := call.MemoizeExpression(distinctExpr)

		// Reuse the ForEach alias from the original quantifier so the
		// result value's correlation references still resolve.
		outerQuantifier := expressions.NamedForEachQuantifier(
			onlyForEachQ.GetAlias(),
			distinctRef,
		)
		outerSelect := expressions.NewSelectExpression(
			resultValue,
			[]expressions.Quantifier{outerQuantifier},
			nil, // no predicates on the outer select
		)
		call.Yield(outerSelect)
	}
}

// orsToDNFTerms computes the cross-product (DNF) of multiple OR
// predicates. Given ORs [(A|B), (C|D)], produces the terms:
// [A AND C, A AND D, B AND C, B AND D].
//
// Each term is either a single predicate (if only one factor) or an
// AndPredicate of the combined children. This is the dual of orToCNF
// in rule_normalize_predicates.go.
func orsToDNFTerms(ors []*predicates.OrPredicate) []predicates.QueryPredicate {
	// Start with a single empty conjunction.
	cross := [][]predicates.QueryPredicate{{}}

	for _, or := range ors {
		var newCross [][]predicates.QueryPredicate
		for _, existing := range cross {
			for _, child := range or.SubPredicates {
				combined := make([]predicates.QueryPredicate, 0, len(existing)+1)
				combined = append(combined, existing...)
				combined = append(combined, child)
				newCross = append(newCross, combined)
			}
		}
		cross = newCross
	}

	// Convert each conjunction list into a single predicate.
	terms := make([]predicates.QueryPredicate, 0, len(cross))
	for _, conjunction := range cross {
		terms = append(terms, buildAnd(conjunction))
	}
	return terms
}

var _ ExpressionRule = (*PredicateToLogicalUnionRule)(nil)
