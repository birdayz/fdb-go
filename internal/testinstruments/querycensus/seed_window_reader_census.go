// Portions derived from FoundationDB Record Layer (PartitionSelectRule.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package querycensus

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The SEED-WINDOW READER census: a STANDING instrument over every keyed read of
// an OrdinalSeedLegWindows map.
//
// The map is keyed by CorrelationIdentifier. Every claim about its readers has
// the shape "this class is EMPTY", and an unreached site prints that
// identically to a site measured clean, so without floors a change that
// silenced a reader would read GREEN.
//
// The census is therefore NARROW. Per read it records only whether a window
// was found, plus the two DECLINE classes that are hard zeros, and it floors
// each site's population. It keeps five readers, and two declines, from going
// quiet.
//
// THE TWO HARD ZEROS each name what their own non-zero re-arms:
//
//   - QUALIFIED-NO-IDENTITY (slotInGatheredSeed): a group-by reference that
//     STATES a qualifier and carries no correlation — the flat-dotted spelling,
//     whose qualifier was sliced out of a column name. It declines, because a
//     qualifier that cannot select a leg must not fall through to the bare
//     namespace where the element answers first. A non-zero says a producer is
//     routing flat-dotted names into the group-slot resolver again, and that
//     shape is now silently going to the name model instead of resolving.
//   - CHILDLESS-BAKED (rebaseLegRefsToBox): a SOURCE-RELATIVE baked read with no
//     child. The walk admits it so it can be rebased, but it carries no
//     correlation, so no window can be selected and it declines. A non-zero says
//     the wrap is turning itself off for real traffic — and that the rebase owes
//     that shape an answer, since its ordinal is leg-relative and there is
//     nothing else that can rebase it.
//
// Neither zero means "this cannot happen". Both mean "this does not happen
// TODAY, and if it starts, the wrap/gather quietly stops firing for that shape".
// That is a fact about coverage, and it is exactly the fact a printed report
// cannot keep.
//
// GATED by LegIdentityCensusEnabled, like every census on this path: disabled,
// each reader pays one predicate. Counts are per READ.

var (
	seedWindowReadMu     sync.Mutex
	seedWindowReadCounts [values.SeedWindowSiteCount][values.SeedWindowReadClassCount]int
)

// RecordSeedWindowRead counts one keyed read of a seed-window map. Sites reach
// it only via values.Census().
func RecordSeedWindowRead(site values.SeedWindowSite, class values.SeedWindowReadClass) {
	if site < 0 || site >= values.SeedWindowSiteCount || class < 0 || class >= values.SeedWindowReadClassCount {
		return
	}
	seedWindowReadMu.Lock()
	defer seedWindowReadMu.Unlock()
	seedWindowReadCounts[site][class]++
}

// seedWindowLookupClass decides the common shape's bucket. Split from the
// counter mutation so the decision can be exercised without process-global
// state, exactly as its sibling censuses split theirs — a test that has to reset
// a package-level counter cannot be parallel, and a census whose classification
// is only reachable through a mutation is a census whose classification is
// untested.
func seedWindowLookupClass(found bool) values.SeedWindowReadClass {
	if found {
		return values.SeedWindowHit
	}
	return values.SeedWindowMiss
}

// seedWindowLookupClassOfKind is the KIND-AWARE classifier: a hit on a NESTED
// window is its own class, because whether that arm is entered at all is the
// live-vs-latent question for RFC-200 step 3d and nothing else can see it.
func seedWindowLookupClassOfKind(found bool, kind values.LegKind) values.SeedWindowReadClass {
	if found && kind == values.LegKindNested {
		return values.SeedWindowNestedHit
	}
	return seedWindowLookupClass(found)
}

// RecordSeedWindowLookupOfKind is the kind-aware counterpart of
// RecordSeedWindowLookup, for the readers that dispatch on the window kind.
func RecordSeedWindowLookupOfKind(site values.SeedWindowSite, found bool, kind values.LegKind) {
	RecordSeedWindowRead(site, seedWindowLookupClassOfKind(found, kind))
}

// RecordSeedWindowLookup is the common shape: found or not.
func RecordSeedWindowLookup(site values.SeedWindowSite, found bool) {
	RecordSeedWindowRead(site, seedWindowLookupClass(found))
}

// SeedWindowReaderCensus reports the per-site class matrix.
func SeedWindowReaderCensus() [values.SeedWindowSiteCount][values.SeedWindowReadClassCount]int {
	seedWindowReadMu.Lock()
	defer seedWindowReadMu.Unlock()
	return seedWindowReadCounts
}

// ResetSeedWindowReaderCensus clears the counters.
func ResetSeedWindowReaderCensus() {
	seedWindowReadMu.Lock()
	defer seedWindowReadMu.Unlock()
	seedWindowReadCounts = [values.SeedWindowSiteCount][values.SeedWindowReadClassCount]int{}
}

// FormatSeedWindowReaderCensus renders the per-site table for a harness to log.
func FormatSeedWindowReaderCensus() string {
	counts := SeedWindowReaderCensus()
	var b strings.Builder
	b.WriteString("seed-window readers (per keyed read):")
	for s := values.SeedWindowSite(0); s < values.SeedWindowSiteCount; s++ {
		total := 0
		for c := 0; c < int(values.SeedWindowReadClassCount); c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(&b, "\n  %-24s reads %d", SeedWindowSiteLabel(s), total)
		for c := values.SeedWindowReadClass(0); c < values.SeedWindowReadClassCount; c++ {
			// NESTED-HIT prints even at ZERO. Its zero is the live-vs-latent
			// answer for RFC-200 step 3d, and a class that vanishes when empty is
			// exactly the reporting habit that makes a latent arm indistinguishable
			// from an absent one.
			if counts[s][c] != 0 || c == values.SeedWindowNestedHit {
				fmt.Fprintf(&b, " | %s %d", SeedWindowReadClassLabel(c), counts[s][c])
			}
		}
	}
	return b.String()
}

// SeedWindowReaderFloors is the minimum read count each site must report over a
// whole suite run.
//
// Floored per SITE rather than in total, because the sites do not substitute for
// one another: a total floor stays satisfied with individual readers dark. That
// is the whole reason this instrument exists — the predecessor's deletion took
// away the only thing that could tell a silenced reader from a quiet corpus.
//
// A total floor would also stay satisfied by the loudest site (gatheredGroupSlot)
// alone, so per-site flooring is what keeps each reader visible.
type SeedWindowReaderFloors struct {
	// Reads is the per-site minimum, indexed by site. A zero entry means the site
	// is not floored, which is a statement about the corpus and not an omission.
	Reads [values.SeedWindowSiteCount]int

	// Retired marks a site whose expected population is ZERO. Its alarm points
	// the other way from a floor: any read means the retired traffic came back.
	// A retired site must not also be floored.
	Retired [values.SeedWindowSiteCount]bool

	// NestedHitMustBeZero asserts that NO read selects a NESTED window.
	//
	// IT IS A TRIPWIRE, NOT A DEFECT GATE, and the inversion is the whole point:
	// a non-zero here is GOOD NEWS that requires ACTION. RFC-200's nested reader
	// arm is correct, cross-agreement-pinned on both entries and unit-pinned on
	// both arms, and no corpus query reaches it — a nested SUB-window is only
	// selected by a reference to a leg buried INSIDE the merge. Gate (a)'s four
	// mutation directions are therefore not yet writable.
	//
	// Without this assertion, activation day changes nothing visible. Whoever
	// produces a query whose reference reaches a buried leg would see a green
	// suite, a printed NESTED-HIT that nobody diffs, and gate (a) left unwritten
	// BY DEFAULT rather than by decision. This is what turns that day into a red
	// test with the hand-over in its failure message.
	//
	// Typing the bare-QOV result values does not reach that day: they are
	// already typed — every declined leg carries a real RecordType — and the leg
	// walk refuses on values.IsPositionalMergeRC, which needs a
	// *RecordConstructorValue that no QuantifiedObjectValue is at any typing.
	// `bare` there means identity PASS-THROUGH, not untyped.
	//
	// What would actually trip this tripwire is the SHAPE conversion: giving the
	// declined leg an RC(_i: QOV(leg_i)) result value, so a reference can reach a
	// leg inside the merge. That is a different change with a different risk
	// surface, and it is booked separately.
	NestedHitMustBeZero bool
}

// AssertSeedWindowReaderCensus checks the two hard zeros and the floors.
//
// floors is nil when the run is NARROWED (-test.run), exactly as its siblings do
// it: the zeros still run, over whatever population the filter reached, and at
// zero they hold vacuously.
func AssertSeedWindowReaderCensus(w io.Writer, floors *SeedWindowReaderFloors) bool {
	return assertSeedWindowReaderCounts(w, SeedWindowReaderCensus(), floors)
}

func assertSeedWindowReaderCounts(w io.Writer, counts [values.SeedWindowSiteCount][values.SeedWindowReadClassCount]int, floors *SeedWindowReaderFloors) bool {
	failed := false
	for s := values.SeedWindowSite(0); s < values.SeedWindowSiteCount; s++ {
		if n := counts[s][values.SeedWindowQualifiedNoIdentity]; n != 0 {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS FAIL: %s reported %d QUALIFIED-NO-IDENTITY reads, want 0.\n"+
				"  A group-by reference STATED a qualifier and carried no correlation — the\n"+
				"  flat-dotted spelling, whose qualifier was sliced out of a column name. It\n"+
				"  DECLINED, which is correct: a qualifier that cannot select a leg must not\n"+
				"  fall through to the bare namespace, where a same-named element answers\n"+
				"  first and the reference silently reads a different column.\n"+
				"  WHAT A NON-ZERO RE-ARMS: some producer is routing flat-dotted names into\n"+
				"  the group-slot resolver again, and that shape is now going to the NAME\n"+
				"  MODEL instead of resolving positionally. Find the producer and give it a\n"+
				"  correlation (CQ-52 hands this site the parser's qualifier/leaf segments).\n"+
				"  Do NOT re-key by the sliced qualifier: that mints an identifier out of a\n"+
				"  column name.\n", SeedWindowSiteLabel(s), n)
		}
		if n := counts[s][values.SeedWindowChildlessBaked]; n != 0 {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS FAIL: %s reported %d CHILDLESS-BAKED reads, want 0.\n"+
				"  A SOURCE-RELATIVE baked read with NO CHILD reached the box rebase's tail.\n"+
				"  The walk admits such a node deliberately so it can be rebased, but with no\n"+
				"  correlation there is no window to select, so the whole wrap DECLINED to the\n"+
				"  name model.\n"+
				"  WHAT A NON-ZERO RE-ARMS: the wrap is switching itself off for real traffic,\n"+
				"  and this shape has no other route — its ordinal is LEG-relative, so nothing\n"+
				"  downstream can rebase it onto the box row. Either the producer should be\n"+
				"  emitting the QOV-shaped spelling (which carries the correlation the rebase\n"+
				"  needs), or the rebase owes this shape an answer.\n", SeedWindowSiteLabel(s), n)
		}
	}
	if floors != nil {
		for s := values.SeedWindowSite(0); s < values.SeedWindowSiteCount; s++ {
			if !floors.Retired[s] {
				continue
			}
			total := 0
			for c := 0; c < int(values.SeedWindowReadClassCount); c++ {
				total += counts[s][c]
			}
			if total == 0 {
				continue
			}
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS FAIL: RETIRED site %s reached %d reads, want 0.\n"+
				"  The alarm direction is GROWTH: this reader's expected population is zero,\n"+
				"  so a read means the traffic it served is being produced again.\n", SeedWindowSiteLabel(s), total)
			if s == values.SeedWindowSiteExistentialRebase {
				fmt.Fprint(w, "  An existential's correlated predicate reached ImplementNestedLoopJoin's\n"+
					"  two-quantifier select beside an ordinal-seed outer. Java's\n"+
					"  PartitionSelectRule cannot produce that: the predicate is placed in the\n"+
					"  lower when the existential does not depend on the lowers, and a positional\n"+
					"  merge of several lowers is refused when it does. Find the producer that\n"+
					"  hoisted the predicate out of the existential's partition.\n")
			}
		}
	}
	if floors != nil && floors.NestedHitMustBeZero {
		for s := values.SeedWindowSite(0); s < values.SeedWindowSiteCount; s++ {
			n := counts[s][values.SeedWindowNestedHit]
			if n == 0 {
				continue
			}
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS: %s reported %d NESTED-HIT read(s).\n"+
				"  NESTED-HIT is non-zero: RFC-200's nested reader arm is now LIVE. Gate\n"+
				"  (a)'s four mutation directions are writable; WRITE THEM.\n"+
				"\n"+
				"  THIS IS NOT A DEFECT. It is the hand-over RFC-200 merged with, and it is\n"+
				"  a red test rather than a printed number precisely so that activation day\n"+
				"  cannot pass unnoticed. The nested arm was correct, cross-agreement-pinned\n"+
				"  on both entries and unit-pinned on both arms, and simply unreached: a\n"+
				"  nested SUB-window is only selected by a reference to a leg buried INSIDE\n"+
				"  the merge, and no corpus query produced one. Something now does.\n"+
				"\n"+
				"  WHAT TO DO. The fixture is already built and carries the three properties\n"+
				"  the gate needs — distinct leg widths (3/1/2), a duplicate column name\n"+
				"  across legs so no name fallback can rescue a wrong ordinal, and a\n"+
				"  projection from a non-first leg at a non-zero leg-local ordinal:\n"+
				"    pkg/relational/sqldriver/nested_merge_leg_wrong_rows_fdb_test.go\n"+
				"  Mutate FOUR directions, each SEPARATELY, and record the outcomes in that\n"+
				"  fixture's own doc comment (a mutation result living only in a PR\n"+
				"  description is a measurement that evaporates):\n"+
				"    1. a NESTED window read as FLAT (Offset + legOrdinal);\n"+
				"    2. a FLAT window read as NESTED (the fused two-step);\n"+
				"    3. leg ORIENTATION swapped relative to the seed's baked layout —\n"+
				"       record the OBSERVED failure mode. The site claims an ordinal read\n"+
				"       there is \"either right or loud\", so a loud unbound-correlation\n"+
				"       error or an unexecutable plan is expected; SILENTLY WRONG ROWS\n"+
				"       would REFUTE that claim and are a finding in their own right,\n"+
				"       needing their own line in RFC-200 and a report before the gate is\n"+
				"       called satisfied;\n"+
				"    4. the census-invisible bare-column arm of query.slotInGatheredSeed\n"+
				"       allowed to contribute a hits++ for a nested window.\n"+
				"  Then clear NestedHitMustBeZero, floor NESTED-HIT instead, and mark\n"+
				"  CQ-67's gate (a) satisfied.\n", SeedWindowSiteLabel(s), n)
		}
	}
	if floors == nil {
		return failed
	}
	for s := values.SeedWindowSite(0); s < values.SeedWindowSiteCount; s++ {
		if floors.Reads[s] == 0 {
			continue
		}
		total := 0
		for c := 0; c < int(values.SeedWindowReadClassCount); c++ {
			total += counts[s][c]
		}
		if total < floors.Reads[s] {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS FAIL: %s reached %d reads, want >= %d.\n"+
				"  Nothing keyed this site's window map, so every zero it reports holds\n"+
				"  VACUOUSLY — including the two decline classes whose emptiness is the whole\n"+
				"  claim that the gather and the wrap are firing on the shapes they exist for.\n"+
				"  A silenced reader and a clean one print the same line; this floor is the\n"+
				"  only thing that tells them apart.\n", SeedWindowSiteLabel(s), total, floors.Reads[s])
		}
	}
	return failed
}
