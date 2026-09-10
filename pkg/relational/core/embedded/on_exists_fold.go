package embedded

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// An INNER join's ON clause and the WHERE above it are one predicate list:
// Java folds every inner-join ON conjunct into the WHERE of the single
// SelectExpression it builds for a query block (QueryVisitor.visitSimpleTable
// conjoins inner-join expressions into the WHERE), so `a JOIN b ON p AND
// EXISTS(s) WHERE q` and `a JOIN b ON p WHERE q AND EXISTS(s)` are one query
// and plan the same. Go keeps a comparison ON conjunct on the join (the
// cluster machinery SARGs it there exactly as it would from the WHERE), but an
// EXISTS is an existential QUANTIFIER, and a join carrying its own quantifier
// is a shape the translator has to special-case everywhere — the cluster
// gate poisoned it N-way, the projection fold only saw it with no WHERE, the
// gathered branch dropped the root's ON predicates with it. So the builder
// folds it: upgradeJoinOnPredicates parks an ON's existential markers and
// subqueries on the join (LogicalJoin.OnExistsSubqueries), and
// foldInnerOnExistsIntoWhere — the last step of every query-block build —
// moves them into the block's WHERE filter, synthesizing the filter when the
// block has no WHERE. A plan leaving the builder never carries an
// OnExistsSubqueries; the translator asserts that rather than consuming it.

// foldInnerOnExistsIntoWhere moves every ON-clause EXISTS of the block's FROM
// cluster into the block's WHERE. The WHERE filter sits directly above the
// FROM join (visitWhere wraps the join before any shell is added), so the
// unary spine from op is followed to its bottom: a filter over an inner join
// takes the lift (markers AND-ed BEFORE the WHERE's own conjuncts, subqueries
// ahead of the WHERE's own — FROM precedes WHERE, as in Java's conjunction
// order); a bare inner join gets a filter synthesized in that position. A
// WHERE the builder could only keep as text (Predicate nil, PredicateText
// set) is left alone with the join unfolded: the translator refuses a text
// filter, so the block fails closed there with the more specific error the
// text fallback exists to preserve. Anything else on the spine's bottom
// carries no ON-EXISTS to lift.
func foldInnerOnExistsIntoWhere(op logical.LogicalOperator) (logical.LogicalOperator, error) {
	var parent logical.LogicalOperator
	cur := op
	for {
		if f, isFilter := cur.(*logical.LogicalFilter); isFilter {
			join, isJoin := f.Input.(*logical.LogicalJoin)
			if !isJoin || join.Kind != logical.JoinInner {
				return op, nil
			}
			lifted, markers, subqueries, err := liftClusterOnExists(join)
			if err != nil {
				return nil, err
			}
			if len(subqueries) == 0 {
				return op, nil
			}
			if f.Predicate == nil && f.PredicateText != "" {
				return op, nil
			}
			f.Input = lifted
			f.Predicate = andOfConjuncts(append(markers, conjunctsOf(f.Predicate)...))
			f.ExistsSubqueries = append(subqueries, f.ExistsSubqueries...)
			return op, nil
		}
		if join, isJoin := cur.(*logical.LogicalJoin); isJoin {
			if join.Kind != logical.JoinInner {
				return op, nil
			}
			lifted, markers, subqueries, err := liftClusterOnExists(join)
			if err != nil {
				return nil, err
			}
			if len(subqueries) == 0 {
				return op, nil
			}
			f := logical.NewFilterWithPredicate(lifted, andOfConjuncts(markers), "")
			f.ExistsSubqueries = subqueries
			if parent == nil {
				return f, nil
			}
			setUnaryInput(parent, f)
			return op, nil
		}
		next, isUnary := unaryInput(cur)
		if !isUnary {
			return op, nil
		}
		parent, cur = cur, next
	}
}

// liftClusterOnExists returns j with every ON-clause EXISTS of its direct
// inner-join nesting cluster removed, plus the removed markers and subqueries
// in FROM order (a nested join's ON before the ON of the join above it). The
// cluster is exactly the one the translator's gatherInnerClusterLegs flattens
// once the lift has run: INNER nodes, stopping at a non-inner join, a lateral-unnest
// right child, or a non-join leg — an OUTER join's ON-EXISTS (rejected by
// upgradeJoinOnPredicates today) and anything below a leg boundary stay where
// they are. Each join is lifted whole or not at all, and the boundary is
// decided BEFORE recursing, so a child's lift can never be collected while its
// parent is left in place. Every touched node is COPIED; j itself comes back
// when nothing lifts.
//
// A join whose ON-EXISTS cannot be lifted is an error, never a silent
// boundary: an EXISTS under an OR has no conjunct to lift (its marker would
// stay under the OR while its quantifier moved — the dangling-existential
// shape), and an EXISTS whose subquery has no marker in conjunct position
// (nested in a scalar expression) would leave a quantifier nothing reads.
// Both are refused with the WHERE's own wording for the same shapes.
func liftClusterOnExists(j *logical.LogicalJoin) (*logical.LogicalJoin, []predicates.QueryPredicate, []logical.ExistsSubquery, error) {
	var markers []predicates.QueryPredicate
	var subqueries []logical.ExistsSubquery
	var walk func(op logical.LogicalOperator) (logical.LogicalOperator, error)
	walk = func(op logical.LogicalOperator) (logical.LogicalOperator, error) {
		nj, isJoin := op.(*logical.LogicalJoin)
		if !isJoin || nj.Kind != logical.JoinInner {
			return op, nil
		}
		if _, isUnnest := nj.Right.(*logical.LogicalUnnest); isUnnest {
			return op, nil
		}
		var ownMarkers []predicates.QueryPredicate
		if len(nj.OnExistsSubqueries) > 0 {
			onPred, isPred := nj.OnPredicate.(predicates.QueryPredicate)
			if !isPred {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS in a JOIN ON clause without a predicate tree cannot be folded into the WHERE")
			}
			if existsUnderDisjunction(onPred) {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"EXISTS within an OR (disjunction) is not supported")
			}
			ownMarkers = extractExistsMarkers(onPred)
			if len(ownMarkers) != len(nj.OnExistsSubqueries) {
				return nil, api.NewError(api.ErrCodeUnsupportedQuery,
					"EXISTS nested in a scalar expression is not yet supported")
			}
		}
		left, err := walk(nj.Left)
		if err != nil {
			return nil, err
		}
		right, err := walk(nj.Right)
		if err != nil {
			return nil, err
		}
		if left == nj.Left && right == nj.Right && len(nj.OnExistsSubqueries) == 0 {
			return op, nil
		}
		lifted := *nj
		lifted.Left = left
		lifted.Right = right
		if len(nj.OnExistsSubqueries) > 0 {
			onPred := nj.OnPredicate.(predicates.QueryPredicate)
			markers = append(markers, ownMarkers...)
			subqueries = append(subqueries, nj.OnExistsSubqueries...)
			lifted.OnPredicate = onPredicateOrNil(splitNonExistsConjuncts(onPred))
			lifted.OnExistsSubqueries = nil
		}
		return &lifted, nil
	}
	out, err := walk(j)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(subqueries) == 0 {
		return j, nil, nil, nil
	}
	return out.(*logical.LogicalJoin), markers, subqueries, nil
}

// extractExistsMarkers returns the EXISTS / NOT EXISTS markers in conjunct
// position of pred, in order: the bare ExistentialValuePredicate, NOT over
// one, and the members of an AND, recursively. A marker below any other node
// (OR, CASE, a comparison) is not in conjunct position and is not returned.
func extractExistsMarkers(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var out []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			out = append(out, extractExistsMarkers(sub)...)
		}
		return out
	}
	return nil
}

// splitNonExistsConjuncts returns pred's conjuncts that are not EXISTS
// markers, in order, an AND flattened.
func splitNonExistsConjuncts(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return nil
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return nil
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var out []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			out = append(out, splitNonExistsConjuncts(sub)...)
		}
		return out
	}
	return []predicates.QueryPredicate{pred}
}

// conjunctsOf returns pred as a conjunct list: nil for nil, an AND's members,
// otherwise the predicate itself.
func conjunctsOf(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		return and.SubPredicates
	}
	return []predicates.QueryPredicate{pred}
}

// andOfConjuncts is the inverse of conjunctsOf: nil for none, the lone
// conjunct for one, an AND for several.
func andOfConjuncts(preds []predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return nil
	case 1:
		return preds[0]
	default:
		return predicates.NewAnd(preds...)
	}
}

// onPredicateOrNil is andOfConjuncts for the join's untyped OnPredicate slot:
// an ON that emptied (it was only the EXISTS) must store an untyped nil, not
// a typed-nil QueryPredicate, so the translator sees "no ON predicate" rather
// than a predicate it cannot walk.
func onPredicateOrNil(preds []predicates.QueryPredicate) any {
	if p := andOfConjuncts(preds); p != nil {
		return p
	}
	return nil
}
