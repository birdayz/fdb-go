// Portions derived from FoundationDB Record Layer (Identifier.java,
// IdentifierVisitor.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package querycensus

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The QUALIFIER RECOVERY census — the DARK SPLITTERS.
//
// Its sibling, the NAME SPLIT census (name_split_census.go), states its own
// scope in its header and then names four sites it does NOT watch. This file is
// those four sites. Nothing counted here is counted there, and the two censuses'
// zeros are zeros over disjoint populations.
//
// WHAT A "DARK SPLITTER" IS. A site that recovers a QUALIFIER from a RENDERED
// name — slices a string at a dot and treats the bytes on one side as a source
// alias — and that has, until this file, had nothing counting it. The rendered
// name is lossy by construction: a qualified reference `A.B` and a delimited
// identifier `"A.B"` are the SAME BYTES, so a site that decides qualification by
// looking for a dot cannot tell them apart. Java never faces the question,
// because identity there is `name` + `List<String> qualifier` built segment by
// segment off the ANTLR FullId (Identifier.java:34-58, IdentifierVisitor.java:56-64)
// and joined only for DISPLAY (Identifier.java:61-63). Go's debt is every place
// that joins and then re-splits.
//
// The classes distinguish redundant text recovery from sites whose producers
// must supply structured identity before text recovery can be removed.
//
// THE CLASSES ANSWER "WAS THE IDENTITY ALREADY IN HAND?". That is the whole
// conversion-readiness question, and it is what separates a site that can be
// converted mechanically from one that cannot be converted at all:
//
//   - CARRIED — the site decided from a STRUCTURED identity (a segment triple, a
//     QuantifiedObjectValue's correlation, an ordinal). No rendered name was
//     sliced. Counted so a converted channel's LIVENESS is visible: a conversion
//     that silently stopped being reached reads exactly like one that succeeded.
//   - AGREED — a qualifier WAS manufactured from text, AND a structured identity
//     was in hand at the same site, AND the two agree. THIS IS THE
//     CONVERSION-READY POPULATION: the split is redundant here, and replacing it
//     with the identity is provably behaviour-preserving over this traffic.
//   - DIVERGED — manufactured from text with an identity in hand that DISAGREES.
//     A live misread. ASSERTED ZERO.
//   - MANUFACTURED — manufactured from text with NO counterparty identity
//     available at the site at all. THE HARD DEBT: this population cannot be
//     converted by a local edit, because there is nothing local to convert TO.
//     It is the population that says a producer has to be taught to carry
//     segments first.
//   - LEAF-ONLY — a dot was found and the LEAF taken, but the qualifier was
//     DISCARDED because an ordinal or an identity decides the actual read. Text
//     was sliced; no qualifier was invented. Not debt, and counted separately so
//     it is not banked as one.
//   - BARE — the split ran and found no dot. Nothing was manufactured. Counted
//     because it shares the arm with the debt classes, and a census reporting
//     only debt cannot tell a CLEAN arm from a DARK one.
//
// A site reporting only CARRIED + AGREED is a mechanical conversion. A site
// reporting MANUFACTURED is a producer change. That distinction is the
// deliverable, and it is measured rather than asserted.
//
// GATED by LegIdentityCensusEnabled — the same gate its siblings ride. What
// production pays census-off is ONE ATOMIC LOAD per decision, and that is the
// whole of it: every recorder helper at every site reads the gate as its first
// statement and returns, so no name is parsed, no qualifier is upper-cased and
// no counterparty is looked up to build an argument the disabled recorder would
// discard. The two callers that must compute an identity BEFORE calling a
// recorder (the EXISTS fold's sortKeyName and sortKeySourceValue) hoist the gate
// above that computation for the same reason.
//
// Counts are per call, one call per resolution decision.

// QualifierRecoverySites returns every site, for callers that walk them.
func QualifierRecoverySites() []values.QualifierRecoverySite {
	out := make([]values.QualifierRecoverySite, 0, values.QualRecSiteCount)
	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		out = append(out, s)
	}
	return out
}

// qualRecWitnessCap bounds each (site, class) witness list. Saturation is a
// FAILURE at the assertion, not a silent truncation — see AssertQualifierRecoveryCensus.
const qualRecWitnessCap = 32

// QualifierRecoveryWitnessCap exposes the cap so a harness can check saturation.
func QualifierRecoveryWitnessCap() int { return qualRecWitnessCap }

var (
	qualRecMu        sync.Mutex
	qualRecCounts    [values.QualRecSiteCount][values.QualRecClassCount]int
	qualRecWitnesses [values.QualRecSiteCount][values.QualRecClassCount][]string
)

// RecordQualifierRecovery records ONE resolution decision at a dark splitter.
//
// `name` is the rendered name the site was handed and `identity` is the
// structured counterparty it had in hand (empty when it had none). Both go into
// the witness so a regrowth arrives with the pair that caused it — for DIVERGED
// in particular, the count alone cannot say which side is wrong.
func RecordQualifierRecovery(site values.QualifierRecoverySite, class values.QualifierRecoveryClass, name, identity string) {
	if !LegIdentityCensusEnabled() ||
		site < 0 || site >= values.QualRecSiteCount ||
		class < 0 || class >= values.QualRecClassCount {
		return
	}
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	qualRecCounts[site][class]++
	// BARE and CARRIED are high-population and their spellings say nothing a
	// count does not. The four classes that involve a slice each earn a witness.
	switch class {
	case values.QualRecAgreed, values.QualRecDiverged, values.QualRecManufactured, values.QualRecLeafOnly, values.QualRecHeuristicDecline:
	default:
		return
	}
	if len(qualRecWitnesses[site][class]) >= qualRecWitnessCap {
		return
	}
	w := fmt.Sprintf("%q", name)
	if identity != "" {
		w = fmt.Sprintf("%q vs identity %q", name, identity)
	}
	for _, e := range qualRecWitnesses[site][class] {
		if e == w {
			return
		}
	}
	qualRecWitnesses[site][class] = append(qualRecWitnesses[site][class], w)
}

// QualifierRecoveryCensus returns a snapshot of the counters and witnesses.
func QualifierRecoveryCensus() ([values.QualRecSiteCount][values.QualRecClassCount]int, [values.QualRecSiteCount][values.QualRecClassCount][]string) {
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	var w [values.QualRecSiteCount][values.QualRecClassCount][]string
	for s := range qualRecWitnesses {
		for c := range qualRecWitnesses[s] {
			w[s][c] = append([]string(nil), qualRecWitnesses[s][c]...)
			sort.Strings(w[s][c])
		}
	}
	return qualRecCounts, w
}

// ResetQualifierRecoveryCensus clears the counters. For tests measuring one shape.
func ResetQualifierRecoveryCensus() {
	qualRecMu.Lock()
	defer qualRecMu.Unlock()
	qualRecCounts = [values.QualRecSiteCount][values.QualRecClassCount]int{}
	qualRecWitnesses = [values.QualRecSiteCount][values.QualRecClassCount][]string{}
}

// QualifierRecoveryFloors is the minimum population each site must report, so a
// site going DARK fails rather than reading as a clean zero.
type QualifierRecoveryFloors struct {
	// Calls is the minimum total calls (all six classes) per site.
	Calls [values.QualRecSiteCount]int

	// Split is the minimum SPLIT population per site — the calls that actually
	// entered a splitting arm (AGREED + DIVERGED + MANUFACTURED + LEAF-ONLY +
	// BARE), as opposed to CARRIED calls that decided without one.
	//
	// Separate from Calls because at a site whose carried channel does the work
	// the two diverge completely: a healthy Calls total says the CARRIED arm is
	// reached and says nothing whatever about the arms this census's zeros are
	// zeros over.
	//
	// A ZERO ENTRY IS A DECLARATION, NOT AN ABSENT FLOOR, and it is CHECKED IN
	// THE STALE DIRECTION: a site declared 0 whose split population is now
	// NON-zero FAILS. The "watched, not proven" label cannot silently outlive the
	// condition that made it honest. Declaring 0 means "this site's split arms
	// are measured empty over this corpus, they are covered by a unit wiring pin
	// instead, and the day the corpus starts driving them somebody re-reads this
	// line."
	Split [values.QualRecSiteCount]int
}

// AssertQualifierRecoveryCensus checks the census's hard zero, its saturation
// guards and its floors, writing a report to w. Returns TRUE if anything FAILED
// — the convention its siblings on this path use.
//
// THE HARD ZERO IS **DIVERGED**, and it is the only zero this census asserts.
// That is a deliberate departure from its sibling, which asserts its debt bucket
// at zero: here MANUFACTURED is expected to be non-zero at several sites, because
// the whole finding is that these splitters exist and are reached. Asserting it
// at zero would either be false on the day it was written or would have to be
// relaxed into meaninglessness. What CANNOT be tolerated is a split that
// contradicts an identity the site was holding at the time — that is not debt,
// it is a wrong answer.
func AssertQualifierRecoveryCensus(w io.Writer, exp *QualifierRecoveryExpectations, corpus string) bool {
	counts, witnesses := QualifierRecoveryCensus()
	return assertQualifierRecoveryCounts(w, counts, witnesses, exp, corpus)
}

// QualifierRecoveryExpectations is what one CORPUS expects of the census. Two
// harnesses drive this instrument and they expect different things, so the
// expectations are a parameter rather than a constant.
type QualifierRecoveryExpectations struct {
	// Floors is the per-site population this corpus must report, or nil under a
	// -test.run filter that makes the population meaningless.
	Floors *QualifierRecoveryFloors

	// AllowedDiverged is every DIVERGED witness this corpus's own test FIXTURES
	// deliberately drive, per site.
	//
	// A corpus of SQL cannot need this: every one of its splits comes from a
	// production producer, so any disagreement there is a defect and the zero is
	// a bare zero. A corpus that calls package-private recorders with hand-built
	// fixtures DOES need it — the fixtures that prove the DIVERGED bucket is
	// REACHABLE necessarily fill it, and without them the census's one asserted
	// zero would be a zero nothing had ever shown could be non-zero.
	//
	// The check is per-witness rather than a count tolerance, so a real
	// divergence at a new spelling cannot hide inside a budget. The residual is
	// stated rather than smoothed: a real divergence spelled EXACTLY like one of
	// these is absorbed, because the witness set dedups by spelling — which is
	// why the entries are listed individually with the fixture that drives each.
	AllowedDiverged map[values.QualifierRecoverySite]map[string]struct{}

	// RetiredSplit names the sites whose SPLITTING ARM IS GONE — not measured
	// empty over this corpus, but structurally unreachable — so the alarm at them
	// has INVERTED from collapse to REVIVAL: any split call is the arm coming
	// back, and it fails.
	//
	// This is deliberately NOT the Floors.Split zero declaration, which says
	// "measured empty here, covered by a unit pin instead". Two differences, and
	// both matter:
	//
	//   - A declaration is a statement about THIS CORPUS. A retirement is a
	//     statement about the TREE, so it holds over any population — including
	//     the empty one a -test.run filter leaves behind. Floors are dropped under
	//     a filter because a population floor describes the unfiltered suite; a
	//     retirement must not be, because skipping it is the one direction that
	//     fails open.
	//   - The failure text differs, and the text is the whole value of a stale
	//     guard: a declaration says "raise this floor", a retirement says "the arm
	//     you deleted is back".
	//
	// A site listed here is exempt from the Floors.Split zero-declaration check,
	// so the two guards report the same event once, with the right words.
	RetiredSplit [values.QualRecSiteCount]bool

	// RetiredCalls forbids every class, including CARRIED, at a deleted site.
	// Like RetiredSplit, this tree invariant survives filtered/empty corpora.
	RetiredCalls [values.QualRecSiteCount]bool
}

// assertQualifierRecoveryCounts is the decision, split from the process-global
// counters so every failure direction can be exercised without a corpus run —
// the same split its siblings make, for the same reason.
func assertQualifierRecoveryCounts(
	w io.Writer,
	counts [values.QualRecSiteCount][values.QualRecClassCount]int,
	witnesses [values.QualRecSiteCount][values.QualRecClassCount][]string,
	exp *QualifierRecoveryExpectations,
	corpus string,
) bool {
	failed := false
	var floors *QualifierRecoveryFloors
	var allowedDiverged map[values.QualifierRecoverySite]map[string]struct{}
	var retiredSplit, retiredCalls [values.QualRecSiteCount]bool
	if exp != nil {
		floors, allowedDiverged, retiredSplit = exp.Floors, exp.AllowedDiverged, exp.RetiredSplit
		retiredCalls = exp.RetiredCalls
	}

	fmt.Fprintf(w, "[%s] qualifier recovery census — the DARK SPLITTERS (per resolution decision):\n", corpus)
	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		total := 0
		for c := values.QualifierRecoveryClass(0); c < values.QualRecClassCount; c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(w, "  %-20s calls %d | carried %d | AGREED %d | DIVERGED %d | MANUFACTURED %d | leafOnly %d | bare %d | heuristicDecline %d\n",
			QualifierRecoverySiteLabel(s), total,
			counts[s][values.QualRecCarried], counts[s][values.QualRecAgreed], counts[s][values.QualRecDiverged],
			counts[s][values.QualRecManufactured], counts[s][values.QualRecLeafOnly], counts[s][values.QualRecBare],
			counts[s][values.QualRecHeuristicDecline])
		for c := values.QualifierRecoveryClass(0); c < values.QualRecClassCount; c++ {
			for _, wit := range witnesses[s][c] {
				fmt.Fprintf(w, "      %s witness: %s\n", QualifierRecoveryClassLabel(c), wit)
			}
		}
	}

	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		if counts[s][values.QualRecDiverged] == 0 {
			continue
		}
		// A corpus whose own fixtures drive the DIVERGED bucket clears it
		// WITNESS BY WITNESS. Anything the fixtures do not account for came from
		// a producer and fails, exactly as if no allowlist existed.
		if allowed, ok := allowedDiverged[s]; ok {
			unaccounted := make([]string, 0, len(witnesses[s][values.QualRecDiverged]))
			for _, wit := range witnesses[s][values.QualRecDiverged] {
				if _, declared := allowed[wit]; !declared {
					unaccounted = append(unaccounted, wit)
				}
			}
			if len(unaccounted) == 0 {
				continue
			}
			failed = true
			fmt.Fprintf(w, "FAIL: %s reported DIVERGED witness(es) that no test fixture accounts\n"+
				"  for: %v.\n"+
				"  The fixtures' deliberate pairs are declared per corpus; a witness outside\n"+
				"  that set came from a PRODUCER, and the fix belongs there — use the identity.\n",
				QualifierRecoverySiteLabel(s), unaccounted)
			continue
		}
		if n := counts[s][values.QualRecDiverged]; n != 0 {
			failed = true
			fmt.Fprintf(w, "FAIL: %s manufactured a qualifier that CONTRADICTS the structured identity\n"+
				"  it was holding, %d time(s). This is not debt — it is a WRONG ANSWER, and it is\n"+
				"  the one population this census asserts at zero. The site sliced a rendered name\n"+
				"  and got a different source than the identity in hand names, which means either\n"+
				"  the rendering is not the identity joined (a delimited identifier containing a\n"+
				"  dot is the canonical case: `A.B` the reference and \"A.B\" the column are the\n"+
				"  same bytes), or a producer rebased one channel and not the other.\n"+
				"  THE FIX IS TO USE THE IDENTITY, never to widen this zero: the identity is what\n"+
				"  the parser saw, the string is a rendering of it. Witnesses above carry both\n"+
				"  sides of each disagreeing pair.\n", QualifierRecoverySiteLabel(s), n)
		}
	}

	// SATURATION. The witness lists are capped, and past the cap a further
	// DISTINCT spelling is counted but retains nothing — so a saturated list is a
	// SUBSET that reads like a whole.
	//
	// Whether that is a FAILURE depends on what the list is used for, and the two
	// answers are opposite:
	//
	//   - DIVERGED and MANUFACTURED are ANOMALY populations. Their witnesses are
	//     the evidence somebody acts on — which pair disagreed, which name had no
	//     counterparty — and a subset presented as a whole would send that person
	//     to fix the spellings that happened to arrive first. Saturation FAILS.
	//   - AGREED, LEAF-ONLY and HEURISTIC-DECLINE are health populations. Their
	//     witnesses illustrate a shape that is working; the COUNT carries the
	//     claim and the list is an example set. Saturation is expected on any
	//     corpus of size (displayLabelStrip alone agrees on HUNDREDS of calls
	//     across dozens of aliases) and is announced, not failed. Failing it
	//     would mean the gate goes red because the suite grew.
	//
	//     NO FIXED FIGURE BELONGS HERE. Two back-to-back unfiltered runs of the
	//     same tree, same 5808 subtests, no cache, read displayLabelStrip at 702
	//     and then 660 calls — the variance sits entirely in the AGREED arm
	//     (674 vs 632) while bare (22) and MANUFACTURED (6) were identical, and
	//     while existsSortSplit was bit-stable at 44/44/0. So this site's total
	//     is a property of the RUN, not of the tree, and any single number
	//     written down here is stale the next time anyone checks it. What is
	//     invariant, and all the health claim needs, is that it is far from
	//     zero: a zero here would mean the instrument is dead rather than clean.
	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		for _, c := range []values.QualifierRecoveryClass{values.QualRecDiverged, values.QualRecManufactured} {
			if len(witnesses[s][c]) >= qualRecWitnessCap {
				failed = true
				fmt.Fprintf(w, "FAIL: %s retained %d %s witnesses, SATURATING the cap of %d.\n"+
					"  This is an ANOMALY population, so its witness list is the evidence acted\n"+
					"  on and a subset presented as a whole sends the reader to whichever\n"+
					"  spellings arrived first. Narrow the corpus or raise the cap — do not read\n"+
					"  the listed witnesses as complete.\n",
					QualifierRecoverySiteLabel(s), len(witnesses[s][c]), QualifierRecoveryClassLabel(c), qualRecWitnessCap)
			}
		}
		for _, c := range []values.QualifierRecoveryClass{values.QualRecAgreed, values.QualRecLeafOnly, values.QualRecHeuristicDecline} {
			if len(witnesses[s][c]) >= qualRecWitnessCap {
				fmt.Fprintf(w, "NOTE: %s %s witnesses TRUNCATED at the cap of %d — the listed\n"+
					"  spellings are examples, not the whole set. The COUNT above is complete and\n"+
					"  is what this class's claim rests on.\n", QualifierRecoverySiteLabel(s), QualifierRecoveryClassLabel(c), qualRecWitnessCap)
			}
		}
	}

	// RETIREMENT. Checked before — and independently of — the floors, because a
	// retired arm is a fact about the TREE and stays true over the empty
	// population a -test.run filter leaves behind, where every floor is dropped.
	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		if retiredCalls[s] {
			total := splitPopulation(counts, s) + counts[s][values.QualRecCarried]
			if total > 0 {
				failed = true
				fmt.Fprintf(w, "FAIL: %s reported %d call(s) at an ALL-CALL RETIRED site.\n"+
					"  Zero is the steady state; GROWTH, including CARRIED, means the deleted classifier returned.\n"+
					"  Do not restore a population floor or manufacture recorder traffic.\n", QualifierRecoverySiteLabel(s), total)
			}
			continue
		}
		if !retiredSplit[s] {
			continue
		}
		split := splitPopulation(counts, s)
		if split == 0 {
			continue
		}
		failed = true
		fmt.Fprintf(w, "FAIL: %s entered a SPLITTING arm %d time(s), and that arm is RETIRED.\n"+
			"  This site's alarm is inverted: zero is the steady state and GROWTH is the\n"+
			"  finding, because a split here means the rendered-name recovery this census\n"+
			"  was built to watch has come BACK. Do not raise a floor to match — find what\n"+
			"  reintroduced the slice and route it through the structured identity.\n",
			QualifierRecoverySiteLabel(s), split)
	}

	if floors == nil {
		return failed
	}
	for s := values.QualifierRecoverySite(0); s < values.QualRecSiteCount; s++ {
		if retiredCalls[s] {
			continue
		}
		total := 0
		for c := values.QualifierRecoveryClass(0); c < values.QualRecClassCount; c++ {
			total += counts[s][c]
		}
		if total < floors.Calls[s] {
			failed = true
			fmt.Fprintf(w, "FAIL: %s reported %d call(s), below its floor of %d — the site has gone\n"+
				"  DARK. A splitter at zero reads exactly like a splitter measured clean, and\n"+
				"  every zero this census asserts about it is worthless over an empty\n"+
				"  population. Either the shapes that drive it stopped being planned, or the\n"+
				"  recorder was dropped from the site.\n", QualifierRecoverySiteLabel(s), total, floors.Calls[s])
		}
		if retiredSplit[s] {
			// The retirement check above already owns this site, with the words
			// that fit it. Running the declaration check too would report one
			// event twice and tell the reader to raise a floor on an arm that is
			// supposed to be gone.
			continue
		}
		split := splitPopulation(counts, s)
		switch f := floors.Split[s]; {
		case f > 0 && split < f:
			failed = true
			fmt.Fprintf(w, "FAIL: %s entered a SPLITTING arm %d time(s), below its split floor of %d.\n"+
				"  Its total call count can stay healthy on CARRIED traffic alone, so this is\n"+
				"  the floor — and the only floor — that says the arms this census measures are\n"+
				"  still being reached.\n", QualifierRecoverySiteLabel(s), split, f)
		case f == 0 && split > 0:
			failed = true
			fmt.Fprintf(w, "FAIL: %s declares a split floor of 0 — WATCHED, NOT PROVEN, meaning its\n"+
				"  splitting arms were measured empty over this corpus and are covered by a unit\n"+
				"  wiring pin instead. They are no longer empty: %d split call(s) now arrive\n"+
				"  here. This is not a defect, it is the DECLARATION GOING STALE. Re-read the\n"+
				"  site, then raise this floor to a real number so the corpus starts carrying\n"+
				"  the guarantee the unit pin was standing in for.\n", QualifierRecoverySiteLabel(s), split)
		}
	}
	return failed
}

// splitPopulation is the calls at a site that entered a SPLITTING arm — every
// class but CARRIED, which is the one that decides from a structured identity
// without slicing a name.
func splitPopulation(counts [values.QualRecSiteCount][values.QualRecClassCount]int, s values.QualifierRecoverySite) int {
	split := 0
	for _, c := range []values.QualifierRecoveryClass{
		values.QualRecAgreed, values.QualRecDiverged, values.QualRecManufactured,
		values.QualRecLeafOnly, values.QualRecBare, values.QualRecHeuristicDecline,
	} {
		split += counts[s][c]
	}
	return split
}
