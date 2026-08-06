package values

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// The SEED-WINDOW READER census: a STANDING instrument over every keyed read of
// an OrdinalSeedLegWindows map.
//
// It replaces a wider, one-shot predecessor. That one — the seed-window KEY
// PROVENANCE census — existed to answer a question that is now settled: the map
// used to be keyed by UPPER-FOLDED TEXT while each window carried a stated leg
// IDENTITY, and every keyed read was therefore a place where a leg was selected
// by text with an identifier sitting inside the thing selected. It measured, per
// lookup, whether the identity would have selected the same window. Over the
// whole real-FDB sqldriver corpus it reported 1400 lookups with every blocking
// class empty (no TEXT-ONLY-HIT, no IDENTITY-ONLY-HIT, no DIVERGED), and the two
// readers that held text and no identity reached ZERO and were unreachable by
// panic probe across ./pkg/relational/... and ./pkg/recordlayer/query/... . That
// is a DATED POINT MEASUREMENT, quoted here as history. The map is keyed by
// CorrelationIdentifier now; the text namespace it measured does not exist, and
// with it gone the predecessor's classes had nothing left to distinguish.
//
// What did NOT go away is the reason the predecessor was load-bearing: it was
// the only thing that could tell an exercised reader from a dark one. Every
// claim the conversion rests on has the shape "this class is EMPTY", and an
// unreached site prints that identically to a site measured clean. Deleting the
// instrument along with the question it answered left five readers with nothing
// asserting they still run — a future change that silenced one would read GREEN.
//
// So this census is NARROW where its predecessor was broad. Per read it records
// only whether a window was found, plus the two DECLINE classes that are hard
// zeros, and it floors each site's population. It is not trying to license a
// conversion; it is keeping five readers, and two declines, from going quiet.
//
// THE TWO HARD ZEROS are the new part, and each names what its own non-zero
// re-arms:
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

// SeedWindowSite is one keyed reader of a seed-window map. These are ALL of them
// in production. The map's remaining call sites consume it as a nil/non-nil
// PREDICATE ("is this an ordinal seed?") and never key it, which is not a read
// and is not counted here.
type SeedWindowSite int

const (
	// SeedWindowSiteExistentialRebase is cascades.rebaseOuterLegValueOrdinal's
	// per-reference window lookup — the EXISTS-over-join ordinal rebase, keyed by
	// the reference's own QuantifiedObjectValue correlation. The corpus's
	// heaviest reader by an order of magnitude.
	SeedWindowSiteExistentialRebase SeedWindowSite = iota

	// SeedWindowSiteBoxLegRef is query.rebaseLegRefsToBox's QOV-shaped arm, keyed
	// by the reference's own correlation. It is also where CHILDLESS-BAKED is
	// recorded: the same walk, one arm further down.
	SeedWindowSiteBoxLegRef

	// SeedWindowSiteBoxSurvivorQOV is that walk's post-verification: does any
	// leg-correlated QuantifiedObjectValue survive the rebase? Keyed by the
	// surviving QOV's own correlation. Its reads are overwhelmingly MISSES by
	// construction — a hit is a decline — so its population, not its outcome, is
	// what this census is watching.
	SeedWindowSiteBoxSurvivorQOV

	// SeedWindowSiteBoxSurvivorCorrelation is the wrap's correct-or-decline net
	// over a translated subquery's correlation set, keyed by each correlation in
	// that set. The smallest population of the five and the one most likely to go
	// dark unnoticed.
	SeedWindowSiteBoxSurvivorCorrelation

	// SeedWindowSiteGatheredGroupSlot is query.slotInGatheredSeed's qualified
	// arm — a group-by key or aggregate operand resolving to a flat seed slot. It
	// is where QUALIFIED-NO-IDENTITY is recorded.
	SeedWindowSiteGatheredGroupSlot

	seedWindowSiteCount
)

func (s SeedWindowSite) String() string {
	switch s {
	case SeedWindowSiteExistentialRebase:
		return "existentialRebase"
	case SeedWindowSiteBoxLegRef:
		return "boxLegRef"
	case SeedWindowSiteBoxSurvivorQOV:
		return "boxSurvivorQOV"
	case SeedWindowSiteBoxSurvivorCorrelation:
		return "boxSurvivorCorrelation"
	case SeedWindowSiteGatheredGroupSlot:
		return "gatheredGroupSlot"
	default:
		return "unknown"
	}
}

// SeedWindowReadClass is one read's bucket. The four partition every read.
type SeedWindowReadClass int

const (
	// SeedWindowHit: the reference's identity selected a window.
	SeedWindowHit SeedWindowReadClass = iota
	// SeedWindowMiss: no window is filed under that identity. Not an error at any
	// of these sites — a non-leg reference, or (at the survivor sites) the
	// ordinary case.
	SeedWindowMiss
	// SeedWindowQualifiedNoIdentity: a QUALIFIED reference arrived with no
	// correlation, so no window could be selected for it and it declined. HARD
	// ZERO. See the header for what a non-zero re-arms.
	SeedWindowQualifiedNoIdentity
	// SeedWindowChildlessBaked: a CHILDLESS source-relative baked read reached
	// the rebase walk's tail, so no window could be selected for it and the whole
	// wrap declined. HARD ZERO. See the header.
	SeedWindowChildlessBaked

	// SeedWindowNestedHit: the reference's identity selected a window whose Kind
	// is LegKindNested — i.e. the read took the FUSED two-step address rather
	// than flat offset arithmetic.
	//
	// IT IS A HIT, COUNTED SEPARATELY, and it exists to answer one question no
	// other number on this path can: is RFC-200's nested reader arm LIVE on this
	// corpus, or is it correct-but-unentered? Those two states print identically
	// everywhere else — 174 leg-local reads unchanged, EXPLAIN identical,
	// MergedReAnchor 0 — and RFC-200 §6 predicts explicitly that
	// existentialRebase GROWS because the newly-accepted firings' existPreds
	// rebase through it. A prediction with no instrument is a prediction nothing
	// can refute.
	//
	// A read counted here is NOT also counted as SeedWindowHit; the classes
	// partition.
	SeedWindowNestedHit

	seedWindowReadClassCount
)

func (c SeedWindowReadClass) String() string {
	switch c {
	case SeedWindowHit:
		return "hit"
	case SeedWindowMiss:
		return "miss"
	case SeedWindowQualifiedNoIdentity:
		return "QUALIFIED-NO-IDENTITY"
	case SeedWindowChildlessBaked:
		return "CHILDLESS-BAKED"
	case SeedWindowNestedHit:
		return "NESTED-HIT"
	default:
		return "unknown"
	}
}

var (
	seedWindowReadMu     sync.Mutex
	seedWindowReadCounts [seedWindowSiteCount][seedWindowReadClassCount]int
)

// RecordSeedWindowRead counts one keyed read of a seed-window map. Callers must
// guard on LegIdentityCensusEnabled().
func RecordSeedWindowRead(site SeedWindowSite, class SeedWindowReadClass) {
	if site < 0 || site >= seedWindowSiteCount || class < 0 || class >= seedWindowReadClassCount {
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
func seedWindowLookupClass(found bool) SeedWindowReadClass {
	if found {
		return SeedWindowHit
	}
	return SeedWindowMiss
}

// seedWindowLookupClassOfKind is the KIND-AWARE classifier: a hit on a NESTED
// window is its own class, because whether that arm is entered at all is the
// live-vs-latent question for RFC-200 step 3d and nothing else can see it.
func seedWindowLookupClassOfKind(found bool, kind LegKind) SeedWindowReadClass {
	if found && kind == LegKindNested {
		return SeedWindowNestedHit
	}
	return seedWindowLookupClass(found)
}

// RecordSeedWindowLookupOfKind is the kind-aware counterpart of
// RecordSeedWindowLookup, for the readers that dispatch on the window kind.
func RecordSeedWindowLookupOfKind(site SeedWindowSite, found bool, kind LegKind) {
	RecordSeedWindowRead(site, seedWindowLookupClassOfKind(found, kind))
}

// RecordSeedWindowLookup is the common shape: found or not.
func RecordSeedWindowLookup(site SeedWindowSite, found bool) {
	RecordSeedWindowRead(site, seedWindowLookupClass(found))
}

// SeedWindowReaderCensus reports the per-site class matrix.
func SeedWindowReaderCensus() [seedWindowSiteCount][seedWindowReadClassCount]int {
	seedWindowReadMu.Lock()
	defer seedWindowReadMu.Unlock()
	return seedWindowReadCounts
}

// ResetSeedWindowReaderCensus clears the counters.
func ResetSeedWindowReaderCensus() {
	seedWindowReadMu.Lock()
	defer seedWindowReadMu.Unlock()
	seedWindowReadCounts = [seedWindowSiteCount][seedWindowReadClassCount]int{}
}

// FormatSeedWindowReaderCensus renders the per-site table for a harness to log.
func FormatSeedWindowReaderCensus() string {
	counts := SeedWindowReaderCensus()
	var b strings.Builder
	b.WriteString("seed-window readers (per keyed read):")
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		total := 0
		for c := 0; c < int(seedWindowReadClassCount); c++ {
			total += counts[s][c]
		}
		fmt.Fprintf(&b, "\n  %-24s reads %d", s, total)
		for c := SeedWindowReadClass(0); c < seedWindowReadClassCount; c++ {
			// NESTED-HIT prints even at ZERO. Its zero is the live-vs-latent
			// answer for RFC-200 step 3d, and a class that vanishes when empty is
			// exactly the reporting habit that makes a latent arm indistinguishable
			// from an absent one.
			if counts[s][c] != 0 || c == SeedWindowNestedHit {
				fmt.Fprintf(&b, " | %s %d", c, counts[s][c])
			}
		}
	}
	return b.String()
}

// SeedWindowReaderFloors is the minimum read count each site must report over a
// whole suite run.
//
// Floored per SITE rather than in total, because the sites do not substitute for
// one another: the existential rebase carries an order of magnitude more traffic
// than the rest put together, so a total floor would stay satisfied with the
// other four dark. That asymmetry is the whole reason this instrument exists —
// the predecessor's deletion took away the only thing that could tell a silenced
// reader from a quiet corpus.
type SeedWindowReaderFloors struct {
	// Reads is the per-site minimum, indexed by site. A zero entry means the site
	// is not floored, which is a statement about the corpus and not an omission.
	Reads [seedWindowSiteCount]int

	// NestedHitMustBeZero asserts that NO read selects a NESTED window.
	//
	// IT IS A TRIPWIRE, NOT A DEFECT GATE, and the inversion is the whole point:
	// a non-zero here is GOOD NEWS that requires ACTION. RFC-200's nested reader
	// arm is correct, cross-agreement-pinned on both entries and unit-pinned on
	// both arms, and no corpus query reaches it — a nested SUB-window is only
	// selected by a reference to a leg buried INSIDE the merge. Gate (a)'s four
	// mutation directions are therefore not writable, and the branch merged with
	// that stated.
	//
	// Without this assertion, activation day changes nothing visible. Whoever
	// produces a query whose reference reaches a buried leg would see a green
	// suite, a printed NESTED-HIT that nobody diffs, and gate (a) left unwritten
	// BY DEFAULT rather than by decision. This is what turns that day into a red
	// test with the hand-over in its failure message.
	//
	// THE ROUTE TO THAT DAY IS NOT THE ONE THIS COMMENT USED TO NAME. It said
	// "typing the 94 bare-QOV result values", which was a plan that has since
	// been REFUTED by measurement: the population is 102 and it is 100% typed
	// already — every declined leg carries a real RecordType (arity 1-3 on the
	// FlatMap legs, 1-4 counting the NestedLoopJoin-legged ones) — and typing
	// could not convert one of them in any case, because legOrdinalSafety refuses
	// on values.IsPositionalMergeRC, which needs a *RecordConstructorValue that no
	// QuantifiedObjectValue is at any typing. `bare` there meant identity
	// PASS-THROUGH, never untyped.
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

func assertSeedWindowReaderCounts(w io.Writer, counts [seedWindowSiteCount][seedWindowReadClassCount]int, floors *SeedWindowReaderFloors) bool {
	failed := false
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		if n := counts[s][SeedWindowQualifiedNoIdentity]; n != 0 {
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
				"  column name.\n", s, n)
		}
		if n := counts[s][SeedWindowChildlessBaked]; n != 0 {
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
				"  needs), or the rebase owes this shape an answer.\n", s, n)
		}
	}
	if floors != nil && floors.NestedHitMustBeZero {
		for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
			n := counts[s][SeedWindowNestedHit]
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
				"  CQ-67's gate (a) satisfied.\n", s, n)
		}
	}
	if floors == nil {
		return failed
	}
	for s := SeedWindowSite(0); s < seedWindowSiteCount; s++ {
		if floors.Reads[s] == 0 {
			continue
		}
		total := 0
		for c := 0; c < int(seedWindowReadClassCount); c++ {
			total += counts[s][c]
		}
		if total < floors.Reads[s] {
			failed = true
			fmt.Fprintf(w, "SEED-WINDOW READER CENSUS FAIL: %s reached %d reads, want >= %d.\n"+
				"  Nothing keyed this site's window map, so every zero it reports holds\n"+
				"  VACUOUSLY — including the two decline classes whose emptiness is the whole\n"+
				"  claim that the gather and the wrap are firing on the shapes they exist for.\n"+
				"  A silenced reader and a clean one print the same line; this floor is the\n"+
				"  only thing that tells them apart.\n", s, total, floors.Reads[s])
		}
	}
	return failed
}
