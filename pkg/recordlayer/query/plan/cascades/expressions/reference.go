package expressions

import (
	"bytes"
	"iter"
	"slices"
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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

// ReferencePlanProperties keeps cached properties synchronized with final-member pruning.
type ReferencePlanProperties interface {
	RetainMembers([]RelationalExpression)
}

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
	// The member lanes only grow by append; every other change installs a new
	// slice. A lane read once is therefore immutable, which lets an admission
	// view hold it without a copy.
	members      []RelationalExpression
	finalMembers []RelationalExpression
	// forced holds the members that arrived after the group's exploration
	// began (a merge's folded members, an out-of-band insert). The next
	// round explores each with every rule, Java's forceExploration for a
	// newly memoized expression; the others re-run only rules whose
	// declared constraints changed.
	forced map[RelationalExpression]struct{}
	// pinnedFinal marks a deliberately disentangled physical selection. Unlike
	// an ordinary one-member plan group, a pinned reference must not be grown by
	// physical rewrites: its parent was constructed over this exact member.
	pinnedFinal bool

	// These two fields are the transitional prepared-admission checkpoint.
	// The root cascades package freezes the exact RELATION result type before
	// any member hash/equality method is called; the version then proves that
	// the member sets did not change between pure preparation and apply.
	// RFC-232's memostore-backed immutable handle replaces both fields.
	admittedResultType values.ExactTypeHandle
	memberVersion      uint64

	// memberHash memoizes HashCodeWithoutChildren per member.
	//
	// A memo member is IMMUTABLE once admitted, so its hash cannot change — but
	// preparing an admission batch tests every intent against every existing
	// member, so without this the hash of each member is re-derived on every
	// batch, and deriving it walks that member's whole result Value and every
	// predicate through FNV. On a six-way star that walk, plus the garbage it
	// produced, dominated planning time.
	//
	// The memo lives on the Reference rather than on the expression so its
	// lifetime is the memo's: nothing survives the query, and no expression type
	// grows a mutable field that a shallow copy could carry across to a value
	// whose hash inputs differ.
	//
	// Admission and memo lookup use this map in the planner's sequential task
	// loop; unlike flowedType, expression constructors do not access it.
	memberHash map[RelationalExpression]uint64
	// signature caches the members' shapes for memo-equality pruning, keyed
	// by memberVersion like flowedType.
	signature atomic.Pointer[memberSignature]

	// flowedType memoizes GetFlowedObjectType's SUCCESSFUL answer, keyed by
	// memberVersion.
	//
	// That derivation walks every member and snapshots each one's result type
	// through ExactRelationOf, and it is called from inside rule bodies —
	// PartitionBinarySelectRule.tryPartition reaches it per match — so the same
	// unchanged member set was re-snapshotted continuously. On a planner sweep
	// snapshotExactType was 14.7% of samples and thaw a further 5.1%, feeding a
	// GC that was 44% of the run; a CI timeout's stack landed dead inside that
	// recursion.
	//
	// ONLY THE SUCCESS IS CACHED. The failures carry q.alias, and two
	// quantifiers can range over one Reference, so a cached error would report
	// the wrong alias to the second one. Failures are rare and short-circuit
	// the loop anyway, so re-deriving them costs nothing worth having.
	//
	// PUBLISHED AS ONE IMMUTABLE VALUE, not as three plain fields, because a
	// Reference is not private to a goroutine the way a Memo is. Nothing in the
	// planner shares one — the task loop is sequential — but a Reference is an
	// ordinary handle that tests and helpers pass around, and one of them
	// (TestRelationalAliasCompleteness) builds quantifiers over a single shared
	// scanRef from parallel subtests. That was safe until this memo existed:
	// before it, the derivation only READ the member list. Three separate fields
	// made every such reader a writer, and -race caught it intermittently — 4
	// runs in 6 — which is the worst possible way to own a data race.
	//
	// An atomic pointer makes concurrent publication safe for this getter. Two
	// goroutines racing to derive the same answer both store a self-consistent
	// (type, version) pair, so a duplicate derivation is wasted work and never a
	// wrong answer.
	flowedType atomic.Pointer[flowedTypeMemo]

	plannerStage PlannerStage
	explState    explorationState
	// constraintsMap is the Java-style tick/watermark exploration
	// bookkeeping (RFC-181 WS-P stage (a)) — maintained ALONGSIDE the
	// member-count convergence during REWRITING first visits; since
	// stage (b) it DRIVES convergence (NeedsExploration) — member
	// growth no longer re-rounds a group; every insert site pushes its
	// new expression's exploration tasks directly. Lazily allocated.
	constraintsMap *ConstraintsMap
	explRounds     int

	planProperties  ReferencePlanProperties
	partialMatchMap map[any][]any // MatchCandidate → []PartialMatch; typed via cascades helpers
	// partialMatchOrder records candidates in first-insertion order so iteration
	// is deterministic, mirroring Java's insertion-ordered LinkedHashMultimap.
	// Ranging over partialMatchMap directly made equal-cost index ties resolve by
	// Go's randomised map order → 2-3 distinct plans for one query (RFC-164
	// NONDETERMINISM / see RFC-167). The map stays for O(1) lookup; this slice fixes the order.
	partialMatchOrder []any

	// winner is the OPTIMIZE-chosen cheapest physical plan for this
	// group. Ordering-specific selection does NOT live here: a parent
	// with a directional requirement scans the members' derived rich
	// orderings (cascades getWinnerForOrdering), so satisfaction is
	// always judged on the full Value+sort-order representation —
	// never on a name-flattened key that would drop NULL placement
	// and collide distinct values sharing a rendered name.
	winner RelationalExpression

	// Publish a complete, read-only correlation set so concurrent readers of a
	// stable graph cannot race on lazy initialization. A non-nil pointer to an
	// empty map caches the uncorrelated case. Member edits and invalidation
	// remain sequential and must not overlap readers.
	correlatedToCache atomic.Pointer[correlationMemo]
	// memberCorrelations keeps each member's snapshot across membership
	// changes; a snapshot is reused only while every child it read still has
	// the snapshot it read, so adding one member does not recompute the rest.
	memberCorrelations atomic.Pointer[map[RelationalExpression]*correlationMemo]
	// correlationBase is the snapshot before the latest member appends; a read
	// extends it with the appended members instead of revisiting every member.
	// memberLayout counts the member changes other than appends, which
	// invalidate it.
	correlationBase atomic.Pointer[correlationMemo]
	memberLayout    uint64

	// aliasAwareDedups counts how many times the ALIAS-AWARE interning tier
	// (the MemoEqual branch in Insert/InsertFinal, gated to merge
	// re-enumeration selects) collapsed an incoming member that the two
	// alias-IDENTITY tiers did NOT catch — the "extra dedup" the alias-aware
	// tier buys (RFC-077 7.5). Per-Reference (each Plan owns its References),
	// so it is race-free without a global. Summed via Memo.AliasAwareDedups,
	// this count is the shadow of the sub-product-sharing that keeps the
	// join re-enumeration task count off the 29915→60044 blowup.
	aliasAwareDedups int

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
	return ExploratoryOfAtStage(e, StageCanonical)
}

// ExploratoryOfAtStage creates a logical group at the memoizing phase's target.
func ExploratoryOfAtStage(e RelationalExpression, stage PlannerStage) *Reference {
	return &Reference{members: []RelationalExpression{e}, plannerStage: stage}
}

// FinalOf returns a Reference holding e as its only FINAL member, at
// StagePlanned — Java's memoizePlan shape (Reference.ofFinalExpressions).
// Used for spine-pinned singletons minted DURING PLANNING: with an empty
// exploratory set and the target stage already reached, no ExploreGroup
// task can explore the singleton and grow it past the pin, and no stage
// advancement can promote-and-clear its finals.
func FinalOf(e RelationalExpression) *Reference {
	return &Reference{finalMembers: []RelationalExpression{e}, plannerStage: StagePlanned}
}

// PinnedFinalOf returns the exact physical-selection shape used by a parent
// alternative that was constructed over one concrete child member. It is
// intentionally distinct from FinalOf: an ordinary singleton plan group may
// still be explored and gain equivalent physical rewrites, while a pinned
// reference is the memo equivalent of baking that child into the parent.
func PinnedFinalOf(e RelationalExpression) *Reference {
	return &Reference{
		finalMembers: []RelationalExpression{e},
		plannerStage: StagePlanned,
		pinnedFinal:  true,
	}
}

// FinalOfAtStage returns a Reference holding e as its only FINAL member at
// the CALLER'S stage, decoupling "which member set" from "which planner
// stage".
//
// FinalOf bundles the two: it is the spine-pin shape, where StagePlanned is
// load-bearing (nothing may explore the singleton past the pin). But
// memoizePlan needs only the first half — a plan belongs in the FINAL set,
// per Java's Reference.ofFinalExpressions — and forcing StagePlanned as a
// side effect changes what ExploreGroupTask does with the reference, which is
// a different decision entirely.
//
// That conflation is why MemoizeFinalExpression still mints via InitialOf and
// lands plans in the EXPLORATORY set despite its name: the only
// final-set constructor available also stamped a stage the caller did not
// intend.
func FinalOfAtStage(e RelationalExpression, stage PlannerStage) *Reference {
	return &Reference{finalMembers: []RelationalExpression{e}, plannerStage: stage}
}

// ReferenceMemberSet identifies one of the two separately interned member
// lanes. It is intentionally smaller than planner stage: exploratory/final is
// member placement, while stage records planner progress.
type ReferenceMemberSet uint8

const (
	ReferenceExploratoryMembers ReferenceMemberSet = iota + 1
	ReferenceFinalMembers
)

// ReferenceAdmissionView is an immutable, defensive snapshot used by the
// RFC-232 transitional prepared-commit boundary. Its fields stay private so an
// apply can exact-recognize the view and tie it to the Reference/version from
// which it was read.
type ReferenceAdmissionView struct {
	reference   *Reference
	version     uint64
	resultType  values.ExactTypeHandle
	exploratory []RelationalExpression
	final       []RelationalExpression
}

// AdmissionView returns one defensive view without compressing a forwarded
// Reference chain. Avoiding path compression matters on a failed preparation:
// even memo topology and lazy forwarding caches must remain unchanged.
func (r *Reference) AdmissionView() *ReferenceAdmissionView {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	return &ReferenceAdmissionView{
		reference:   r,
		version:     r.memberVersion,
		resultType:  r.admittedResultType,
		exploratory: slices.Clip(r.members),
		final:       slices.Clip(r.finalMembers),
	}
}

// Members returns a defensive copy of the requested member lane.
func (v *ReferenceAdmissionView) Members(set ReferenceMemberSet) []RelationalExpression {
	if v == nil {
		return nil
	}
	switch set {
	case ReferenceExploratoryMembers:
		return append([]RelationalExpression(nil), v.exploratory...)
	case ReferenceFinalMembers:
		return append([]RelationalExpression(nil), v.final...)
	default:
		return nil
	}
}

// ResultType returns the immutable exact RELATION type already stored for this
// group, or nil while a legacy-created group has not crossed checked admission.
func (v *ReferenceAdmissionView) ResultType() values.ExactTypeHandle {
	if v == nil {
		return nil
	}
	return v.resultType
}

func canonicalReferenceReadOnly(r *Reference) *Reference {
	for r != nil && r.forwardedTo != nil {
		r = r.forwardedTo
	}
	return r
}

// ApplyPreparedMemberBatch is the sole transitional method-free member apply
// adapter. The root cascades package first exact-recognizes and freezes the
// complete batch, validates one exact RELATION type, and precomputes dedup. This
// method then verifies the exact view/version and performs only slice/counter
// writes; it invokes no proposed expression method and cannot fail after its
// first mutation.
//
// RFC-232 slice 3 replaces this exported source-gated seam with memostore's
// locked transaction. Keeping it narrow makes that deletion mechanical.
func (r *Reference) ApplyPreparedMemberBatch(
	view *ReferenceAdmissionView,
	relationType values.ExactTypeHandle,
	exploratory []RelationalExpression,
	final []RelationalExpression,
	aliasAwareDedups int,
) error {
	return r.applyPreparedMemberBatch(view, relationType, exploratory, final, aliasAwareDedups, nil)
}

// ApplyPreparedMemberBatch is Reference.ApplyPreparedMemberBatch carrying the
// batch's proof, if it found one, that the new members leave the group's
// correlations unchanged; then no reader above the group needs to revalidate.
func (p *PreparedMemberEquality) ApplyPreparedMemberBatch(
	r *Reference,
	view *ReferenceAdmissionView,
	relationType values.ExactTypeHandle,
	exploratory []RelationalExpression,
	final []RelationalExpression,
	aliasAwareDedups int,
) error {
	return r.applyPreparedMemberBatch(view, relationType, exploratory, final, aliasAwareDedups, p.unchanged)
}

func (r *Reference) applyPreparedMemberBatch(
	view *ReferenceAdmissionView,
	relationType values.ExactTypeHandle,
	exploratory []RelationalExpression,
	final []RelationalExpression,
	aliasAwareDedups int,
	unchanged *unchangedCorrelations,
) error {
	canonical := canonicalReferenceReadOnly(r)
	if canonical == nil || view == nil || view.reference != canonical {
		return &values.ResolutionError{ErrorCode: values.MemoInvalidHandle, Path: "memo.reference", Detail: "prepared view does not belong to Reference"}
	}
	exact, ok := values.AsExactTypeHandle(relationType)
	if !ok || exact == nil {
		return &values.ResolutionError{ErrorCode: values.MemoMissingRelationWrapper, Path: "memo.resultType", Detail: "prepared batch has no exact relation type"}
	}
	inner, relation := exact.RelationInner()
	if !relation || inner == nil {
		return &values.ResolutionError{ErrorCode: values.MemoMissingRelationWrapper, Path: "memo.resultType", Detail: "prepared batch type is not RELATION<T>"}
	}
	if _, doubled := inner.RelationInner(); doubled {
		return &values.ResolutionError{ErrorCode: values.MemoDoubleRelationWrapper, Path: "memo.resultType", Detail: "prepared batch type is RELATION<RELATION<T>>"}
	}
	if canonical.memberVersion != view.version {
		return &values.ResolutionError{ErrorCode: values.MemoBatchConflict, Path: "memo.reference", Detail: "Reference members changed after preparation"}
	}
	if canonical.admittedResultType != nil &&
		!bytes.Equal(canonical.admittedResultType.CanonicalBytes(), exact.CanonicalBytes()) {
		return &values.ResolutionError{ErrorCode: values.MemoResultTypeMismatch, Path: "memo.resultType", Detail: "prepared relation type disagrees with Reference"}
	}
	if aliasAwareDedups < 0 {
		return &values.ResolutionError{ErrorCode: values.MemoBatchConflict, Path: "memo.reference", Detail: "negative alias-aware dedup delta"}
	}
	for _, member := range exploratory {
		if member == nil {
			return &values.ResolutionError{ErrorCode: values.MemoUnsupportedExpression, Path: "memo.member", Detail: "nil exploratory member in prepared batch"}
		}
	}
	for _, member := range final {
		if member == nil {
			return &values.ResolutionError{ErrorCode: values.MemoUnsupportedExpression, Path: "memo.member", Detail: "nil final member in prepared batch"}
		}
	}

	// Everything that can fail is above this line.
	if canonical.admittedResultType != nil && len(exploratory) == 0 && len(final) == 0 && aliasAwareDedups == 0 {
		return nil
	}
	canonical.members = append(canonical.members, exploratory...)
	canonical.finalMembers = append(canonical.finalMembers, final...)
	canonical.aliasAwareDedups += aliasAwareDedups
	canonical.admittedResultType = exact
	canonical.memberVersion++
	if len(exploratory)+len(final) > 0 {
		// Prepared admission is the planner's normal insertion path. A winner
		// ranks the member set that existed when it was stamped; publishing any
		// genuinely new member invalidates that snapshot just as Insert and
		// InsertFinal do.
		canonical.winner = nil
	}
	if !unchanged.install(canonical, view.version) {
		bumpCorrelationEpoch()
		if len(exploratory)+len(final) > 0 {
			canonical.saveCorrelationBase()
			canonical.correlatedToCache.Store(nil)
		}
	}
	return nil
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
		if r.Insert(m) {
			r.MarkForcedExploration(m)
		}
	}
	for _, m := range loser.finalMembers {
		if r.InsertFinal(m) {
			r.MarkForcedExploration(m)
		}
	}
	if len(r.members) > before {
		// New members arrived: re-arm exploration so the survivor
		// explores them under ITS OWN identity (rule bindings and
		// partial matches are (group, expression)-scoped). Bounded by
		// maxRoundsPerRef in the planner. The epoch tick is the same
		// re-arm in the tick/watermark model.
		if r.explState == explorationDone {
			r.explState = explorationInProgress
		}
		r.ConstraintsMap().ReArm()
	}
	// Fold the loser's alias-aware dedup shadow into the survivor. The
	// re-insertions above count only NEW dedups against the survivor's
	// members; the loser's HISTORICAL dedups (members it already suppressed —
	// never appended, so not reinserted here) are a disjoint quantity that
	// would otherwise be discarded when AliasAwareDedups canonicalizes to the
	// survivor, undercounting the shadow-delta metric after a Memo.merge.
	r.aliasAwareDedups += loser.aliasAwareDedups
	if loser.constraintsMap != nil {
		if r.constraintsMap == nil {
			// The survivor never explored, so no rule ever ran on ITS members
			// under its identity, however far the loser got with its own (the
			// loser's queued rule tasks then fail ContainsExactly on the
			// survivor). Take the loser's constraints but not its progress:
			// the survivor's first exploration runs every rule.
			r.constraintsMap = NewConstraintsMap()
			r.constraintsMap.InheritFromOther(loser.constraintsMap)
			r.constraintsMap.ForgetExploration()
		} else {
			// Both sides carry epoch state: fold the loser's constraints
			// in through the REAL per-key lattice combine (registered by
			// the cascades package) — a subsumed fold neither stores nor
			// ticks, exactly like a subsumed push (Java pushProperty).
			for _, k := range loser.constraintsMap.order {
				if e, ok := loser.constraintsMap.entries[k]; ok {
					r.constraintsMap.PushProperty(k, e.property, combineFor(k))
				}
			}
		}
	}
	r.correlatedToCache.Store(nil)
	r.memberLayout++
	bumpCorrelationEpoch()
	loser.forwardedTo = r
}

// AbsorbPlanningState completes a PLANNING merge whose caller already folded
// the loser's members into the receiver through checked admission: the
// loser's partial matches move over in its candidate order, its dedup shadow
// is kept, and it forwards to the receiver. Exploration state is the
// caller's, because only the planner knows which goals each side explored.
// Returns how many partial matches were new to the receiver.
func (r *Reference) AbsorbPlanningState(loser *Reference) int {
	added := 0
	for _, candidate := range loser.partialMatchOrder {
		for _, match := range loser.partialMatchMap[candidate] {
			if r.AddPartialMatch(candidate, match) {
				added++
			}
		}
	}
	r.aliasAwareDedups += loser.aliasAwareDedups
	r.correlatedToCache.Store(nil)
	r.memberLayout++
	bumpCorrelationEpoch()
	loser.forwardedTo = r
	return added
}

// RemoveExploratoryMember drops e from the exploratory members — the memo
// removing a duplicate that a merge made of two members of one group. Tasks
// still holding e skip it through ContainsExactly.
func (r *Reference) RemoveExploratoryMember(e RelationalExpression) bool {
	r = r.Canonical()
	for i, member := range r.members {
		if member != e {
			continue
		}
		r.members = append(r.members[:i:i], r.members[i+1:]...)
		delete(r.memberHash, e)
		r.memberVersion++
		r.memberLayout++
		bumpCorrelationEpoch()
		r.correlatedToCache.Store(nil)
		return true
	}
	return false
}

// Get returns the (first) member — the convenience accessor for
// single-member References (fresh InitialOf refs, matcher bindings);
// explored multi-member refs are iterated via Members / AllMembers.
// Returns nil if the Reference is empty (defensive; InitialOf never
// constructs one).
func (r *Reference) Get() RelationalExpression {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	if len(r.members) == 0 {
		// A finals-only Reference (FinalOf — a spine-pinned singleton) has
		// no exploratory members; its identity for semantic equality and
		// child-cost lookups is the pinned FINAL. Without this fallback two
		// otherwise-identical wrappers over DIFFERENT pinned children both
		// exposed nil children, so SemanticEquals collapsed them and
		// InsertFinal deduplicated a distinct ordered alternative away —
		// and costing treated the pinned subtree as unknown cardinality.
		if len(r.finalMembers) > 0 {
			return r.finalMembers[0]
		}
		return nil
	}
	return r.members[0]
}

// Members returns a defensive copy of the exploratory members.
func (r *Reference) Members() []RelationalExpression {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	return append([]RelationalExpression(nil), r.members...)
}

// HasExploratoryMember reports pointer-identity membership in the
// exploratory lane without copying it.
func (r *Reference) HasExploratoryMember(e RelationalExpression) bool {
	r = canonicalReferenceReadOnly(r)
	return r != nil && slices.Contains(r.members, e)
}

// HasFinalMember reports pointer-identity membership in the final lane
// without copying it.
func (r *Reference) HasFinalMember(e RelationalExpression) bool {
	r = canonicalReferenceReadOnly(r)
	return r != nil && slices.Contains(r.finalMembers, e)
}

// BorrowedMembers returns the view's own member lane, read-only and clipped
// so an append copies.
func (v *ReferenceAdmissionView) BorrowedMembers(set ReferenceMemberSet) []RelationalExpression {
	if v == nil {
		return nil
	}
	switch set {
	case ReferenceExploratoryMembers:
		return slices.Clip(v.exploratory)
	case ReferenceFinalMembers:
		return slices.Clip(v.final)
	default:
		return nil
	}
}

// MembersWithHash iterates exploratory members without copying their slice.
// Like MemberHash, it requires the owning planner's sequential task loop;
// membership must not change during iteration.
func (r *Reference) MembersWithHash(hash uint64) iter.Seq[RelationalExpression] {
	return func(yield func(RelationalExpression) bool) {
		ref := canonicalReferenceReadOnly(r)
		if ref == nil || !ref.memberSignature().hasExploratoryHash(hash) {
			return
		}
		for _, member := range ref.members {
			if ref.MemberHash(member) == hash && !yield(member) {
				return
			}
		}
	}
}

// AliasAwareDedups returns how many incoming members this Reference collapsed
// via the alias-aware interning tier (the extra dedup beyond alias-identity).
// A regression test sums this across the memo to bound the join
// re-enumeration task count.
func (r *Reference) AliasAwareDedups() int {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return 0
	}
	return r.aliasAwareDedups
}

// AllMembers returns all members of this Reference — both exploratory
// and final. Mirrors Java's getAllMembers() which unions the two sets.
func (r *Reference) AllMembers() []RelationalExpression {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	all := make([]RelationalExpression, 0, len(r.members)+len(r.finalMembers))
	all = append(all, r.members...)
	all = append(all, r.finalMembers...)
	return all
}

// ResultType returns the exact stored RELATION result type for a group that
// has crossed checked root admission. Empty, legacy-unadmitted, and forwarded
// invalid reads are errors rather than absence. Type() thaws a fresh graph, so
// callers cannot mutate the Reference's identity.
func (r *Reference) ResultType() (values.Type, error) {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil, &values.ResolutionError{ErrorCode: values.MemoInvalidHandle, Path: "memo.reference", Detail: "Reference is nil"}
	}
	if len(r.members)+len(r.finalMembers) == 0 {
		return nil, &values.ResolutionError{ErrorCode: values.MemoEmptyReference, Path: "memo.reference", Detail: "Reference has no members"}
	}
	exact, ok := values.AsExactTypeHandle(r.admittedResultType)
	if !ok || exact == nil {
		return nil, &values.ResolutionError{ErrorCode: values.MemoInvalidHandle, Path: "memo.resultType", Detail: "Reference has not crossed checked result-type admission"}
	}
	return exact.Type(), nil
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

// MemberVersion identifies the current exploratory/final population. A cached
// dependency must also check canonical identity, since forwarding can change it.
func (r *Reference) MemberVersion() uint64 {
	return r.Canonical().memberVersion
}

// Winner returns the OPTIMIZE-chosen cheapest plan for this group, or
// nil if none has been stored.
func (r *Reference) Winner() RelationalExpression {
	return r.Canonical().winner
}

// SetWinner stores the OPTIMIZE-chosen cheapest plan for this group.
func (r *Reference) SetWinner(expr RelationalExpression) {
	r.Canonical().winner = expr
}

// ClearWinners removes the stored winner. Used by advancePlannerStage
// to discard EXPLORE-phase winners before PLANNING.
func (r *Reference) ClearWinners() {
	r.Canonical().winner = nil
}

// HasWinner reports whether a winner has been stored.
func (r *Reference) HasWinner() bool {
	return r.Canonical().winner != nil
}

// HasWinnersOrMatches reports whether this Reference carries any
// PLANNING-phase bookkeeping (winners or partial matches). Used by the
// REWRITING Memo.merge as a scope tripwire: it folds neither (PLANNING
// merges go through AbsorbPlanningState).
func (r *Reference) HasWinnersOrMatches() bool {
	r = r.Canonical()
	return r.winner != nil || len(r.partialMatchMap) > 0
}

// Insert adds e to the equivalence class if no existing member already
// matches. Returns true if the member was inserted, false if a duplicate
// was found.
//
// Dedup contract — four-tier:
//
//  1. Fast path: EqualsWithoutChildren on the local node + pointer-
//     identity on every Quantifier's child Reference. Hits when a
//     rule yields output that reuses the input's existing Quantifiers
//     (the pattern most rules follow). O(1) check.
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
//  4. ExactReplica: any expression equal up to a renaming of planner merge
//     aliases, which nothing outside the expression can name.
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
// MarkForcedExploration records that e's next exploration runs every rule.
func (r *Reference) MarkForcedExploration(e RelationalExpression) {
	r = r.Canonical()
	if r.forced == nil {
		r.forced = make(map[RelationalExpression]struct{})
	}
	r.forced[e] = struct{}{}
}

// TakeForcedExploration reports whether e's exploration is forced, and
// clears the mark: one forced exploration per arrival.
func (r *Reference) TakeForcedExploration(e RelationalExpression) bool {
	r = r.Canonical()
	if _, ok := r.forced[e]; !ok {
		return false
	}
	delete(r.forced, e)
	return true
}

func (r *Reference) Insert(e RelationalExpression) bool {
	r = r.Canonical()
	if e == nil {
		panic("Reference.Insert: nil expression")
	}
	eHash := e.HashCodeWithoutChildren()
	// e is invariant across the loop, so resolve the alias-aware opt-in once
	// (hoisted out of the hot inner loop — avoids a type-switch + virtual call
	// per member). See the alias-aware tier below.
	aliasAware := InternsAliasAware(e)
	mergeAliased := !aliasAware && bindsMergeAlias(e)
	for _, m := range r.members {
		// Fast path: pointer-identity on child References + local
		// EqualsWithoutChildren. Hits when a rule yields output that
		// reuses the input's existing Quantifiers (the pattern most
		// rules follow).
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
		if r.MemberHash(m) == eHash && m.EqualsWithoutChildren(e, EmptyAliasMap()) && MemoEqualWithHashes(m, e, eHash, eHash) {
			return false
		}
		// Alias-aware tier (RFC-077 7.5), GATED to expressions that opt in via
		// InternsAliasAware (merge selects and local filter bindings — see
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
		// Hash pre-filter mirrors tier 2: MemoEqual also hash-guards internally, but
		// the explicit guard keeps the early-exit symmetric across tiers and cheap if
		// an opt-in type's hash is not alias-invariant.
		if aliasAware && r.MemberHash(m) == eHash && MemoEqualWithHashes(m, e, eHash, eHash) {
			r.aliasAwareDedups++
			return false
		}
		// A planner merge alias has no consumer outside its expression, so a
		// twin differing only in one is the same member (ExactReplica).
		if mergeAliased && r.MemberHash(m) == eHash && ExactReplica(m, e) {
			return false
		}
	}
	r.members = append(r.members, e)
	// A winner is a choice over the member set that existed when OptimizeGroup
	// stamped it. Growing that set invalidates the choice: implementation and
	// data-access rules can publish a cheaper alternative after an earlier
	// OptimizeGroup task has run. Leaving the old winner installed makes every
	// winner-first consumer (plan construction, ordering lookup, extraction)
	// permanently prefer the earlier member and turns task order into plan
	// quality. Duplicate insertions do not mutate the set and therefore keep the
	// existing winner.
	r.winner = nil
	// A direct legacy insertion did not cross the root registry. Never retain
	// stale exact authority across it; the next prepared ingress re-admits the
	// complete group before hashing.
	r.admittedResultType = nil
	r.memberVersion++
	bumpCorrelationEpoch()
	r.saveCorrelationBase()
	r.correlatedToCache.Store(nil)
	return true
}

// flowedTypeMemo pairs the memoized answer with the member version it was
// derived at, so the two are published and read as ONE value — see the
// flowedType field.
type flowedTypeMemo struct {
	typ     values.Type
	version uint64
	// prototypes cache the QOV prototypes of typ, plain and nullable-widened.
	prototypes [2]atomic.Pointer[values.QOVPrototype]
}

// cachedFlowedType returns the memoized GetFlowedObjectType answer when it was
// derived from the CURRENT member set. Every mutation of the member sets bumps
// memberVersion, so a stale entry cannot be observed.
func (r *Reference) cachedFlowedType() (values.Type, bool) {
	if r == nil {
		return nil, false
	}
	memo := r.flowedType.Load()
	if memo == nil || memo.version != r.memberVersion {
		return nil, false
	}
	return memo.typ, true
}

// cachedFlowedMemo is the flowed-type memo for the current member set, or nil.
func (r *Reference) cachedFlowedMemo() *flowedTypeMemo {
	if r == nil {
		return nil
	}
	if memo := r.flowedType.Load(); memo != nil && memo.version == r.memberVersion {
		return memo
	}
	return nil
}

func (r *Reference) setCachedFlowedType(typ values.Type) {
	if r == nil || typ == nil {
		return
	}
	r.flowedType.Store(&flowedTypeMemo{typ: typ, version: r.memberVersion})
}

// MemberHashes returns HashCodeWithoutChildren for each member, memoized on
// this Reference. See the memberHash field for why the memo is safe and why it
// lives here rather than on the expressions.
func (r *Reference) MemberHashes(members []RelationalExpression) []uint64 {
	if r == nil || len(members) == 0 {
		return nil
	}
	if r.memberHash == nil {
		r.memberHash = make(map[RelationalExpression]uint64, len(members))
	}
	hashes := make([]uint64, len(members))
	for i, m := range members {
		hashes[i] = r.MemberHash(m)
	}
	return hashes
}

// MemberHash memoizes an immutable admitted member's hash. Like MemberHashes,
// it must only be called from the owning planner's sequential task loop.
func (r *Reference) MemberHash(member RelationalExpression) uint64 {
	if cached, ok := r.memberHash[member]; ok {
		return cached
	}
	if r.memberHash == nil {
		r.memberHash = make(map[RelationalExpression]uint64)
	}
	hash := member.HashCodeWithoutChildren()
	r.memberHash[member] = hash
	return hash
}

// PreparedMemberDuplicate runs Reference's three memo-equality tiers without
// mutating a Reference. The root cascades admission boundary calls it only
// after every expression in the complete batch has been exact-recognized and
// result-type checked. It is not itself an admission API: it deliberately
// invokes expression hash/equality methods. Its dedicated equality traversal
// follows forwarding read-only and uses invocation-local correlation state, so
// preparation neither path-compresses child References nor fills shared caches.
//
// aliasAwareOnly reports that only the third tier found the duplicate, so the
// later method-free apply can preserve Reference's diagnostic counter.
func PreparedMemberDuplicate(members []RelationalExpression, e RelationalExpression) (duplicate bool, aliasAwareOnly bool) {
	return PreparedMemberDuplicateWithHashes(members, nil, e)
}

// PreparedMemberDuplicateWithHashes is PreparedMemberDuplicate with the
// members' HashCodeWithoutChildren already computed by the caller. A nil or
// short hashes slice falls back to computing the missing ones, so the two entry
// points cannot disagree about the answer — only about how often the hash is
// derived.
//
// WHY THE CALLER GETS TO HOIST IT. A batch tests every INTENT against every
// existing MEMBER, so computing each member's hash inside this function makes
// it O(intents × members) derivations of a value that cannot change: a memo
// member is immutable, and HashCodeWithoutChildren walks its whole result Value
// through FNV. Precomputing per batch makes it O(members). Measured on a
// six-way star, that walk and the garbage it made were a double-digit
// percentage of planning time.
func PreparedMemberDuplicateWithHashes(
	members []RelationalExpression, hashes []uint64, e RelationalExpression,
) (duplicate bool, aliasAwareOnly bool) {
	var preparation PreparedMemberEquality
	return preparation.DuplicateWithHashes(members, hashes, e)
}

// PreparedMemberEquality shares read-only derivations across a single admission
// batch. Its zero value is ready to use. After a successful commit, publish and
// discard it: local derivations must not outlive mutations to the graph.
type PreparedMemberEquality struct {
	equality  memoEquality
	inputs    map[*Reference]preparedInputSignature
	unchanged *unchangedCorrelations
}

// unchangedCorrelations is a group's correlation snapshot after a batch whose
// members add no correlation the group did not already have, derived on the
// graph of one correlation epoch.
type unchangedCorrelations struct {
	reference *Reference
	version   uint64
	epoch     uint64
	snapshot  *correlationMemo
}

// PrepareCorrelations records whether adding members to ref provably leaves
// ref's correlations unchanged. It reads only snapshots already current, so it
// never walks a stale subgraph, and it publishes nothing.
func (p *PreparedMemberEquality) PrepareCorrelations(ref *Reference, members []RelationalExpression) {
	p.unchanged = nil
	ref = canonicalReferenceReadOnly(ref)
	if ref == nil {
		return
	}
	reader := &p.equality.correlations
	epoch := correlationEpoch.Load()
	if reader.epoch != 0 && reader.epoch != epoch {
		return
	}
	old := reader.current(ref, epoch)
	if old == nil || old.version != ref.memberVersion {
		return
	}
	// The old snapshot's dependencies are shared until a member adds one.
	dependencies, owned := old.dependencies, false
	var seen map[*Reference]struct{}
	for _, member := range members {
		snapshot, ok := reader.expressions[member]
		if !ok {
			stale := false
			snapshot = &correlationMemo{content: newCorrelationContent()}
			snapshot.correlations = expressionCorrelations(member, func(child *Reference) map[values.CorrelationIdentifier]struct{} {
				current := reader.current(child, epoch)
				if current == nil {
					stale = stale || canonicalReferenceReadOnly(child) != nil
					return nil
				}
				snapshot.dependencies = append(snapshot.dependencies, correlationDependency{child, current})
				return current.correlations
			})
			if stale {
				return
			}
		}
		for alias := range snapshot.correlations {
			if _, ok := old.correlations[alias]; !ok {
				return
			}
		}
		for _, dependency := range snapshot.dependencies {
			if seen == nil {
				seen = make(map[*Reference]struct{}, len(dependencies)+len(snapshot.dependencies))
				for _, known := range dependencies {
					seen[known.reference] = struct{}{}
				}
			}
			if _, dup := seen[dependency.reference]; dup {
				continue
			}
			seen[dependency.reference] = struct{}{}
			if !owned {
				dependencies, owned = append([]correlationDependency(nil), dependencies...), true
			}
			dependencies = append(dependencies, dependency)
		}
	}
	p.unchanged = &unchangedCorrelations{
		reference: ref,
		version:   ref.memberVersion,
		epoch:     epoch,
		snapshot: &correlationMemo{
			correlations: old.correlations,
			content:      old.content,
			dependencies: dependencies,
		},
	}
}

// install publishes the prepared snapshot for r's new member version when the
// graph is still the one it was derived on.
func (u *unchangedCorrelations) install(r *Reference, version uint64) bool {
	if u == nil || u.reference != r || u.version != version || u.epoch != correlationEpoch.Load() {
		return false
	}
	u.snapshot.version = r.memberVersion
	u.snapshot.layout, u.snapshot.members, u.snapshot.finals = r.memberLayout, len(r.members), len(r.finalMembers)
	u.snapshot.validated.Store(u.epoch)
	r.correlatedToCache.Store(u.snapshot)
	return true
}

// SeedMemberCorrelations lets the batch reuse ref's member snapshots from
// earlier batches, each revalidated before use.
func (p *PreparedMemberEquality) SeedMemberCorrelations(ref *Reference) {
	if ref = canonicalReferenceReadOnly(ref); ref != nil {
		if prior := ref.memberCorrelations.Load(); prior != nil {
			p.equality.correlations.priors = *prior
		}
	}
}

// PublishMemberCorrelations records the batch's member snapshots on ref after
// a successful commit, for the next batch to seed from.
func (p *PreparedMemberEquality) PublishMemberCorrelations(ref *Reference) {
	if ref = canonicalReferenceReadOnly(ref); ref != nil {
		p.equality.correlations.publishMembers(ref)
	}
}

// PublishCorrelations retains completed derivations after successful admission.
// The owning planner must call this sequentially; dependency changes are still
// validated by every later reader, including changes made by the commit itself.
func (p *PreparedMemberEquality) PublishCorrelations() {
	for ref, snapshot := range p.equality.correlations.memo {
		if ref.forwardedTo != nil || ref.memberVersion != snapshot.version {
			continue
		}
		cached := ref.correlatedToCache.Load()
		if cached != snapshot {
			ref.correlatedToCache.CompareAndSwap(cached, snapshot)
		}
	}
}

// DuplicateWithHashes compares admitted members without publishing shared caches.
func (p *PreparedMemberEquality) DuplicateWithHashes(
	members []RelationalExpression, hashes []uint64, e RelationalExpression,
) (duplicate bool, aliasAwareOnly bool) {
	equality := &p.equality
	eHash := equality.hash(e)
	aliasAware := InternsAliasAware(e)
	mergeAliased := !aliasAware && bindsMergeAlias(e)
	eArity := len(e.GetQuantifiers())
	for i, m := range members {
		mHash := uint64(0)
		known := i < len(hashes)
		if known {
			mHash = hashes[i]
		} else {
			mHash = equality.hash(m)
		}
		// Every test below needs equal node hashes (node equality implies them)
		// and equal quantifier counts, and both are cheaper than node equality,
		// which ignores a select's quantifier list.
		if mHash != eHash || len(m.GetQuantifiers()) != eArity {
			continue
		}
		nodeEqual := m.EqualsWithoutChildren(e, EmptyAliasMap())
		if nodeEqual && (preparedSameChildReferences(m, e) || equality.equalWithHashes(nil, m, mHash, nil, e, eHash, EmptyAliasMap())) {
			return true, false
		}
		if aliasAware && equality.equalWithHashes(nil, m, mHash, nil, e, eHash, EmptyAliasMap()) {
			return true, true
		}
		if mergeAliased && ExactReplica(m, e) {
			return true, false
		}
	}
	return false, false
}

// aliasAwareInterner is implemented by expressions whose quantifier aliases are
// planner-internal (no external consumer resolves them by identity), so they
// intern ALIAS-AWARE in Insert/InsertFinal. See SelectExpression.InternsAliasAware
// (RFC-077 7.5). Merge selects and filters with local bindings opt in.
type aliasAwareInterner interface{ InternsAliasAware() bool }

// InternsAliasAware reports whether e opts into the ALIAS-AWARE memo-interning
// tier (RFC-077 7.5): true only for expressions whose quantifier aliases are
// planner-internal (merge selects and local filter bindings — see
// SelectExpression.InternsAliasAware). Exported so every site that runs
// MemoEqual across expressions NOT already known to be the same memo member —
// not just Insert/InsertFinal — gates identically. Reference.Insert /
// InsertFinal collapse MEMBERS within one group; Memo.findEquivalentRef (in
// package cascades) decides whether to merge two DIFFERENT groups — both
// widen memo equivalence beyond alias-identity and both hit the same landmine
// documented on SelectExpression.InternsAliasAware if left ungated: a
// non-opted-in expression's alias-renamed twin can survive as the sole member
// of a merged/collapsed group, orphaning any external structure (Go's
// identity-based column resolution) that resolved through the discarded
// alias.
func InternsAliasAware(e RelationalExpression) bool {
	aware := false
	if interner, ok := e.(aliasAwareInterner); ok {
		aware = interner.InternsAliasAware()
	}
	if disableAliasAwareInterning {
		aware = false // test-only alias-identity baseline (shadow-delta pin)
	}
	return aware
}

// disableAliasAwareInterning is a TEST-ONLY switch that suppresses the
// alias-aware interning tier, so a corpus can be planned in the
// alias-IDENTITY baseline to recover the pre-interning member population.
// It is read in Insert/InsertFinal's hot path but only ever WRITTEN by a
// NON-PARALLEL test: Go runs non-parallel tests sequentially, before the parallel
// phase, with a happens-before barrier between the phases, so a plain bool is
// race-free (the write is never concurrent with a read). The same
// non-parallel test-only global-flag discipline is used for other
// planner test switches.
//
// The non-parallel constraint is enforced by -race, NOT just convention: adding
// t.Parallel() to the setter's caller makes the write concurrent with a hot-path
// read, which -race reports — so a regression is loud, not silent. The clean
// removal (thread the flag through per-Memo state, as mergeAliasCounter does, so
// no global is read in Insert at all) is tracked in TODO.md as a follow-up; it
// is deferred because Reference.Insert has no Memo handle today.
var disableAliasAwareInterning bool

// SetDisableAliasAwareInterning toggles the alias-identity baseline mode. MUST
// be called only from a NON-PARALLEL test and reset via defer (see the var doc).
func SetDisableAliasAwareInterning(v bool) { disableAliasAwareInterning = v }

// FinalMembers returns PLANNING-phase physical plans. Empty until
// implementation rules or data access generation populate it.
func (r *Reference) FinalMembers() []RelationalExpression {
	r = canonicalReferenceReadOnly(r)
	if r == nil {
		return nil
	}
	return append([]RelationalExpression(nil), r.finalMembers...)
}

// InsertFinal adds e to the finalMembers set only. Does NOT add to
// exploratory members. Mirrors Java's Reference.insertFinalExpression.
func (r *Reference) InsertFinal(e RelationalExpression) bool {
	r = r.Canonical()
	if e == nil {
		panic("Reference.InsertFinal: nil expression")
	}
	eHash := e.HashCodeWithoutChildren()
	// Resolve the alias-aware opt-in once (hoisted out of the loop) — see Insert.
	aliasAware := InternsAliasAware(e)
	mergeAliased := !aliasAware && bindsMergeAlias(e)
	for _, m := range r.finalMembers {
		if m.EqualsWithoutChildren(e, EmptyAliasMap()) && sameChildReferences(m, e) {
			return false
		}
		if r.MemberHash(m) == eHash && m.EqualsWithoutChildren(e, EmptyAliasMap()) && MemoEqualWithHashes(m, e, eHash, eHash) {
			return false
		}
		// Alias-aware tier (GATED) — see Insert. finalMembers intern the same way
		// (RFC-077 7.5); the PLANNING yield path inserts into BOTH member sets, so
		// both must dedup alias-aware or the merge re-enumeration's physical
		// alternatives duplicate under fresh merge-quantifier aliases.
		if aliasAware && r.MemberHash(m) == eHash && MemoEqualWithHashes(m, e, eHash, eHash) {
			r.aliasAwareDedups++
			return false
		}
		if mergeAliased && r.MemberHash(m) == eHash && ExactReplica(m, e) {
			return false
		}
	}
	r.finalMembers = append(r.finalMembers, e)
	// See Insert: the previously stamped winner was computed over a smaller
	// physical candidate set and is no longer authoritative.
	r.winner = nil
	r.admittedResultType = nil
	r.memberVersion++
	bumpCorrelationEpoch()
	r.saveCorrelationBase()
	r.correlatedToCache.Store(nil)
	return true
}

// ConstraintsMap returns the Reference's tick/watermark constraint map
// (lazily allocated). Canonical-forwarding like every other accessor.
func (r *Reference) ConstraintsMap() *ConstraintsMap {
	r = r.Canonical()
	if r.constraintsMap == nil {
		r.constraintsMap = NewConstraintsMap()
	}
	return r.constraintsMap
}

// AdvancePlannerStage transitions this Reference to a new planner stage.
// Clears exploratory members, promotes final members as the new
// exploratory seed, clears finals and plan properties, resets
// exploration state. PartialMatchMap is preserved (data access rules
// consume it in PLANNING). Mirrors Java's advancePlannerStageUnchecked.
func (r *Reference) AdvancePlannerStage(newStage PlannerStage) {
	r = r.Canonical()
	r.plannerStage = newStage
	r.members = slices.Clone(r.finalMembers)
	r.finalMembers = nil
	r.memberVersion++
	r.memberLayout++
	bumpCorrelationEpoch()
	r.planProperties = nil
	r.explState = explorationNever
	r.explRounds = 0
	r.winner = nil
	if r.constraintsMap != nil {
		r.constraintsMap.AdvancePlannerStage()
	}
}

// Stage returns the current planner stage.
func (r *Reference) Stage() PlannerStage { r = r.Canonical(); return r.plannerStage }

// IsPinnedFinal reports whether this reference is an exact physical child
// selection rather than an ordinary equivalence group.
func (r *Reference) IsPinnedFinal() bool {
	r = canonicalReferenceReadOnly(r)
	return r != nil && r.pinnedFinal
}

// AdvanceStagePreservingMembers sets a reference's stage at CONSTRUCTION,
// keeping both member lanes as they are and resetting the per-stage
// exploration bookkeeping: a reference built directly at a stage (the memo's
// referenceOfAt, test fixtures). It is never a planner crossing: the planner
// crosses a group only through AdvancePlannerStage, which requires exactly
// one final (Java's Reference.advancePlannerStage), so no unfinalized or
// unpruned group carries its members into the next stage.
//
// Mirrors AdvancePlannerStage's reset EXCEPT the finals→members promotion and
// the member wipe. Members, partial matches, and forwarding are untouched.
func (r *Reference) AdvanceStagePreservingMembers(newStage PlannerStage) {
	r = r.Canonical()
	r.plannerStage = newStage
	// A prepared member lane is phase-sensitive: committing an exploratory
	// intent after this transition could publish it into the wrong phase. Make
	// the transition conflict with every earlier admission view even though it
	// deliberately preserves the member slices.
	r.memberVersion++
	r.memberLayout++
	bumpCorrelationEpoch()
	r.planProperties = nil
	r.explState = explorationNever
	r.explRounds = 0
	r.winner = nil
	if r.constraintsMap != nil {
		r.constraintsMap.AdvancePlannerStage()
	}
}

// NeedsExploration is Java Reference.needsExploration: EPOCH-driven
// (RFC-181 WS-P stage (b) — the convergence handover). A Reference
// needs exploration when it has never been explored, or when it is
// between rounds with constraint pushes newer than the last round's
// goal. Member growth no longer drives group re-rounds: every insert
// site pushes its new expression's exploration tasks directly (Java
// executeRuleCall), so the group loop re-runs only on epoch signals.
func (r *Reference) NeedsExploration() bool {
	r = r.Canonical()
	if r.constraintsMap == nil {
		// Never explored, never pushed: the fresh-group first visit.
		return r.explState == explorationNever
	}
	if r.constraintsMap.HasNeverBeenExplored() && !r.constraintsMap.IsExploring() {
		return true
	}
	return r.constraintsMap.NeedsExploration()
}

// StartExploration marks exploration as in-progress and records the
// current exploratory member count for convergence detection.
func (r *Reference) StartExploration() {
	r = r.Canonical()
	r.explState = explorationInProgress
	r.explRounds++
	r.ConstraintsMap().StartExploration()
}

// ExplRounds returns how many exploration rounds have been started.
func (r *Reference) ExplRounds() int { r = r.Canonical(); return r.explRounds }

// CommitExploration marks exploration as converged.
func (r *Reference) CommitExploration() {
	r = r.Canonical()
	r.explState = explorationDone
	r.ConstraintsMap().CommitExploration()
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
	r.finalMembers = []RelationalExpression{expr}
	if r.planProperties != nil {
		r.planProperties.RetainMembers(r.finalMembers)
	}
	r.winner = nil
	r.memberVersion++
	r.memberLayout++
	bumpCorrelationEpoch()
}

// PruneToSet keeps exactly the final members present in `keep` (by
// identity), preserving their existing order, and drops the rest.
// Exploratory members are untouched — same contract as PruneWith.
func (r *Reference) PruneToSet(keep map[RelationalExpression]struct{}) {
	r = r.Canonical()
	kept := make([]RelationalExpression, 0, len(r.finalMembers))
	for _, m := range r.finalMembers {
		if _, ok := keep[m]; ok {
			kept = append(kept, m)
		}
	}
	r.finalMembers = kept
	if r.planProperties != nil {
		r.planProperties.RetainMembers(r.finalMembers)
	}
	r.winner = nil
	r.memberVersion++
	r.memberLayout++
	bumpCorrelationEpoch()
}

// ClearFinalMembers removes all final members.
func (r *Reference) ClearFinalMembers() {
	r = r.Canonical()
	r.finalMembers = nil
	if r.planProperties != nil {
		r.planProperties.RetainMembers(nil)
	}
	r.winner = nil
	r.memberVersion++
	r.memberLayout++
	bumpCorrelationEpoch()
}

// GetPlanProperties returns the planner-phase property map stored on this Reference.
func (r *Reference) GetPlanProperties() ReferencePlanProperties {
	r = r.Canonical()
	return r.planProperties
}

// SetPlanProperties sets the planner-phase property map on this Reference.
func (r *Reference) SetPlanProperties(m ReferencePlanProperties) {
	r = r.Canonical()
	r.planProperties = m
}

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
	existing, seen := r.partialMatchMap[candidate]
	for _, e := range existing {
		if e == match {
			return false // already present
		}
	}
	if !seen {
		// First match for this candidate — record its insertion order.
		r.partialMatchOrder = append(r.partialMatchOrder, candidate)
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
	// Iterate in insertion order (Java LinkedHashMultimap.values()), not Go's
	// randomised map order, so tie resolution is deterministic.
	for _, candidate := range r.partialMatchOrder {
		result = append(result, r.partialMatchMap[candidate]...)
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
	// Insertion order (Java LinkedHashMultimap.keySet()), not Go's randomised map
	// order, so candidate iteration is deterministic.
	result := make([]any, 0, len(r.partialMatchOrder))
	result = append(result, r.partialMatchOrder...)
	return result
}

// saveCorrelationBase keeps r's snapshot as the base later reads extend, before
// members are appended and the cache dropped.
func (r *Reference) saveCorrelationBase() {
	if cached := r.correlatedToCache.Load(); cached != nil && cached.layout == r.memberLayout {
		r.correlationBase.Store(cached)
	}
}

// InvalidateCorrelatedToCache drops the cached correlation set so the
// next GetCorrelatedTo recomputes. Called by Memo.merge up the DAG after
// a merge (RFC-037 §3 step 5). Operates on the canonical Reference.
func (r *Reference) InvalidateCorrelatedToCache() {
	r = r.Canonical()
	r.correlatedToCache.Store(nil)
	r.memberLayout++
	bumpCorrelationEpoch()
}

// sameChildReferences returns true if a and b have the same
// Quantifier count AND every Quantifier's Reference resolves to the
// same canonical Reference on both sides. Used by Reference.Insert as
// the second clause of the dedup contract. Comparison is via
// GetRangesOver (which resolves forwarding read-only), so a merged-away child
// and its survivor compare equal without mutating topology during comparison.
func sameChildReferences(a, b RelationalExpression) bool {
	return preparedSameChildReferences(a, b)
}
