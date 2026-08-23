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
//	recursiveRemap       retired/no traffic               retired/no traffic
//	existsSortSplit      AGREED 44, everything else 0     5 calls, FIXTURE traffic only
//	derivedUnnestSource  carried 15, everything else 0    5 calls, FIXTURE traffic only
//
// The right-hand column's last two entries are THIS FILE. Stating them as "the
// site is reached" would be circular — the pins below are what reaches it — so
// they are named as fixture traffic and carry no claim about production. The
// production coverage of those two sites is the sqldriver column and nothing
// else. recursiveRemap is retained only as an inert compatibility entry.
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
	agreedType := &values.RecordType{Fields: []values.Field{
		{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong},
	}}

	cases := []struct {
		name  string
		key   logical.SortKey
		class values.QualifierRecoveryClass
		why   string
	}{
		{
			name:  "AGREED",
			key:   logical.SortKey{Value: exactTestField(t, exactTestQOV(t, "T2", agreedType), 0)},
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
				src.sortKeyName(tc.key)
			})
			if got < 1 {
				t.Fatalf("existsSortSplit recorded %d %v decision(s) — want at least 1.\n%s",
					got, tc.class, tc.why)
			}
		})
	}
}

// Source-value recovery no longer parses rendered sort text. Exact values pass
// through unchanged; missing resolver metadata makes the fold inapplicable.
func TestQualRecWiring_SortKeySourceValueDoesNotRecoverFromText(t *testing.T) {
	t.Parallel()
	src := sortSource{isJoin: true, legAliases: []string{"T1", "T2"}}
	typ := &values.RecordType{Fields: []values.Field{{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong}}}
	exact := exactTestField(t, exactTestQOV(t, "T2", typ), 0)
	if got := src.sortKeySourceValue(logical.SortKey{Expr: "OTHER.SK", Value: exact}); got != exact {
		t.Fatalf("exact sort value was reconstructed: got %v, want original value", got)
	}
	if got := src.sortKeySourceValue(logical.SortKey{Expr: "T2.SK"}); got != nil {
		t.Fatalf("text-only sort key recovered %v, want nil without exact resolver authority", got)
	}
}

// TestQualRecWiring_DerivedUnnestSourceCountsEveryArm pins the classification at
// the site that CONVERTED: classifyDerivedUnnestArray now decides from the
// parse-tree triple and only falls back to the split when a slot has none.
//
// THE ARMS INVERTED WITH THAT CONVERSION, which is the whole reason this file
// is worth reading rather than skimming. While the site split unconditionally,
// a PRESENT triple produced AGREED (split and triple concur) or DIVERGED (they
// do not) — and DIVERGED was the misread the workstream was about. Now a
// present triple produces CARRIED, because nothing was sliced: the triple
// answered. AGREED and DIVERGED are unreachable through a present triple, and
// asserting them would pin a classification the code can no longer make.
//
// So the arms below are the CURRENT partition, and each says what it would mean
// for the class to move:
//
//   - CARRIED for any present triple, qualified or not;
//   - MANUFACTURED for an ABSENT triple over a dotted name — the fallback, and
//     the only surviving splitting arm;
//   - bare for a name with no dot, which needs no decision at all.
//
// SCOPE, stated because it is narrower than the two pins above. This exercises
// the CLASSIFIER (recordDerivedUnnestSplit), not the call from
// classifyDerivedUnnestArray: reaching that call needs a translator with live
// metadata and a derived-table body, and the real-FDB corpus drives it, so the
// call-site wiring is what the corpus floor guards. What a corpus cannot guard
// is the BUCKETING, because production reaches only some of these classes.
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
			name: "CARRIED qualified",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "ARR", Qualifier: "T", Qualified: true}},
			},
			src:   "T.ARR",
			class: values.QualRecCarried,
			why: "the slot's triple states qualifier T and the site now uses it. This " +
				"USED to record AGREED — the split independently recovering T — and " +
				"recording that today would report a slice that never happened",
		},
		{
			name: "CARRIED one delimited segment containing a dot",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "T.ARR", Qualifier: "", Qualified: false}},
			},
			src:   "T.ARR",
			class: values.QualRecCarried,
			why: "the shape the conversion was FOR: one delimited `\"T.ARR\"`, which the " +
				"split read as qualifier T over column ARR and bound the wrong column " +
				"for. It recorded DIVERGED then. Recording DIVERGED now would hard-fail " +
				"a census-enabled run over a decision the site no longer takes",
		},
		{
			name: "MANUFACTURED — the surviving splitting arm",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.ARR"},
				ProjectionRefs: nil,
			},
			src:   "T.ARR",
			class: values.QualRecManufactured,
			why: "no triple captured for the slot, so the split runs and this is the ONE " +
				"arm that still slices. An ABSENT ColumnRef means UNKNOWN, never " +
				"UNQUALIFIED — the triple's own contract — so it buckets as " +
				"no-counterparty and never as a disagreement",
		},
		{
			name: "CARRIED undotted",
			proj: &logical.LogicalProject{
				Projections:    []string{"ARR"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "ARR"}},
			},
			src:   "ARR",
			class: values.QualRecCarried,
			why: "a present triple with no dot. It recorded `bare` while the site split " +
				"unconditionally; CARRIED is right now, and the distinction is not " +
				"cosmetic — `bare` counts toward splitPopulation and CARRIED does not",
		},
		{
			name: "bare — absent triple, no dot",
			proj: &logical.LogicalProject{
				Projections:    []string{"ARR"},
				ProjectionRefs: nil,
			},
			src:   "ARR",
			class: values.QualRecBare,
			why: "THE ARM THE PRESENT-TRIPLE ROW ABOVE STOPPED COVERING. Turning that " +
				"row's fixture Present flipped it to CARRIED and left QualRecBare " +
				"unreached by this test — and the corpora reach only CARRIED and the " +
				"dotted MANUFACTURED fallback, so bare classification could break with " +
				"everything green. Absent triple AND no dot is the one input that still " +
				"produces it",
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
