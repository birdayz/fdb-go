package expressions

// Reference is the planner's handle on an equivalence class of
// RelationalExpressions — Cascades' "memo group". Once the Memo lands
// (Track B3) a Reference will hold a SET of equivalent expressions; for
// the seed it holds exactly one.
//
// References are passed around by pointer (Java passes `Reference`
// objects, which are reference types under the hood). Two Quantifiers
// that range over the same Reference share the same equivalence class —
// this is how the planner avoids re-optimising the same subtree twice.
//
// Ports the relevant subset of Java's
// `com.apple.foundationdb.record.query.plan.cascades.Reference`. The
// Java class is 1068 lines; we expose:
//   - InitialOf: build a Reference holding a single expression
//   - Get: read the (currently single) member
//   - Members: read the full member list (always size 1 in the seed)
//   - Insert: append a member (no-op if EqualsWithoutChildren matches)
//
// Insert's dedup uses a two-tier check: pointer-identity on child
// References (fast path) plus SemanticEquals fallback (catches
// fresh-Reference wrapping). See Insert's doc comment for the full
// contract. This matches Java's ExpressionPartition behaviour with
// the addition of structural-equivalence dedup needed by rules that
// introduce wrapping operators around fresh sub-trees.
type Reference struct {
	members []RelationalExpression
}

// InitialOf returns a Reference holding the single expression e as its
// only member. Equivalent to Java's `Reference.initialOf(e)`.
func InitialOf(e RelationalExpression) *Reference {
	return &Reference{members: []RelationalExpression{e}}
}

// Get returns the (first) member. For seed References this is the only
// member; once the Memo lands, callers will iterate via Members instead.
// Returns nil if the Reference is empty (shouldn't happen for
// seed-constructed References — guards against future Memo bugs).
func (r *Reference) Get() RelationalExpression {
	if len(r.members) == 0 {
		return nil
	}
	return r.members[0]
}

// Members returns the equivalence-class members. The slice is read-only;
// callers must not mutate it.
func (r *Reference) Members() []RelationalExpression {
	return r.members
}

// Insert adds e to the equivalence class if no existing member already
// matches. Returns true if the member was inserted, false if a duplicate
// was found.
//
// Dedup contract — two-tier:
//
//  1. Fast path: EqualsWithoutChildren on the local node + pointer-
//     identity on every Quantifier's child Reference. Hits when a
//     rule yields output that reuses the input's existing Quantifiers
//     (the pattern most seed rules follow). O(1) check.
//  2. Fallback: full SemanticEquals walk (recursive structural match
//     with alias-aware child comparison). Catches the case where a
//     rule yields output wrapping a FRESH Reference whose held
//     expression is structurally equivalent to an existing member's
//     child Reference. Without this, rules like
//     PushFilterThroughDistinctRule would non-terminate. Gated on
//     hash equality (HashCodeWithoutChildren) for early-exit on
//     non-matching shapes — the HashConsistency invariant
//     (FuzzSemanticEquals_Properties) guarantees SemanticEquals can
//     only return true when local hashes agree.
//
// Soundness of the fallback: SemanticEquals's recursion compares
// child-Reference contents structurally with alias-aware AliasMap
// composition. Two Filters over scanA-References with structurally-
// equal scans ARE equivalent — they hold the same row stream,
// even if the Reference pointers differ. The doc comment's earlier
// "different inner row streams" warning was about cross-scan
// false-equivalence (different record types) — SemanticEquals
// correctly distinguishes those via EqualsWithoutChildren on the
// scan node info. The full Memo (B3 follow-on) generalises this
// further to merge equivalence classes across the whole memo.
func (r *Reference) Insert(e RelationalExpression) bool {
	if e == nil {
		// Defensive: callers should never insert nil. Panic loudly so a
		// regression is caught at the offending call site rather than
		// later when Reference.Members() returns a slice with a nil
		// entry that crashes the next walk.
		panic("Reference.Insert: nil expression")
	}
	eHash := e.HashCodeWithoutChildren()
	for _, m := range r.members {
		// Fast path: pointer-identity on child References + local
		// EqualsWithoutChildren. Hits when a rule yields output that
		// reuses the input's existing Quantifiers (the pattern most of
		// the seed rules follow).
		if m.EqualsWithoutChildren(e, EmptyAliasMap()) && sameChildReferences(m, e) {
			return false
		}
		// Fallback: full SemanticEquals walk. Catches the case where a
		// rule yields output wrapping a fresh Reference whose held
		// expression IS structurally equal to an existing member's
		// child Reference. Without this fallback, rules like
		// PushFilterThroughDistinctRule would non-terminate (each fire
		// adds a fresh-Reference duplicate). SemanticEquals is O(tree
		// size) but only walks when the pointer-identity fast path
		// misses AND the local hash matches — non-matching hashes prove
		// inequality without the deep walk (HashCodeWithoutChildren
		// must agree when SemanticEquals returns true at the top
		// level, by HashConsistency invariant pinned in fuzz).
		if m.HashCodeWithoutChildren() == eHash && SemanticEquals(m, e, EmptyAliasMap()) {
			return false
		}
	}
	r.members = append(r.members, e)
	return true
}

// sameChildReferences returns true if a and b have the same
// Quantifier count AND every Quantifier's Reference is the same
// pointer on both sides. Used by Reference.Insert as the second
// clause of the dedup contract.
func sameChildReferences(a, b RelationalExpression) bool {
	aQs := a.GetQuantifiers()
	bQs := b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	for i := range aQs {
		if aQs[i].GetRangesOver() != bQs[i].GetRangesOver() {
			return false
		}
	}
	return true
}
