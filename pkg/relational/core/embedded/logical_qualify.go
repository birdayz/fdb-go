package embedded

import (
	recordlayer "fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// buildQualifyPredicate builds the QUALIFY-clause predicate for sq and applies
// the ROW_NUMBER→DistanceRank rewrite (Java's
// RowNumberValue.transformComparisonMaybe). For the vector K-NN surface the
// QUALIFY expression is `ROW_NUMBER() OVER (... ORDER BY <distance>(field, q))
// {<,<=,=} K`, which lowers to a DistanceRank comparison the vector index
// match candidate can satisfy.
//
// Three states, so an unsupported QUALIFY is never silently dropped (codex
// Finding 1):
//   - (nil, nil)  — no QUALIFY clause; caller leaves the plan unchanged.
//   - (nil, err)  — QUALIFY present but unbuildable: the window expression failed
//     to resolve (e.g. DESC ordering / RANK(), rejected by the walker), OR a
//     ROW_NUMBER() comparison survived the DistanceRank transform un-lowered
//     (an unsupported window shape, e.g. `= K` or a non-distance ORDER BY).
//     The query MUST fail rather than execute with the QUALIFY filter ignored.
//   - (pred, nil) — the QUALIFY predicate (the vector K-NN DistanceRank).
func buildQualifyPredicate(
	md *recordlayer.RecordMetaData,
	schemaName string,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
) (predicates.QueryPredicate, error) {
	if sq == nil || sq.qualifyExpr == nil {
		return nil, nil
	}
	resolver := buildSelectScope(sq, md, schemaName, cteScopes)
	if resolver == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"QUALIFY clause could not be resolved against the query scope")
	}
	pred, err := resolver.WalkPredicate(sq.qualifyExpr)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"unsupported QUALIFY clause: %v", err)
	}
	pred = applyDistanceRankTransform(pred)
	pred = predicates.SimplifyPredicateValues(pred)
	// A ROW_NUMBER() that survives the transform un-lowered is an unsupported
	// window shape (only the vector K-NN ROW_NUMBER() {<,<=} K form lowers to a
	// DistanceRank). Java has no other window-function surface — fail loud rather
	// than attach a predicate that can't be evaluated and would drop every row.
	if predicateHasUnloweredRowNumber(pred) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"unsupported window function in QUALIFY: only ROW_NUMBER() OVER "+
				"(... ORDER BY <distance>(vec, q)) {<,<=} K is supported")
	}
	return pred, nil
}

// combineQualifyPred AND-combines the QUALIFY-clause predicate (the vector
// K-NN ROW_NUMBER() <= K DistanceRank comparison) with the WHERE predicate so
// both attach to the same LogicalFilter. Returns pred unchanged when there is
// no QUALIFY clause; propagates a build error for an unsupported QUALIFY.
func combineQualifyPred(
	md *recordlayer.RecordMetaData,
	schemaName string,
	sq *selectQuery,
	cteScopes map[string]semantic.ScopeSource,
	pred predicates.QueryPredicate,
) (predicates.QueryPredicate, error) {
	qualPred, err := buildQualifyPredicate(md, schemaName, sq, cteScopes)
	if err != nil {
		return nil, err
	}
	if qualPred == nil {
		return pred, nil
	}
	if pred == nil {
		return qualPred, nil
	}
	return predicates.NewAnd(pred, qualPred), nil
}

// predicateHasUnloweredRowNumber reports whether the predicate tree still
// contains a raw RowNumberValue after the DistanceRank transform — i.e. an
// unsupported window shape that did not lower to a vector scan.
func predicateHasUnloweredRowNumber(p predicates.QueryPredicate) bool {
	found := false
	predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
		if found {
			return false
		}
		cp, ok := node.(*predicates.ComparisonPredicate)
		if !ok {
			return true
		}
		for _, v := range []values.Value{cp.Operand, cp.Comparison.Operand} {
			if v == nil {
				continue
			}
			values.WalkValue(v, func(n values.Value) bool {
				if _, isRN := n.(*values.RowNumberValue); isRN {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
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

// wrapGlobalRankVectorLimit implements the RFC-156 Phase B canonical lowering
// for a GLOBAL-rank vector K-NN query (a QUALIFY ROW_NUMBER() <op> K with NO
// PARTITION BY — the un-partitioned §1 shape). It inserts a LogicalLimit(K)
// DIRECTLY above the LogicalFilter that carries the (just-attached) DistanceRank
// predicate, producing the property-driven shape
//
//	Limit(K) → Filter(residual) → VectorIndexScan(distance-ordered)
//
// where a residual WHERE is present, and `Limit(K) → VectorIndexScan` otherwise
// (which SinkLimitIntoVectorScanRule folds back into the scan's self-limiting
// top-k). The distance ranking stays as the index-only DistanceRank predicate so
// the vector match candidate consumes it (the ordering is intrinsic to the
// scan); the rank LIMIT becomes a plain row Limit over the order-preserving
// filtered stream — exactly "the K nearest rows that satisfy the predicate".
//
// PARTITIONED ranks (non-empty PARTITION BY) are left UNCHANGED: their rank is
// per-partition and a single global Limit cannot express it, so the legacy
// self-limiting per-partition fan-out (RFC-046) is retained.
func wrapGlobalRankVectorLimit(op logical.LogicalOperator, qualPred predicates.QueryPredicate) logical.LogicalOperator {
	if op == nil || qualPred == nil {
		return op
	}
	limit, limitValue, ok := globalRankVectorLimit(qualPred)
	if !ok {
		return op
	}
	makeLimit := func(child logical.LogicalOperator) logical.LogicalOperator {
		if limitValue != nil {
			return logical.NewRuntimeLimit(child, limitValue, 0)
		}
		return logical.NewLimit(child, limit, 0)
	}
	// Insert the Limit directly above the FIRST LogicalFilter on the unary spine
	// (where the QUALIFY predicate was just attached). Keeps any SELECT
	// projection above the Limit (Project → Limit → Filter → Scan).
	var parent logical.LogicalOperator
	for cur := op; cur != nil; {
		if _, isFilter := cur.(*logical.LogicalFilter); isFilter {
			wrapped := makeLimit(cur)
			if parent == nil {
				return wrapped
			}
			setUnaryInput(parent, wrapped)
			return op
		}
		child, isUnary := unaryInput(cur)
		if !isUnary {
			break
		}
		parent = cur
		cur = child
	}
	// No filter on the spine (should not happen for a QUALIFY query) — wrap the
	// whole plan so the rank limit is never silently dropped.
	return makeLimit(op)
}

// globalRankVectorLimit lowers a GLOBAL-rank (empty PARTITION BY) vector
// DistanceRank comparison into a faithful row Limit above the (distance-ordered)
// vector scan. It ALWAYS produces a Limit for an un-partitioned rank so the
// ordered scan is never left to stream unbounded (codex correctness blocker):
//
//   - literal positive K (K for rank<=K, K-1 for rank<K) → static limit, ok.
//   - adjusted-zero / negative cap (e.g. ROW_NUMBER() < 1) → static Limit(0),
//     ok: SinkLimitIntoVectorScanRule declines on limit<=0 so the Limit(0) stays
//     above the ordered scan and yields the correct EMPTY result.
//   - NON-LITERAL / parameterized K (`<= ?`) → a RUNTIME limit Value (the second
//     return), ok: the K Value itself for rank<=K, or a K-1 arithmetic Value for
//     rank<K. The executor evaluates it against the bound parameters; the ordered
//     scan + Filter + Limit(?) compose the true K nearest MATCHING rows. (For a
//     self-limiting fold the runtime cap can't be sunk, so SinkLimit declines and
//     the Limit(?) stays above the ordered scan — still bounded, never unbounded.)
//
// A PARTITION BY (non-empty partitioning) is per-partition and a single global
// Limit cannot express it, so it declines (ok=false) and keeps the legacy
// self-limiting per-partition fan-out (RFC-046).
//
// Returns (staticLimit, runtimeLimitValue, ok). Exactly one of staticLimit /
// runtimeLimitValue is meaningful: runtimeLimitValue!=nil ⇒ runtime cap.
func globalRankVectorLimit(p predicates.QueryPredicate) (int64, values.Value, bool) {
	var limit int64
	var limitValue values.Value
	found := false
	predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
		if found {
			return false
		}
		cp, ok := node.(*predicates.ComparisonPredicate)
		if !ok {
			return true
		}
		partitioning, isDist := distanceRowNumberPartitioning(cp.Operand)
		if !isDist || len(partitioning) != 0 {
			return true // not a distance rank, or a PARTITION BY (per-partition) rank
		}
		var adjust int64
		switch cp.Comparison.Type {
		case predicates.ComparisonDistanceRankLessThanOrEq:
			adjust = 0
		case predicates.ComparisonDistanceRankLessThan:
			adjust = -1
		default:
			return true // EQUALS is rejected upstream; nothing else is a top-k rank
		}
		operand := cp.Comparison.Operand
		if operand == nil {
			return true
		}
		kv, err := operand.Evaluate(nil)
		if err == nil && kv != nil {
			k, ok := asInt64Literal(kv)
			if !ok {
				return true // a non-integer literal K is not a top-k rank
			}
			// Literal K. An adjusted-zero / negative cap clamps to Limit(0) (EMPTY)
			// rather than declining — declining would leave the ordered scan
			// unbounded.
			adjusted := k + adjust
			if adjusted < 0 {
				adjusted = 0
			}
			limit = adjusted
			found = true
			return false
		}
		// Non-literal (parameterized / runtime) K — emit a RUNTIME cap: K for
		// rank<=K, K-1 (a runtime arithmetic Value) for rank<K. Evaluate(nil)
		// returns (nil, nil) for an unbound parameter or an error for a value that
		// cannot fold at plan time — both mean "not a plan-time literal", so the
		// cap must be carried as a Value and evaluated against the bound params at
		// execution (never left as an unbounded ordered scan).
		if adjust == 0 {
			limitValue = operand
		} else {
			limitValue = &values.ArithmeticValue{
				Op:    values.OpSub,
				Left:  operand,
				Right: values.LiteralValue(int64(1)),
			}
		}
		found = true
		return false
	})
	return limit, limitValue, found
}

// distanceRowNumberPartitioning returns the PARTITION BY values of a
// metric-specific DistanceRowNumberValue (the LHS a global/partitioned vector
// rank lowers to), or ok=false for any other value.
func distanceRowNumberPartitioning(v values.Value) ([]values.Value, bool) {
	switch t := v.(type) {
	case *values.EuclideanDistanceRowNumberValue:
		return t.PartitioningValues, true
	case *values.EuclideanSquareDistanceRowNumberValue:
		return t.PartitioningValues, true
	case *values.CosineDistanceRowNumberValue:
		return t.PartitioningValues, true
	case *values.DotProductDistanceRowNumberValue:
		return t.PartitioningValues, true
	default:
		return nil, false
	}
}

// asInt64Literal coerces an evaluated comparand to int64 (the K of a distance
// rank). Returns ok=false for non-integer kinds.
func asInt64Literal(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
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
