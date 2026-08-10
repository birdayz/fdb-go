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

// TestNameSplitRecorder_SplitQualifiedIsUnreachable is the INVERSE of the
// SPLIT-QUALIFIED floors this file used to carry, at both sites.
//
// Those floors asserted `>= 1`: the debt bucket had to be reachable, because a
// bucket the corpus drives zero times is one whose recorder can be dropped
// without any number moving. That made COLLAPSE the alarm.
//
// Both bakers now decide qualification from the parser's segment count alone
// and never slice a rendered name, so the class has no producer left and the
// alarm direction has INVERTED: zero is the steady state, and any count at all
// means a first-dot re-split was reintroduced. The old floors are unsatisfiable
// and are replaced here rather than deleted, so the expectation stays watched.
func TestNameSplitRecorder_SplitQualifiedIsUnreachable(t *testing.T) {
	t.Parallel()

	cols := []string{"L_ID", "L_NAME"}

	t.Run("legQOVSegmentsOf", func(t *testing.T) {
		t.Parallel()
		outer := twoLegSelect()
		got := nameSplitDelta(t, values.NameSplitSiteLegQOVSegmentsOf, values.NameSplitQualified, func() {
			bakeDottedRefsToLegQOVWithRef(
				values.NewFlatFieldValue("L.NAME", values.UnknownType), logical.ColumnRef{}, outer)
		})
		if got != 0 {
			t.Fatalf("legQOVSegmentsOf recorded %d SPLIT-QUALIFIED decision(s) for a dotted "+
				"name carrying NO segments — want 0. A non-zero here means the arm is "+
				"slicing a rendered name at its first dot again, which cannot tell a "+
				"qualified A.B from a quoted \"A.B\"", got)
		}
	})

	t.Run("flatColumnBake", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitQualified, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("L.L_NAME", values.UnknownType), cols)
		})
		if got != 0 {
			t.Fatalf("flatColumnBake recorded %d SPLIT-QUALIFIED decision(s) for a dotted "+
				"name — want 0. The leg list is gone from this baker's signature, so a "+
				"count here means both the parameter and the re-split came back", got)
		}
	})

	// CONTROL: the census gate and the recorder are both live, so the two zeros
	// above are the class being unreachable rather than the instrument being
	// dark. Without this, deleting every RecordNameSplit call would pass.
	t.Run("control_instrument_is_live", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitBare, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("L.L_NAME", values.UnknownType), cols)
		})
		if got < 1 {
			t.Fatalf("flatColumnBake recorded %d splitBare decision(s) — want at least 1. "+
				"The same call that must report ZERO SPLIT-QUALIFIED must report a BARE "+
				"decision, or the zero above is a dark instrument rather than a property", got)
		}
	})
}

// TestNameSplitRecorder_FlatColumnBakeCountsItsOneArm pins the flat baker's
// surviving class.
//
// It used to pin TWO, against the split condition rather than beside it: the
// recorder lived inside the same if/else that decided the split, so that
// structure was load-bearing against an edit to either predicate leaving the
// census reporting the other one's population. There is no longer a split
// condition to be on the wrong side of — every call records BARE — so what is
// left to pin is that the recorder still fires at all. The SPLIT-QUALIFIED half
// moved to TestNameSplitRecorder_SplitQualifiedIsUnreachable, inverted.
func TestNameSplitRecorder_FlatColumnBakeCountsItsOneArm(t *testing.T) {
	t.Parallel()

	// A flat layout the names do NOT appear in, so the exact-name precedence
	// cannot resolve them first and the recorder at the tail is really reached.
	cols := []string{"L_ID", "L_NAME"}

	t.Run("splitBare_plain", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitBare, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("ABSENT", values.UnknownType), cols)
		})
		if got < 1 {
			t.Fatalf("flatColumnBake recorded %d splitBare decision(s) for a bare name — "+
				"want at least 1. This is now the site's ONLY class, so it is this bucket "+
				"whose floor would go quietly false", got)
		}
	})

	// A DOTTED name lands in the same bucket, and that is the point rather than
	// an accident: with no segments behind it, a dot is a character in the name
	// and the decision is BARE. A census that filed this under SPLIT-QUALIFIED
	// would be reporting a qualifier nobody derived.
	t.Run("splitBare_dotted", func(t *testing.T) {
		t.Parallel()
		got := nameSplitDelta(t, values.NameSplitSiteFlatColumnBake, values.NameSplitBare, func() {
			bakeFlatRefsAgainstColumns(
				values.NewFlatFieldValue("L.L_NAME", values.UnknownType), cols)
		})
		if got < 1 {
			t.Fatalf("flatColumnBake recorded %d splitBare decision(s) for a DOTTED name — "+
				"want at least 1. Without parse-tree segments a dot is part of the name, "+
				"so this call must be counted as a bare decision", got)
		}
	})
}
