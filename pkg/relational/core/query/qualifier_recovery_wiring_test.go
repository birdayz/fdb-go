package query

// The QUALIFIER RECOVERY census's recorder wiring at the THREE translator dark
// splitters, pinned PER SITE and PER CLASS.
//
// WHAT THE CORPORA CANNOT SEE, which is the whole reason this file exists. Both
// corpora floor these sites, and those floors catch a recorder dropped from a
// class the traffic actually drives. They catch nothing else, and the classes
// they miss are precisely the ones the census was built to watch:
//
//	site                 sqldriver corpus                 translator corpus
//	recursiveRemap       MANUFACTURED 0, carried 0        MANUFACTURED 3, carried 4
//	existsSortSplit      AGREED 44, everything else 0     5 calls, FIXTURE traffic only
//	derivedUnnestSource  bare 13, everything else 0       4 calls, FIXTURE traffic only
//
// The right-hand column's last two entries are THIS FILE. Stating them as "the
// site is reached" would be circular — the pins below are what reaches it — so
// they are named as fixture traffic and carry no claim about production. The
// production coverage of those two sites is the sqldriver column and nothing
// else; recursiveRemap is the one site whose translator-corpus population is
// real planning traffic, and it is the only corpus anywhere that reports this
// family's MANUFACTURED bucket from a producer.
//
// So MANUFACTURED at existsSortSplit, DIVERGED anywhere, and every dotted class
// at derivedUnnestSource are invisible to every corpus that runs: the counter is
// 0 whether the recorder is wired, misbucketed, or absent. A census whose
// interesting classes are all unpinned zeros is a census that cannot fail.
//
// The per-site deltas also pin ATTRIBUTION. All six sites share one class enum,
// so a copy-paste filing one site's calls under another keeps every total
// plausible and every floor green while destroying the only thing this census
// reports — WHICH splitter recovered a qualifier and whether it had an identity
// to check it against. A swap in either direction leaves one site's delta at 0.
//
// White-box for the reason every instrument pin on this path is: the thing under
// test is a MEASUREMENT, and a measurement's correctness is not observable from
// the query's answer. A wrong bucket, a recorder on the far side of a branch,
// and no recorder at all all produce identical rows.

import (
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// qualRecProbeMu serializes the census-gate flip. The gate is a process-global
// atomic, so two parallel probes toggling it would let one turn it off under the
// other and read a delta of zero from a perfectly wired recorder.
var qualRecProbeMu sync.Mutex

// qualRecDelta runs fn with the census gate ON and returns the DELTA at one
// (site, class) counter.
//
// Deltas rather than absolutes, and callers assert ">= 1" rather than "== 1":
// the counters are process-global and other tests in this package drive the same
// sites, so while the gate is up their traffic can land in these buckets too. A
// delta FLOOR is immune to that and still fails hard on a bucket that never
// moves, which is the mutation being pinned. An upper bound would not be, and is
// deliberately not asserted anywhere in this file.
func qualRecDelta(t *testing.T, site values.QualifierRecoverySite, class values.QualifierRecoveryClass, fn func()) int {
	t.Helper()
	qualRecProbeMu.Lock()
	defer qualRecProbeMu.Unlock()

	restore := values.LegIdentityCensusEnabled()
	values.SetLegIdentityCensusEnabled(true)
	defer values.SetLegIdentityCensusEnabled(restore)

	before, _ := values.QualifierRecoveryCensus()
	fn()
	after, _ := values.QualifierRecoveryCensus()
	return after[site][class] - before[site][class]
}

// TestQualRecWiring_RecursiveRemapCountsEveryArm pins the four classes
// recursiveRemapValues can reach.
//
// This is the site the census header calls STRICTLY THE WORST: its MANUFACTURED
// arm does not build a qualifier string to look up in a table, it builds a
// CorrelationIdentifier out of the bytes before the FIRST dot. A misread there
// does not fail to resolve — it resolves against a correlation that does not
// exist. It is also the site whose debt bucket the real-FDB corpus reports as 0
// while the translator corpus reports 2, which is the concrete reason a claim
// made from one corpus about this family is not a claim about Go.
func TestQualRecWiring_RecursiveRemapCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cols       []string
		verbatim   []bool
		positional bool
		class      values.QualifierRecoveryClass
		why        string
	}{
		{
			name:     "MANUFACTURED",
			cols:     []string{"B.ID"},
			verbatim: []bool{true},
			class:    values.QualRecManufactured,
			why: "a verbatim identifier carrying a dot became QOV(\"B\") — a correlation " +
				"invented from bytes, with nothing at this site to check it against. " +
				"cols is []string; there is no identity here to convert TO, which is why " +
				"this site's debt is a PRODUCER change and not a local edit",
		},
		{
			name:     "carried",
			cols:     []string{"(B.ID + 1)"},
			verbatim: []bool{false},
			class:    values.QualRecCarried,
			why: "the NAME-PROVENANCE flag declined the split. A structured fact decided " +
				"the outcome, which is what carried means — and it is the arm that stops " +
				"a computed rendering's dots from being read as qualifier boundaries",
		},
		{
			name:     "bare",
			cols:     []string{"ID"},
			verbatim: []bool{true},
			class:    values.QualRecBare,
			why: "no dot, nothing manufactured. Counted because it shares the site with " +
				"the debt class, and a census reporting only debt cannot tell a CLEAN " +
				"site from a DARK one",
		},
		{
			name:       "leafOnly",
			cols:       []string{"C.ORDER_ID"},
			verbatim:   []bool{true},
			positional: true,
			class:      values.QualRecLeafOnly,
			why: "an ORDINALIZED body reads slot i by ordinal, so the qualifier is sliced " +
				"off and DISCARDED rather than manufactured into anything. Not debt, and " +
				"bucketed apart so it is never banked as some",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := qualRecDelta(t, values.QualRecSiteRecursiveRemap, tc.class, func() {
				recursiveRemapValues(tc.cols, tc.verbatim, false, tc.positional)
			})
			if got < 1 {
				t.Fatalf("recursiveRemap recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.cols, tc.why)
			}
		})
	}
}

// TestQualRecWiring_ExistsSortSplitCountsEveryArm pins the classes at the site
// whose ENTIRE measured population is a single class.
//
// existsSortSplit reports AGREED 44 and every other class 0 over the real-FDB
// corpus, and no other corpus reaches it with production traffic — the 5 calls
// the translator corpus reports are the fixtures below. So this file is the only
// thing standing between "MANUFACTURED and DIVERGED are genuinely empty here"
// and "the recorder never reaches those branches" — the two read identically in
// every report.
//
// There is no CARRIED class at this site by construction: the recorder sits on
// the join arm, which always splits. That zero is structure, not a finding.
func TestQualRecWiring_ExistsSortSplitCountsEveryArm(t *testing.T) {
	t.Parallel()

	src := sortSource{isJoin: true, legAliases: []string{"T1", "T2"}, legTypes: []*values.RecordType{nil, nil}}

	cases := []struct {
		name  string
		key   logical.SortKey
		class values.QualifierRecoveryClass
		why   string
	}{
		{
			name: "AGREED",
			key: logical.SortKey{Value: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T2")),
				"SK", values.UnknownType)},
			class: values.QualRecAgreed,
			why: "THE ROUND TRIP. sortKeyFieldRef RENDERS `T2.SK` out of the very " +
				"correlation the key already holds, and splitQualifier then slices that " +
				"rendering back apart. The identity was never lost — it was joined and " +
				"re-parsed — and this bucket is what makes that measurable instead of " +
				"merely arguable",
		},
		{
			name:  "MANUFACTURED",
			key:   logical.SortKey{Expr: "T2.SK"},
			class: values.QualRecManufactured,
			why: "a key with no resolved Value and no captured triple: the qualifier is " +
				"manufactured with no counterparty at all. The corpus reports 0 here, so " +
				"nothing but this pin goes red when the recorder leaves this branch",
		},
		{
			name:  "DIVERGED",
			key:   logical.SortKey{Expr: "T2.SK", Bare: "SK", Qualifier: "T9", Qualified: true},
			class: values.QualRecDiverged,
			why: "the parse-tree triple says the reference is qualified by T9 and the " +
				"rendering says T2. THE ONE POPULATION THIS CENSUS ASSERTS AT ZERO, which " +
				"makes its zero worthless unless something proves the bucket can be " +
				"reached at all. This is that proof",
		},
		{
			name:  "bare",
			key:   logical.SortKey{Expr: "SK", Bare: "SK"},
			class: values.QualRecBare,
			why:   "no dot in the rendering, nothing manufactured",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := qualRecDelta(t, values.QualRecSiteExistsSortSplit, tc.class, func() {
				src.sortKeySourceValue(tc.key)
			})
			if got < 1 {
				t.Fatalf("existsSortSplit recorded %d %v decision(s) — want at least 1.\n%s",
					got, tc.class, tc.why)
			}
		})
	}
}

// TestQualRecWiring_ExistsSortSplitCountsTheOtherCaller pins the SECOND caller.
//
// splitQualifier is reached from two places and only one of them is covered
// above. resolveKeyName is the other, and a recorder present at one caller and
// missing at the other produces a census that is quietly measuring half the
// site — with a healthy total, a passing floor, and a hard zero that is a zero
// over the wrong population.
func TestQualRecWiring_ExistsSortSplitCountsTheOtherCaller(t *testing.T) {
	t.Parallel()

	src := sortSource{isJoin: true, legAliases: []string{"T1", "T2"}, legTypes: []*values.RecordType{nil, nil}}
	key := logical.SortKey{Expr: "T2.SK", Bare: "SK", Qualifier: "T9", Qualified: true}

	got := qualRecDelta(t, values.QualRecSiteExistsSortSplit, values.QualRecDiverged, func() {
		src.sortKeyName(key)
	})
	if got < 1 {
		t.Fatalf("existsSortSplit recorded %d DIVERGED decision(s) through resolveKeyName — "+
			"want at least 1. sortKeySourceValue is not the only caller, and a census wired "+
			"at one of two callers reports a healthy total for half a site", got)
	}
}

// TestQualRecWiring_DerivedUnnestSourceCountsEveryArm pins the classification at
// the site whose dotted population is the thinnest in the family: ONE production
// call, on one corpus.
//
// That one call is the embedded corpus's `TD.ARR`
// (TestDerivedUnnest_QualifiedPassthrough), and it AGREES. It arrived after this
// file did, and it is why the header's "empty on every corpus" reading is gone:
// the dotted arm is live, it is just barely so. DIVERGED and MANUFACTURED remain
// unreached by production anywhere.
//
// SCOPE, stated because it is narrower than the two pins above and the
// difference matters. This exercises the CLASSIFIER
// (recordDerivedUnnestSplit), not the call from classifyDerivedUnnestArray:
// reaching that call needs a translator with live metadata and a derived-table
// body, and the real-FDB corpus already drives it 13 times, so the call-site
// wiring is what the corpus floor guards. What the corpora cannot guard is the
// BUCKETING: all 13 sqldriver calls are bare, and the embedded corpus's 8 add
// exactly one non-bare call. DIVERGED and MANUFACTURED are unreached by
// production anywhere, and an inverted comparison there would be invisible in
// every report.
func TestQualRecWiring_DerivedUnnestSourceCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		proj  *logical.LogicalProject
		src   string
		class values.QualifierRecoveryClass
		why   string
	}{
		{
			name: "AGREED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "ARR", Qualifier: "T", Qualified: true}},
			},
			src:   "T.ARR",
			class: values.QualRecAgreed,
			why: "the slot's parse-tree triple states qualifier T and the split recovered " +
				"T. CONVERSION-READY: the split is redundant over this shape and " +
				"replacing it with the triple is provably behaviour-preserving. The " +
				"embedded corpus's `TD.ARR` is the one production instance of it, which " +
				"is a population of ONE and not a conversion warrant",
		},
		{
			name: "DIVERGED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "T.ARR", Qualifier: "", Qualified: false}},
			},
			src:   "T.ARR",
			class: values.QualRecDiverged,
			why: "the triple says ONE segment — a delimited `\"T.ARR\"` — and the split " +
				"manufactured a qualifier T out of it. This is the exact misread the " +
				"whole workstream is about, and it must be reachable for the hard zero " +
				"to mean anything",
		},
		{
			name: "MANUFACTURED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: nil,
			},
			src:   "T.ARR",
			class: values.QualRecManufactured,
			why: "no triple captured for the slot. An ABSENT ColumnRef means UNKNOWN, " +
				"never UNQUALIFIED — the triple's own contract — so it must bucket as " +
				"no-counterparty and never as a disagreement",
		},
		{
			name: "bare",
			proj: &logical.LogicalProject{
				Projections:    []string{"ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "ARR"}},
			},
			src:   "ARR",
			class: values.QualRecBare,
			why: "no dot; this is nearly the site's entire production population — all 13 " +
				"sqldriver calls and 7 of the embedded corpus's 8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := qualRecDelta(t, values.QualRecSiteDerivedUnnestSource, tc.class, func() {
				recordDerivedUnnestSplit(tc.proj, 0, tc.src)
			})
			if got < 1 {
				t.Fatalf("derivedUnnestSource recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.src, tc.why)
			}
		})
	}
}
