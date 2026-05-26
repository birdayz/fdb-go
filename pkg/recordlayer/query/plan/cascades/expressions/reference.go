package expressions

// PlannerStage tracks which planner phase has processed a Reference.
type PlannerStage int

const (
	StageInitial   PlannerStage = iota // client-created, no planner transformations
	StageCanonical                     // result of REWRITING phase
	StagePlanned                       // result of PLANNING phase
)

// Precedes returns true if s comes before other in the stage order.
func (s PlannerStage) Precedes(other PlannerStage) bool { return s < other }

// exploration state tracks per-Reference exploration progress within a phase.
type explorationState int

const (
	explorationNever      explorationState = iota // never explored in current phase
	explorationInProgress                         // exploration tasks pushed, not yet converged
	explorationDone                               // exploration converged
)

// Reference is the planner's handle on an equivalence class of
// RelationalExpressions — Cascades' "memo group".
//
// Members holds exploratory expressions (logical rewrites, physical
// wrappers from ExpressionRules). FinalMembers holds final expressions
// (physical plans from ImplementationRules). Mirrors Java's Reference
// which maintains exploratoryMembers and finalMembers as separate sets.
type Reference struct {
	members      []RelationalExpression
	finalMembers []RelationalExpression

	plannerStage PlannerStage
	explState    explorationState

	planProperties  any           // set during PLANNING phase; typed as *cascades.PlanPropertiesMap via cascades package
	partialMatchMap map[any][]any // MatchCandidate → []PartialMatch; typed via cascades helpers

	// winners stores per-properties best plans following Graefe 1995 §2.
	winners map[any]RelationalExpression
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

// AllMembers returns all members of this Reference.
func (r *Reference) AllMembers() []RelationalExpression {
	return r.members
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

// Winner returns the best plan for the given physical properties key,
// or nil if no winner has been stored. The key must be a comparable
// PhysicalProperties value (defined in the cascades package).
func (r *Reference) Winner(propsKey any) RelationalExpression {
	if r.winners == nil {
		return nil
	}
	return r.winners[propsKey]
}

// SetWinner stores the best plan for the given physical properties.
func (r *Reference) SetWinner(propsKey any, expr RelationalExpression) {
	if r.winners == nil {
		r.winners = make(map[any]RelationalExpression)
	}
	r.winners[propsKey] = expr
}

// ClearWinners removes all stored winners. Used by advancePlannerStage
// to discard EXPLORE-phase winners before PLANNING.
func (r *Reference) ClearWinners() {
	r.winners = nil
}

// HasWinner reports whether a winner exists for the given properties.
func (r *Reference) HasWinner(propsKey any) bool {
	if r.winners == nil {
		return false
	}
	_, ok := r.winners[propsKey]
	return ok
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

// FinalMembers returns PLANNING-phase physical plans. Empty until
// implementation rules or data access generation populate it.
func (r *Reference) FinalMembers() []RelationalExpression {
	return r.finalMembers
}

// InsertFinal adds e to the finalMembers set (PLANNING-phase physical
// plans). Uses the same dedup logic as Insert. Also inserts into
// members so that AllMembers remains a superset. Returns true if e was
// newly added to finalMembers (regardless of whether it was already in
// members). Mirrors Java's Reference.insertFinalExpression.
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
	r.Insert(e)
	return true
}

// AdvancePlannerStage transitions this Reference to a new planner stage.
// Clears exploratory members, promotes final members as the new
// exploratory seed, clears finals and plan properties, resets
// exploration state. PartialMatchMap is preserved (data access rules
// consume it in PLANNING). Mirrors Java's advancePlannerStageUnchecked.
func (r *Reference) AdvancePlannerStage(newStage PlannerStage) {
	r.plannerStage = newStage
	r.members = append(r.members[:0], r.finalMembers...)
	r.finalMembers = r.finalMembers[:0]
	r.planProperties = nil
	r.explState = explorationNever
	r.winners = nil
}

// Stage returns the current planner stage.
func (r *Reference) Stage() PlannerStage { return r.plannerStage }

// NeedsExploration returns true if the Reference should be explored in
// the current phase (not yet started or new members added).
func (r *Reference) NeedsExploration() bool {
	return r.explState != explorationDone
}

// StartExploration marks exploration as in-progress.
func (r *Reference) StartExploration() {
	r.explState = explorationInProgress
}

// CommitExploration marks exploration as converged.
func (r *Reference) CommitExploration() {
	r.explState = explorationDone
}

// PruneWith replaces final members with the single best expression.
// Mirrors Java's Reference.pruneWith.
func (r *Reference) PruneWith(expr RelationalExpression) {
	r.finalMembers = append(r.finalMembers[:0], expr)
}

// ClearFinalMembers removes all final members.
func (r *Reference) ClearFinalMembers() {
	r.finalMembers = r.finalMembers[:0]
}

// GetPlanProperties returns the planner-phase property map stored on this Reference.
func (r *Reference) GetPlanProperties() any { return r.planProperties }

// SetPlanProperties sets the planner-phase property map on this Reference.
func (r *Reference) SetPlanProperties(m any) { r.planProperties = m }

// AddPartialMatch stores a partial match for the given candidate.
// Returns true if newly added. Uses any-typed parameters to avoid
// circular imports (cascades → expressions); the cascades package
// provides typed wrappers. Mirrors Java's
// Reference.addPartialMatchForCandidate.
func (r *Reference) AddPartialMatch(candidate any, match any) bool {
	if r.partialMatchMap == nil {
		r.partialMatchMap = make(map[any][]any)
	}
	existing := r.partialMatchMap[candidate]
	for _, e := range existing {
		if e == match {
			return false // already present
		}
	}
	r.partialMatchMap[candidate] = append(existing, match)
	return true
}

// GetPartialMatchesFor returns all partial matches for the given
// candidate. Mirrors Java's Reference.getPartialMatchesForCandidate.
func (r *Reference) GetPartialMatchesFor(candidate any) []any {
	if r.partialMatchMap == nil {
		return nil
	}
	return r.partialMatchMap[candidate]
}

// GetAllPartialMatches returns all partial matches across all
// candidates. Mirrors Java's partialMatchMap.values().
func (r *Reference) GetAllPartialMatches() []any {
	if r.partialMatchMap == nil {
		return nil
	}
	var result []any
	for _, matches := range r.partialMatchMap {
		result = append(result, matches...)
	}
	return result
}

// GetPartialMatchCandidates returns all candidates that have partial
// matches. Mirrors Java's partialMatchMap.keySet().
func (r *Reference) GetPartialMatchCandidates() []any {
	if r.partialMatchMap == nil {
		return nil
	}
	result := make([]any, 0, len(r.partialMatchMap))
	for k := range r.partialMatchMap {
		result = append(result, k)
	}
	return result
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
