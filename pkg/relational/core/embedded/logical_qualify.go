package embedded

import (
	recordlayer "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
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

// combineQualifyPred AND-combines the QUALIFY-clause predicate (the vector
// K-NN ROW_NUMBER() <= K DistanceRank comparison) with the WHERE predicate so
// both attach to the same LogicalFilter. Returns pred unchanged when there is
// no QUALIFY clause.
func combineQualifyPred(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
	pred predicates.QueryPredicate,
) predicates.QueryPredicate {
	qualPred, ok := buildQualifyPredicate(md, sq, cteScopes)
	if !ok {
		return pred
	}
	if pred == nil {
		return qualPred
	}
	return predicates.NewAnd(pred, qualPred)
}

// attachOrSynthesizeFilter attaches pred to the first LogicalFilter on op's
// unary spine; if there is none (a query with QUALIFY but no WHERE builds no
// filter), it synthesizes a LogicalFilter directly above the base LogicalScan
// — the same position a WHERE filter occupies, so the predicate is not
// silently dropped. Returns the (possibly new) plan root.
func attachOrSynthesizeFilter(op logical.LogicalOperator, pred predicates.QueryPredicate) logical.LogicalOperator {
	if upgradeFirstFilter(op, pred) {
		return op
	}
	if _, isScan := op.(*logical.LogicalScan); isScan {
		return logical.NewFilterWithPredicate(op, pred, "")
	}
	for cur := op; cur != nil; {
		child, ok := unaryInput(cur)
		if !ok {
			// Not a unary spine to a scan — wrap the whole plan as a fallback
			// so the predicate still filters (never dropped).
			return logical.NewFilterWithPredicate(op, pred, "")
		}
		if _, isScan := child.(*logical.LogicalScan); isScan {
			setUnaryInput(cur, logical.NewFilterWithPredicate(child, pred, ""))
			return op
		}
		cur = child
	}
	return op
}

// unaryInput returns the single child of a unary logical operator.
func unaryInput(op logical.LogicalOperator) (logical.LogicalOperator, bool) {
	switch o := op.(type) {
	case *logical.LogicalFilter:
		return o.Input, true
	case *logical.LogicalProject:
		return o.Input, true
	case *logical.LogicalSort:
		return o.Input, true
	case *logical.LogicalLimit:
		return o.Input, true
	case *logical.LogicalDistinct:
		return o.Input, true
	case *logical.LogicalAggregate:
		return o.Input, true
	default:
		return nil, false
	}
}

// setUnaryInput repoints the single child of a unary logical operator.
func setUnaryInput(op, child logical.LogicalOperator) {
	switch o := op.(type) {
	case *logical.LogicalFilter:
		o.Input = child
	case *logical.LogicalProject:
		o.Input = child
	case *logical.LogicalSort:
		o.Input = child
	case *logical.LogicalLimit:
		o.Input = child
	case *logical.LogicalDistinct:
		o.Input = child
	case *logical.LogicalAggregate:
		o.Input = child
	}
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
		// ROW_NUMBER() <op> K — the row-number value is the LHS.
		if rn, ok := pred.Operand.(*values.RowNumberValue); ok {
			if transformed, ok := predicates.TransformRowNumberDistanceRankMaybe(
				rn, pred.Comparison.Type, pred.Comparison.Operand); ok {
				return transformed
			}
		}
		// K <op> ROW_NUMBER() — the row-number value is the RHS; invert the
		// comparison so it reads ROW_NUMBER() <op'> K (Java tries both
		// orderings in transformComparisonMaybe).
		if rn, ok := pred.Comparison.Operand.(*values.RowNumberValue); ok && pred.Operand != nil {
			if inv, ok := invertRowNumberComparison(pred.Comparison.Type); ok {
				if transformed, ok := predicates.TransformRowNumberDistanceRankMaybe(
					rn, inv, pred.Operand); ok {
					return transformed
				}
			}
		}
		return pred
	default:
		return p
	}
}

// invertRowNumberComparison maps `K <op> ROW_NUMBER()` to the equivalent
// `ROW_NUMBER() <op'> K` comparison type. Only =, <, <=, >, >= invert to a
// supported DistanceRank form; others return false.
func invertRowNumberComparison(t predicates.ComparisonType) (predicates.ComparisonType, bool) {
	switch t {
	case predicates.ComparisonEquals:
		return predicates.ComparisonEquals, true
	case predicates.ComparisonGreaterThanEq: // K >= RN  ≡  RN <= K
		return predicates.ComparisonLessThanOrEq, true
	case predicates.ComparisonGreaterThan: // K > RN  ≡  RN < K
		return predicates.ComparisonLessThan, true
	default:
		return 0, false
	}
}
