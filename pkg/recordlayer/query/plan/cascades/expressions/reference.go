package expressions

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

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
//
// # Cross-group merging (RFC-037)
//
// Two References that are independently created but later discovered to
// be logically equivalent are merged via union-find. The survivor keeps
// its state; the loser sets forwardedTo to the survivor and becomes a
// transparent forwarder: every state-bearing method below resolves the
// receiver to Canonical() at entry, so any pointer to a merged-away
// Reference (held by an in-flight task, a Quantifier, or a binding) reads
// the survivor's state. The raw fields of a forwarded Reference are inert
// but readable; nothing is cleared. See Memo.merge.
//
// Methods that MUST NOT canonicalize: Canonical, ID, IsForwarded, and the
// merge primitive absorb — they operate on the receiver's own identity.
type Reference struct {
	members      []RelationalExpression
	finalMembers []RelationalExpression

	plannerStage    PlannerStage
	explState       explorationState
	explMemberCount int
	explRounds      int

	planProperties  any           // set during PLANNING phase; typed as *cascades.PlanPropertiesMap via cascades package
	partialMatchMap map[any][]any // MatchCandidate → []PartialMatch; typed via cascades helpers

	// winners stores per-properties best plans following Graefe 1995 §2.
	winners map[any]RelationalExpression

	correlatedToCache map[values.CorrelationIdentifier]struct{}

	// id is a monotonic identity assigned by the Memo on first
	// registration (0 ⇒ never registered, e.g. standalone-test
	// References). Merge picks the lower id as the survivor, giving a
	// deterministic winner independent of map iteration order.
	id uint64

	// forwardedTo is nil for a canonical (live) Reference. When this
	// Reference has been merged into another, forwardedTo points at the
	// survivor and all state access resolves through Canonical().
	forwardedTo *Reference
}

// InitialOf returns a Reference holding the single expression e as its
// only member. The Reference starts at StageCanonical so REWRITING-
// phase exploration doesn't need to advance it.
func InitialOf(e RelationalExpression) *Reference {
	return &Reference{members: []RelationalExpression{e}, plannerStage: StageCanonical}
}

// Canonical follows the forwarding chain to the surviving Reference and
// compresses the path so subsequent lookups are O(1). For a live
// (non-forwarded) Reference it returns the receiver unchanged. Safe on
// nil (returns nil). Does NOT recurse into other Reference methods.
func (r *Reference) Canonical() *Reference {
	if r == nil || r.forwardedTo == nil {
		return r
	}
	// Find the root of the forwarding chain.
	root := r.forwardedTo
	for root.forwardedTo != nil {
		root = root.forwardedTo
	}
	// Path compression: point every node on the chain straight at root.
	for r.forwardedTo != root {
		next := r.forwardedTo
		r.forwardedTo = root
		r = next
	}
	return root
}

// ID returns the Reference's Memo-assigned identity (0 if unregistered).
// Does NOT canonicalize — callers comparing identity want this object's id.
func (r *Reference) ID() uint64 { return r.id }

// AssignMemoID sets the Memo identity if not already set. Idempotent;
// intended to be called only by the Memo on first registration.
func (r *Reference) AssignMemoID(id uint64) {
	if r.id == 0 {
		r.id = id
	}
}

// IsForwarded reports whether this Reference has been merged away.
func (r *Reference) IsForwarded() bool { return r.forwardedTo != nil }

// Absorb folds the loser's state into the receiver (the survivor) and
// marks the loser as forwarding to the survivor. The receiver must be
// canonical and distinct from loser; this is enforced by Memo.merge,
// the only intended caller.
//
// Folds exploratory + final members (pointer-preserving — Insert/
// InsertFinal append the same expression pointers, so pointer-identity
// scans elsewhere still find them) and re-arms exploration if genuinely
// new members were added, so the survivor re-explores them. The
// re-explore is bounded by the planner's maxRoundsPerRef backstop.
//
// Does NOT canonicalize either side: it operates on the two raw objects.
func (r *Reference) Absorb(loser *Reference) {
	before := len(r.members)
	for _, m := range loser.members {
		r.Insert(m)
	}
	for _, m := range loser.finalMembers {
		r.InsertFinal(m)
	}
	if len(r.members) > before {
		// New members arrived: re-arm exploration so the survivor
		// explores them. Bounded by maxRoundsPerRef in the planner.
		if r.explState == explorationDone {
			r.explState = explorationInProgress
		}
	}
	r.correlatedToCache = nil
	loser.forwardedTo = r
}

// Get returns the (first) member. For seed References this is the only
// member; once the Memo lands, callers will iterate via Members instead.
// Returns nil if the Reference is empty (shouldn't happen for
// seed-constructed References — guards against future Memo bugs).
func (r *Reference) Get() RelationalExpression {
	r = r.Canonical()
	if len(r.members) == 0 {
		return nil
	}
	return r.members[0]
}

// Members returns the exploratory members. The slice is read-only.
func (r *Reference) Members() []RelationalExpression {
	r = r.Canonical()
	return r.members
}

// AllMembers returns all members of this Reference — both exploratory
// and final. Mirrors Java's getAllMembers() which unions the two sets.
func (r *Reference) AllMembers() []RelationalExpression {
	r = r.Canonical()
	if len(r.finalMembers) == 0 {
		return r.members
	}
	all := make([]RelationalExpression, 0, len(r.members)+len(r.finalMembers))
	all = append(all, r.members...)
	all = append(all, r.finalMembers...)
	return all
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
	r = r.Canonical()
	if r.winners == nil {
		return nil
	}
	return r.winners[propsKey]
}

// SetWinner stores the best plan for the given physical properties.
func (r *Reference) SetWinner(propsKey any, expr RelationalExpression) {
	r = r.Canonical()
	if r.winners == nil {
		r.winners = make(map[any]RelationalExpression)
	}
	r.winners[propsKey] = expr
}

// ClearWinners removes all stored winners. Used by advancePlannerStage
// to discard EXPLORE-phase winners before PLANNING.
func (r *Reference) ClearWinners() {
	r = r.Canonical()
	r.winners = nil
}

// HasWinner reports whether a winner exists for the given properties.
func (r *Reference) HasWinner(propsKey any) bool {
	r = r.Canonical()
	if r.winners == nil {
		return false
	}
	_, ok := r.winners[propsKey]
	return ok
}

// GetWinners returns the winners map for iteration. Returns nil if no
// winners are stored. Callers must not mutate the map.
func (r *Reference) GetWinners() map[any]RelationalExpression {
	r = r.Canonical()
	return r.winners
}

// HasWinnersOrMatches reports whether this Reference carries any
// PLANNING-phase bookkeeping (winners or partial matches). Used by
// Memo.merge as a scope tripwire: cross-group merging is REWRITING-only
// (RFC-037 §0), and these structures hold/embed References that the
// merge does not canonicalize.
func (r *Reference) HasWinnersOrMatches() bool {
	r = r.Canonical()
	return len(r.winners) > 0 || len(r.partialMatchMap) > 0
}

// Insert adds e to the equivalence class if no existing member already
// matches. Returns true if the member was inserted, false if a duplicate
// was found.
//
// Dedup contract — three-tier:
//
//  1. Fast path: EqualsWithoutChildren on the local node + pointer-
//     identity on every Quantifier's child Reference. Hits when a
//     rule yields output that reuses the input's existing Quantifiers
//     (the pattern most seed rules follow). O(1) check.
//  2. SemanticEquals walk (recursive structural match with alias-aware
//     child comparison, under the EmptyAliasMap = alias-IDENTITY at the
//     top level). Catches the case where a rule yields output wrapping a
//     FRESH Reference whose held expression is structurally equivalent to
//     an existing member's child Reference. Without this, rules like
//     PushFilterThroughDistinctRule would non-terminate. Gated on
//     hash equality (HashCodeWithoutChildren) for early-exit on
//     non-matching shapes — the HashConsistency invariant
//     (FuzzSemanticEquals_Properties) guarantees SemanticEquals can
//     only return true when local hashes agree.
//  3. MemoEqual (alias-AWARE): members equal up to a consistent
//     quantifier-alias renaming are one member (RFC-039/077). Tiers 1–2
//     are alias-identity, so a rule yielding an alternative that differs
//     only in a fresh quantifier alias would slip past them; this tier
//     interns it, matching memoizeNonLeaf's child interning and Java's
//     containsInMemo. Strictly additive — never dedups less than 1–2.
//
// Soundness of the fallback: SemanticEquals's recursion compares
// child-Reference contents structurally with alias-aware AliasMap
// composition. Two Filters over scanA-References with structurally-
// equal scans ARE equivalent — they hold the same row stream,
// even if the Reference pointers differ. The doc comment's earlier
// "different inner row streams" warning was about cross-scan
// false-equivalence (different record types) — SemanticEquals
// correctly distinguishes those via EqualsWithoutChildren on the
// scan node info. Cross-Reference merging (RFC-037) generalises this
// further: when an equivalent member already lives in a *different*
// Reference, Memo.merge collapses the two groups.
func (r *Reference) Insert(e RelationalExpression) bool {
	r = r.Canonical()
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
		// Alias-aware tier (RFC-077 7.5), GATED to expressions that opt in via
		// InternsAliasAware (merge re-enumeration selects only — see
		// SelectExpression.InternsAliasAware). Two such members equal up to a
		// CONSISTENT quantifier-alias renaming are the same memo member: MemoEqual
		// builds the node's own quantifier-alias map (RFC-039) and compares under
		// it, exactly as memoizeNonLeaf already does for child interning and as
		// Java's Reference.containsInMemo does for insert. The two tiers above are
		// alias-IDENTITY only (EmptyAliasMap), so a re-enumeration that wraps a
		// shared merge sub-product under a fresh uniqueId merge quantifier would
		// otherwise add a duplicate member and re-explore it per path (super-linear
		// blowup with join arity). The gate confines this to planner-internal merge
		// aliases — expressions whose aliases external consumers resolve by identity
		// keep alias-IDENTITY dedup. Added (not substituted), so it can only ever
		// dedup MORE, never less, than the alias-identity tiers — termination holds.
		if interner, ok := e.(aliasAwareInterner); ok && interner.InternsAliasAware() && MemoEqual(m, e) {
			return false
		}
	}
	r.members = append(r.members, e)
	r.correlatedToCache = nil
	return true
}

// aliasAwareInterner is implemented by expressions whose quantifier aliases are
// planner-internal (no external consumer resolves them by identity), so they
// intern ALIAS-AWARE in Insert/InsertFinal. See SelectExpression.InternsAliasAware
// (RFC-077 7.5). Only merge re-enumeration selects opt in today.
type aliasAwareInterner interface{ InternsAliasAware() bool }

// FinalMembers returns PLANNING-phase physical plans. Empty until
// implementation rules or data access generation populate it.
func (r *Reference) FinalMembers() []RelationalExpression {
	r = r.Canonical()
	return r.finalMembers
}

// InsertFinal adds e to the finalMembers set only. Does NOT add to
// exploratory members. Mirrors Java's Reference.insertFinalExpression.
func (r *Reference) InsertFinal(e RelationalExpression) bool {
	r = r.Canonical()
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
		// Alias-aware tier (GATED) — see Insert. finalMembers intern the same way
		// (RFC-077 7.5); the PLANNING yield path inserts into BOTH member sets, so
		// both must dedup alias-aware or the merge re-enumeration's physical
		// alternatives duplicate under fresh merge-quantifier aliases.
		if interner, ok := e.(aliasAwareInterner); ok && interner.InternsAliasAware() && MemoEqual(m, e) {
			return false
		}
	}
	r.finalMembers = append(r.finalMembers, e)
	r.correlatedToCache = nil
	return true
}

// AdvancePlannerStage transitions this Reference to a new planner stage.
// Clears exploratory members, promotes final members as the new
// exploratory seed, clears finals and plan properties, resets
// exploration state. PartialMatchMap is preserved (data access rules
// consume it in PLANNING). Mirrors Java's advancePlannerStageUnchecked.
func (r *Reference) AdvancePlannerStage(newStage PlannerStage) {
	r = r.Canonical()
	r.plannerStage = newStage
	r.members = append(r.members[:0], r.finalMembers...)
	r.finalMembers = r.finalMembers[:0]
	r.planProperties = nil
	r.explState = explorationNever
	r.explRounds = 0
	r.explMemberCount = 0
	r.winners = nil
}

// Stage returns the current planner stage.
func (r *Reference) Stage() PlannerStage { r = r.Canonical(); return r.plannerStage }

// SetStage updates the planner stage without clearing members. Used
// when a Reference created ad-hoc during a phase (by rule yields)
// has no finals to promote — its members are already valid at the
// target stage level.
func (r *Reference) SetStage(s PlannerStage) { r = r.Canonical(); r.plannerStage = s }

// NeedsExploration returns true if the Reference has never been explored
// or if new exploratory members were added since the last round.
func (r *Reference) NeedsExploration() bool {
	r = r.Canonical()
	if r.explState == explorationNever {
		return true
	}
	if r.explState == explorationDone {
		return false
	}
	return len(r.members) > r.explMemberCount
}

// StartExploration marks exploration as in-progress and records the
// current exploratory member count for convergence detection.
func (r *Reference) StartExploration() {
	r = r.Canonical()
	r.explState = explorationInProgress
	r.explMemberCount = len(r.members)
	r.explRounds++
}

// ExplRounds returns how many exploration rounds have been started.
func (r *Reference) ExplRounds() int { r = r.Canonical(); return r.explRounds }

// ExplMemberCount returns the member count recorded at the last
// StartExploration. Used to explore only NEW members on re-entry.
func (r *Reference) ExplMemberCount() int {
	r = r.Canonical()
	return r.explMemberCount
}

// CommitExploration marks exploration as converged.
func (r *Reference) CommitExploration() {
	r = r.Canonical()
	r.explState = explorationDone
}

// ContainsExactly returns true if expr is a member of this Reference
// (by pointer identity). Used by transform tasks to skip rules on
// expressions that have been removed or replaced.
func (r *Reference) ContainsExactly(expr RelationalExpression) bool {
	r = r.Canonical()
	for _, m := range r.members {
		if m == expr {
			return true
		}
	}
	for _, m := range r.finalMembers {
		if m == expr {
			return true
		}
	}
	return false
}

// PruneWith replaces final members with the single best expression.
// Mirrors Java's Reference.pruneWith.
func (r *Reference) PruneWith(expr RelationalExpression) {
	r = r.Canonical()
	r.finalMembers = append(r.finalMembers[:0], expr)
}

// ClearFinalMembers removes all final members.
func (r *Reference) ClearFinalMembers() {
	r = r.Canonical()
	r.finalMembers = r.finalMembers[:0]
}

// GetPlanProperties returns the planner-phase property map stored on this Reference.
func (r *Reference) GetPlanProperties() any { r = r.Canonical(); return r.planProperties }

// SetPlanProperties sets the planner-phase property map on this Reference.
func (r *Reference) SetPlanProperties(m any) { r = r.Canonical(); r.planProperties = m }

// AddPartialMatch stores a partial match for the given candidate.
// Returns true if newly added. Uses any-typed parameters to avoid
// circular imports (cascades → expressions); the cascades package
// provides typed wrappers. Mirrors Java's
// Reference.addPartialMatchForCandidate.
func (r *Reference) AddPartialMatch(candidate any, match any) bool {
	r = r.Canonical()
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
	r = r.Canonical()
	if r.partialMatchMap == nil {
		return nil
	}
	return r.partialMatchMap[candidate]
}

// GetAllPartialMatches returns all partial matches across all
// candidates. Mirrors Java's partialMatchMap.values().
func (r *Reference) GetAllPartialMatches() []any {
	r = r.Canonical()
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
	r = r.Canonical()
	if r.partialMatchMap == nil {
		return nil
	}
	result := make([]any, 0, len(r.partialMatchMap))
	for k := range r.partialMatchMap {
		result = append(result, k)
	}
	return result
}

// InvalidateCorrelatedToCache drops the cached correlation set so the
// next GetCorrelatedTo recomputes. Called by Memo.merge up the DAG after
// a merge (RFC-037 §3 step 5). Operates on the canonical Reference.
func (r *Reference) InvalidateCorrelatedToCache() {
	r = r.Canonical()
	r.correlatedToCache = nil
}

// GetCorrelatedTo returns the full (transitive) set of correlation
// identifiers this Reference depends on. Unions each member's own
// correlations with its children's correlations, excluding aliases
// bound by each member's own quantifiers. Result is cached after
// first computation.
func (r *Reference) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	r = r.Canonical()
	if r.correlatedToCache != nil {
		return r.correlatedToCache
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for _, m := range r.AllMembers() {
		for k := range m.GetCorrelatedToWithoutChildren() {
			result[k] = struct{}{}
		}
		ownAliases := make(map[values.CorrelationIdentifier]struct{})
		for _, q := range m.GetQuantifiers() {
			ownAliases[q.GetAlias()] = struct{}{}
		}
		for _, q := range m.GetQuantifiers() {
			childRef := q.GetRangesOver()
			if childRef == nil {
				continue
			}
			for k := range childRef.GetCorrelatedTo() {
				if _, bound := ownAliases[k]; !bound {
					result[k] = struct{}{}
				}
			}
		}
	}
	r.correlatedToCache = result
	return result
}

// sameChildReferences returns true if a and b have the same
// Quantifier count AND every Quantifier's Reference resolves to the
// same canonical Reference on both sides. Used by Reference.Insert as
// the second clause of the dedup contract. Comparison is via
// GetRangesOver (which canonicalizes), so a merged-away child and its
// survivor compare equal.
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
