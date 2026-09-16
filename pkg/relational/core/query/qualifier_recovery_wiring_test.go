package query

// Recorder wiring for the live translator sort splitter. Derived UNNEST no
// longer recovers a qualifier from a body projection; its stable census site
// is guarded against any new traffic by QualifierRecoveryExpectations.

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
