package embedded

import (
	"flag"
	"fmt"
	"io"
	"os"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMain runs this package's corpus under the qualifier recovery census — the
// THIRD harness for it, and the one whose absence was a measured hole rather
// than an oversight.
//
// WHY THIS PACKAGE NEEDS ITS OWN, established by probe and not by argument. The
// census shipped with two harnesses, the real-FDB SQL suite and the translator
// corpus, and both reported the same reassuring shape at three sites: the split
// arms are reached, and they never manufacture anything. A PANIC wired into
// those arms and run at this package's reach refuted two of those readings
// immediately:
//
//   - the derived-unnest source split's DOTTED arm, reported bare 13 / dotted 0
//     over the SQL corpus, is driven here by TestDerivedUnnest_QualifiedPassthrough
//     with the source `TD.ARR`.
//   - the display-label PARENTHESIS HEURISTIC, reported 0 over 750 SQL-corpus
//     calls — the reading that would have made it deletable — fires here on the
//     aggregate label `MAX(E.SALARY)`. Without the guard that label's display
//     name is stripped to `SALARY)`.
//
// Neither is exotic. Both are ordinary SQL that this package plans directly
// through the generator without going near the driver, and the lesson is the
// one the census header already states about its own zeros: a zero is a fact
// about the CORPUS until some corpus that could contradict it has run. Two
// corpora agreeing is not two pieces of evidence when both are blind in the same
// direction.
//
// The census counters are package-scoped in values and Go orders no tests, so
// the assertion belongs after m.Run() and nowhere else.
func TestMain(m *testing.M) {
	values.ResetQualifierRecoveryCensus()
	values.SetLegIdentityCensusEnabled(true)
	code := m.Run()
	values.SetLegIdentityCensusEnabled(false)

	if failed := assertEmbeddedQualifierRecoveryCensus(os.Stderr); failed && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// embeddedQualifierRecoveryFloors is the minimum population this corpus must
// report at the sites it drives.
//
// It floors only those. A floor on a site this package never reaches would
// assert something about a different corpus; the three harnesses' floor sets
// together are what keeps every zero non-vacuous somewhere.
//
// Set below the measured values because these totals vary run to run. The floor
// detects COLLAPSE — the shapes stopping, a recorder being routed around — not
// drift.
// Measured over this corpus: recursiveRemap 2, derivedUnnestSource 8,
// projScopeClassify 18, projQualVsScan 4, displayLabelStrip 11,
// existsSortSplit 6.
//
// existsSortSplit's zero was the header's own point about zeros: it read "this
// package plans no sorted EXISTS fold over a join", which was a fact about the
// CORPUS, not about the package. derived_source_exists_plan_test.go plans
// exactly that shape — `ORDER BY d.id` over a correlated EXISTS across a
// derived source — and the site reports 6 calls, all AGREED, with the split
// qualifier matching the leg identity on every one (`C.ID`/C, `D.ID`/D,
// `T1.ID`/T1). So the arm is now carried by the corpus rather than standing on
// the unit wiring pin, and its floor is a real number below the measurement like
// every other one here.
//
// WHAT IS PRODUCTION TRAFFIC HERE AND WHAT IS FIXTURE, because the two do not
// support the same claims and a merged total hides which:
//
//   - PRODUCTION: derivedUnnestSource's `TD.ARR` AGREED, and displayLabelStrip's
//     `MAX(E.SALARY)` heuristic decline plus its `E.SALARY` / `E.SAL-ARY`
//     MANUFACTURED pair. These are ordinary SQL planned through the generator,
//     and all four are invisible to both other corpora.
//   - FIXTURE: everything spelled `T.COL`, `T2.SK`, `A.NAME` and `SUM(A.VAL).X`
//     comes from qualifier_recovery_wiring_test.go, which exists to prove the
//     dotted buckets are reachable at the two embedded sites no corpus
//     populates. Those calls make the floors below say "the pins still run";
//     they say nothing about production reach.
//
// recursiveRemap's floor is 1 against a measured 2, and it is the one that had
// to be argued rather than halved. Its population is the smallest here, so
// "below the measured value" leaves exactly one rung — and an earlier revision
// set it AT the measured 2, which is the shape this block's own policy exists to
// forbid: a floor equal to the measurement is a drift detector wearing a
// collapse detector's clothes, and it reds when the corpus grows a second
// recursive body or the memo explores one fewer time. 1 still detects the only
// thing this floor is for, the site ceasing to be reached at all.
var embeddedQualifierRecoveryFloors = values.QualifierRecoveryFloors{
	Calls: [6]int{
		values.QualRecSiteRecursiveRemap:      1,
		values.QualRecSiteExistsSortSplit:     3,
		values.QualRecSiteDerivedUnnestSource: 4,
		values.QualRecSiteProjScopeClassify:   6,
		values.QualRecSiteProjQualVsScan:      2,
		values.QualRecSiteDisplayLabelStrip:   4,
	},
	Split: [6]int{
		values.QualRecSiteRecursiveRemap:      1,
		values.QualRecSiteExistsSortSplit:     3,
		values.QualRecSiteDerivedUnnestSource: 4,
		values.QualRecSiteProjScopeClassify:   6,
		values.QualRecSiteProjQualVsScan:      2,
		values.QualRecSiteDisplayLabelStrip:   4,
	},
}

// qualifierRecoveryNegativeControls is every DIVERGED witness this package's
// test FIXTURES deliberately drive.
//
// They exist because the census asserts DIVERGED at ZERO, and a zero nothing has
// shown could be non-zero is not a measurement. Each is an asserted reach of the
// bucket in qualifier_recovery_wiring_test.go — the pins for the two embedded
// sites whose dotted classes no corpus populates.
//
// The residual is stated rather than smoothed: a REAL divergence spelled exactly
// like one of these is absorbed, because witnesses dedup by spelling. That is
// why each is listed with the fixture that drives it — a new anomaly at a new
// spelling, which is what a producer change looks like, cannot hide.
var qualifierRecoveryNegativeControls = map[values.QualifierRecoverySite]map[string]struct{}{
	values.QualRecSiteProjQualVsScan: {
		`"T.COL" vs identity "<unqualified>"`: {},
	},
	values.QualRecSiteDisplayLabelStrip: {
		`"A.NAME" vs identity "Z"`: {},
	},
}

// assertEmbeddedQualifierRecoveryCensus checks this corpus's dark-splitter
// census, dropping the population floors when -test.run narrows the corpus. The
// DIVERGED walk and the saturation guards still run — they are defects over any
// population — while the floors describe the unfiltered suite.
func assertEmbeddedQualifierRecoveryCensus(w io.Writer) bool {
	floors := &embeddedQualifierRecoveryFloors
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		fmt.Fprintf(w, "qualifier recovery census: population floors NOT checked "+
			"(-test.run=%q narrowed the corpus). The DIVERGED walk and the witness "+
			"saturation guards still run, over whatever population this filter reached.\n",
			f.Value.String())
		floors = nil
	}
	return values.AssertQualifierRecoveryCensus(w, &values.QualifierRecoveryExpectations{
		Floors:          floors,
		AllowedDiverged: qualifierRecoveryNegativeControls,
	}, "embedded corpus")
}
