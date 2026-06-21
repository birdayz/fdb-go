package predicates

// IsLeafQueryPredicate reports whether `p` is a leaf in the
// predicate tree (no children). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.
// LeafQueryPredicate` marker interface — Go expresses this via a
// runtime function on the existing Children() method instead of a
// per-type marker mixin (Go-idiomatic).
//
// Use case: a rule that matches only leaf predicates checks
// IsLeafQueryPredicate(p) before recursing into Children();
// avoids tight coupling to specific concrete types
// (ConstantPredicate / ValuePredicate / ComparisonPredicate /
// ExistentialValuePredicate / etc.).
func IsLeafQueryPredicate(p QueryPredicate) bool {
	return p != nil && len(p.Children()) == 0
}
