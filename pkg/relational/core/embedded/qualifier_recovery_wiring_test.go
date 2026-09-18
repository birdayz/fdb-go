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

// TestQualRecWiring_DisplayLabelStripCountsEveryArm pins the four classes at the
// site the CQ-94 entry singles out for its PARENTHESIS HEURISTIC.
//
// The heuristicDecline bucket is the reason that class exists at all. Folded
// into `bare` it would be invisible, and the two are opposite findings: bare
// means the site was handed a name with no qualifier in it, while a heuristic
// decline means the site WAS handed a dotted name, had to decide whether the dot
// was a qualifier boundary, and decided by looking for parentheses in a
// rendering. A census that reported them together would show this site's
// riskiest population as its cleanest.
//
// That bucket is 0 across 750 sqldriver calls, and for one revision this file
// read that as "the heuristic never fires". It fires: this package's own corpus
// drives it once from production traffic, on the aggregate label
// `MAX(E.SALARY)`, and without the guard that label's display name is stripped to
// `SALARY)`. The pin below is no longer the only thing separating "unreachable"
// from "unexercised" at this class — but it is still the only thing that holds
// if that one label stops being planned.
func TestQualRecWiring_DisplayLabelStripCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		label  string
		source values.ProjectionAliasSource
		class  values.QualifierRecoveryClass
		// wantOut/wantStripped are the OUTCOME, asserted beside the census
		// class. The census says which decision was FILED; these say what the
		// caller was actually handed, and the two are independent — the class
		// comes from recordDisplayLabelStrip's inputs and the label from the
		// branch below it. The return value used to be discarded here, so a
		// strip that bucketed correctly and handed back the wrong label passed.
		wantOut      string
		wantStripped bool
		why          string
	}{
		{
			name:         "AGREED",
			label:        "A.NAME",
			source:       values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
			class:        values.QualRecAgreed,
			wantOut:      "NAME",
			wantStripped: true,
			why: "the machinery minted this alias from correlation A and the split " +
				"recovered A. 722 of the site's 750 corpus calls land here — the " +
				"largest conversion-ready population in the family",
		},
		{
			name:         "DIVERGED",
			label:        "A.NAME",
			source:       values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("Z")),
			class:        values.QualRecDiverged,
			wantOut:      "NAME",
			wantStripped: true,
			why: "the value names correlation Z and the label's bytes name A. The one " +
				"population this census asserts at zero, and it must be reachable for " +
				"that zero to mean anything",
		},
		{
			name:         "MANUFACTURED",
			label:        "A.NAME",
			source:       values.ProjectionAliasSource{},
			class:        values.QualRecManufactured,
			wantOut:      "NAME",
			wantStripped: true,
			why: "a dotted label over a value carrying no correlation at all: the " +
				"qualifier is stripped with nothing to check it against. The sqldriver " +
				"corpus reports 6 and this package's production traffic 3 more " +
				"(`E.SALARY`, `E.SAL-ARY`), which is why this site is NOT a mechanical " +
				"conversion despite 722 agreements",
		},
		{
			name:         "heuristicDecline",
			label:        "SUM(A.VAL).X",
			source:       values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
			class:        values.QualRecHeuristicDecline,
			wantOut:      "SUM(A.VAL).X",
			wantStripped: false,
			why: "a dotted label containing parentheses. isPlainQualifiedColumnReference " +
				"rejects it by looking for `()` in the RENDERING — a heuristic, not a " +
				"parse — and that decision is bucketed apart from `bare` so it cannot " +
				"be read as a site that simply saw no dot. Not a hypothetical arm: " +
				"this package plans `MAX(E.SALARY)` through it for real",
		},
		{
			name:         "bare",
			label:        "NAME",
			source:       values.ProjectionAliasSource{},
			class:        values.QualRecBare,
			wantOut:      "NAME",
			wantStripped: false,
			why:          "no dot in the label; 22 of the sqldriver corpus's 750 calls and 2 of this package's 6",
		},
		{
			name:         "AGREED_three_segment",
			label:        "A.N.SK",
			source:       values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
			class:        values.QualRecAgreed,
			wantOut:      "SK",
			wantStripped: true,
			why: "an alias-qualified struct descent. The SOURCE qualifier is the " +
				"LEADING segment, and it agrees with the correlation; the second " +
				"dot is inside the struct path and is not a qualifier boundary at " +
				"all. Split at the LAST dot this reads as a source \"A.N\" and " +
				"records DIVERGED against an identity that plainly says A — which " +
				"is what it did, and what made the census fire on a correct read",
		},
		{
			name:         "DIVERGED_three_segment",
			label:        "Z.N.SK",
			source:       values.NewProjectionAliasSource(values.NamedCorrelationIdentifier("A")),
			class:        values.QualRecDiverged,
			wantOut:      "SK",
			wantStripped: true,
			why: "the leading segment names Z and the value names A, so the label " +
				"is not a rendering of this identity. DIVERGED must stay reachable " +
				"at three segments too: the arm above makes the common case agree, " +
				"and without this one that agreement could be a split that never " +
				"disagrees with anything",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotOut string
			var gotStripped bool
			got := qualRecDelta(t, values.QualRecSiteDisplayLabelStrip, tc.class, func() {
				gotOut, gotStripped = stripDisplayLabelQualifier(tc.label, tc.source)
			})
			if got < 1 {
				t.Fatalf("displayLabelStrip recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.label, tc.why)
			}
			// THE OUTCOME, not only the bucket the decision was filed in. This
			// call's return was discarded, so a strip that classified correctly
			// and handed the caller the wrong label satisfied the counter and
			// mislabelled every column it touched.
			if gotOut != tc.wantOut || gotStripped != tc.wantStripped {
				t.Fatalf("stripDisplayLabelQualifier(%q) = (%q, %v), want (%q, %v).\n%s",
					tc.label, gotOut, gotStripped, tc.wantOut, tc.wantStripped, tc.why)
			}
		})
	}
}
