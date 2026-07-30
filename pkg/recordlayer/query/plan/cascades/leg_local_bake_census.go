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
	// ordinal. Nothing in the planner passes this today by construction — there is
	// no leg-local bake arm. It exists so the arm CQ-63 restores has to STATE that
	// it baked instead of letting the census infer it from a type.
	legLocalBakeBaked
)

type legLocalBakeCounters struct {
	// Total is every firing of the leg-match arm.
	Total int
	// Baked: reads the arm converted to a leg-local ordinal on their own alias.
	// Zero on this branch by construction; see legLocalBakeBaked.
	Baked int
	// Minted: reads that were re-anchored onto the merge correlation with the leg
	// packed into the column name. The channel's actual live traffic.
	Minted int
	// The rest classify MINTED reads only — why the mint was reached.
	//
	// UntypedLeg: no layout for this leg at all, so no leg-local ordinal could
	// exist. These are the reads that would have to DECLINE.
	UntypedLeg int
	// ColumnAbsent: a layout exists but does not declare a column of this read's
	// name. Census-side name lookup (see the header): it separates a missing
	// layout from a mismatched one, and decides nothing.
	ColumnAbsent int
	// LayoutAvailable: a layout exists and declares the read's name. A mint that a
	// leg-local ordinal COULD have carried — the number CQ-63 has to move.
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

// legLocalBakeClass is one firing's bucket.
type legLocalBakeClass int

const (
	legLocalBakeClassBaked legLocalBakeClass = iota
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
	if outcome == legLocalBakeBaked {
		return legLocalBakeClassBaked
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
	fmt.Fprintf(&b, "leg-local bakeability: total %d (baked %d, minted %d); "+
		"minted residue: untypedLeg %d, columnAbsent %d, layoutAvailable %d; "+
		"legs: flowed %d, underivable %d, memberDisagreement %d",
		c.Total, c.Baked, c.Minted,
		c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable,
		c.FlowedLegs, c.UnderivableLegs, c.DisagreeingLegs)
	fmt.Fprintf(&b, "; legDerivations %d", c.LegDerivations)
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

	if got := c.Baked + c.Minted; got != c.Total {
		failed = true
		fmt.Fprintf(w, "LEG-LOCAL BAKE CENSUS FAIL: Baked(%d) + Minted(%d) = %d, but Total = %d.\n"+
			"  Every firing of the leg-match arm is one or the other, so a gap means a\n"+
			"  firing was counted into Total and classified into neither — or a new\n"+
			"  outcome was added without a counter. The residue percentages this census\n"+
			"  feeds are computed against Total, so they are wrong by exactly the gap.\n",
			c.Baked, c.Minted, got, c.Total)
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
