package embedded

// The QUALIFIER RECOVERY census's remaining embedded recorder wiring, pinned
// per site and class. projScopeClassify is fully retired: semantic projection
// resolution now carries exact source identity and the all-call revival alarm
// lives in qualifier_recovery_census_main_test.go.
//
// The remaining fixtures drive every class that the production corpora do not
// reliably populate at projQualVsScan and displayLabelStrip. Their per-site
// deltas pin attribution as well as reach: all census sites share one class enum,
// so filing a call under the wrong site can leave aggregate totals plausible.

import (
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// qualRecProbeMu serializes the census-gate flip; the gate is a process-global
// atomic and two parallel probes toggling it would let one turn it off under the
// other and read a delta of zero from a perfectly wired recorder.
var qualRecProbeMu sync.Mutex

// qualRecDelta runs fn with the census gate ON and returns the DELTA at one
// (site, class) counter. Deltas, and ">= 1" rather than "== 1", because the
// counters are process-global and this package's other tests drive the same
// sites: a delta FLOOR is immune to their traffic and still fails hard on a
// bucket that never moves, which is the mutation being pinned.
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

func qualRecTestField(t testing.TB, correlation, fieldName string) values.FieldValue {
	t.Helper()
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: fieldName, Ordinal: 0, FieldType: values.NotNullLong},
	}}
	owner, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), rowType)
	if err != nil {
		t.Fatalf("QOV %q: %v", correlation, err)
	}
	resolved, err := values.ResolveFieldOrdinals(owner, []int{0})
	if err != nil {
		t.Fatalf("field %q: %v", fieldName, err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("resolved %q is not an exact field", fieldName)
	}
	return field
}

// TestQualRecWiring_ProjQualVsScanCountsEveryArm pins the classes at the site
// with the sharpest consequence in the family.
//
// A disagreement here does not merely resolve the wrong row: the mismatch arm
// raises ErrCodeUndefinedColumn on a column the parser saw perfectly well. Over
// the real-FDB corpus this site reports 4 calls, ALL BARE, and this package's own
// corpus does not reach it with production traffic at all — the qualified arm is
// entered by nothing anywhere, so every dotted class is an unpinned zero without
// this file. Every call the embedded census reports at this site is driven from
// right here.
func TestQualRecWiring_ProjQualVsScanCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		proj  *logical.LogicalProject
		upper string
		class values.QualifierRecoveryClass
		why   string
	}{
		{
			name: "AGREED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.COL"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "COL", Qualifier: "T", Qualified: true}},
			},
			upper: "T.COL",
			class: values.QualRecAgreed,
			why: "the slot's parse-tree triple states qualifier T and the split recovered " +
				"T. CONVERSION-READY over this shape",
		},
		{
			name: "DIVERGED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.COL"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "T.COL", Qualified: false}},
			},
			upper: "T.COL",
			class: values.QualRecDiverged,
			why: "the triple says ONE segment — a delimited `\"T.COL\"` — and the split " +
				"manufactured the qualifier T. Here that does not silently read the " +
				"wrong row, it REJECTS the query with ErrCodeUndefinedColumn. The " +
				"census asserts this bucket at zero, and a zero nothing has shown can " +
				"be non-zero is not a measurement",
		},
		{
			name: "MANUFACTURED",
			proj: &logical.LogicalProject{
				Projections:    []string{"T.COL"},
				ProjectionRefs: nil,
			},
			upper: "T.COL",
			class: values.QualRecManufactured,
			why: "no triple captured. An ABSENT ColumnRef means UNKNOWN, never " +
				"UNQUALIFIED — the triple's own contract — so it must bucket as " +
				"no-counterparty and never as a disagreement",
		},
		{
			name: "bare",
			proj: &logical.LogicalProject{
				Projections:    []string{"COL"},
				ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "COL"}},
			},
			upper: "COL",
			class: values.QualRecBare,
			why: "no dot; this is the site's ENTIRE production population anywhere — 4 " +
				"sqldriver calls, and nothing at all from this package",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := qualRecDelta(t, values.QualRecSiteProjQualVsScan, tc.class, func() {
				recordProjQualVsScan(tc.proj, 0, tc.upper, parseColRef(tc.upper))
			})
			if got < 1 {
				t.Fatalf("projQualVsScan recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.upper, tc.why)
			}
		})
	}
}
