package sqldriver_test

// The ordering-claim termination reaches the AGGREGATE producers, not only the
// scan producers.
//
// `RecordQueryStreamingAggregationPlan.HintOrdering` and
// `RecordQueryAggregateIndexPlan.HintOrdering` each advertise group order from
// their own state. Both were unconditional, and both returned WRONG ROWS on a
// real cluster for `SELECT d, SUM(a) FROM t GROUP BY d ORDER BY d` with d
// DOUBLE: the negative-NaN group came back FIRST, where CompareFloat64 ranks
// every NaN GREATEST.
//
// The streaming-aggregation case is NOT derivative of the inner scan.
// StreamingAggFromIndexRule matches grouping keys against index column NAMES
// and never reads the inner's ordering claim, so terminating the index scan's
// claim leaves this producer stating the same false order over the same
// physical stream. That is why each producer needs its own shape here — a test
// that only covers one is not coverage for the other.
//
// MEASURED before the fix, both indexed paths against the same ladder:
//
//	StreamingAgg(IndexScan(AI_DA))  -> NaN(7) -Inf -1.5 -0 0 1.5 1e308 +Inf NaN(8)
//	AggregateIndex(SUM_BY_D)        -> NaN(7) -Inf -1.5 -0 0 1.5 1e308 +Inf NaN(8)
//	unindexed oracle                -> -Inf -1.5 -0 0 1.5 1e308 +Inf NaN(8) NaN(7)

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
)

// aggFloatGroup is one row of `SELECT d, SUM(a) ... GROUP BY d`.
type aggFloatGroup struct {
	d   float64
	sum int64
}

func (g aggFloatGroup) String() string {
	return fmt.Sprintf("{%v(%#016x) sum=%d}", g.d, math.Float64bits(g.d), g.sum)
}

// aggFloatLadder writes the IEEE-754 probe ladder into tbl.
//
// Two properties are load-bearing:
//
//   - BOTH NaN signs, with DISTINCT payloads. A positive NaN alone hides the
//     defect entirely: it is physically and logically last. The negative NaN is
//     the one that packs BEFORE -Inf while comparing GREATEST.
//   - id 80 is BALLAST, and it is not decoration. The non-finite rows can only
//     be created by UPDATE (INSERT ... VALUES and bind parameters reject
//     non-finite floats with 22023), and they are seeded at 1.0e308. Without a
//     row that KEEPS 1.0e308, those updates vacate that group — and the SUM
//     index then reports a phantom `{1e+308 sum=0}` group that no oracle
//     produces. That is an index-maintenance defect, entirely independent of
//     ordering, and it is tracked separately; the ballast keeps it out of this
//     test so a red here means what this test says it means.
func aggFloatLadder(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO %s (id, d, a) VALUES "+
			"(10, 1.0e308, 1), (20, -1.5, 2), (30, -0.0, 3), (40, 0.0, 4), "+
			"(50, 1.5, 5), (60, 1.0e308, 6), (70, 1.0e308, 7), (5, 0.0, 8), "+
			"(80, 1.0e308, 9)", tbl))
	// -Inf, +Inf, and the sign-bit-SET quiet NaN that an invalid operation
	// (+Inf added to -Inf) yields — the physically FIRST row in the table.
	mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE %s SET d = d * -10.0 WHERE id = 10", tbl))
	mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE %s SET d = d * 10.0 WHERE id = 60", tbl))
	mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE %s SET d = (d * 10.0) + (d * -10.0) WHERE id = 70", tbl))
	// A DIFFERENT NaN payload, and positive, so "two bit patterns, one logical
	// value" is exercised and the two NaNs land in the two disjoint physical
	// blocks at opposite ends of the key space.
	mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE %s SET d = CAST('NaN' AS DOUBLE) WHERE id = 5", tbl))
}

// assertAggFloatLadderStored fails loudly if the ladder did not land. Every
// assertion below is vacuous without it: if the write path ever starts
// rejecting non-finite values through UPDATE too, this file would be comparing
// tables of ordinary finite doubles and would pass with the defect present.
func assertAggFloatLadderStored(t *testing.T, db *sql.DB, ctx context.Context, tbl string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT id, d FROM %s", tbl))
	if err != nil {
		t.Fatalf("ladder readback on %s: %v", tbl, err)
	}
	defer rows.Close()
	got := map[int64]float64{}
	for rows.Next() {
		var id int64
		var d sql.NullFloat64
		if err := rows.Scan(&id, &d); err != nil {
			t.Fatalf("ladder scan on %s: %v", tbl, err)
		}
		if !d.Valid {
			t.Fatalf("%s id=%d stored NULL", tbl, id)
		}
		got[id] = d.Float64
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ladder rows on %s: %v", tbl, err)
	}
	check := func(id int64, ok func(float64) bool, want string) {
		t.Helper()
		v, present := got[id]
		if !present {
			t.Fatalf("%s: id %d missing", tbl, id)
		}
		if !ok(v) {
			t.Fatalf("%s: id %d stored %v (bits %#016x), want %s — the ladder did not land, "+
				"so every ordering assertion in this file is vacuous", tbl, id, v, math.Float64bits(v), want)
		}
	}
	check(10, func(v float64) bool { return math.IsInf(v, -1) }, "-Inf")
	check(60, func(v float64) bool { return math.IsInf(v, +1) }, "+Inf")
	check(30, func(v float64) bool { return v == 0 && math.Signbit(v) }, "-0.0")
	check(40, func(v float64) bool { return v == 0 && !math.Signbit(v) }, "+0.0")
	check(70, func(v float64) bool { return math.IsNaN(v) && math.Signbit(v) }, "a NEGATIVE NaN")
	check(5, func(v float64) bool { return math.IsNaN(v) && !math.Signbit(v) }, "a POSITIVE NaN")
	check(80, func(v float64) bool { return v == 1.0e308 }, "the 1.0e308 ballast")
	if math.Float64bits(got[70]) == math.Float64bits(got[5]) {
		t.Fatalf("%s: the two NaN rows share bit pattern %#016x; the ladder must carry two "+
			"DISTINCT payloads", tbl, math.Float64bits(got[70]))
	}
}

func aggFloatGroups(t *testing.T, db *sql.DB, ctx context.Context, q string) []aggFloatGroup {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []aggFloatGroup
	for rows.Next() {
		var g aggFloatGroup
		if err := rows.Scan(&g.d, &g.sum); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", q, err)
	}
	return out
}

func aggFloatExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

// splitAggFloatGroups partitions an answer into its non-NaN prefix and its NaN
// groups, and reports the position of the first NaN and the last non-NaN.
func splitAggFloatGroups(in []aggFloatGroup) (body, nans []aggFloatGroup, firstNaN, lastNonNaN int) {
	firstNaN, lastNonNaN = -1, -1
	for i, g := range in {
		if math.IsNaN(g.d) {
			if firstNaN < 0 {
				firstNaN = i
			}
			nans = append(nans, g)
			continue
		}
		body = append(body, g)
		lastNonNaN = i
	}
	return body, nans, firstNaN, lastNonNaN
}

// sameAggFloatGroups compares group sequences exactly, by BIT PATTERN — so
// -0.0 and +0.0 are different groups, which `==` says they are not.
func sameAggFloatGroups(a, b []aggFloatGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float64bits(a[i].d) != math.Float64bits(b[i].d) || a[i].sum != b[i].sum {
			return false
		}
	}
	return true
}

func sortedAggFloatGroups(in []aggFloatGroup) []aggFloatGroup {
	out := make([]aggFloatGroup, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].sum < out[j].sum })
	return out
}

// TestFDB_FloatOrderingClaim_Aggregate_Differential runs each aggregate
// producer against an unindexed oracle holding identical rows.
func TestFDB_FloatOrderingClaim_Aggregate_Differential(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_focagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_focagg")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE focagg "+
		// ai: an ORDINARY index over (d, a) — the streaming-aggregation shape.
		"CREATE TABLE ai (id BIGINT, d DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		// ag: an AGGREGATE index grouped by d — the aggregate-index shape.
		"CREATE TABLE ag (id BIGINT, d DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		// ao: the oracle — identical rows, NO index, so the planner has no
		// claimed order to elide onto and must sort with CompareFloat64.
		"CREATE TABLE ao (id BIGINT, d DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX ai_da ON ai (d, a) "+
		"CREATE INDEX sum_by_d AS SELECT SUM(a) FROM ag GROUP BY d")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_focagg/s WITH TEMPLATE focagg")
	dsn := fmt.Sprintf("fdbsql:///testdb_focagg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, tbl := range []string{"ai", "ag", "ao"} {
		aggFloatLadder(t, db, ctx, tbl)
		assertAggFloatLadderStored(t, db, ctx, tbl)
	}

	refQ := "SELECT d, SUM(a) FROM ao GROUP BY d ORDER BY d"
	ref := aggFloatGroups(t, db, ctx, refQ)
	refBody, refNaNs, _, _ := splitAggFloatGroups(ref)
	if len(refNaNs) == 0 {
		t.Fatalf("the oracle produced no NaN group, so the shape under test is not present: %v", ref)
	}

	// wantBody is asserted OUTRIGHT as well as differentially: a differential
	// alone passes when both sides are wrong the same way.
	wantBody := []aggFloatGroup{
		{d: math.Inf(-1), sum: 1},
		{d: -1.5, sum: 2},
		{d: math.Copysign(0, -1), sum: 3},
		{d: 0, sum: 4},
		{d: 1.5, sum: 5},
		{d: 1.0e308, sum: 9},
		{d: math.Inf(+1), sum: 6},
	}
	if !sameAggFloatGroups(refBody, wantBody) {
		t.Fatalf("the ORACLE's non-NaN groups are %v, want %v — the oracle itself is wrong, "+
			"so nothing below can be trusted", refBody, wantBody)
	}

	// The NaN tie order is NOT asserted. All NaN payloads are ONE logical value
	// under CompareFloat64, so GROUP BY leaves them tied and their relative
	// order is free. (That GROUP BY produces TWO NaN groups rather than one is a
	// separate, unfixed dedup question — it holds identically on the oracle, so
	// it is not what any assertion here turns on.)
	for _, tc := range []struct {
		name, table, wantPlan string
	}{
		{
			name:  "streaming_aggregation_over_an_ordinary_index",
			table: "ai",
			// The producer under test: StreamingAgg advertising group order over
			// an index scan whose leading key is the DOUBLE.
			wantPlan: "STREAMINGAGG",
		},
		{
			name:  "aggregate_index",
			table: "ag",
			// This case USED to read the aggregate index directly
			// (wantPlan: "AGGREGATEINDEX"). It no longer can, and the reason is
			// worth stating rather than relaxing away.
			//
			// A grouped SUM index cannot decide group existence on its own, so
			// RFC-209 §5.3 either companion-joins it or declines it. The
			// companion join is a MERGE, and a merge needs both streams to be
			// physically ordered congruently with the comparison. An UNBOUND raw
			// DOUBLE grouping coordinate is exactly the case where that fails:
			// its FDB tuple key order is not its value order, because a
			// negative-NaN payload packs before -Inf. So the merge declines and
			// planning falls back to streaming aggregation over base rows.
			//
			// The rows below are still asserted against the oracle, so this case
			// keeps testing what it always tested — that a DOUBLE grouping
			// column's KEY order is never advertised as its VALUE order. What it
			// no longer covers is the aggregate-index PRODUCER for this shape.
			// Re-arming that requires the group-existence merge to become safe on
			// a raw DOUBLE coordinate, not weakening this expectation.
			wantPlan: "STREAMINGAGG",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := fmt.Sprintf("SELECT d, SUM(a) FROM %s GROUP BY d ORDER BY d", tc.table)
			plan := aggFloatExplain(t, db, ctx, q)
			up := strings.ToUpper(strings.ReplaceAll(plan, " ", ""))
			if !strings.Contains(up, tc.wantPlan) {
				t.Fatalf("the plan does not contain %s, so this shape never reaches the "+
					"producer it exists to test\n  query: %s\n  plan:  %s", tc.wantPlan, q, plan)
			}
			got := aggFloatGroups(t, db, ctx, q)
			body, nans, firstNaN, lastNonNaN := splitAggFloatGroups(got)

			if !sameAggFloatGroups(body, wantBody) {
				t.Errorf("non-NaN groups came back as %v, want %v — the aggregate advertised the "+
					"FDB tuple KEY order of a DOUBLE grouping column as its VALUE order\n"+
					"  query: %s\n  plan:  %s", body, wantBody, q, plan)
			}
			if firstNaN >= 0 && firstNaN < lastNonNaN {
				t.Errorf("a NaN group landed at position %d, before the last non-NaN group at "+
					"position %d. CompareFloat64 ranks every NaN GREATEST, but a negative-NaN "+
					"payload packs BEFORE -Inf, so an aggregate that claims its physical group "+
					"order emits it first\n  got:   %v\n  query: %s\n  plan:  %s",
					firstNaN, lastNonNaN, got, q, plan)
			}
			// The differential: same groups, same sums, same position of the NaN
			// block — only the free tie order inside it may differ.
			if !sameAggFloatGroups(sortedAggFloatGroups(nans), sortedAggFloatGroups(refNaNs)) {
				t.Errorf("DIFFERENTIAL MISMATCH on the NaN tie class: indexed=%v oracle=%v\n"+
					"  query: %s\n  plan:  %s\n  ref:   %s", nans, refNaNs, q, plan, refQ)
			}
			if !sameAggFloatGroups(body, refBody) {
				t.Errorf("DIFFERENTIAL MISMATCH: indexed=%v oracle=%v\n  query: %s\n  plan:  %s\n  ref: %s",
					body, refBody, q, plan, refQ)
			}
		})
	}
}
