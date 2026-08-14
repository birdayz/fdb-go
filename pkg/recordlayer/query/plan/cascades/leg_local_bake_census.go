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
	// legLocalBakeMergedReAnchor: the read was re-anchored onto the merge correlation as
	// `QOV(merged)."LEG.COL"`. The only outcome this arm produces today.
	legLocalBakeMergedReAnchor legLocalBakeOutcome = iota
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

// legRebaseSite is WHICH lowering reached the leg-match arm. Threaded from the
// two entry points rather than reconstructed, because reconstructing it means
// asking the arm which of its callers it has — and it has no way to know.
//
// It exists because the SPLIT is a live claim. Phase 3 redirected on it: the
// booking said the arm was reached "without a stated merged layout on the
// EXISTS-over-join and RFC-153 buried-leg paths", and the measurement said all
// of the firings are the EXISTS site and NONE are the buried site — which also
// states a layout on 314 of 362 firings. DIVERGENCES.md and TODO.md now assert
// that split in prose. A number asserted in prose and checked by nothing is the
// failure mode this whole census family exists to prevent: change the lowering
// so a buried leg reaches this arm, and both documents go on asserting a zero
// that stopped being true, with every gate green.
type legRebaseSite int

const (
	// legRebaseSiteUnknown is deliberately the ZERO value and is counted into
	// NEITHER site. A third entry point that threads a forgotten zero-valued
	// site then increments Total without either site counter, and the partition
	// assertion (SiteExists + SiteBuried == Total) goes red — instead of the
	// newcomer being silently counted as EXISTS, which is what a zero-valued
	// legRebaseSiteExists would have done.
	legRebaseSiteUnknown legRebaseSite = iota
	// legRebaseSiteExists: implementJoinWithExistential's two-level NLJ→FlatMap
	// lowering, where the merged layout is ordinalSeedLegWindowsOf(step1RV) and
	// is nil exactly when foldStep1Seed declined.
	legRebaseSiteExists
	// legRebaseSiteBuried: buildCorrelatedFlatMapPlan's RFC-153 buried-preserved
	// leg rebase, which computes a layout with buriedLegOrdinalLayout.
	legRebaseSiteBuried
)

func (s legRebaseSite) String() string {
	switch s {
	case legRebaseSiteBuried:
		return "BURIED(buildCorrelatedFlatMapPlan)"
	case legRebaseSiteExists:
		return "EXISTS(implementJoinWithExistential)"
	}
	return "UNKNOWN-SITE"
}

// legRebaseOrigin is everything the leg-match arm needs to know about the firing
// that reached it, threaded from the entry point for the same reason the site
// alone was: the arm has no way to ask which of its callers it has.
//
// It carries a SECOND dimension beside the site, and that dimension is the one
// RFC-200 gate (d) is denominated in. The two instruments in play count
// different events at different sites — the bakeability census counts READS
// (once per leg-correlated FieldValue the walk matches), the foldStep1Seed
// outcome census counts RULE FIRINGS — so `102 + 108 = 210 ≠ 190` and
// neither can be apportioned into the other by arithmetic. The only thing that
// maps one onto the other is carrying the firing's class down to the read, which
// is what Step1 does.
type legRebaseOrigin struct {
	// Site is WHICH lowering reached the arm.
	Site legRebaseSite
	// Step1 is the enclosing firing's foldStep1Seed outcome, or
	// foldStep1ClassNone at the BURIED site, which makes no step-1 seed decision
	// at all. The zero value is that honest "none" rather than a misfiled class.
	Step1 foldStep1Class

	// Step1LegShape is the SHAPE of the leg that firing's seed reconstruction
	// refused, meaningful only when Step1 is foldStep1DeclineReconstructNil.
	//
	// The class alone is not enough for RFC-200 gate (d), and the measurement
	// that showed why is worth stating: over the real-FDB corpus EVERY
	// leg-local read occurs under a reconstruct-nil firing — but that class
	// splits by leg SHAPE at the FIRING level, and only the positional-merge half
	// was ever in RFC-200's scope. Denominating the gate in the class would demand
	// that the bare-QOV residue (§Residues, explicitly out of scope and LARGER)
	// also reach zero, which that change could not and did not claim to do.
	//
	// The split it was measured against was 94 bare-QOV / 60 positional-merge.
	// Both halves have since moved and the CURRENT measurement is 102 bare-QOV /
	// 0 positional-merge: RFC-200 converted its 60, and its own gate fixtures
	// added 8 more bare-QOV firings. The shape dimension is what makes that
	// legible — in the CLASS alone the total went 154 to 102 and the composition
	// change would have been invisible.
	Step1LegShape foldStep1LegShape
}

type legLocalBakeCounters struct {
	// Total is every firing of the leg-match arm.
	Total int
	// SiteExists / SiteBuried partition Total by WHICH lowering reached the arm.
	//
	// Measured 190 / 0 — and only the ZERO is durable; the denominator is a
	// corpus-sized number that has already moved once (174 → 190) without any
	// claim here changing. The zero is the half a
	// census cannot prove on its own — an unreached site and a site measured
	// empty print identically — so SiteExists is FLOORED and SiteBuried is
	// asserted zero with its re-arm condition named at the assertion.
	SiteExists int
	SiteBuried int
	// Step1 cuts Total a THIRD way, orthogonally to site and outcome: by the
	// foldStep1Seed class of the firing the read happened under.
	//
	// This is the read→firing MAPPING, and it exists because the two censuses on
	// this path count different events with different denominators and their
	// numbers do not sum. Before it, "this change moves 60 of the 174 reads" was
	// arithmetic over a category error. Step1[foldStep1DeclineReconstructNil] is
	// the population that can move at all; the sub-classification by refused-leg
	// shape lives in the outcome census, which is the instrument that can see it.
	//
	// Reads at the BURIED site file under foldStep1ClassNone, which is correct
	// rather than a gap: that lowering makes no step-1 seed decision.
	Step1 [foldStep1ClassCount]int
	// Step1ReconstructNilShape cuts the reconstruct-nil reads by the SHAPE of the
	// leg their firing refused. This is the number RFC-200 gate (d) is stated
	// against, and it is a strictly finer cut than Step1 above: the class holds
	// the whole reconstruct-nil population while only its positional-merge share
	// is in scope.
	Step1ReconstructNilShape [foldStep1LegShapeCount]int
	// Baked: reads that KEPT their own leg alias and their own leg-local ordinal
	// — the pass-through. The arm's live population.
	Baked int
	// MergedReAnchor: reads re-anchored onto the merge correlation at a MERGED
	// ordinal, with the leg's qualified name riding along as a display string.
	//
	// It is NAMED for the re-anchor and not for a mint, because a mint is not
	// what it counts. It was called Minted while the arm had a name-keyed mint
	// in it; that mint is deleted, and a counter that kept the deleted thing's
	// name would have read as "the RFC-197 channel is still firing" for a
	// population that is Java's own move (PartitionSelectRule.java:296-303) and
	// not a residue at all.
	//
	// Reported rather than asserted at zero: it is the legitimate arm. But it is
	// the only thing left here that spells a leg into a name, and the name it
	// spells is load-bearing for nothing — TestLazyLegMintReachesNoWinningPlan
	// measures zero dotted merged-row keys in the WINNING plan for every shape
	// that reaches here.
	MergedReAnchor int
	// Declined: reads that stated no identity at all. Asserted ZERO — see
	// legLocalBakeDeclined for why this is a producer's residue and not this
	// arm's.
	Declined int
	// The rest classify MERGED-RE-ANCHORED reads only — why that arm was reached.
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
	// EVERY firing classified a SECOND way, orthogonally to the three reasons
	// above: by whether the read ITSELF states a column identity in its own
	// leg's domain.
	//
	// THESE PARTITION Total, NOT MergedReAnchor, and that is a correction rather
	// than a widening. They partitioned MergedReAnchor when the merged re-anchor
	// was the arm's whole population. It is now ZERO — every read reaching this
	// site keeps its own leg alias and its own ordinal — so a cut of it is a cut
	// of nothing, and the partition assertion over it was 0 == 0 while the
	// instrument's whole purpose is to measure whether the live population can
	// state its identity. An instrument aimed at the population that left is not
	// an instrument.
	//
	// The three reasons above ask about the LAYOUT — is there a row type for
	// this leg, and does it declare a column of this name. That is the question
	// CQ-63 was gated on. It is NOT the question a leg-local bake turns on.
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
	// small next to a four-figure leg count and would read exactly the same if the
	// derivation had stopped running on all but 82 legs. The acceptance number for CQ-63 is a
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
// recordLegRebaseSite counts one firing against the lowering that reached the
// arm. Separate from recordLegLocalBakeability so the site dimension is recorded
// once per FIRING at the one place that has the site in hand — the arm recorder
// — rather than being inferred from anything the classification already did.
// The two cuts partition the same denominator and neither constrains the other,
// which is the property the partition assertion checks.
func recordLegRebaseSite(origin legRebaseOrigin) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	switch origin.Site {
	case legRebaseSiteExists:
		legLocalBakeCounts.SiteExists++
	case legRebaseSiteBuried:
		legLocalBakeCounts.SiteBuried++
	}
	// legRebaseSiteUnknown (the zero value) is deliberately counted into
	// neither: the partition assertion turns it red rather than misfiling it.
	if origin.Step1 >= 0 && origin.Step1 < foldStep1ClassCount {
		legLocalBakeCounts.Step1[origin.Step1]++
	}
	if origin.Step1 == foldStep1DeclineReconstructNil &&
		origin.Step1LegShape >= 0 && origin.Step1LegShape < foldStep1LegShapeCount {
		legLocalBakeCounts.Step1ReconstructNilShape[origin.Step1LegShape]++
	}
}

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
		// reads reached it — which is the fact that retirement has to be decided
		// on, and the fact a shape-level test has to be able to find its own
		// firing by.
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
		legLocalBakeCounts.MergedReAnchor++
		legLocalBakeCounts.UntypedLeg++
		addLegLocalBakeWitness(fmt.Sprintf("UNTYPED-LEG %s.%s (leg type %v; derivable leg layouts %v)", leg.Name(), column, legTyp, available))
	case legLocalBakeClassColumnAbsent:
		legLocalBakeCounts.MergedReAnchor++
		legLocalBakeCounts.ColumnAbsent++
		addLegLocalBakeWitness(fmt.Sprintf("COLUMN-ABSENT %s.%s (leg columns %v)", leg.Name(), column, recordTypeFieldNames(legTyp.(*values.RecordType))))
	case legLocalBakeClassLayoutAvailable:
		legLocalBakeCounts.MergedReAnchor++
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
// The outcome is STATED by the deciding arm and the identity CLASS is PASSED
// rather than recomputed, for one reason in both cases: a census that
// re-derives its own subject answers a question the code did not ask.
//
// identity is the class the arm ALSO dispatched on — one value computed once at
// the top of the arm and handed to whichever branch fires. It used to be a bool
// each branch supplied for itself, and every branch's answer was fixed by the
// guard it sat behind: the merged re-anchor passed a literal `true` from inside
// `if identityInLegDomain`, so the counter it fed could only ever record one of
// its three values and the other two were structurally unreachable. A parameter
// whose value the call site cannot vary is not an input; it is the shape of the
// call site restated, and a census built on one measures nothing.
//
// The census gate is checked here, before the legLocalTypes probe, because the
// gate's stated contract is that a disabled census costs the planner nothing
// and legLocalTypes is consulted for no other purpose at this site.
func recordRebaseOuterLegArm(
	outcome legLocalBakeOutcome,
	fv values.FieldValue,
	qov values.QuantifiedObjectValue,
	legLocalTypes map[values.CorrelationIdentifier]*values.RecordType,
	identity legReadIdentity,
	origin legRebaseOrigin,
) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	recordLegRebaseSite(origin)
	legTypeFor, haveLegType := legLocalTypes[qov.Correlation()]
	column := strings.ToUpper(fv.DisplayName())
	recordLegLocalBakeability(outcome, qov.Correlation(),
		legTypeOrUntyped(legTypeFor, haveLegType, qov.FlowedType()),
		column, legLocalTypeKeys(legLocalTypes)...)
	// EVERY firing is classified, so the three identity counters partition Total.
	// They partitioned the merged re-anchor alone until that population went to
	// zero, at which point the cut described nobody — while the reads it should
	// have been describing (the pass-through, which is now the whole arm) went
	// unmeasured on the one axis the retirement decision rests on.
	why := ""
	if identity != legReadIdentityInLegDomain {
		why = describeLegIdentityDecline(fv, qov, legTypeFor, haveLegType)
	}
	recordLegReadIdentity(qov.Correlation(), column, identity, why)
}

// legReadIdentity is what a leg-correlated read states about its OWN column
// identity. See the three counters it feeds for why this is a different
// question from the layout one beside it.
//
// It is a CLASS rather than a bool because it is what the rebase arm dispatches
// on AND what the census records, and those two must be the same value. While
// it was a bool the arm computed it, branched on it, and then handed each branch
// a constant restating the branch it was already in — which is how the census
// came to report a partition of a population it had itself made empty.
type legReadIdentity int

const (
	// legReadIdentityInLegDomain: legSlotIdentity answered — a resolved,
	// non-negative ordinal in a known domain that IS the leg's row layout.
	legReadIdentityInLegDomain legReadIdentity = iota
	// legReadIdentityOtherDomain: a resolved path this leg's layout cannot
	// read.
	legReadIdentityOtherDomain
	// legReadIdentityLazyNameOnly: no resolved path; the display name is all there is.
	legReadIdentityLazyNameOnly
)

// String names the class. The classifier's failure mode is two arms swapping,
// and a diagnostic that renders them as bare ordinals reports the mix-up in the
// one notation that makes it hardest to read.
func (c legReadIdentity) String() string {
	switch c {
	case legReadIdentityInLegDomain:
		return "InLegDomain"
	case legReadIdentityOtherDomain:
		return "OtherDomain"
	case legReadIdentityLazyNameOnly:
		return "LazyNameOnly"
	default:
		return fmt.Sprintf("legReadIdentity(%d)", int(c))
	}
}

// classifyLegReadIdentity decides which of the three a leg-correlated read is,
// from the two facts the arm has in hand: whether the read carries a resolved
// path at all, and whether that path states an identity IN THE LEG'S OWN DOMAIN
// (legSlotIdentity's answer).
//
// The ordering matters and is the whole content: a read that states an identity
// in the leg's domain is that, whatever else is true of it. Only then does the
// absence of a resolved path separate the two residues, which have different
// owners — an other-domain path is a reference baked against the wrong layout
// (a producer stating a domain it did not resolve against), a lazy one is a
// reference that was never resolved at all.
func classifyLegReadIdentity(hasResolved, identityInLegDomain bool) legReadIdentity {
	if identityInLegDomain {
		return legReadIdentityInLegDomain
	}
	if hasResolved {
		return legReadIdentityOtherDomain
	}
	return legReadIdentityLazyNameOnly
}

// recordLegReadIdentity files one leg-correlated read by what it states about
// its own identity. Called for EVERY firing of the arm, so the three counters
// partition Total exactly.
func recordLegReadIdentity(leg values.CorrelationIdentifier, column string, class legReadIdentity, why string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	switch class {
	case legReadIdentityInLegDomain:
		legLocalBakeCounts.IdentityInLegDomain++
	case legReadIdentityOtherDomain:
		legLocalBakeCounts.IdentityOtherDomain++
		addLegLocalBakeWitness(fmt.Sprintf("MINTED-OTHER-DOMAIN %s.%s (%s)", leg.Name(), column, why))
	case legReadIdentityLazyNameOnly:
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
func describeLegIdentityDecline(fv values.FieldValue, qov values.QuantifiedObjectValue, legType *values.RecordType, haveLegType bool) string {
	var parts []string
	path := fv.Path()
	if path == nil {
		return "no resolved path"
	}
	parts = append(parts, fmt.Sprintf("accessors=%d", path.Len()))
	ordinals := path.Ordinals()
	if len(ordinals) > 0 {
		parts = append(parts, fmt.Sprintf("rootOrdinal=%d", ordinals[0]))
	}
	pathDomain := path.RootDomain()
	parts = append(parts, fmt.Sprintf("pathDomainKnown=%t", pathDomain.IsKnown()))
	qovDomain := values.OrdinalDomainOfType(qov.FlowedType())
	parts = append(parts, fmt.Sprintf("qovTypeDomainKnown=%t", qovDomain.IsKnown()))
	if qovDomain.IsKnown() && pathDomain.IsKnown() {
		parts = append(parts, fmt.Sprintf("qovDomainMatches=%t", qovDomain == pathDomain))
	}
	// The decisive one: the leg layout the census already reports AVAILABLE.
	if haveLegType && legType != nil {
		legDomain := values.OrdinalDomainOfType(legType)
		parts = append(parts, fmt.Sprintf("legTypeDomainKnown=%t", legDomain.IsKnown()))
		if legDomain.IsKnown() && pathDomain.IsKnown() {
			parts = append(parts, fmt.Sprintf("legTypeDomainMatches=%t", legDomain == pathDomain))
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
	if _, found := rt.FieldIndexUnique(column); !found {
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
// which the sqldriver TestMain runs over the whole corpus. Five partitions
// (Total = Baked+MergedReAnchor+Declined; MergedReAnchor = the three layout
// reasons; Total = the three identity classes; LegDerivations = the three
// per-leg outcomes; MergeSlots = the four slot typings), a population floor on
// the denominators, and an explicit VACUOUS notice naming any partition whose
// denominator reached zero — because a partition over an empty population holds
// as 0 == 0 and reads exactly like a checked one. Before
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
	fmt.Fprintf(&b, "leg-local bakeability: total %d (baked %d, mergedReAnchor %d, declined %d); "+
		"mergedReAnchor residue: untypedLeg %d, columnAbsent %d, layoutAvailable %d; "+
		"legs: flowed %d, underivable %d, memberDisagreement %d",
		c.Total, c.Baked, c.MergedReAnchor, c.Declined,
		c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable,
		c.FlowedLegs, c.UnderivableLegs, c.DisagreeingLegs)
	fmt.Fprintf(&b, "; by CALL SITE: exists %d, buried %d", c.SiteExists, c.SiteBuried)
	// The read→FIRING mapping. This is the ONLY thing that relates this census's
	// READ count to the foldStep1Seed outcome census's FIRING count; the two
	// denominators do not sum and never could.
	b.WriteString("; by STEP-1 CLASS of the firing:")
	for cl := foldStep1Class(0); cl < foldStep1ClassCount; cl++ {
		fmt.Fprintf(&b, " [%s %d]", cl, c.Step1[cl])
	}
	b.WriteString("; reconstruct-nil reads by REFUSED LEG SHAPE:")
	for s := foldStep1LegShape(0); s < foldStep1LegShapeCount; s++ {
		fmt.Fprintf(&b, " [%s %d]", s, c.Step1ReconstructNilShape[s])
	}
	fmt.Fprintf(&b, "; ALL reads by OWN identity: identityInLegDomain %d, "+
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
	Total int
	// SiteExists floors the ONE call site that is supposed to be live. Total
	// alone does not cover it: if the EXISTS lowering stopped reaching the arm
	// and something else started, Total could hold while the population this
	// census describes changed identity entirely — and the SiteBuried zero above
	// would still pass, vacuously.
	SiteExists     int
	LegDerivations int
	// MergeSlots is floored for the same reason the other two are:
	// MergeSlotsUntyped is a SHARE of it, and a share of a collapsed denominator
	// reads as progress while measuring nothing.
	MergeSlots int
	// Step1ReconstructNilMustBeZero turns RFC-200 gate (d) on: no leg-local read
	// may occur under a firing whose seed reconstruction returned nil.
	//
	// It is a flag rather than an always-on zero because the gate is only true
	// AFTER the nested window is activated. Before that it is the MEASUREMENT the
	// gate is denominated in, and asserting it early would red on the very number
	// the sequencing step exists to produce.
	Step1ReconstructNilMustBeZero bool
}

// AssertLegLocalBakeCensus checks the bakeability census's invariants and
// reports whether it failed, mirroring values.AssertLegIdentityCensus.
//
// The three partition checks are the point. Every other number this census
// prints is a SHARE of one of two totals, and a share is only meaningful if the
// shares add up: if MergedReAnchor and the three layout-reason counters drift apart, the
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

	if got := c.Baked + c.MergedReAnchor + c.Declined; got != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: Baked(%d) + MergedReAnchor(%d) + Declined(%d) = %d, "+
			"but Total = %d.\n"+
			"  Every firing of the leg-match arm is exactly one of the three, so a gap\n"+
			"  means a firing was counted into Total and classified into none — or a new\n"+
			"  outcome was added without a counter. The residue percentages this census\n"+
			"  feeds are computed against Total, so they are wrong by exactly the gap.\n",
			c.Baked, c.MergedReAnchor, c.Declined, got, c.Total)
	}

	if got := c.UntypedLeg + c.ColumnAbsent + c.LayoutAvailable; got != c.MergedReAnchor {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: UntypedLeg(%d) + ColumnAbsent(%d) + "+
			"LayoutAvailable(%d) = %d, but MergedReAnchor = %d.\n"+
			"  The three reasons classify MERGED-RE-ANCHORED reads and nothing else, so\n"+
			"  they must partition MergedReAnchor exactly. LayoutAvailable is the number\n"+
			"  CQ-63 has to move; if the reasons do not sum, it is a share of an unknown\n"+
			"  whole.\n",
			c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable, got, c.MergedReAnchor)
	}

	if got := c.IdentityInLegDomain + c.IdentityOtherDomain + c.LazyNameOnly; got != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: IdentityInLegDomain(%d) + "+
			"IdentityOtherDomain(%d) + LazyNameOnly(%d) = %d, but Total = %d.\n"+
			"  These three classify EVERY firing by what the read states about its OWN\n"+
			"  identity, so they must partition Total exactly. They partitioned the\n"+
			"  merged re-anchor alone until that population reached zero, at which point\n"+
			"  the check was 0 == 0 and the live population — the pass-through, which is\n"+
			"  now the whole arm — went unmeasured on the one axis the qualified-name\n"+
			"  channel's retirement rests on. IdentityInLegDomain is the number of reads\n"+
			"  that can state their own slot with no name consulted anywhere; if the\n"+
			"  three do not sum, that number is a share of an unknown whole.\n",
			c.IdentityInLegDomain, c.IdentityOtherDomain, c.LazyNameOnly, got, c.Total)
	}

	// THE CALL-SITE CUT. A third partition of Total, and the one that turns a
	// prose claim into a checked one.
	if got := c.SiteExists + c.SiteBuried; got != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: SiteExists(%d) + SiteBuried(%d) = %d, "+
			"but Total = %d.\n"+
			"  Every firing enters the arm from exactly one lowering, so these must\n"+
			"  partition Total. A gap means a THIRD entry point now reaches\n"+
			"  rebaseOuterLegValue and is threading a site value that names neither —\n"+
			"  which is worth finding, because the split below is asserted as a fact in\n"+
			"  DIVERGENCES.md and in TODO.md's CQ-53 phase-3 block.\n",
			c.SiteExists, c.SiteBuried, got, c.Total)
	}
	// THE READ→FIRING CUT. A fourth partition of Total, and the one RFC-200 gate
	// (d) is denominated in.
	//
	// It is a partition and not a floor because the mapping it carries is the
	// whole point: the two censuses on this path count different events (reads
	// here, rule firings there) and their totals do not sum, so "this change
	// moves N of the leg-local reads" is only answerable by carrying the firing's class
	// down to the read. A gap here means a read arrived under a firing whose
	// class was not threaded, and every apportionment of this census's population
	// is then arithmetic over an unknown remainder — which is exactly the
	// category error revision 1 of RFC-200 was NAK'd for.
	step1Sum := 0
	for cl := foldStep1Class(0); cl < foldStep1ClassCount; cl++ {
		step1Sum += c.Step1[cl]
	}
	if step1Sum != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: the STEP-1 CLASS cut sums to %d, but "+
			"Total = %d.\n"+
			"  Every firing arrives under exactly one foldStep1Seed class (or under NONE,\n"+
			"  which is what the BURIED lowering honestly threads), so these must\n"+
			"  partition Total. A gap means a read reached the arm carrying a class value\n"+
			"  outside the enum — a new entry point that forgot to thread one.\n",
			step1Sum, c.Total)
	}
	if c.SiteExists != 0 && c.Step1[foldStep1ClassNone] > c.SiteBuried {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: %d read(s) arrived with NO step-1 class "+
			"while only %d came from the BURIED site.\n"+
			"  The EXISTS lowering ALWAYS has a foldStep1Seed class in hand — it computes\n"+
			"  one immediately before the rebase — so an EXISTS-site read filed under\n"+
			"  NONE is a call site that dropped the class on the way down. The read→firing\n"+
			"  mapping RFC-200 gate (d) is denominated in is wrong by exactly that many\n"+
			"  reads, and it fails SILENTLY: the class simply reads as \"not a\n"+
			"  reconstruct-nil firing\".\n",
			c.Step1[foldStep1ClassNone], c.SiteBuried)
	}
	shapeSum := 0
	for s := foldStep1LegShape(0); s < foldStep1LegShapeCount; s++ {
		shapeSum += c.Step1ReconstructNilShape[s]
	}
	if shapeSum != c.Step1[foldStep1DeclineReconstructNil] {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: the reconstruct-nil reads cut by REFUSED\n"+
			"  LEG SHAPE sum to %d, but the reconstruct-nil class counted %d reads.\n"+
			"  The shape rides down with the class from the same origin struct, so a gap is\n"+
			"  a call site threading one and not the other — and the finer cut is the one\n"+
			"  RFC-200 gate (d) is stated against.\n",
			shapeSum, c.Step1[foldStep1DeclineReconstructNil])
	}
	if floors != nil && floors.Step1ReconstructNilMustBeZero &&
		c.Step1ReconstructNilShape[foldStep1LegShapePositionalMerge] != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: %d leg-local read(s) still occur under a "+
			"reconstruct-nil firing whose refused leg was a POSITIONAL MERGE, want 0.\n"+
			"  This is RFC-200 gate (d). The retirement condition for the runtime\n"+
			"  binding-namespace widening (executor.bindMergedOuterLegs) is that a\n"+
			"  leg-correlated read be rewritten to ofOrdinalNumber against the merged\n"+
			"  quantifier BEFORE execution, so no sibling alias need be resolvable at\n"+
			"  runtime. A read arriving here under a firing whose seed reconstruction\n"+
			"  returned nil is a read that took the pass-through because no merged layout\n"+
			"  existed — precisely the population the nested window was built to remove.\n"+
			"  Read the foldStep1Seed outcome census beside this one: its refused-leg\n"+
			"  sub-partition says WHICH shape is still being refused, and only the\n"+
			"  positional-merge bucket is in RFC-200's scope. A residue in the bare-QOV\n"+
			"  bucket is the LARGER, explicitly fenced population (RFC-200 §Residues) and\n"+
			"  this gate must be re-stated, not relaxed, if that is what it is seeing.\n",
			c.Step1[foldStep1DeclineReconstructNil])
	}
	if c.SiteBuried != 0 {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: SiteBuried = %d, want 0.\n"+
			"  A leg-correlated read reached the leg-match arm from the RFC-153\n"+
			"  BURIED-leg lowering (buildCorrelatedFlatMapPlan). Measured over the whole\n"+
			"  real-FDB corpus that population is ZERO: EVERY read is the EXISTS site,\n"+
			"  and the buried site reaches no matching leaf at all even though it DOES\n"+
			"  compute a layout (buriedLegOrdinalLayout answers on most of its firings).\n"+
			"  The EXISTS total itself is a corpus-sized number that moves — it read 174\n"+
			"  when this message was written and 190 on re-measurement — so this message\n"+
			"  states the ZERO, which is the durable claim, and not a denominator that\n"+
			"  would be stale by the time anyone read it here.\n"+
			"  THIS IS NOT NECESSARILY A BUG — it is a REFUTED PREMISE RE-ARMING. Two\n"+
			"  documents now assert this zero in prose: DIVERGENCES.md's sibling-alias\n"+
			"  entry (\"there is no buried-leg work in this retirement\") and TODO.md's\n"+
			"  CQ-53 phase-3 refutation 2. Both were written FROM this measurement. If\n"+
			"  the lowering changed so a buried leg now reaches here, those documents are\n"+
			"  stale and the buried path is back in scope: re-measure the split, fix the\n"+
			"  prose in both places, and only then decide whether the new firings are\n"+
			"  correct. Do not simply raise this bound — the bound IS the claim.\n",
			c.SiteBuried)
	}

	// THE TWO CUTS AGAINST EACH OTHER. Both partitions above are sums over the
	// same denominator, and neither says anything about which firing landed in
	// which bucket of the OTHER cut. So a census that files the identity class
	// wholesale wrong — every firing recorded under one constant class — keeps
	// both sums exact and the gate green, while the one number the qualified-name
	// channel's retirement rests on reads its own opposite. That is not a
	// hypothetical shape: filing a constant class at one arm's census call site
	// inverts IdentityInLegDomain from the whole corpus to none of it, with no
	// counter out of balance anywhere.
	//
	// What closes it is that the two cuts are not independent measurements. The
	// arm dispatches on the identity CLASS: arms 1 and 2 are both guarded on
	// legReadIdentityInLegDomain, and arm 3 is reached exactly when that guard
	// fails. So the outcome and the identity are two readings of ONE decision, and
	// these two checks state that fact — a firing's identity class is derivable
	// from its outcome and vice versa, so a census where they disagree is
	// measuring something other than the arm.
	//
	// They coincide whenever both partitions above hold, and diverge when one does
	// not; both are stated so a report names which end drifted rather than leaving
	// it to be inferred from four numbers.
	if got := c.Baked + c.MergedReAnchor; got != c.IdentityInLegDomain {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: Baked(%d) + MergedReAnchor(%d) = %d, "+
			"but IdentityInLegDomain = %d.\n"+
			"  Arms 1 and 2 are BOTH guarded on the read stating an identity in its own\n"+
			"  leg's domain, so every firing with either outcome states one and no other\n"+
			"  firing does. A gap means the class the census FILED is not the class the\n"+
			"  arm DISPATCHED on — the two partitions above stay exact under a wholesale\n"+
			"  misfiling, and this is the check that does not.\n",
			c.Baked, c.MergedReAnchor, got, c.IdentityInLegDomain)
	}

	if got := c.IdentityOtherDomain + c.LazyNameOnly; got != c.Declined {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: IdentityOtherDomain(%d) + "+
			"LazyNameOnly(%d) = %d, but Declined = %d.\n"+
			"  The same fact from the decline side: arm 3 is reached exactly when the\n"+
			"  identity guard fails, so the two residue classes and the declined outcome\n"+
			"  count the SAME firings. If Declined is 0 while a residue class is not, the\n"+
			"  census is reporting reads the arm did not decline as reads it could not\n"+
			"  bake — which is the retirement number reading its own opposite.\n",
			c.IdentityOtherDomain, c.LazyNameOnly, got, c.Declined)
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

	// A partition over an EMPTY denominator holds as 0 == 0. That is not a
	// failure — a zero merged re-anchor is the arm's correct present state — but
	// it means the check above it proved nothing, and a green line in a report is
	// indistinguishable from a checked one unless it says so.
	//
	// This is the residual of the same defect the identity cut had: that cut was
	// aimed at a population that had gone to zero, and nothing in the report said
	// the assertion over it was vacuous, so it read as a live check for as long
	// as anyone cared to read it. The cut is repointed at Total, which is
	// FLOORED. The layout-reason cut cannot be repointed — the three reasons are
	// genuinely about re-anchored reads — so it announces itself instead.
	for _, v := range []struct {
		partition   string
		denominator string
		got         int
		what        string
	}{
		{
			"UntypedLeg + ColumnAbsent + LayoutAvailable", "MergedReAnchor", c.MergedReAnchor,
			"No read was re-anchored onto the merge correlation, so nothing was\n" +
				"  classified by WHY. LayoutAvailable — CQ-63's convertibility number — is\n" +
				"  zero because the population is zero, not because the layouts went away.",
		},
		{
			"MergeSlotsTyped + Scavenged + Scalar + Untyped", "MergeSlots", c.MergeSlots,
			"No positional merge built a slot, so MergeSlotsUntyped is not an upper\n" +
				"  bound on anything.",
		},
	} {
		if v.got == 0 {
			fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS VACUOUS: %s partitions %s, which is 0 — "+
				"the check held as 0 == 0 and proved nothing.\n  %s\n",
				v.partition, v.denominator, v.what)
		}
	}

	if floors != nil {
		for _, f := range []struct {
			name  string
			got   int
			floor int
		}{
			{"Total", c.Total, floors.Total},
			{"SiteExists", c.SiteExists, floors.SiteExists},
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
