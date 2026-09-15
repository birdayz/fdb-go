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

// embeddedQualifierRecoveryFloors watches collapse at the live sites reached by
// this corpus. Derived UNNEST and projection-scope classification are fully
// retired and forbid all calls independently of these floors.
var embeddedQualifierRecoveryFloors = values.QualifierRecoveryFloors{
	Calls: [6]int{
		values.QualRecSiteExistsSortSplit:   3,
		values.QualRecSiteProjQualVsScan:    2,
		values.QualRecSiteDisplayLabelStrip: 4,
	},
	Split: [6]int{
		values.QualRecSiteExistsSortSplit:   3,
		values.QualRecSiteProjQualVsScan:    2,
		values.QualRecSiteDisplayLabelStrip: 4,
	},
}

// embeddedQualifierRecoveryRetiredSplit names the sites whose splitting arm is
// gone from the tree rather than merely unreached here. Their alarm is inverted:
// any split is the arm coming back.
//
//   - recursiveRemap: values.RecordQualifierRecovery is not called with this site
//     anywhere in non-test sources.
var embeddedQualifierRecoveryRetiredSplit = func() (r [6]bool) {
	r[values.QualRecSiteRecursiveRemap] = true
	return r
}()

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
		// The THREE-SEGMENT reach. It is listed separately rather than folded
		// into the two-segment one because the arity is the thing under test:
		// at three segments the leading segment and "everything before the last
		// dot" are different strings, and a split that confuses them reports
		// DIVERGED for a perfectly correct read. Only a disagreement in the
		// LEADING segment may reach this bucket now.
		`"Z.N.SK" vs identity "A"`: {},
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
		RetiredSplit:    embeddedQualifierRecoveryRetiredSplit,
		RetiredCalls: [6]bool{
			values.QualRecSiteDerivedUnnestSource: true,
			values.QualRecSiteProjScopeClassify:   true,
		},
	}, "embedded corpus")
}
