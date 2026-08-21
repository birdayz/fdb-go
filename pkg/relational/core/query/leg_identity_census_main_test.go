package query

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMain runs this package's corpus under the leg-identity census and asserts
// its invariants — the second of the two harnesses that drive it (CQ-61).
//
// WHY THIS PACKAGE NEEDS ITS OWN. The census started with a single harness, the
// real-FDB SQL suite, and that suite's corpus is executor-shaped: it drives the
// per-row binders heavily and the TRANSLATOR's own leg readers barely. The site
// that resolves a qualified column inside a leg window is planning-time, and the
// shapes that stress it — multi-alias box outers, buried leaves under a gated
// join, seed-window derivation over a clustered box — are built from logical
// operators here. Leaving this corpus uninstrumented is why the anomalous pairs
// this package drives could be attributed to live traffic: nothing measured them,
// so nothing could say where they came from.
//
// WHAT THIS CORPUS IS, AND WHAT THAT COSTS. Unlike the SQL suite, whose every leg
// comes from a production producer because its input is SQL, this package's tests
// call package-private resolvers with HAND-BUILT leg fixtures — including one that
// exists precisely to drive the forgery pairs the identity comparison must
// decline. Those pairs land in the census like any other, so this corpus cannot
// assert a bare zero: the fixture's deliberate declines would fail it, and
// deleting the fixture to make the gate green would delete the test that proves
// the conversion does anything at all.
//
// So the assertion here is a set equality, not a zero — see
// negativeControlWitnesses. Any anomalous pair the fixtures do not account for is
// a producer defect and fails.
//
// The census counters are package-scoped in values and Go orders no tests, so the
// assertion belongs after m.Run() and nowhere else.
func TestMain(m *testing.M) {
	values.ResetLegIdentityCensus()
	values.ResetQualifierRecoveryCensus()
	values.SetLegIdentityCensusEnabled(true)
	code := m.Run()
	values.SetLegIdentityCensusEnabled(false)

	values.ReportLegIdentityCensus(os.Stderr, "translator corpus")
	if failed := assertTranslatorLegIdentityCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	// The QUALIFIER RECOVERY census — the four DARK SPLITTERS. It runs on BOTH
	// corpora, and this one is not the junior partner: three of its six sites
	// live in the translator and two of those are reached by shapes this
	// package builds from logical operators directly. A census asserted only
	// over the SQL suite would report them dark and be believed.
	if failed := assertTranslatorQualifierRecoveryCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// translatorQualifierRecoveryFloors is the minimum population this corpus must
// report at the sites it actually drives.
//
// Floors only those. A floor on a site this package never reaches would assert
// something about a different corpus; the sqldriver harness floors over its own,
// and the two sets together are what keeps every zero non-vacuous SOMEWHERE.
//
// Set well below the measured populations for the same reason its siblings are:
// these totals vary run to run (several sites sit inside Cascades rules and the
// memo may explore a rule once or many times for one query), so a floor at the
// measured value would fail on exploration order. The floor detects COLLAPSE —
// the shapes stopping, a recorder being routed around — not drift.
// Measured over this corpus after recursive remapping was retired:
// existsSortSplit 5, derivedUnnestSource 4.
//
// THE PROVENANCE OF EACH FLOOR IS NOT THE SAME, and reading them as one kind of
// number is how a floor comes to guarantee something it does not:
//
//   - recursiveRemap is intentionally zero: recursiveRemapValues is a retired
//     compatibility no-op, pinned by TestQualRecWiring_RetiredRecursiveRemapIsInert.
//     It is declared in translatorQualifierRecoveryRetiredSplit rather than as a
//     Split zero, because a retirement is a fact about the tree and must survive
//     a -test.run filter that drops every floor.
//   - existsSortSplit's and derivedUnnestSource's populations here are FIXTURE
//     traffic, driven entirely by qualifier_recovery_wiring_test.go. Their
//     floors say THE PINS STILL RUN — nothing more. Production coverage of
//     these two sites lives in the sqldriver corpus (44 and 13 calls), and it
//     is that corpus's floors that carry it.
//
// They are declared rather than omitted because the split floor is checked in
// the STALE direction: a site whose split population went non-zero cannot keep a
// "watched, not proven" 0. That check is what forced these two lines to be
// written when the pins landed, which is the mechanism working.
//
// The remaining three sites are 0 here and their zero is STRUCTURAL:
// projScopeClassify, projQualVsScan and displayLabelStrip live in core/embedded,
// which this package does not import. No floor on them could ever be anything
// but 0, and their production coverage is the sqldriver corpus's alone.
//
// Set below the measured values because these totals vary run to run — several
// sites sit inside Cascades rules and the memo may explore one once or many
// times for a single query. The floor detects COLLAPSE, not drift.
var translatorQualifierRecoveryFloors = values.QualifierRecoveryFloors{
	Calls: [6]int{
		values.QualRecSiteExistsSortSplit:     2,
		values.QualRecSiteDerivedUnnestSource: 2,
	},
	Split: [6]int{
		values.QualRecSiteExistsSortSplit: 2,
		// 2 -> 1 when classifyDerivedUnnestArray converted to the parse-tree
		// triple. Only a slot with NO triple still splits, and this package's
		// population is the wiring pin: three of its four arms now record
		// CARRIED and one records MANUFACTURED.
		//
		// This floor no longer measures "does the site split" — it measures
		// "does the FALLBACK still exist and get exercised", which is what
		// keeps the fallback from rotting untested while every real caller
		// carries segments.
		values.QualRecSiteDerivedUnnestSource: 1,
	},
}

// assertTranslatorQualifierRecoveryCensus checks this corpus's dark-splitter
// census, dropping the population floors when -test.run narrows the corpus.
//
// Same split as every gate on this path: the DIVERGED zero and the saturation
// guards run over whatever population the filter reached, the floors describe
// the unfiltered corpus and are meaningless under one.
func assertTranslatorQualifierRecoveryCensus(w io.Writer) bool {
	floors := &translatorQualifierRecoveryFloors
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "qualifier recovery census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The DIVERGED hard zero and the witness "+
			"saturation guards still run, over whatever population this filter reached — "+
			"at zero the former holds VACUOUSLY.\n", f.Value.String())
		floors = nil
	}
	return values.AssertQualifierRecoveryCensus(w, &values.QualifierRecoveryExpectations{
		Floors:          floors,
		AllowedDiverged: qualifierRecoveryNegativeControls,
		RetiredSplit:    translatorQualifierRecoveryRetiredSplit,
	}, "translator corpus")
}

// translatorQualifierRecoveryRetiredSplit declares the one site in this package
// whose splitting arm is gone from the tree. Any split there is the retired
// recursive remap coming back, and the check survives a narrowed run because a
// tree fact holds over any population — including the empty one.
var translatorQualifierRecoveryRetiredSplit = func() (r [6]bool) {
	r[values.QualRecSiteRecursiveRemap] = true
	return r
}()

// qualifierRecoveryNegativeControls is every DIVERGED witness this package's
// test FIXTURES deliberately drive, and it is complete: any witness outside it
// is a real producer manufacturing a qualifier that contradicts an identity it
// was holding.
//
// They exist because the census asserts DIVERGED at ZERO, and a zero that
// nothing has ever shown could be non-zero is not a measurement. These fixtures
// are what make it one — each is an asserted reach of the bucket in
// qualifier_recovery_wiring_test.go:
//
//	existsSortSplit     "T2.SK" vs identity "T9"
//	    a sort key whose parse-tree triple names T9 while its rendering names T2,
//	    driven from BOTH callers (sortKeySourceValue and sortKeyName) so the
//	    second caller's wiring is pinned too.
//	derivedUnnestSource "T.ARR" vs identity "<unqualified>"
//	    a projection slot whose triple states ONE segment — the delimited
//	    `"T.ARR"` — against a split that manufactured the qualifier T. The
//	    canonical misread this whole workstream is about.
//
// The residual, stated rather than smoothed: a REAL divergence spelled exactly
// like one of these would be absorbed, because witnesses dedup by spelling. That
// is why each is listed individually with the fixture that drives it — a new
// anomaly at a new spelling, which is what a producer change looks like, cannot
// hide.
var qualifierRecoveryNegativeControls = map[values.QualifierRecoverySite]map[string]struct{}{
	values.QualRecSiteExistsSortSplit: {
		`"T2.SK" vs identity "T9"`: {},
	},
	values.QualRecSiteDerivedUnnestSource: {
		`"T.ARR" vs identity "<unqualified>"`: {},
	},
}

// translatorLegIdentityFloors is the minimum population this corpus must report
// at the sites it actually drives.
//
// It floors only those. A floor on a site this package never reaches would be a
// floor asserting something about a different corpus, and the sqldriver harness
// already floors all eight over its own. The two floor sets together are what
// keeps every site's zeros non-vacuous somewhere.
//
// Set an order of magnitude below the measured populations, for the same reason
// the sqldriver floors are: these totals VARY run to run (several sites sit
// inside Cascades rules, and the memo may explore a rule once or many times for
// one query), so a floor at the measured value would fail on exploration order.
// The floor detects COLLAPSE — the shapes stopping, a reader being routed around
// — not drift.
// Measured over this corpus: text-vs-identity 44, finalizeSeedWindows 7,
// ordinalSlotInLegWindow 37. expressionOutputLegs is a retired producer with
// no callers; keeping a population floor for it would assert traffic through
// dead compatibility code rather than exercise a live identity boundary.
var translatorLegIdentityFloors = map[values.LegIdentitySite]int64{
	values.LegSiteTextVsIdentity:         8,
	values.LegSiteFinalizeSeedWindows:    2,
	values.LegSiteOrdinalSlotInLegWindow: 8,
}

// negativeControlWitnesses is every anomalous pair this package's test FIXTURES
// deliberately drive, and it is complete: any witness outside it is a real
// producer storing a leg under one spelling and reading it under another.
//
// All of them come from TestOrdinalSlotInLegWindow, whose whole subject is the
// forgery the identity comparison exists to decline, and every one is an asserted
// DECLINE there:
//
//	"B vs b"     a case-variant qualifier against leg B — a different leg
//	"q$5 vs Q$5" the forgery proper: a lowercase MINTED leg read by the upper
//	             spelling that is also its Name, so only the identity declines
//	"Q$5 vs q$5" the same fixture leg seen at the text-vs-identity site, where the
//	             pair is the leg's own two spellings
//	" vs A"      the malformed-bounds fixture, a leg literal that states no
//	             identity at all
//	"A vs "      that same leg at the text-vs-identity site
//
// The check is SET equality against the witnesses the census retained, so a
// producer that starts diverging shows up as an unaccounted witness. The residual:
// a real divergence spelled EXACTLY like one of these would be absorbed, because
// the witness set dedups by spelling. That is why the entries are listed
// individually with their source rather than as a wildcard — a new anomaly at a
// new spelling, which is what a producer change looks like, cannot hide.
var negativeControlWitnesses = map[values.LegIdentitySite]map[string]struct{}{
	values.LegSiteOrdinalSlotInLegWindow: {
		"B vs b":     {},
		"q$5 vs Q$5": {},
		" vs A":      {},
		// TestOrdinalSlotInLegWindowCensusRecordsTheEvaluatedPair's minted leg. Its
		// spelling is deliberately used by nothing else, so that test can assert on
		// its own witness under t.Parallel.
		"zz$9 vs ZZ$9": {},
	},
	values.LegSiteTextVsIdentity: {
		"Q$5 vs q$5":   {},
		"A vs ":        {},
		"ZZ$9 vs zz$9": {},
	},
}

// assertTranslatorLegIdentityCensus checks this corpus's census.
//
// Same split as the sqldriver harness for the FLOORS — they describe the
// unfiltered corpus and are dropped under -test.run. Everything else runs always:
// a witness the fixtures do not account for is a defect over any population.
//
// Where this harness DIFFERS from values.AssertLegIdentityCensus, and it matters
// for reading the announcement below: that gate asserts five ZEROS, while this one
// cannot (its fixtures deliberately drive declines), so it converts four of them —
// fold-only, unstated, retired-verdict divergence and text-vs-identity divergence
// — into unaccounted-WITNESS checks against negativeControlWitnesses, keeps the
// instrument-channel zero as a zero, and adds a witness-set SATURATION check
// without which the witness walks are not complete. The announcement enumerates
// all of those, because a message that says only "the witness checks ran" hides
// which populations were actually cleared — the retired-verdict one especially,
// since it is the population that measures the conversion rather than the
// representation.
func assertTranslatorLegIdentityCensus(w io.Writer) bool {
	floors := translatorLegIdentityFloors
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "leg-identity census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). Still checked: the unaccounted-witness "+
			"walks over fold-only, unstated, retired-verdict-divergence and "+
			"text-vs-identity-divergence witnesses, the witness-set saturation guard, and "+
			"the instrument-channel zero.\n", f.Value.String())
		floors = nil
	}
	failed := false
	for _, site := range values.LegIdentitySites() {
		c := values.LegIdentityCensusOf(site)
		if floor, ok := floors[site]; ok && c.Total < floor {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Total = %d, want >= %d.\n"+
				"  A site this corpus no longer reaches makes every claim about it VACUOUS.\n",
				site, c.Total, floor)
		}
		if c.Channel == values.LegChannelMixed {
			failed = true
			fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s recorded in BOTH namespaces; its\n"+
				"  counts then describe neither comparison.\n", site)
		}
		allowed := negativeControlWitnesses[site]
		for _, group := range []struct {
			what     string
			count    int64
			witness  []string
			indictor string
		}{
			{
				"FoldOnlyEqual", c.FoldOnlyEqual, c.FoldOnlySamples,
				"a leg stored under one spelling and looked up under another",
			},
			{
				"Unstated", c.Unstated, c.UnstatedSamples,
				"a comparison one side of which carries no identity at all",
			},
			{
				"RetiredVerdictDivergent", c.RetiredVerdictDivergent, c.RetiredVerdictSamples,
				"a pair on which this site's answer CHANGED when it was converted",
			},
		} {
			if group.count == 0 {
				continue
			}
			// The allowlist walk below can only clear witnesses it can SEE, and the
			// witness set is capped. Saturated, a further DISTINCT anomaly increments
			// the count and retains nothing, so every visible witness clears and the
			// gate passes with a real divergence counted. The count alone cannot reveal
			// that — the allowlisted fixtures legitimately drive many pairs — so the
			// saturation itself is the failure.
			if len(group.witness) >= values.LegIdentitySampleCap() {
				failed = true
				fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s retained %d %s witnesses, "+
					"SATURATING the cap of %d.\n"+
					"  Beyond the cap an anomaly is counted but unwitnessed, so the\n"+
					"  unaccounted-witness check below is no longer complete and this gate can\n"+
					"  no longer clear this site. Narrow the corpus or raise the cap — do not\n"+
					"  read the passing witness walk as a clean result.\n",
					site, len(group.witness), group.what, values.LegIdentitySampleCap())
			}
			for _, wit := range group.witness {
				// The retired-verdict witnesses carry a " (retired=...)" suffix naming
				// the direction; the pair itself is what the allowlist declares.
				pair := wit
				if i := strings.Index(pair, " (retired="); i >= 0 {
					pair = pair[:i]
				}
				if _, ok := allowed[pair]; ok {
					continue
				}
				failed = true
				fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported %s witness %q that no\n"+
					"  test fixture accounts for — %s.\n"+
					"  The fixtures' deliberate pairs are listed in negativeControlWitnesses; a\n"+
					"  witness outside that set came from a PRODUCER, and the fix belongs there.\n",
					site, group.what, wit, group.indictor)
			}
		}
		if values.LegSiteNeitherMustBeZero(site) && c.Neither != 0 {
			// Same saturation hazard as the grouped walk above.
			if len(c.NeitherSamples) >= values.LegIdentitySampleCap() {
				failed = true
				fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s retained %d Neither witnesses, "+
					"SATURATING the cap of %d — beyond it an anomaly is counted but "+
					"unwitnessed, so the walk below cannot clear this site.\n",
					site, len(c.NeitherSamples), values.LegIdentitySampleCap())
			}
			for _, wit := range c.NeitherSamples {
				if _, ok := allowed[wit]; ok {
					continue
				}
				failed = true
				fmt.Fprintf(w, "LEG IDENTITY CENSUS FAIL: site %s reported Neither witness %q that no\n"+
					"  test fixture accounts for: a leg's TEXT and its IDENTITY name different\n"+
					"  things, so the two channels have diverged at a producer.\n", site, wit)
			}
		}
	}
	return failed
}
