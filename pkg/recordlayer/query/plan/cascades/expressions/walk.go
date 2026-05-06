package expressions

// Walk invokes `visit` on `e` and (if visit returns true) recursively
// on every descendant RelationalExpression reachable through e's
// Quantifiers' References. Returning false from `visit` short-circuits
// the walk for that subtree (siblings + ancestors continue).
//
// Counterpart to predicates.WalkPredicate and values.WalkValue. Useful
// for tree-wide searches:
//
//   - "Does any sub-expression contain a LogicalFilter?"
//   - "Find every Reference in this tree."
//   - "Does this tree contain an aggregate?"
//
// Safe on nil: returns immediately. Safe on cyclic Reference graphs
// (the planner doesn't construct cycles, but the walker is defensive
// — it doesn't track visited References, relying on the caller's
// `visit` predicate to detect repeated visits if needed).
//
// Walk visits each RelationalExpression at most once per Reference
// occurrence — if two Quantifiers in the tree range over the same
// Reference, the inner expression is visited twice. Future MaxMatchMap
// integration will introduce visited-Reference tracking; the seed
// keeps the walk simple.
func Walk(e RelationalExpression, visit func(RelationalExpression) bool) {
	if e == nil {
		return
	}
	if !visit(e) {
		return
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			Walk(m, visit)
		}
	}
}

// Size returns the total number of RelationalExpression nodes
// reachable from `e` (including e itself, including duplicate visits
// of shared References). Counterpart to predicates.PredicateSize and
// values.ValueSize.
//
// Returns 0 for nil input.
func Size(e RelationalExpression) int {
	count := 0
	Walk(e, func(_ RelationalExpression) bool {
		count++
		return true
	})
	return count
}
