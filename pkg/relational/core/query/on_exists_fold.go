package query

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/query/logical"
)

// An INNER join's ON clause and the WHERE above it are one predicate list:
// Java folds every inner-join ON conjunct into the WHERE of the single
// SelectExpression it builds for the FROM clause (QueryVisitor
// .visitSimpleTable conjoins inner-join expressions into the WHERE), so
// `a JOIN b ON p AND EXISTS(s) WHERE q` and `a JOIN b ON p WHERE q AND
// EXISTS(s)` are the same query and plan the same. The builder keeps an
// ON-clause EXISTS on the join (LogicalJoin.OnExistsSubqueries, its marker in
// OnPredicate) because an OUTER join's ON-EXISTS is NOT a WHERE-EXISTS; for an
// inner cluster the translator moves it into the WHERE before any route
// inspects the filter, so every consumer — the projection fold, the gathered
// cluster wrap, the binary existential flatten, the cluster gate — sees the
// WHERE-EXISTS shape it already plans, and a join carrying its own
// existential never has to ride the ≥3-quantifier partition machinery (the
// shape the gate poisons). Without the fold `… ON … AND EXISTS(d) WHERE
// EXISTS(f)` over three legs was refused as un-ordinalizable, and the
// projected-EXISTS gathered branch dropped the root's ON predicates outright.

// liftClusterOnExists returns j with every ON-clause EXISTS of its direct
// inner-join nesting cluster removed, plus the removed markers and
// subqueries, in cluster walk order. The cluster is exactly the one
// gatherInnerClusterLegs flattens once the lift has run: INNER nodes,
// stopping at a non-inner join, a lateral-unnest right child, or a
// non-join leg — an OUTER join's ON-EXISTS (rejected by the builder today)
// and anything below a leg boundary stay where they are. The tree is shared
// with the generator's guards, so every touched node is COPIED, never
// mutated; j itself comes back when nothing lifts.
func liftClusterOnExists(j *logical.LogicalJoin) (*logical.LogicalJoin, []predicates.QueryPredicate, []logical.ExistsSubquery) {
	var markers []predicates.QueryPredicate
	var subqueries []logical.ExistsSubquery
	var walk func(op logical.LogicalOperator) logical.LogicalOperator
	walk = func(op logical.LogicalOperator) logical.LogicalOperator {
		nj, isJoin := op.(*logical.LogicalJoin)
		if !isJoin || nj.Kind != logical.JoinInner {
			return op
		}
		if _, isUnnest := nj.Right.(*logical.LogicalUnnest); isUnnest {
			return op
		}
		left := walk(nj.Left)
		right := walk(nj.Right)
		if left == nj.Left && right == nj.Right && len(nj.OnExistsSubqueries) == 0 {
			return op
		}
		lifted := *nj
		lifted.Left = left
		lifted.Right = right
		if len(nj.OnExistsSubqueries) > 0 {
			onPred, isPred := nj.OnPredicate.(predicates.QueryPredicate)
			if !isPred {
				// The builder installs the marker and the subqueries together;
				// a join carrying subqueries with no predicate tree has no
				// marker to lift, so leave it for the join's own arm to refuse.
				return op
			}
			markers = append(markers, extractExistsPredicates(onPred)...)
			lifted.OnPredicate = onPredicateOrNil(splitNonExistsPredicates(onPred))
			lifted.OnExistsSubqueries = nil
			subqueries = append(subqueries, nj.OnExistsSubqueries...)
		}
		return &lifted
	}
	out := walk(j)
	if len(subqueries) == 0 {
		return j, nil, nil
	}
	return out.(*logical.LogicalJoin), markers, subqueries
}

// onPredicateOrNil is andOf for the join's untyped OnPredicate slot: a
// conjunction that emptied (the ON was only the EXISTS) must store an
// untyped nil, not a typed-nil QueryPredicate, so translateJoin sees "no ON
// predicate" rather than a predicate it cannot walk.
func onPredicateOrNil(preds []predicates.QueryPredicate) any {
	if p := andOf(preds); p != nil {
		return p
	}
	return nil
}

// foldInnerOnExistsIntoFilter returns f with the ON-clause EXISTS of the
// inner-join cluster directly under it moved into the WHERE: the markers join
// f.Predicate as conjuncts, the subqueries join f.ExistsSubqueries, and the
// cluster keeps only its non-EXISTS ON conjuncts. f itself comes back when
// there is nothing to lift. Applied where the translator first sees a filter
// (translateFilter, findExistsFilterUnderUnaryChain) so no route ever
// observes the ON-EXISTS spelling; translateProjection's bare-join lift is
// the same fold for a FROM with no WHERE at all.
func foldInnerOnExistsIntoFilter(f *logical.LogicalFilter) *logical.LogicalFilter {
	join, ok := f.Input.(*logical.LogicalJoin)
	if !ok || join.Kind != logical.JoinInner {
		return f
	}
	lifted, markers, subqueries := liftClusterOnExists(join)
	if len(subqueries) == 0 {
		return f
	}
	folded := *f
	folded.Input = lifted
	var conjuncts []predicates.QueryPredicate
	if f.Predicate != nil {
		if and, isAnd := f.Predicate.(*predicates.AndPredicate); isAnd {
			conjuncts = append(conjuncts, and.SubPredicates...)
		} else {
			conjuncts = append(conjuncts, f.Predicate)
		}
	}
	folded.Predicate = andOf(append(conjuncts, markers...))
	folded.ExistsSubqueries = append(
		append([]logical.ExistsSubquery(nil), f.ExistsSubqueries...),
		subqueries...,
	)
	return &folded
}
