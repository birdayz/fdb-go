package embedded

// The QUALIFIER RECOVERY census's recorder wiring at the THREE parseColRef dark
// splitters, pinned PER SITE and PER CLASS.
//
// WHY parseColRef IS THREE SITES AND NOT ONE. It has 27 production call sites.
// The overwhelming majority are display or lookup — they take `.bare()` and
// never ask whether the name was qualified — and instrumenting those would
// measure a helper's popularity rather than any decision. Three call sites
// MANUFACTURE a qualifier that then DECIDES something, and they are three
// different decisions with three different counterparties:
//
//	projScopeClassify  inner- vs outer-scoping of a projection field.
//	                   Counterparty: a QuantifiedObjectValue child — but the
//	                   split arm runs only where there ISN'T one, so this site
//	                   can never report AGREED. Its debt is a producer change.
//	projQualVsScan     a projected column's qualifier against the scan's
//	                   name/alias, raising ErrCodeUndefinedColumn on a mismatch.
//	                   Counterparty: the slot's ProjectionRefs triple.
//	displayLabelStrip  the machinery-alias display label, guarded by the
//	                   PARENTHESIS HEURISTIC. Counterparty: the projected
//	                   value's correlation.
//
// A single merged "parseColRef" number could answer the conversion question for
// none of them.
//
// WHAT THE CORPUS CANNOT SEE. This package has no TestMain census gate — the
// real-FDB sqldriver suite is its only corpus — and over that corpus every one
// of these classes is 0:
//
//	projScopeClassify  MANUFACTURED 0 of 71 calls (carried 11, bare 60)
//	projQualVsScan     every dotted class 0 of 4 calls (bare 4)
//	displayLabelStrip  DIVERGED 0, heuristicDecline 0 of 750 calls
//
// So each of those buckets reads identically whether the recorder is wired,
// misbucketed, or absent. That is what this file covers, and it is the only
// thing that does.
//
// The per-site deltas also pin ATTRIBUTION: all six census sites share one class
// enum, so a copy-paste filing one site's calls under another keeps every total
// plausible and every floor green while destroying the only thing the census
// reports — WHICH splitter recovered a qualifier and whether it had an identity
// to check it against.

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

func qov(name string) values.Value {
	return values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(name))
}

// TestQualRecWiring_ProjScopeClassifyCountsEveryArm pins the three classes
// projScopeAlias can reach.
//
// AGREED is structurally impossible here and that is the site's finding, not an
// omission: the split runs in the ELSE of the correlation check, so a call that
// splits is by construction a call with no identity to agree with. This site
// cannot be converted by a local edit — there is nothing local to convert TO.
func TestQualRecWiring_ProjScopeClassifyCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		fv    *values.FieldValue
		class values.QualifierRecoveryClass
		want  string
		why   string
	}{
		{
			name:  "carried",
			fv:    values.NewFieldValue(qov("T2"), "SK", values.UnknownType),
			class: values.QualRecCarried,
			want:  "T2",
			why: "the correlation IS the alias — carried, no string sliced. Counted so " +
				"the site's converted share is visible: a census reporting only the " +
				"split arm could not tell a site that mostly carries from one that " +
				"mostly manufactures, and that is the difference between a mechanical " +
				"conversion and a producer change",
		},
		{
			name:  "MANUFACTURED",
			fv:    values.NewFlatFieldValue("T2.SK", values.UnknownType),
			class: values.QualRecManufactured,
			want:  "T2",
			why: "a dotted rendered name with NO correlation on it: the alias that " +
				"decides inner- vs outer-scoping is manufactured out of the bytes " +
				"before the last dot. The corpus reports 0 here over 71 calls, so " +
				"nothing but this pin goes red when the recorder leaves this branch",
		},
		{
			name:  "bare",
			fv:    values.NewFlatFieldValue("SK", values.UnknownType),
			class: values.QualRecBare,
			want:  "",
			why: "no dot, nothing manufactured — and this is 60 of the site's 71 measured " +
				"calls, so it is the bucket whose floor would go quietly false",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			delta := qualRecDelta(t, values.QualRecSiteProjScopeClassify, tc.class, func() {
				got = projScopeAlias(tc.fv)
			})
			if delta < 1 {
				t.Fatalf("projScopeClassify recorded %d %v decision(s) for %q — want at least 1.\n%s",
					delta, tc.class, tc.fv.Field, tc.why)
			}
			// The recorder must sit inside the branch that DECIDES, not beside
			// it: pinning the class without the alias would pass for a recorder
			// moved to the far side of the if.
			if got != tc.want {
				t.Fatalf("projScopeAlias(%q) = %q, want %q — the recorder and the decision "+
					"have come apart", tc.fv.Field, got, tc.want)
			}
		})
	}
}

// TestQualRecWiring_ProjQualVsScanCountsEveryArm pins the classes at the site
// with the sharpest consequence in the family.
//
// A disagreement here does not merely resolve the wrong row: the mismatch arm
// raises ErrCodeUndefinedColumn on a column the parser saw perfectly well. Over
// the real-FDB corpus this site reports 4 calls, ALL BARE — the qualified arm
// is never entered — so every dotted class is an unpinned zero without this.
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
			why:   "no dot; this is the site's ENTIRE measured population (4 calls)",
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
// Over the real-FDB corpus that bucket is 0 across 750 calls — the heuristic
// never fires — so nothing but this pin can distinguish "no label ever needed
// it" from "the guard is unreachable" from "the recorder is not there".
func TestQualRecWiring_DisplayLabelStripCountsEveryArm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		label string
		v     values.Value
		class values.QualifierRecoveryClass
		why   string
	}{
		{
			name:  "AGREED",
			label: "A.NAME",
			v:     values.NewFieldValue(qov("A"), "NAME", values.UnknownType),
			class: values.QualRecAgreed,
			why: "the machinery minted this alias from correlation A and the split " +
				"recovered A. 722 of the site's 750 corpus calls land here — the " +
				"largest conversion-ready population in the family",
		},
		{
			name:  "DIVERGED",
			label: "A.NAME",
			v:     values.NewFieldValue(qov("Z"), "NAME", values.UnknownType),
			class: values.QualRecDiverged,
			why: "the value names correlation Z and the label's bytes name A. The one " +
				"population this census asserts at zero, and it must be reachable for " +
				"that zero to mean anything",
		},
		{
			name:  "MANUFACTURED",
			label: "A.NAME",
			v:     values.NewFlatFieldValue("NAME", values.UnknownType),
			class: values.QualRecManufactured,
			why: "a dotted label over a value carrying no correlation at all: the " +
				"qualifier is stripped with nothing to check it against. The corpus " +
				"reports 6 of these, which is why this site is NOT a mechanical " +
				"conversion despite 722 agreements",
		},
		{
			name:  "heuristicDecline",
			label: "SUM(A.VAL).X",
			v:     values.NewFieldValue(qov("A"), "VAL", values.UnknownType),
			class: values.QualRecHeuristicDecline,
			why: "a dotted label containing parentheses. isPlainQualifiedColumnReference " +
				"rejects it by looking for `()` in the RENDERING — a heuristic, not a " +
				"parse — and that decision is bucketed apart from `bare` so it cannot " +
				"be read as a site that simply saw no dot",
		},
		{
			name:  "bare",
			label: "NAME",
			v:     values.NewFlatFieldValue("NAME", values.UnknownType),
			class: values.QualRecBare,
			why:   "no dot in the label; 22 of the site's 750 corpus calls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := qualRecDelta(t, values.QualRecSiteDisplayLabelStrip, tc.class, func() {
				recordDisplayLabelStrip(tc.label, tc.v)
			})
			if got < 1 {
				t.Fatalf("displayLabelStrip recorded %d %v decision(s) for %q — want at least 1.\n%s",
					got, tc.class, tc.label, tc.why)
			}
		})
	}
}
