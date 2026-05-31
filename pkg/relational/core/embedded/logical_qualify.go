package embedded

import (
	recordlayer "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// buildQualifyPredicate builds the QUALIFY-clause predicate for sq and applies
// the ROW_NUMBER→DistanceRank rewrite (Java's
// RowNumberValue.transformComparisonMaybe). For the vector K-NN surface the
// QUALIFY expression is `ROW_NUMBER() OVER (... ORDER BY <distance>(field, q))
// {<,<=,=} K`, which lowers to a DistanceRank comparison the vector index
// match candidate can satisfy.
//
// Returns (nil, false) when there is no QUALIFY clause or the predicate can't
// be built/resolved (caller leaves the plan unchanged).
func buildQualifyPredicate(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
) (predicates.QueryPredicate, bool) {
	if sq == nil || sq.qualifyExpr == nil {
		return nil, false
	}
	resolver := buildSelectScope(sq, md, cteScopes)
	if resolver == nil {
		return nil, false
	}
	pred, err := resolver.WalkPredicate(sq.qualifyExpr)
	if err != nil {
		return nil, false
	}
	pred = applyDistanceRankTransform(pred)
	pred = predicates.SimplifyPredicateValues(pred)
	return pred, true
}

// applyDistanceRankTransform walks a predicate tree and rewrites every
// comparison whose LHS is a ROW_NUMBER() window value into a DistanceRank
// comparison via predicates.TransformRowNumberDistanceRankMaybe. Non-matching
// nodes are returned unchanged. The recursion handles AND/OR/NOT so the
// "combine multiple HNSW searches with OR" QUALIFY shape transforms correctly.
func applyDistanceRankTransform(p predicates.QueryPredicate) predicates.QueryPredicate {
	switch pred := p.(type) {
	case *predicates.AndPredicate:
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			subs[i] = applyDistanceRankTransform(s)
		}
		return predicates.NewAnd(subs...)
	case *predicates.OrPredicate:
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			subs[i] = applyDistanceRankTransform(s)
		}
		return predicates.NewOr(subs...)
	case *predicates.NotPredicate:
		return predicates.NewNot(applyDistanceRankTransform(pred.Child))
	case *predicates.ComparisonPredicate:
		if rn, ok := pred.Operand.(*values.RowNumberValue); ok {
			if transformed, ok := predicates.TransformRowNumberDistanceRankMaybe(
				rn, pred.Comparison.Type, pred.Comparison.Operand); ok {
				return transformed
			}
		}
		return pred
	default:
		return p
	}
}
