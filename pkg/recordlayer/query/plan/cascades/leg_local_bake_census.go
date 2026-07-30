package cascades

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The LEG-LOCAL BAKEABILITY census.
//
// It measures ONE question, at the one site that packs a leg into a column name:
// when `rebaseOuterLegValue`'s leg-match arm re-anchors a leg-correlated read onto
// the merge correlation as `QOV(merged)."LEG.COL"`, is there a stated LAYOUT for
// that leg — the thing a leg-local `ofOrdinal(QOV(leg), i)` would have to index?
//
// Why the question decides something. Java never re-anchors a leg-correlated read
// through a name: its FlatMap binds one correlation per quantifier
// (RecordQueryFlatMapPlan.java:135-140 over the parent chain at
// Bindings.java:116-134) for the chained outer→inner PAIR, and a SIBLING leg is
// re-anchored by ORDINAL against the merged quantifier
// (PartitionSelectRule.java:296-303 — `FieldValue.ofOrdinalNumber(QOV(newUpper),
// index)`). Either way the reference is an alias plus an ordinal and no name is
// consulted (QuantifiedObjectValue.java:84-85 is a map lookup). Go's two-level
// NLJ→FlatMap lowering has no ordinal to offer when the leg states no layout, and
// the qualified name is what fills that hole. The census measures how big the hole
// is, so the channel's retirement rests on a measurement of the live traffic
// rather than on the shape of the one arm that is easy to read.
//
// WHAT IT DOES NOT MEASURE. It never claims a read "would have baked". The
// leg-local bake that once sat at this arm resolved the read's DISPLAY NAME
// against the leg's row type to mint an ordinal — RFC-197's forbidden move, and
// invisible to the `.Field` decision gate because a type lookup by name is neither
// a comparison nor a map key (`field_name_decision_test.go`'s second documented
// blind spot). It is deleted. What survives here is the LAYOUT question, and the
// per-read name lookup below is a census-side DIAGNOSTIC that never reaches a
// plan: it separates "this leg states no layout at all" from "it states one that
// does not declare the column this read names", which are different residues with
// different fixes.
//
// It is GATED (LegIdentityCensusEnabled) like the leg-identity census it sits
// beside: the site is inside a Cascades rule, so its totals count RULE FIRINGS,
// not queries — the memo may explore one rule once or many times per query. Read
// the absolute numbers as a planning artifact; the ratio, and the count of
// DISTINCT witnesses, are the facts about the corpus.
const legLocalBakeWitnessCap = 256

// legLocalBakeOutcome is what the arm ACTUALLY did with the read. It is passed in
// rather than re-derived from the leg type, because those two answers come apart
// exactly when it matters most: once CQ-63 types the leg quantifiers, a census
// that re-derives would report every read as convertible while the mint is still
// the only thing the arm emits — "deletable" and "live" at the same time.
type legLocalBakeOutcome int

const (
	// legLocalBakeMinted: the read was re-anchored onto the merge correlation as
	// `QOV(merged)."LEG.COL"`. The only outcome this arm produces today.
	legLocalBakeMinted legLocalBakeOutcome = iota
	// legLocalBakeBaked: the read kept its own leg alias and carried a leg-local
	// ordinal — the PASS-THROUGH, and the arm's whole live population now that
	// every reference arrives carrying its ordinal.
	legLocalBakeBaked
	// legLocalBakeDeclined: the read stated NO identity, so there was nothing to
	// keep and nothing honest to mint. The arm hands it back untouched.
	//
	// This is a RESIDUE and it is asserted at zero. It is not a gap in this arm:
	// a reference that reaches a planner rule without a resolved path was minted
	// unresolved by its producer, and no rewrite here can supply what the
	// producer did not. The arm used to mint `QOV(merged)."LEG.COL"` for exactly
	// this case — a display name standing in for an identity, which is the
	// channel RFC-197 exists to remove — and the mint is deleted rather than
	// kept as a fallback, because a fallback that spells a name is how the
	// channel survives a migration that was supposed to end it.
	legLocalBakeDeclined
)

type legLocalBakeCounters struct {
	// Total is every firing of the leg-match arm.
	Total int
	// Baked: reads that KEPT their own leg alias and their own leg-local ordinal
	// — the pass-through. The arm's live population.
	Baked int
	// Minted: reads re-anchored onto the merge correlation at a MERGED ordinal,
	// with the leg's qualified name riding along as a display string.
	//
	// This is Java's own move (PartitionSelectRule.java:296-303) and not a
	// residue, so it is reported rather than asserted at zero — but it is the
	// only thing left in this arm that spells a leg into a name, and the name it
	// spells is load-bearing for nothing: TestLazyLegMintReachesNoWinningPlan
	// measures zero dotted merged-row keys in the WINNING plan for every shape
	// that reaches here.
	Minted int
	// Declined: reads that stated no identity at all. Asserted ZERO — see
	// legLocalBakeDeclined for why this is a producer's residue and not this
	// arm's.
	Declined int
	// The rest classify MINTED reads only — why the mint was reached.
	//
	// UntypedLeg: no layout for this leg at all, so no leg-local ordinal could
	// exist. These are the reads that would have to DECLINE.
	UntypedLeg int
	// ColumnAbsent: a layout exists but does not declare a column of this read's
	// name. Census-side name lookup (see the header): it separates a missing
	// layout from a mismatched one, and decides nothing.
	ColumnAbsent int
	// LayoutAvailable: a layout exists and declares the read's name.
	//
	// WHAT IT MUST NEVER BE READ AS: the precondition for a leg-local bake. It
	// is a PROXY for one, and the two came apart in the way that costs the most
	// — silently, while the proxy read as success.
	//
	// The distinction: this counter answers "does the LEG have a layout". A bake
	// needs "can the READ state an ordinal in that layout". The second is what
	// IdentityInLegDomain below measures, and it is the real instrument.
	// Measured: LayoutAvailable reached 126 of 126 and was read as the
	// precondition being met, while IdentityInLegDomain was ZERO the whole time
	// — every read declined, because the reference's own quantifier object was
	// minted untyped and the frontier derived from it was unknown. A whole
	// migration step was scheduled against the proxy.
	//
	// This is the PROXY-INSTRUMENT failure class, and it is worth naming because
	// it is not any of the ones this file already defends against. Those are all
	// counters that STOP measuring — a dead counter, a vacuous zero, a collapsed
	// denominator — and the floors and partitions below exist to catch exactly
	// that. This counter never stopped measuring. It was live, correctly
	// partitioned, floored against collapse, and reported an honest number about
	// a DIFFERENT QUESTION than the one being asked of it. No floor and no
	// partition can detect that; only a second instrument measuring the actual
	// precondition can, which is why one was added rather than this one retuned.
	LayoutAvailable int
	// UnderivableLegs counts LEGS (not reads) neither derivation could state, so
	// every read correlated to them falls through.
	UnderivableLegs int
	// FlowedLegs counts LEGS the ONE authority stated — the quantifier's own
	// flowed object type, which is what Java's translateCorrelations rebases
	// against.
	//
	// It used to be reported beside a WALK-ONLY counter, which named legs the
	// quantifier could not state and a SUBORDINATE walk over the chosen physical
	// plan could. That walk was a documented divergence kept under a retirement
	// condition of "walkOnly reaches zero"; the condition was met (flowed 848 of
	// 848) and the walk was deleted, so the bucket is gone with it rather than
	// kept asserting a zero no site can increment. What re-arms suspicion is
	// UnderivableLegs: with one authority, a leg it cannot state has no second
	// opinion and contributes no layout at all.
	FlowedLegs int
	// DisagreeingLegs counts LEGS whose reference members flow different row
	// types — a memo defect, Java's Verify failure (Reference.java:504-513).
	DisagreeingLegs int
	// The POSITIONAL MERGE's slot typing, counted where the slot is built.
	//
	// The merge asks each collapsed leg's quantifier for its flowed value; when
	// that value states no row it falls back to SCAVENGING one off the select's
	// own value surfaces (legRowTypes), and when that misses too the slot enters
	// the merged record constructor UNTYPED. An untyped merge slot is the only
	// remaining path by which a leg's row type is lost on this route, and it is
	// silent: the reference degrades to source-relative, a source-relative operand
	// pushed into a leg's scan evaluates to NULL against the build-bound row, and
	// the join returns zero rows with no error.
	//
	// Counted rather than remembered. The residual was previously asserted in prose
	// on the strength of an argument about which shapes could reach it, and prose
	// is what the shape of a fallback changes underneath.
	//
	// MergeSlotsTyped: the quantifier stated a ROW. The intended path for a leg.
	MergeSlotsTyped int
	// MergeSlotsScavenged: the quantifier did not, and legRowTypes recovered the
	// row from a baked reference elsewhere in the select. The fallback FIRING.
	MergeSlotsScavenged int
	// MergeSlotsScalar: the quantifier stated a NON-ROW type. This is the mixed
	// seed's whole-object element — an unnest quantifier flows one array element,
	// which is a scalar and never a record — and it is CORRECT, not a residual.
	//
	// It is a bucket of its own because the site's own gate cannot tell it from the
	// residual: the gate asks "is this a RecordType", so a scalar element and a leg
	// that states nothing both fail it and both reach the scavenger. A census that
	// inherited that test would report every unnest element as a defect. The gate
	// is deliberately NOT tightened — an unnest element over an array of STRUCTS is
	// a genuine whole-object element whose type is UNKNOWN because Go does not
	// infer array element types that far, so demanding a stated type there breaks
	// `SELECT "X" FROM TS, TS."ITEMS" AS "X"` (measured). The discrimination lives
	// here, in the counting, not in the predicate.
	MergeSlotsScalar int
	// MergeSlotsUntyped: the slot states NO type at all. The residual, and the
	// defect: the reference degrades to source-relative, a source-relative operand
	// pushed into a leg's scan evaluates to NULL against the build-bound row, and
	// the join returns zero rows with no error.
	//
	// It cannot be separated from a struct-array element by type alone — both are
	// UNKNOWN, for unrelated reasons — so this counter is an UPPER BOUND on the
	// residual rather than the residual itself, and it is reported rather than
	// asserted at zero for exactly that reason. What it does buy is a number that
	// MOVES: the previous state of this question was an argument about which shapes
	// could reach the fallback, and an argument is what a change to the fallback
	// invalidates without telling anyone.
	MergeSlotsUntyped int
	// MergeSlots is the denominator the four above must partition.
	MergeSlots int
	// The MINTED reads classified a SECOND way, orthogonally to the three
	// reasons above: by whether the read ITSELF states a column identity in its
	// own leg's domain.
	//
	// The three reasons above ask about the LAYOUT — is there a row type for
	// this leg, and does it declare a column of this name. That is the question
	// CQ-63 was gated on, and it is now answered (layoutAvailable is the whole
	// mint population). It is NOT the question a leg-local bake turns on.
	//
	// A bake needs an ORDINAL, and there are exactly two places one can come
	// from: the read's own resolved path, or a fresh resolution of the read's
	// DISPLAY NAME against the layout. The second is the move that was deleted
	// from this arm — RFC-197's forbidden one, a name deciding a column's
	// identity, and renaming the helper that performs it does not change what it
	// does. So "the layout is available" does not imply "a bake is available":
	// it implies it only for reads that already carry an identity, and these
	// three counters are what separate those from the rest.
	//
	// IdentityInLegDomain: the read's own resolved path states a non-negative
	// ordinal in a KNOWN domain that IS the leg's row layout (legSlotIdentity
	// answers). These reads can bake with no name consulted anywhere.
	IdentityInLegDomain int
	// IdentityOtherDomain: the read carries a resolved path, but not one this
	// leg's layout can read — a different domain, a fused multi-accessor path,
	// or a name-only negative ordinal. Baking it against the leg's row would
	// index a layout the ordinal does not address, which is the ordinal
	// conflation the domain token exists to prevent.
	IdentityOtherDomain int
	// LazyNameOnly: the read carries NO resolved path at all. Its display name
	// is the only thing it states, so the only bake available to it is the
	// deleted one. A residue here is not a gap in this arm — it is a reference
	// that reached the planner unresolved, and it closes at the producer that
	// minted it, not here.
	LazyNameOnly int
	// LegDerivations counts every leg that ENTERED the layout derivation with a
	// stated alias. It is the denominator the three per-leg outcomes above must
	// sum to, and it exists so they can be asserted as a PARTITION rather than
	// as three numbers that happen to be printed together.
	//
	// Without it, "underivable 82" is a count with no denominator: it reads as
	// small next to 846 and would read exactly the same if the derivation had
	// stopped running on all but 82 legs. The acceptance number for CQ-63 is a
	// RATIO, so the ratio's other half has to be measured too.
	LegDerivations int
}

var (
	legLocalBakeMu        sync.Mutex
	legLocalBakeCounts    legLocalBakeCounters
	legLocalBakeWitnesses []string
)

// recordLegLocalBakeability classifies one firing of the leg-match arm.
//
// outcome is what the arm DID, stated by the arm. legTyp is the layout available
// for the leg (the quantifier's flowed row type when one could be stated,
// otherwise the reference's own untyped quantifier object), and column is the
// read's column name as the arm folds it into the qualified key. Deliberately
// takes the SAME inputs the arm itself has in hand, so the census cannot answer a
// different question than the one the arm decides. available names the leg layouts
// the caller could state. It is carried into the UNTYPED-LEG witness because the
// interesting case is not "no layouts existed" but "layouts existed and this leg
// was not among them" — a reference correlated to an alias no chosen leg carries,
// which is a different defect from a leg whose quantifier yields no row type.
func recordLegLocalBakeability(outcome legLocalBakeOutcome, leg values.CorrelationIdentifier, legTyp values.Type, column string, available ...string) {
	class := classifyLegLocalBake(outcome, legTyp, column)
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.Total++
	switch class {
	case legLocalBakeClassBaked:
		legLocalBakeCounts.Baked++
		// A witness, not a bare increment. Baked is the population the qualified
		// name channel's retirement rests on, and a count alone cannot say WHICH
		// reads reached it — which is the fact a reviewer of that retirement needs,
		// and the fact a shape-level test has to be able to find its own firing by.
		addLegLocalBakeWitness(fmt.Sprintf("LEG-LOCAL %s.%s (kept its own correlation "+
			"and its own ordinal; leg type %v)", leg.Name(), column, legTyp))
		return
	case legLocalBakeClassDeclined:
		legLocalBakeCounts.Declined++
		addLegLocalBakeWitness(fmt.Sprintf("DECLINED %s.%s (states no identity — the "+
			"reference reached the planner unresolved; leg type %v)",
			leg.Name(), column, legTyp))
		return
	case legLocalBakeClassUntypedLeg:
		legLocalBakeCounts.Minted++
		legLocalBakeCounts.UntypedLeg++
		addLegLocalBakeWitness(fmt.Sprintf("UNTYPED-LEG %s.%s (leg type %v; derivable leg layouts %v)", leg.Name(), column, legTyp, available))
	case legLocalBakeClassColumnAbsent:
		legLocalBakeCounts.Minted++
		legLocalBakeCounts.ColumnAbsent++
		addLegLocalBakeWitness(fmt.Sprintf("COLUMN-ABSENT %s.%s (leg columns %v)", leg.Name(), column, recordTypeFieldNames(legTyp.(*values.RecordType))))
	case legLocalBakeClassLayoutAvailable:
		legLocalBakeCounts.Minted++
		legLocalBakeCounts.LayoutAvailable++
		addLegLocalBakeWitness(fmt.Sprintf("LAYOUT-AVAILABLE-BUT-MINTED %s.%s (leg columns %v)", leg.Name(), column, recordTypeFieldNames(legTyp.(*values.RecordType))))
	}
}

// recordRebaseOuterLegArm records ONE firing of rebaseOuterLegValue's leg-match
// arm, from the arm that decided it.
//
// It is one entry point rather than the two the arm used to call in sequence,
// and that is not tidying: the two calls used to run BEFORE the arms, so the
// census stated an outcome the function had not decided yet. That was harmless
// only while every path produced the same outcome. It stops being harmless the
// moment the arm has a second disposition — which is exactly what the
// pass-through is — because the census would then report a mint for every read
// the arm handed back untouched, and the number the channel's retirement rests
// on would be the number of FIRINGS rather than the number of MINTS.
//
// The outcome is STATED by the deciding arm and the identity cut is PASSED
// rather than recomputed, for one reason in both cases: a census that
// re-derives its own subject answers a question the code did not ask.
//
// The census gate is checked here, before the legLocalTypes probe, because the
// gate's stated contract is that a disabled census costs the planner nothing
// and legLocalTypes is consulted for no other purpose at this site.
func recordRebaseOuterLegArm(
	outcome legLocalBakeOutcome,
	fv *values.FieldValue,
	qov *values.QuantifiedObjectValue,
	legLocalTypes map[values.CorrelationIdentifier]*values.RecordType,
	identityInLegDomain bool,
) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	legTypeFor, haveLegType := legLocalTypes[qov.Correlation]
	column := strings.ToUpper(fv.Field)
	recordLegLocalBakeability(outcome, qov.Correlation,
		legTypeOrUntyped(legTypeFor, haveLegType, qov.Typ),
		column, legLocalTypeKeys(legLocalTypes)...)
	// The three identity counters partition MINTED reads and nothing else, so a
	// baked read must not reach them — see the partition assertion that pins it.
	if outcome != legLocalBakeMinted {
		return
	}
	why := ""
	if !identityInLegDomain {
		why = describeLegIdentityDecline(fv, qov, legTypeFor, haveLegType)
	}
	recordMintedReadIdentity(qov.Correlation, column,
		fv.Resolved != nil, identityInLegDomain, why)
}

// mintedReadIdentity is what a MINTED read states about its OWN column
// identity. See the three counters it feeds for why this is a different
// question from the layout one beside it.
type mintedReadIdentity int

const (
	// mintedReadIdentityInLegDomain: legSlotIdentity answered — a resolved,
	// non-negative ordinal in a known domain that IS the leg's row layout.
	mintedReadIdentityInLegDomain mintedReadIdentity = iota
	// mintedReadIdentityOtherDomain: a resolved path this leg's layout cannot
	// read.
	mintedReadIdentityOtherDomain
	// mintedReadLazyNameOnly: no resolved path; the display name is all there is.
	mintedReadLazyNameOnly
)

// classifyMintedReadIdentity decides which of the three a minted read is, from
// the two facts the arm has in hand: whether the read carries a resolved path
// at all, and whether that path states an identity IN THE LEG'S OWN DOMAIN
// (legSlotIdentity's answer, passed rather than re-derived so the census cannot
// answer a different question than the bake would ask).
//
// The ordering matters and is the whole content: a read that states an identity
// in the leg's domain is that, whatever else is true of it. Only then does the
// absence of a resolved path separate the two residues, which have different
// owners — an other-domain path is a reference baked against the wrong layout
// (a producer stating a domain it did not resolve against), a lazy one is a
// reference that was never resolved at all.
func classifyMintedReadIdentity(hasResolved, identityInLegDomain bool) mintedReadIdentity {
	if identityInLegDomain {
		return mintedReadIdentityInLegDomain
	}
	if hasResolved {
		return mintedReadIdentityOtherDomain
	}
	return mintedReadLazyNameOnly
}

// recordMintedReadIdentity classifies one MINTED read by what it states about
// its own identity. Called from the same guard, and only for reads the arm
// minted, so the three counters partition Minted exactly.
func recordMintedReadIdentity(leg values.CorrelationIdentifier, column string, hasResolved, identityInLegDomain bool, why string) {
	class := classifyMintedReadIdentity(hasResolved, identityInLegDomain)
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	switch class {
	case mintedReadIdentityInLegDomain:
		legLocalBakeCounts.IdentityInLegDomain++
	case mintedReadIdentityOtherDomain:
		legLocalBakeCounts.IdentityOtherDomain++
		addLegLocalBakeWitness(fmt.Sprintf("MINTED-OTHER-DOMAIN %s.%s (%s)", leg.Name(), column, why))
	case mintedReadLazyNameOnly:
		legLocalBakeCounts.LazyNameOnly++
		addLegLocalBakeWitness(fmt.Sprintf("MINTED-LAZY %s.%s (no resolved path — the display name is the only identity it states)", leg.Name(), column))
	}
}

// describeLegIdentityDecline says WHY legSlotIdentity refused a minted read, in
// the terms OrdinalIn fails closed on, plus the one fact that decides whether
// the refusal is fixable HERE or upstream: whether the leg's separately-derived
// row layout (the quantifier's flowed type, which the layout census reports
// available) would have accepted the read's ordinal.
//
// The distinction it exists to draw: a read whose ordinal addresses NO known
// layout is a producer defect, and a read whose ordinal addresses the leg's
// layout but whose own QOV cannot say so is a TYPE-PLUMBING gap — the reference
// carries the right ordinal and the wrong domain token. Those have different
// fixes and neither is "resolve the display name".
func describeLegIdentityDecline(fv *values.FieldValue, qov *values.QuantifiedObjectValue, legType *values.RecordType, haveLegType bool) string {
	var parts []string
	if fv.Resolved == nil {
		return "no resolved path"
	}
	parts = append(parts, fmt.Sprintf("accessors=%d", len(fv.Resolved.Accessors)))
	if len(fv.Resolved.Accessors) > 0 {
		parts = append(parts, fmt.Sprintf("rootOrdinal=%d", fv.Resolved.Root().Ordinal))
	}
	parts = append(parts, fmt.Sprintf("pathDomainKnown=%t", fv.Resolved.Domain.IsKnown()))
	qovDomain := values.OrdinalDomainOfType(qov.Typ)
	parts = append(parts, fmt.Sprintf("qovTypeDomainKnown=%t", qovDomain.IsKnown()))
	if qovDomain.IsKnown() && fv.Resolved.Domain.IsKnown() {
		parts = append(parts, fmt.Sprintf("qovDomainMatches=%t", qovDomain == fv.Resolved.Domain))
	}
	// The decisive one: the leg layout the census already reports AVAILABLE.
	if haveLegType && legType != nil {
		legDomain := values.OrdinalDomainOfType(legType)
		parts = append(parts, fmt.Sprintf("legTypeDomainKnown=%t", legDomain.IsKnown()))
		if legDomain.IsKnown() && fv.Resolved.Domain.IsKnown() {
			parts = append(parts, fmt.Sprintf("legTypeDomainMatches=%t", legDomain == fv.Resolved.Domain))
		}
	} else {
		parts = append(parts, "noLegType")
	}
	return strings.Join(parts, " ")
}

// legLocalBakeClass is one firing's bucket.
type legLocalBakeClass int

const (
	legLocalBakeClassBaked legLocalBakeClass = iota
	legLocalBakeClassDeclined
	legLocalBakeClassUntypedLeg
	legLocalBakeClassColumnAbsent
	legLocalBakeClassLayoutAvailable
)

// classifyLegLocalBake is the census's decision, split out from the counter
// mutation so it can be exercised without touching process-global state.
//
// The OUTCOME dominates: a read the arm minted is a minted read whatever its leg
// type says. The layout classification only ever refines a MINTED read, and that
// ordering is the whole content of this function — inverting it is how a census
// comes to report a live channel as retired.
func classifyLegLocalBake(outcome legLocalBakeOutcome, legTyp values.Type, column string) legLocalBakeClass {
	switch outcome {
	case legLocalBakeBaked:
		return legLocalBakeClassBaked
	case legLocalBakeDeclined:
		return legLocalBakeClassDeclined
	}
	rt, isRT := legTyp.(*values.RecordType)
	if !isRT {
		return legLocalBakeClassUntypedLeg
	}
	if _, found := rt.FieldIndex(column); !found {
		return legLocalBakeClassColumnAbsent
	}
	return legLocalBakeClassLayoutAvailable
}

// addLegLocalBakeWitness retains distinct witnesses up to the cap. Caller holds
// the mutex.
func addLegLocalBakeWitness(w string) {
	if len(legLocalBakeWitnesses) >= legLocalBakeWitnessCap {
		return
	}
	for _, seen := range legLocalBakeWitnesses {
		if seen == w {
			return
		}
	}
	legLocalBakeWitnesses = append(legLocalBakeWitnesses, w)
}

// legTypeOrUntyped picks the layout the census should classify a fallen-through
// read against: the leg's stated row type when the caller could derive one (the
// read is then classified COLUMN-ABSENT or LAYOUT-AVAILABLE), otherwise the
// reference's own untyped quantifier object (the read is UNTYPED-LEG — no layout
// existed at all). Distinguishing the two is the whole point: they are different
// residues with different fixes.
func legTypeOrUntyped(legType *values.RecordType, haveLegType bool, refType values.Type) values.Type {
	if haveLegType && legType != nil {
		return legType
	}
	return refType
}

// recordLegDerivation counts one leg entering the layout derivation. Called
// once per stated-alias leg, before any of the four outcomes is decided, so the
// outcomes can be asserted to partition it.
func recordLegDerivation() {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.LegDerivations++
}

// recordUnderivableLegLayout names a leg whose quantifier states no row layout,
// by the shape that declined.
//
// This is the residue's cause rather than its symptom. The bakeability counters
// say how many reads still need the qualified name; this says WHY — which shapes
// the layout derivation has no answer for. Kept separate from the counters
// because it is per-LEG, not per-READ: one underivable leg accounts for every
// read correlated to it.
//
// shape is a RENDERED description, not the value itself, because the two call
// sites do not discriminate the same way. A leg that HAS a plan is discriminated
// by the plan's concrete type (`*plans.RecordQueryFlatMapPlan` is the finding).
// A leg with NO plan is discriminated by nothing at all under `%T`:
// expressions.Quantifier is a STRUCT, so every such witness renders the identical
// type name and the whole no-plan population collapses to one witness per alias —
// a census line that says only "some leg declined", which is what it was already
// counting. Rendering is therefore the caller's, and the no-plan caller renders
// the quantifier's own members (see describeLegQuantifier).
func recordUnderivableLegLayout(alias values.CorrelationIdentifier, shape string, escape string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.UnderivableLegs++
	addLegLocalBakeWitness(fmt.Sprintf("NO-LAYOUT leg %s: %s states no row layout [%s]", alias.Name(), shape, escape))
}

// recordMergeSlotTyping counts one positional-merge slot by what it ended up
// stating. Called once per collapsed leg per merge, after the fallback chain has
// run, so the four outcomes partition the slots.
//
// slotType is the slot's FINAL type, which is what the merged record constructor
// will carry. The classification is deliberately finer than the site's own gate —
// see MergeSlotsScalar for why the gate cannot make this distinction and must not
// be changed to try.
func recordMergeSlotTyping(alias values.CorrelationIdentifier, slotType values.Type, scavenged bool) {
	class := classifyMergeSlot(slotType, scavenged)
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.MergeSlots++
	switch class {
	case mergeSlotClassScavenged:
		legLocalBakeCounts.MergeSlotsScavenged++
		addLegLocalBakeWitness(fmt.Sprintf("SCAVENGED-MERGE-SLOT leg %s: the quantifier "+
			"stated no row and a baked reference in the select supplied one", alias.Name()))
	case mergeSlotClassTyped:
		legLocalBakeCounts.MergeSlotsTyped++
	case mergeSlotClassUntyped:
		legLocalBakeCounts.MergeSlotsUntyped++
		addLegLocalBakeWitness(fmt.Sprintf("UNTYPED-MERGE-SLOT leg %s: neither the "+
			"quantifier nor the select's own value surfaces state anything. Either a leg "+
			"whose row is lost (the defect) or a whole-object element over an array of "+
			"STRUCTS, whose element type Go does not infer (correct) — the two are not "+
			"separable by type", alias.Name()))
	case mergeSlotClassScalar:
		legLocalBakeCounts.MergeSlotsScalar++
	}
}

// mergeSlotClass is one merge slot's bucket. The four partition MergeSlots.
type mergeSlotClass int

const (
	mergeSlotClassScavenged mergeSlotClass = iota
	mergeSlotClassTyped
	mergeSlotClassUntyped
	mergeSlotClassScalar
)

// String names the bucket. The classifier's failure mode is two arms swapping, so
// a diagnostic that renders them as bare ordinals reports the mix-up in the one
// notation that makes it hardest to read.
func (c mergeSlotClass) String() string {
	switch c {
	case mergeSlotClassScavenged:
		return "Scavenged"
	case mergeSlotClassTyped:
		return "Typed"
	case mergeSlotClassUntyped:
		return "Untyped"
	case mergeSlotClassScalar:
		return "Scalar"
	default:
		return fmt.Sprintf("mergeSlotClass(%d)", int(c))
	}
}

// classifyMergeSlot is the merge-slot census's decision, split out from the
// counter mutation so it can be exercised without touching process-global state.
//
// Two orderings are the content of this function. Scavenged DOMINATES: a slot the
// select's own surfaces rescued is a scavenged slot whatever type it ended up
// holding, and folding it into Typed would report the quantifier stating a row it
// never stated. And UNTYPED is decided against Typed's complement, not against
// Scalar's: an unstated type and a stated non-row type are different findings —
// untyped is the residue that may be a lost leg row, scalar is a slot the mixed
// gate deliberately admits — and collapsing them is how a lost leg row comes to be
// counted as an ordinary scalar and disappears from the residue the acceptance
// number reads.
func classifyMergeSlot(slotType values.Type, scavenged bool) mergeSlotClass {
	switch {
	case scavenged:
		return mergeSlotClassScavenged
	case isRecordSlotType(slotType):
		return mergeSlotClassTyped
	case slotType == nil || slotType.Code() == values.TypeCodeUnknown:
		return mergeSlotClassUntyped
	default:
		return mergeSlotClassScalar
	}
}

// isRecordSlotType reports whether a merge slot states a ROW.
//
// NOT values.IsMixedSeedElementType, and the difference is the point: that
// predicate answers "is this the seed's whole-object element", and it admits an
// UNTYPED value on purpose, because a struct-array element is untyped for a reason
// that has nothing to do with a leg losing its row. This census has to tell those
// two apart, so it asks a narrower question — does the slot state a record — and
// splits the rest by whether anything at all is stated.
func isRecordSlotType(t values.Type) bool {
	_, isRecord := t.(*values.RecordType)
	return isRecord
}

// recordFlowedLegLayout names a leg the one authority stated on its own.
func recordFlowedLegLayout(alias values.CorrelationIdentifier, rt *values.RecordType) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.FlowedLegs++
	addLegLocalBakeWitness(fmt.Sprintf("FLOWED leg %s: %d columns %v", alias.Name(), len(rt.Fields), recordTypeFieldNames(rt)))
}

// recordLegTypeDisagreement names a leg whose reference members flow different
// row types. Java Verify-fails on this (Reference.java:504-513); Go surfaces it
// as an error the caller declines on, and the census counts it so a memo defect
// cannot hide inside the underivable residue.
func recordLegTypeDisagreement(alias values.CorrelationIdentifier, err error) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.DisagreeingLegs++
	addLegLocalBakeWitness(fmt.Sprintf("MEMBER-DISAGREEMENT leg %s: %v", alias.Name(), err))
}

func recordTypeFieldNames(rt *values.RecordType) []string {
	out := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		out[i] = f.Name
	}
	return out
}

// LegLocalBakeCensus reports the counters and the retained witnesses.
//
// The acceptance number for CQ-63 is UnderivableLegs OVER LegDerivations, and
// the denominator is not decoration. While a leg states no row layout, every
// read correlated to it has no ordinal it could honestly carry, so the qualified
// name is the only channel available to it — but "underivable is down to 8" says
// nothing on its own, because it falls identically whether the derivation got
// better or stopped being called. Quote the pair.
//
// UntypedLeg is that same residue counted per READ. LayoutAvailable is the reads
// a restored leg-local bake could convert; it is the number that has to become
// the total before the channel can retire, and it is reported separately from
// Baked precisely so a future typed-but-still-minting state reads as what it is.
//
// These counts are ASSERTED, not merely reported — see AssertLegLocalBakeCensus,
// which the sqldriver TestMain runs over the whole corpus. Three partitions
// (Total = Baked+Minted; Minted = the three mint reasons; LegDerivations = the
// three per-leg outcomes) plus a population floor on both denominators. Before
// that existed the census was printed and nothing checked it, so every number
// above could drift or collapse to zero with the suite still green — and a
// residue that reaches zero by its arm going UNREACHED is indistinguishable,
// in a printed report, from one that reaches zero by being fixed.
func LegLocalBakeCensus() (legLocalBakeCounters, []string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	out := make([]string, len(legLocalBakeWitnesses))
	copy(out, legLocalBakeWitnesses)
	return legLocalBakeCounts, out
}

// FormatLegLocalBakeCensus renders the census for a harness to log.
func FormatLegLocalBakeCensus() string {
	c, witnesses := LegLocalBakeCensus()
	var b strings.Builder
	fmt.Fprintf(&b, "leg-local bakeability: total %d (baked %d, minted %d, declined %d); "+
		"minted residue: untypedLeg %d, columnAbsent %d, layoutAvailable %d; "+
		"legs: flowed %d, underivable %d, memberDisagreement %d",
		c.Total, c.Baked, c.Minted, c.Declined,
		c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable,
		c.FlowedLegs, c.UnderivableLegs, c.DisagreeingLegs)
	fmt.Fprintf(&b, "; minted reads by OWN identity: identityInLegDomain %d, "+
		"identityOtherDomain %d, lazyNameOnly %d",
		c.IdentityInLegDomain, c.IdentityOtherDomain, c.LazyNameOnly)
	fmt.Fprintf(&b, "; legDerivations %d", c.LegDerivations)
	fmt.Fprintf(&b, "; mergeSlots %d (typed %d, scavenged %d, scalar %d, untyped %d)",
		c.MergeSlots, c.MergeSlotsTyped, c.MergeSlotsScavenged, c.MergeSlotsScalar, c.MergeSlotsUntyped)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  distinct witnesses (%d, cap %d):\n    %s",
			len(sorted), legLocalBakeWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

// LegLocalBakeFloors is the minimum population the bakeability census must
// report over a whole suite run, for the two totals every other number in it is
// a share of.
//
// The reason it exists is the reason its sibling's floors exist, stated by that
// one at length: an acceptance number that can reach zero by its arm going
// UNREACHED is not an acceptance number. UnderivableLegs is CQ-63's gate. It
// falls when the derivation gets better, and it falls exactly as fast when the
// derivation stops being called at all — and only one of those is progress. A
// bare "underivable is down to 8" reads as success in both cases.
//
// Set an order of magnitude below the measured populations, matching the
// leg-identity floors and for the same reason: these counts are RULE FIRINGS,
// the memo may explore one rule once or many times per query depending on
// exploration order, and the corpus grows and shrinks with unrelated work. What
// a floor detects is COLLAPSE, not drift.
type LegLocalBakeFloors struct {
	Total          int
	LegDerivations int
	// MergeSlots is floored for the same reason the other two are:
	// MergeSlotsUntyped is a SHARE of it, and a share of a collapsed denominator
	// reads as progress while measuring nothing.
	MergeSlots int
}

// AssertLegLocalBakeCensus checks the bakeability census's invariants and
// reports whether it failed, mirroring values.AssertLegIdentityCensus.
//
// The three partition checks are the point. Every other number this census
// prints is a SHARE of one of two totals, and a share is only meaningful if the
// shares add up: if Minted and the three mint-reason counters drift apart, the
// residue can be read off the same report two ways and get two answers. That is
// not a hypothetical failure mode for a census whose numbers are quoted into an
// RFC as migration arithmetic — it is the ordinary consequence of adding a
// fourth reason and forgetting one call site.
//
// floors is nil when the run is NARROWED (a -test.run filter), because the
// floors describe a whole-suite population. The partition checks are NOT
// dropped: they hold over any population, one firing or eighty thousand, so a
// filtered invocation checks them exactly as the full suite does.
func AssertLegLocalBakeCensus(w io.Writer, floors *LegLocalBakeFloors) bool {
	c, _ := LegLocalBakeCensus()
	return assertLegLocalBakeCounters(w, c, floors)
}

// assertLegLocalBakeCounters is AssertLegLocalBakeCensus's decision, split out
// from the process-global read exactly as classifyLegLocalBake is split out from
// the counter mutation, and for the same reason: a gate is a claim about which
// counter states FAIL, and a claim reachable only by driving the whole planner
// into a defective state is a claim nothing pins. Every zero and every partition
// below can now be shown red on the counter state that violates it.
func assertLegLocalBakeCounters(w io.Writer, c legLocalBakeCounters, floors *LegLocalBakeFloors) bool {
	failed := false

	if got := c.Baked + c.Minted + c.Declined; got != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: Baked(%d) + Minted(%d) + Declined(%d) = %d, "+
			"but Total = %d.\n"+
			"  Every firing of the leg-match arm is exactly one of the three, so a gap\n"+
			"  means a firing was counted into Total and classified into none — or a new\n"+
			"  outcome was added without a counter. The residue percentages this census\n"+
			"  feeds are computed against Total, so they are wrong by exactly the gap.\n",
			c.Baked, c.Minted, c.Declined, got, c.Total)
	}

	if got := c.UntypedLeg + c.ColumnAbsent + c.LayoutAvailable; got != c.Minted {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: UntypedLeg(%d) + ColumnAbsent(%d) + "+
			"LayoutAvailable(%d) = %d, but Minted = %d.\n"+
			"  The three reasons classify MINTED reads and nothing else, so they must\n"+
			"  partition Minted exactly. LayoutAvailable is the number CQ-63 has to\n"+
			"  move; if the reasons do not sum, it is a share of an unknown whole.\n",
			c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable, got, c.Minted)
	}

	if got := c.IdentityInLegDomain + c.IdentityOtherDomain + c.LazyNameOnly; got != c.Minted {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: IdentityInLegDomain(%d) + "+
			"IdentityOtherDomain(%d) + LazyNameOnly(%d) = %d, but Minted = %d.\n"+
			"  These three classify MINTED reads by what the read states about its OWN\n"+
			"  identity, so they must partition Minted exactly — the same population the\n"+
			"  three layout reasons partition, cut a second way. IdentityInLegDomain is\n"+
			"  the number a leg-local bake can actually convert WITHOUT resolving a\n"+
			"  display name; if the three do not sum, that number is a share of an\n"+
			"  unknown whole and the bake's reach is unmeasured.\n",
			c.IdentityInLegDomain, c.IdentityOtherDomain, c.LazyNameOnly, got, c.Minted)
	}

	if got := c.FlowedLegs + c.UnderivableLegs + c.DisagreeingLegs; got != c.LegDerivations {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: Flowed(%d) + Underivable(%d) + "+
			"Disagreeing(%d) = %d, but LegDerivations = %d.\n"+
			"  These three are the only outcomes a leg entering the derivation can\n"+
			"  reach, so they must partition it. A leg that reaches none of them left\n"+
			"  the derivation by a path with no counter on it, and UnderivableLegs —\n"+
			"  CQ-63's acceptance number — is then a count of the paths that ARE\n"+
			"  instrumented rather than of the legs that cannot state a layout.\n",
			c.FlowedLegs, c.UnderivableLegs, c.DisagreeingLegs,
			got, c.LegDerivations)
	}

	if got := c.MergeSlotsTyped + c.MergeSlotsScavenged + c.MergeSlotsScalar + c.MergeSlotsUntyped; got != c.MergeSlots {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: MergeSlotsTyped(%d) + Scavenged(%d) + "+
			"Scalar(%d) + Untyped(%d) = %d, but MergeSlots = %d.\n"+
			"  These four are the only things a positional-merge slot can end up stating,\n"+
			"  so they must partition it. MergeSlotsUntyped is an UPPER BOUND on the one\n"+
			"  remaining path by which a leg's row type is silently lost here, and a bound\n"+
			"  is only a bound while its siblings account for the rest.\n",
			c.MergeSlotsTyped, c.MergeSlotsScavenged, c.MergeSlotsScalar, c.MergeSlotsUntyped,
			got, c.MergeSlots)
	}

	// The two counters CQ-63 moved, asserted at the value it moved them TO.
	//
	// A census that only reports these lets them drift back silently, and drifting
	// back is not exotic: both are "the quantifier could not state the row it
	// flows", and every one of them was a site handing expression construction an
	// untyped QuantifiedObjectValue where Java's is
	// `QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`
	// (Quantifier.java:801-803). One new untyped flowed value anywhere on this path
	// puts them back.
	//
	// These are ZEROS, not floors. A zero is only a claim about the population that
	// was actually reached, which is why the population floors below exist and why
	// a narrowed run announces the population it checked instead of claiming these
	// hold universally: at zero population every one of them holds VACUOUSLY. The
	// two checks are a proof only together.
	for _, z := range []struct {
		name string
		got  int
		why  string
	}{
		{
			"UnderivableLegs", c.UnderivableLegs,
			"A leg whose row layout cannot be derived has no ordinal it can honestly\n" +
				"  carry on its own alias, so every read through it falls through to the\n" +
				"  qualified NAME — the channel RFC-197 exists to remove, kept alive by a\n" +
				"  quantifier that will not say what it flows. With the subordinate\n" +
				"  physical-plan walk retired this is also the ONLY residue counter left on\n" +
				"  the derivation: there is no second authority to absorb a leg the\n" +
				"  quantifier stops stating, so a regression lands here rather than\n" +
				"  disappearing into a fallback.",
		},
		{
			"Declined", c.Declined,
			"A leg-correlated read reached the rebase arm stating NO column identity —\n" +
				"  no resolved path at all, so its display name is the only thing it\n" +
				"  carries. The arm used to mint `QOV(merged).\"LEG.COL\"` for exactly this\n" +
				"  case; that mint is DELETED, because a fallback that spells a name is how\n" +
				"  the RFC-197 channel survives the migration meant to end it. The defect is\n" +
				"  at the PRODUCER that built the reference unresolved, and no rewrite here\n" +
				"  can supply what the producer did not — find the producer, do not restore\n" +
				"  the mint.",
		},
		{
			"UntypedLeg", c.UntypedLeg,
			"A minted read that declined because the LEG quantifier stated no type. Same\n" +
				"  defect as the above seen from the mint side: the reason there is no\n" +
				"  layout to bake against.",
		},
		{
			// A leg whose GetFlowedObjectType ERRORS gets no layout, exactly like an
			// underivable one — and lands in the one bucket of the partition nothing
			// asserted. Its cause is worse than an underivable leg's, not better: two
			// members of one equivalence class flowing different rows is Java's Verify
			// failure (Reference.java:504-513), a memo defect rather than a gap in
			// inference. Leaving it unasserted meant the partition could stay exact
			// while legs migrated from FlowedLegs into it, and the only visible effect
			// would have been FlowedLegs drifting down against a floor set an order of
			// magnitude below it.
			"DisagreeingLegs", c.DisagreeingLegs,
			"Two members of one equivalence class flowing DIFFERENT row shapes. The\n" +
				"  derivation declines the leg — picking a member picks a row layout by memo\n" +
				"  insertion order — so the leg contributes no layout and every read through\n" +
				"  it falls back to the qualified name, the same cost as an underivable leg\n" +
				"  for a strictly worse reason. Java Verify-fails here; Go counts it, and\n" +
				"  asserting the count is what keeps a memo defect from hiding inside a\n" +
				"  partition that still adds up.",
		},
	} {
		if z.got != 0 {
			failed = true
			fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: %s = %d, want 0.\n  %s\n"+
				"  Look for a flowed object value built UNTYPED — Quantifier.GetFlowedObjectValue\n"+
				"  where GetFlowedObjectValueTyped is what the site needs.\n",
				z.name, z.got, z.why)
		}
	}

	if floors != nil {
		for _, f := range []struct {
			name  string
			got   int
			floor int
		}{
			{"Total", c.Total, floors.Total},
			{"LegDerivations", c.LegDerivations, floors.LegDerivations},
			{"MergeSlots", c.MergeSlots, floors.MergeSlots},
		} {
			if f.got < f.floor {
				failed = true
				fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: %s = %d, want >= %d.\n"+
					"  The census reached a population it cannot describe the corpus from.\n"+
					"  Every other number it prints is a share of this one, so a collapse\n"+
					"  here makes the residue counts — including UnderivableLegs, which\n"+
					"  gates CQ-63 — look like PROGRESS while measuring nothing. Either the\n"+
					"  shapes that drive this arm stopped being planned, or the arm stopped\n"+
					"  being reached. Find out which before adjusting this floor.\n",
					f.name, f.got, f.floor)
			}
		}
	}
	return failed
}

func legLocalTypeKeys(m map[values.CorrelationIdentifier]*values.RecordType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.Name())
	}
	sort.Strings(out)
	return out
}
