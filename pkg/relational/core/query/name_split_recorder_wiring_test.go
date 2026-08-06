package query

// The name-split census's recorder wiring, pinned PER ARM and PER CLASS.
//
// The census (values/name_split_census.go) floors its two sites over the
// real-FDB sqldriver corpus, and those floors catch a recorder dropped from an
// arm the corpus actually drives. They cannot catch one dropped from an arm the
// corpus drives ZERO times — and legQOVSegmentsOf's two SPLITTING classes are
// exactly that: measured 9 calls, 0 splits, so a dropped recorder there leaves
// every number in the census identical, and its split floor is a declared 0
// rather than a guarantee. That is the mutation the corpus is blind to, and this
// file is its coverage.
//
// The arms are not dead — that was measured, not assumed. A panic at
// segmentsOf's fallback entry is reached with a DOTTED name by
// TestRecursiveBodyGatesOrdinal ("C.ORDER_ID"), i.e. the SPLIT-QUALIFIED bucket
// itself is reachable, which is why the arm was kept and instrumented rather
// than deleted the way singleForEachBake's qualifier arm was.
//
// The per-site assertions also pin ATTRIBUTION: both bakers record the same
// three classes, so a copy-paste filing one baker's calls under the other site
// keeps every total plausible and every floor green while destroying the only
// thing this census reports — which arm split. A swap in either direction leaves
// one of the two sites below its expected delta here.
//
// White-box for the same reason the sibling pins on this path are: the thing
// under test is an INSTRUMENT, and an instrument's correctness is not observable
// from the query's answer. A wrong bucket, a recorder on the far side of a
// branch, and no recorder at all all produce identical rows.

import (
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// nameSplitProbeMu serializes the census-gate flip. The gate is a process-global
// atomic, so two parallel probes toggling it would let one turn it off under the
// other and read a delta of zero from a perfectly wired recorder.
var nameSplitProbeMu sync.Mutex

// nameSplitDelta runs fn with the census gate ON and returns the DELTA at one
// (site, class) counter.
//
// Deltas rather than absolutes, and callers assert ">= 1" rather than "== 1":
// the counters are process-global and other tests in this package drive the same
// bakers, so while the gate is up their traffic can land in these buckets too. A
// delta FLOOR is immune to that and still fails hard on a bucket that never
// moves, which is the mutation being pinned. An upper bound would not be, and is
// deliberately not asserted anywhere in this file.
func nameSplitDelta(t *testing.T, site values.NameSplitSite, class values.NameSplitClass, fn func()) int {
	t.Helper()
	nameSplitProbeMu.Lock()
	defer nameSplitProbeMu.Unlock()

	restore := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	defer values.SetLegIdentityCensusEnabled(restore)

	before, _ := values.NameSplitCensus()
	fn()
	after, _ := values.NameSplitCensus()
	return after[site][class] - before[site][class]
}

// TestNameSplitRecorder_LegQOVSegmentsOfCountsEveryArm pins all three classes at
// the site whose split population the corpus cannot see.
func TestNameSplitRecorder_LegQOVSegmentsOfCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		field string
		ref   logical.ColumnRef
		class values.NameSplitClass
		why   string
	}{
		{
			name:  "segmented",
			field: "L.NAME",
			ref:   logical.ColumnRef{Present: true, Bare: "NAME", Qualifier: "L", Qualified: true},
			class: values.NameSplitSegmented,
			why: "the parser's triple decided and no string was sliced. This is the " +
				"CONVERTED channel, and a census that stopped counting it could not " +
				"distinguish a conversion still carrying the traffic from one that " +
				"silently stopped being reached",
		},
		{
			name:  "SPLIT-QUALIFIED",
			field: "L.NAME",
			ref:   logical.ColumnRef{},
			class: values.NameSplitQualified,
			why: "a carrier stating NO segments reached the arm with a dot in its " +
				"rendered name, and a qualifier was manufactured out of the bytes " +
				"before it. THE DEBT BUCKET. The corpus reports 0 here, so nothing " +
				"but this pin goes red when the recorder leaves this arm",
		},
		{
			name:  "splitBare",
			field: "NAME",
			ref:   logical.ColumnRef{},
			class: values.NameSplitBare,
			why: "the fallback ran and found no dot, so nothing was manufactured. " +
				"Counted because it shares the arm with the debt bucket, and a census " +
				"reporting only the debt cannot tell a clean arm from a dark one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outer := twoLegSelect()
			got := nameSplitDelta(t, values.NameSplitSiteLegQOVSegmentsOf, tc.class, func() {
				bakeDottedRefsToLegQOVWithRef(
					values.NewFlatFieldValue(tc.field, values.UnknownType), tc.ref, outer)
			})
			if got < 1 {
				t.Fatalf("legQOVSegmentsOf recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.field, tc.why)
			}
		})
	}
}

// TestNameSplitRecorder_FlatColumnBakeCountsBothArms pins the flat baker's two
// classes against the SPLIT CONDITION rather than beside it.
//
// The recorder there lives inside the same if/else that decides the split, and
// this test is what makes that structure load-bearing: moved back out into a
// separate `if dot > 0`, an edit to either predicate can leave the census
// reporting the other one's population with both totals still plausible.
func TestNameSplitRecorder_FlatColumnBakeCountsBothArms(t *testing.T) {
	t.Parallel()

	// A flat layout the dotted name does NOT appear in, so the exact-name
	// precedence cannot resolve it first and the dotted arm is really entered.
	cols := []string{"L_ID", "L_NAME"}
	legs := []values.RecordTypeLeg{{Name: "L", Start: 0, Width: 2}}

	t.Run("SPLIT-QUALIFIED", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitQualified, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("L.L_NAME", values.UnknownType), cols, legs...)
		})
		if got < 1 {
			t.Fatalf("flatColumnBake recorded %d SPLIT-QUALIFIED decision(s) for a dotted "+
				"name — want at least 1. Over the corpus this bucket is 0 by traffic, so "+
				"an unwired debt bucket there is indistinguishable from a clean one", got)
		}
	})

	t.Run("splitBare", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitBare, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("ABSENT", values.UnknownType), cols, legs...)
		})
		if got < 1 {
			t.Fatalf("flatColumnBake recorded %d splitBare decision(s) for a bare name — "+
				"want at least 1. This is the site's ENTIRE measured population (2 calls), "+
				"so it is this bucket whose floor would go quietly false", got)
		}
	})
}
