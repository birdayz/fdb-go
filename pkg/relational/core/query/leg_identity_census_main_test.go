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
	values.SetLegIdentityCensusEnabled(true)
	code := m.Run()
	values.SetLegIdentityCensusEnabled(false)

	values.ReportLegIdentityCensus(os.Stderr, "translator corpus")
	if failed := assertTranslatorLegIdentityCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	os.Exit(code)
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
// ordinalSlotInLegWindow 37, expressionOutputLegs 5.
var translatorLegIdentityFloors = map[values.LegIdentitySite]int64{
	values.LegSiteTextVsIdentity:         8,
	values.LegSiteFinalizeSeedWindows:    2,
	values.LegSiteSelectOutputLegs:       2,
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
// unfiltered corpus and are dropped under -test.run. The anomaly check runs
// always: a witness the fixtures do not account for is a defect over any
// population.
func assertTranslatorLegIdentityCensus(w io.Writer) bool {
	floors := translatorLegIdentityFloors
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "leg-identity census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The unaccounted-witness and "+
			"instrument-channel checks ARE run.\n", f.Value.String())
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
