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
	case *quantifiedObjectValue:
		bv, ok := b.(*quantifiedObjectValue)
		mapped, valid := mapAlias(aliases, av.correlation)
		return ok && valid && mapped == bv.correlation && exactTypesEqual(av.flowed, bv.flowed)
	case *QuantifiedRecordValue:
		bv, ok := b.(*QuantifiedRecordValue)
		mapped, valid := mapAlias(aliases, av.Alias)
		return ok && valid && mapped == bv.Alias
	case *ObjectValue:
		bv, ok := b.(*ObjectValue)
		mapped, valid := mapAlias(aliases, av.Alias)
		return ok && valid && mapped == bv.Alias
	case *ConstantObjectValue:
		bv, ok := b.(*ConstantObjectValue)
		mapped, valid := mapAlias(aliases, av.Alias)
		return ok && valid && mapped == bv.Alias && av.ConstantID == bv.ConstantID
	// ExistsValue is a transparent composite (RFC-141): the structural
	// path below compares EqualsWithoutChildren (both *ExistsValue) and
	// recurses into the child QuantifiedObjectValue, whose own case maps
	// the alias. No dedicated alias case here.
	case *ScalarSubqueryValue:
		bv, ok := b.(*ScalarSubqueryValue)
		mapped, valid := mapAlias(aliases, av.Alias)
		return ok && valid && mapped == bv.Alias
	case *UnmatchedAggregateValue:
		bv, ok := b.(*UnmatchedAggregateValue)
		mapped, valid := mapAlias(aliases, av.UnmatchedID)
		return ok && valid && mapped == bv.UnmatchedID
		// NOTE: IndexEntryObjectValue is deliberately NOT intercepted here. Its
		// canonical EqualsWithoutChildren compares Source + OrdinalPath and IGNORES
		// the alias, so it falls through to the structural path below (Source +
		// OrdinalPath compare, no children) — consistent with its alias-excluded,
		// Source + OrdinalPath SemanticHashCode. Source (KEY vs VALUE) is a semantic
		// discriminator: Evaluate reads PrimaryKey() for KEY and IndexValues() for
		// VALUE, so KEY[p] and VALUE[p] must NOT compare equal. An alias-only
		// intercept would drop both and violate the equal⟹same-hash invariant.
	}
	// Structural: node-info equality + alias-aware recursion into children.
	if !EqualsWithoutChildren(a, b) {
		return false
	}
	var aScratch, bScratch [1]Value
	ac, bc := valueChildren(a, &aScratch), valueChildren(b, &bScratch)
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
func mapAlias(aliases AliasMap, x CorrelationIdentifier) (CorrelationIdentifier, bool) {
	validated, ok := asAliasMap(aliases)
	if !ok {
		return CorrelationIdentifier{}, false
	}
	if y, found := validated.Target(x); found {
		return y, true
	}
	return x, true
}
