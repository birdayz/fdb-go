package embedded_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/embedded"
)

// Float-leading ORDER BY must NOT have its sort elided.
//
// An index or primary-key scan visits a FLOAT/DOUBLE column in FDB tuple KEY
// order, which is not that column's VALUE order: negative NaN payloads pack
// before -Inf and positive ones after +Inf, while the comparator ranks all NaN
// equal and greatest (pinned by
// values.TestFloatPhysicalOrderDivergesFromLogicalOrder). Eliding the sort
// therefore returns rows in an order the engine's own comparator disagrees
// with, and — because all NaNs are one logical tie class split across two
// disjoint physical ranges — leaves any LATER sort column unordered within the
// tie.
//
// These assertions are PLAN-SHAPE assertions on purpose. The wrong rows only
// appear when the data actually contains a negative NaN, so a rows-only test
// passes with the defect fully present on ordinary data; what is broken at all
// times is the CLAIM. The end-to-end row-level proof lives in the sqldriver
// differential tests.
const floatOrderingSchema = `CREATE TABLE t (
	id BIGINT,
	a BIGINT,
	d DOUBLE,
	e FLOAT,
	PRIMARY KEY (id)
)
CREATE INDEX idx_d ON t (d)
CREATE INDEX idx_e ON t (e)
CREATE INDEX idx_a ON t (a)
CREATE INDEX idx_ad ON t (a, d)`

func planExplainForTest(t *testing.T, sql string) string {
	t.Helper()
	plan, err := embedded.PlanPhysicalForTest(sql, floatOrderingSchema, nil)
	if err != nil {
		t.Fatalf("planning %q: %v", sql, err)
	}
	if plan == nil {
		t.Fatalf("planning %q returned a nil plan", sql)
	}
	return plan.Explain()
}

func TestFloatLeadingOrderByKeepsItsSort(t *testing.T) {
	t.Parallel()

	// Every shape here has a float as the LEADING sort column, so no scan can
	// deliver the requested order and a materialized sort must survive.
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"double alone", "SELECT id FROM t ORDER BY d"},
		{"float alone", "SELECT id FROM t ORDER BY e"},
		{"double with predicate", "SELECT id FROM t WHERE d > 5.0 ORDER BY d"},
		{"float with predicate", "SELECT id FROM t WHERE e > 5.0 ORDER BY e"},
		{"double then pk", "SELECT id FROM t ORDER BY d, id"},
		{"float then pk", "SELECT id FROM t ORDER BY e, id"},
		{"float then pk with predicate", "SELECT id FROM t WHERE e > 5.0 ORDER BY e, id"},
		{"double then other indexed col", "SELECT id FROM t ORDER BY d, a"},
		{"double desc", "SELECT id FROM t ORDER BY d DESC"},
		{"double nulls first", "SELECT id FROM t ORDER BY d NULLS FIRST, id"},
		{"float nulls first", "SELECT id FROM t ORDER BY e NULLS FIRST, id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explain := planExplainForTest(t, tc.sql)
			if !strings.Contains(explain, "Sort") {
				t.Fatalf("%s\n\nplan elided the sort on a FLOAT/DOUBLE leading "+
					"sort column — the scan does not deliver that column's "+
					"value order (negative NaN packs before -Inf).\n\nplan: %s",
					tc.sql, explain)
			}
		})
	}
}

// A non-float leading column must still elide its sort. Without this, the fix
// could "pass" by disabling sort elimination wholesale, which is the failure
// mode a one-directional test cannot see.
func TestNonFloatOrderByStillElidesItsSort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"pk", "SELECT id FROM t ORDER BY id"},
		{"indexed bigint", "SELECT id FROM t ORDER BY a"},
		// `ORDER BY a, id` over index (a) is deliberately ABSENT. It does not
		// elide its sort — but it does not elide on master either, with none of
		// the ordering-claim work applied (measured against a master worktree:
		// `Project([ID#0], InMemorySort([A ASC, ID ASC], Scan(T)))`, identical
		// plan on both sides). That is a pre-existing missed elision of the
		// index's implicit primary-key suffix, unrelated to float ordering and
		// out of scope here. Listing it would make this test assert a bug the
		// float work neither caused nor claims to fix.
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explain := planExplainForTest(t, tc.sql)
			if strings.Contains(explain, "Sort") {
				t.Fatalf("%s\n\nplan kept a sort on a non-float ordering that an "+
					"index/PK scan does deliver. The float termination has "+
					"over-reached and is deleting ordinary sort elimination.\n\n"+
					"plan: %s", tc.sql, explain)
			}
		})
	}
}

// The claim TERMINATES at the float; it does not void the columns before it.
// `ORDER BY a` against index (a, d) must still elide — `a` is claimable, and
// `d` sitting after it in the key is irrelevant to a request that stops at `a`.
func TestOrderingClaimTerminatesAtFloatRatherThanVoiding(t *testing.T) {
	t.Parallel()

	explain := planExplainForTest(t, "SELECT id FROM t WHERE a = 1 ORDER BY a")
	if strings.Contains(explain, "Sort") {
		t.Fatalf("ORDER BY a over index (a, d) kept a sort. A float LATER in "+
			"the key must not retract the claim on the columns BEFORE it.\n\n"+
			"plan: %s", explain)
	}

	// The prefix predicate is what makes this a claim-continuation question at
	// all, so pin the helper's view of the same layout directly.
	layout := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 2},
	})
	if got := values.ClaimableOrderingPrefix(layout, []string{"A", "D", "ID"}); got != 1 {
		t.Fatalf("ClaimableOrderingPrefix([A D ID]) = %d, want 1", got)
	}
}

// eqBoundFloatSchema exists separately from floatOrderingSchema because the
// shapes below need each column reachable through its OWN single-column index.
// floatOrderingSchema carries a compound (a, d), which swallows
// `a = 3 AND d = 2.5` into one scan and destroys the two-leg intersection that
// is this file's sharpest detector.
const eqBoundFloatSchema = `CREATE TABLE t2 (
	id BIGINT,
	a BIGINT,
	s STRING,
	d DOUBLE,
	e FLOAT,
	PRIMARY KEY (id)
)
CREATE INDEX i_d ON t2 (d)
CREATE INDEX i_e ON t2 (e)
CREATE INDEX i_a ON t2 (a)
CREATE INDEX i_s ON t2 (s)`

func eqBoundExplain(t *testing.T, sql string) string {
	t.Helper()
	plan, err := embedded.PlanPhysicalForTest(sql, eqBoundFloatSchema, nil)
	if err != nil {
		t.Fatalf("planning %q: %v", sql, err)
	}
	return plan.Explain()
}

// An EQUALITY-bound float is a FIXED coordinate, not a sorted one: the scan
// range is a single tuple-encoded key prefix, every row under it carries the
// identical float bits, and so the primary-key suffix after it IS delivered in
// key order. The claim must continue through it.
//
// This is the axis that was unprobed while the ordering-claim work went in, and
// the gap was not academic — the claim terminated here too. Nothing was wrong
// in the ROWS, which is exactly why only a plan-shape assertion can see it.
//
// Every shape here is chosen because it DISCRIMINATES. Plain
// `WHERE d = 2.5 ORDER BY id` deliberately is not among them: it keeps its
// elision either way, because the plan-side derivation reaches the same answer
// without consulting the matched ordering parts at all, so it stays green with
// the gate broken and would only pad the count. The two observables that do
// move are the ones listed:
//
//   - DIRECTION. A reverse scan needs the candidate's ordering parts to prove
//     `ORDER BY id DESC` is servable, so a terminated claim costs the elision
//     outright and a sort appears.
//   - INTERSECTION. An intersection needs a common ordering from BOTH legs; a
//     float leg that claims nothing cannot supply one, so the plan silently
//     degrades to a single-index scan plus a filter.
//
// The other direction — that a float still terminates the claim when it is
// SORTED rather than fixed — is TestFloatLeadingOrderByKeepsItsSort above.
func TestEqualityBoundFloatDoesNotTerminateTheClaim(t *testing.T) {
	t.Parallel()

	// want is the substring the plan must contain; the shapes split into the
	// direction group (no sort survives) and the intersection group.
	for _, tc := range []struct {
		name    string
		sql     string
		forbid  string
		require string
	}{
		{name: "double eq, reverse pk", sql: "SELECT id FROM t2 WHERE d = 2.5 ORDER BY id DESC", forbid: "Sort"},
		// FLOAT packs under a different tuple type code than DOUBLE, so it
		// needs its own shape: a predicate keyed on TypeCodeDouble alone would
		// pass the line above and still terminate here.
		{name: "float eq, reverse pk", sql: "SELECT id FROM t2 WHERE e = 2.5 ORDER BY id DESC", forbid: "Sort"},
		{name: "double eq intersects bigint", sql: "SELECT id FROM t2 WHERE d = 2.5 AND a = 3 ORDER BY id", require: "Intersection"},
		{name: "float eq intersects bigint", sql: "SELECT id FROM t2 WHERE e = 2.5 AND a = 3 ORDER BY id", require: "Intersection"},
		{name: "double eq intersects string", sql: "SELECT id FROM t2 WHERE d = 2.5 AND s = 'x' ORDER BY id", require: "Intersection"},
		{name: "intersection under reverse", sql: "SELECT id FROM t2 WHERE d = 2.5 AND a = 3 ORDER BY id DESC", require: "Intersection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explain := eqBoundExplain(t, tc.sql)
			if tc.forbid != "" && strings.Contains(explain, tc.forbid) {
				t.Fatalf("%s\n\nplan materialized a sort over a float coordinate that an "+
					"equality PINS to one physical key. A fixed coordinate claims no order "+
					"of its own, so the primary-key suffix after it is delivered in key "+
					"order and the sort is dead weight. The claim has terminated where it "+
					"should have continued.\n\nplan: %s", tc.sql, explain)
			}
			if tc.require != "" && !strings.Contains(explain, tc.require) {
				t.Fatalf("%s\n\nplan lost its %s. Both legs must supply a common "+
					"primary-key ordering, and the float leg supplies one only while an "+
					"equality-pinned float coordinate stays claimable. A terminated claim "+
					"degrades this to one index plus a residual filter — same rows, more "+
					"work, and nothing but the plan shape shows it.\n\nplan: %s",
					tc.sql, tc.require, explain)
			}
		})
	}
}

// The one float equality that pins NOTHING: zero.
//
// -0.0 and +0.0 are IEEE-equal but pack to two DISTINCT adjacent keys, so the
// executor widens `= 0.0` to span both. The scan then covers TWO physical
// prefixes and the primary-key suffix RESTARTS at the boundary — over rows
// (-0.0, 9) and (+0.0, 1), an elided `WHERE d = 0 ORDER BY id` hands back
// [9 1].
//
// This is the guard rail on the exemption above. Exempting equalities from the
// float termination by asking `IsEquality()` alone would pass every assertion
// in TestEqualityBoundFloatDoesNotTerminateTheClaim and silently re-arm this
// bug, so the two tests have to be read as a pair.
func TestZeroFloatEqualityStillTerminatesTheClaim(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sql     string
		forbid  string
		require string
	}{
		{name: "double positive zero", sql: "SELECT id FROM t2 WHERE d = 0.0 ORDER BY id", require: "Sort"},
		// Written as -0.0 in the query text: the same widening applies, since
		// the two zeros are one logical value spanning two keys either way.
		{name: "double negative zero", sql: "SELECT id FROM t2 WHERE d = -0.0 ORDER BY id", require: "Sort"},
		{name: "float positive zero", sql: "SELECT id FROM t2 WHERE e = 0.0 ORDER BY id", require: "Sort"},
		{name: "zero under reverse", sql: "SELECT id FROM t2 WHERE d = 0.0 ORDER BY id DESC", require: "Sort"},
		// The intersection counterpart: a zero-float leg cannot supply the
		// common primary-key ordering an intersection merges on, so the plan
		// must NOT form one here even though the nonzero shape above does.
		{name: "zero eq does not intersect", sql: "SELECT id FROM t2 WHERE d = 0.0 AND a = 3 ORDER BY id", forbid: "Intersection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			explain := eqBoundExplain(t, tc.sql)
			if tc.require != "" && !strings.Contains(explain, tc.require) {
				t.Fatalf("%s\n\nplan elided the sort on a ZERO float equality. That equality "+
					"pins no single key — it spans both -0.0 and +0.0, two distinct adjacent "+
					"keys — so the scan covers two physical prefixes and the primary-key "+
					"suffix restarts at the boundary. The rows come back unsorted.\n\n"+
					"plan: %s", tc.sql, explain)
			}
			if tc.forbid != "" && strings.Contains(explain, tc.forbid) {
				t.Fatalf("%s\n\nplan formed an %s over a ZERO float equality. That leg spans "+
					"two physical prefixes and supplies no primary-key ordering, so the "+
					"merge has no common key to trust.\n\nplan: %s", tc.sql, tc.forbid, explain)
			}
		})
	}
}
