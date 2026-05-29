package values

// SemanticEqualsUnderAliasMap reports whether two Values are equal up to the
// quantifier-alias correspondence in `aliases` — the bool, alias-map-keyed
// counterpart of EqualsWithoutChildren+children, for memo interning and
// relational EqualsWithoutChildren (RFC-040 040.2). Correlation-bearing leaf
// Values compare their alias through the map (an unmapped alias maps to
// itself, so identical aliases compare equal under the empty map); every other
// Value compares structurally via EqualsWithoutChildren and recurses children
// under the same map.
//
// This is consistent with SemanticHashCode: when this returns true, the two
// values have equal SemanticHashCode (both alias-invariant on the leaf
// aliases). Distinct from the cascades ValueEquivalence path, which carries
// QueryPlanConstraints for match-candidate compensation; this is the
// constraint-free bool primitive the expression/memo layer needs.
func SemanticEqualsUnderAliasMap(a, b Value, aliases AliasMap) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Correlation-bearing leaves: compare the alias THROUGH the map.
	switch av := a.(type) {
	case *QuantifiedObjectValue:
		bv, ok := b.(*QuantifiedObjectValue)
		return ok && mapAlias(aliases, av.Correlation) == bv.Correlation
	case *QuantifiedRecordValue:
		bv, ok := b.(*QuantifiedRecordValue)
		return ok && mapAlias(aliases, av.Alias) == bv.Alias
	case *ObjectValue:
		bv, ok := b.(*ObjectValue)
		return ok && mapAlias(aliases, av.Alias) == bv.Alias
	case *ConstantObjectValue:
		bv, ok := b.(*ConstantObjectValue)
		return ok && mapAlias(aliases, av.Alias) == bv.Alias && av.ConstantID == bv.ConstantID
	case *ExistsValue:
		bv, ok := b.(*ExistsValue)
		return ok && mapAlias(aliases, av.Alias) == bv.Alias
	case *ScalarSubqueryValue:
		bv, ok := b.(*ScalarSubqueryValue)
		return ok && mapAlias(aliases, av.Alias) == bv.Alias
	case *UnmatchedAggregateValue:
		bv, ok := b.(*UnmatchedAggregateValue)
		return ok && mapAlias(aliases, av.UnmatchedID) == bv.UnmatchedID
	// NOTE: IndexEntryObjectValue is deliberately NOT intercepted here. Its
	// canonical EqualsWithoutChildren compares OrdinalPath and IGNORES the
	// alias, so it falls through to the structural path below (OrdinalPath
	// compare, no children) — consistent with its alias-excluded + OrdinalPath
	// SemanticHashCode. Intercepting it to compare only the alias dropped
	// OrdinalPath and violated equal⟹same-hash (@claude review of PR #214).
	case *JoinMergeResultValue:
		bv, ok := b.(*JoinMergeResultValue)
		return ok &&
			mapAlias(aliases, av.OuterAlias) == bv.OuterAlias &&
			mapAlias(aliases, av.InnerAlias) == bv.InnerAlias
	}
	// Structural: node-info equality + alias-aware recursion into children.
	if !EqualsWithoutChildren(a, b) {
		return false
	}
	ac, bc := a.Children(), b.Children()
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if !SemanticEqualsUnderAliasMap(ac[i], bc[i], aliases) {
			return false
		}
	}
	return true
}

// mapAlias returns the target `x` maps to under `aliases`, or `x` itself if
// unmapped (identity), so two identical aliases compare equal under an empty map.
func mapAlias(aliases AliasMap, x CorrelationIdentifier) CorrelationIdentifier {
	if y, ok := aliases[x]; ok {
		return y
	}
	return x
}
