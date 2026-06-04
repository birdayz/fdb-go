package values

import "sort"

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
	// canonical EqualsWithoutChildren compares Source + OrdinalPath and IGNORES
	// the alias, so it falls through to the structural path below (Source +
	// OrdinalPath compare, no children) — consistent with its alias-excluded,
	// Source + OrdinalPath SemanticHashCode. Source (KEY vs VALUE) is a semantic
	// discriminator: Evaluate reads PrimaryKey() for KEY and IndexValues() for
	// VALUE, so KEY[p] and VALUE[p] must NOT compare equal. An alias-only
	// intercept would drop both and violate the equal⟹same-hash invariant.
	case *JoinMergeAllValue:
		// The sole join-merge value (the binary JoinMergeResultValue was retired
		// in RFC-074). Compare the alias SETS (order-independent), through the
		// alias map: the merge of a given leg-set is the same logical sub-product
		// regardless of leg order (Graefe condition 1 — commutative/associative).
		// We compare SORTED mapped names (a multiset compare): dup-safe by
		// construction (does NOT assume callers feed dup-free slices) and
		// allocation-light.
		//
		// The Seed provenance bit IS compared. The collapse retired TWO Go types
		// (translator seed JoinMergeResultValue vs re-enumeration JoinMergeAllValue)
		// into one struct; those two types were never equal, so a translator seed
		// and a re-enumeration of the same leg-set never interned. We preserve that
		// exactly — the collapse is behavior-preserving, NOT a budget change. Were
		// Seed excluded, a translator binary seed and a re-enumeration of the same
		// pair would suddenly intern and trigger the RFC-037 cross-group merge the
		// two-type design never did — measured to blow the ≥4-way STAR past the task
		// budget (the FDB N-way regression). Consistent with SemanticHashCode (folds
		// arity + Seed). Seed is excluded only from Evaluate (merged-row identical).
		bv, ok := b.(*JoinMergeAllValue)
		if !ok || av.Seed != bv.Seed || len(av.Aliases) != len(bv.Aliases) {
			return false
		}
		am := make([]string, len(av.Aliases))
		for i, a := range av.Aliases {
			am[i] = mapAlias(aliases, a).Name()
		}
		bm := make([]string, len(bv.Aliases))
		for i, a := range bv.Aliases {
			bm[i] = a.Name()
		}
		sort.Strings(am)
		sort.Strings(bm)
		for i := range am {
			if am[i] != bm[i] {
				return false
			}
		}
		return true
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
