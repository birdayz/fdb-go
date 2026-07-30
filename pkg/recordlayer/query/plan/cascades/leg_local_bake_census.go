package cascades

import (
	"fmt"
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
	// FlowedLegs counts LEGS the FAITHFUL instrument stated — the quantifier's own
	// flowed object type, which is what Java's translateCorrelations rebases
	// against. Reported beside WalkOnlyLegs because the pair is the whole question:
	// zero here means the faithful instrument answers nowhere and every layout in
	// hand came from the subordinate walk.
	FlowedLegs int
	// WalkOnlyLegs counts LEGS the faithful instrument could not state but the
	// SUBORDINATE physical-plan walk could. Nonzero is the walk's justification.
	WalkOnlyLegs int
	// DisagreeingLegs counts LEGS whose reference members flow different row
	// types — a memo defect, Java's Verify failure (Reference.java:504-513).
	DisagreeingLegs int
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

// recordUnderivableLegLayout names a leg whose quantifier states no row layout,
// by the shape that declined.
//
// This is the residue's cause rather than its symptom. The bakeability counters
// say how many reads still need the qualified name; this says WHY — which shapes
// the layout derivation has no answer for. Kept separate from the counters
// because it is per-LEG, not per-READ: one underivable leg accounts for every
// read correlated to it.
func recordUnderivableLegLayout(alias values.CorrelationIdentifier, shape any, escape string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.UnderivableLegs++
	addLegLocalBakeWitness(fmt.Sprintf("NO-LAYOUT leg %s: %T states no row layout [%s]", alias.Name(), shape, escape))
}

// recordWalkOnlyLegLayout names a leg the FAITHFUL instrument (the quantifier's
// flowed object type, Java's Quantifier.getFlowedObjectType) could not state and
// the SUBORDINATE physical-plan walk could.
//
// This counter is the walk's entire justification. The two derivations are not
// the same authority: the flowed type is what Java's translateCorrelations
// rebases against, while the walk reconstructs a concat from the chosen plan's
// scan leaves. Keeping a second authority is only defensible while it is
// answering somewhere the first cannot, and this says how often.
func recordWalkOnlyLegLayout(alias values.CorrelationIdentifier, plan any) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.WalkOnlyLegs++
	addLegLocalBakeWitness(fmt.Sprintf("WALK-ONLY leg %s: plan %T states a layout the flowed object type does not", alias.Name(), plan))
}

// recordFlowedLegLayout names a leg the faithful instrument stated on its own.
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
// The acceptance number for CQ-63 is UnderivableLegs: while a leg states no row
// layout, every read correlated to it has no ordinal it could honestly carry, so
// the qualified name is the only channel available to it. UntypedLeg is that same
// residue counted per READ. LayoutAvailable is the reads a restored leg-local
// bake could convert; it is the number that has to become the total before the
// channel can retire, and it is reported separately from Baked precisely so a
// future typed-but-still-minting state reads as what it is.
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
		"legs: flowed %d, walkOnly %d, underivable %d, memberDisagreement %d",
		c.Total, c.Baked, c.Minted,
		c.UntypedLeg, c.ColumnAbsent, c.LayoutAvailable,
		c.FlowedLegs, c.WalkOnlyLegs, c.UnderivableLegs, c.DisagreeingLegs)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  distinct witnesses (%d, cap %d):\n    %s",
			len(sorted), legLocalBakeWitnessCap, strings.Join(sorted, "\n    "))
	}
	return b.String()
}

func legLocalTypeKeys(m map[values.CorrelationIdentifier]*values.RecordType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.Name())
	}
	sort.Strings(out)
	return out
}
