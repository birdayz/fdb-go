package expressions

// Reference is the planner's handle on an equivalence class of
// RelationalExpressions — Cascades' "memo group".
//
// Java's Reference distinguishes exploratoryMembers (rewriting phase)
// from finalMembers (planning phase). We mirror this: `members` holds
// exploratory expressions, `finalMembers` holds implementation-phase
// results. During the REWRITING phase, rules Insert into members.
// During the PLANNING phase, ImplementationCascadesRules InsertFinal
// into finalMembers. AllMembers() returns the union for code that
// doesn't care about the distinction (matcher, cost extraction).
type Reference struct {
	members        []RelationalExpression
	finalMembers   []RelationalExpression
	planProperties any // set during PLANNING phase; typed as *cascades.PlanPropertiesMap via cascades package
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

// Members returns the exploratory members. The slice is read-only.
func (r *Reference) Members() []RelationalExpression {
	return r.members
}

// FinalMembers returns the final (implementation-phase) members.
func (r *Reference) FinalMembers() []RelationalExpression {
	return r.finalMembers
}

// AllMembers returns the union of exploratory and final members.
func (r *Reference) AllMembers() []RelationalExpression {
	if len(r.finalMembers) == 0 {
		return r.members
	}
	if len(r.members) == 0 {
		return r.finalMembers
	}
	all := make([]RelationalExpression, 0, len(r.members)+len(r.finalMembers))
	all = append(all, r.members...)
	all = append(all, r.finalMembers...)
	return all
}

// InsertFinal adds e to the final members if no existing final member
// matches. Used by ImplementationCascadesRules during the PLANNING phase.
func (r *Reference) InsertFinal(e RelationalExpression) bool {
	if e == nil {
		panic("Reference.InsertFinal: nil expression")
	}
	eHash := e.HashCodeWithoutChildren()
	for _, m := range r.finalMembers {
		if m.EqualsWithoutChildren(e, EmptyAliasMap()) && sameChildReferences(m, e) {
			return false
		}
		if m.HashCodeWithoutChildren() == eHash && SemanticEquals(m, e, EmptyAliasMap()) {
			return false
		}
	}
	r.finalMembers = append(r.finalMembers, e)
	return true
}

// NewFinalReference creates a new Reference containing only the given
// final expressions. Used by FinalMemoizer to create disentangled
// references during the PLANNING phase.
func NewFinalReference(exprs []RelationalExpression) *Reference {
	final := make([]RelationalExpression, len(exprs))
	copy(final, exprs)
	return &Reference{finalMembers: final}
}

// GetBest returns the cheapest member of this Reference under the
// `less` comparator. Equivalent to Java's `Reference.get(comparator)`
// — the cost-driven extraction step.
//
// Returns nil if the Reference is empty. If multiple members are
// tied at the comparator's minimum, returns the FIRST such member
// (determinism — the comparator must be a total order on Cost; ties
// at Cost.Total + Cost.Cardinality break by insertion order). Single-
// member References return that member without invoking `less`.
//
// `less` must NOT be nil.
func (r *Reference) GetBest(less func(a, b RelationalExpression) bool) RelationalExpression {
	all := r.AllMembers()
	if len(all) == 0 {
		return nil
	}
	best := all[0]
	for _, m := range all[1:] {
		if less(m, best) {
			best = m
		}
	}
	return best
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

// GetPlanProperties returns the planner-phase property map stored on this Reference.
func (r *Reference) GetPlanProperties() any { return r.planProperties }

// SetPlanProperties sets the planner-phase property map on this Reference.
func (r *Reference) SetPlanProperties(m any) { r.planProperties = m }

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
