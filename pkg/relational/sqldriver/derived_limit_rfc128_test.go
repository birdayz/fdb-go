package sqldriver_test

// RFC-128 — SQL LIMIT/OFFSET is a uniform RecordQueryLimitPlan operator applied
// at its pipeline position, NOT a post-execution hoist. These FDB integration
// tests pin §4 scenarios 1-13: derived-table / CTE / union LIMIT correctness,
// plain top-level LIMIT, multi-page rollover (the continuation envelope), the
// EXPLAIN Limit node + Limit(Sort) ordering gate, plan-cache reuse, scan-bound
// non-regression, and shared-combinator resume. The pre-fix bug returned
// [7,8,9,10] for scenario 1 instead of [5,6,7]; every scenario here is
// revert-proven.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// rfc128DB seeds a single-column table t(id) with ids 1..10 and returns a *sql.DB
// bound to it. Each call uses a unique db/template name (tag) for t.Parallel
// isolation.
func rfc128DB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/rfc128_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "rfc128_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE t (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE t2 (id BIGINT, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9),(10)"); err != nil {
		t.Fatalf("seed t: %v", err)
	}
	// t2: v has duplicates so SELECT DISTINCT v is meaningful.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1,10),(2,10),(3,20),(4,20),(5,30),(6,30),(7,40),(8,40)"); err != nil {
		t.Fatalf("seed t2: %v", err)
	}
	return db, ctx
}

// getInts runs a single-int-column query and returns the rows in order.
func getInts(t *testing.T, ctx context.Context, db queryer, q string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan (%s): %v", q, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (%s): %v", q, err)
	}
	return out
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func wantInts(t *testing.T, got, want []int64, ctx string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", ctx, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", ctx, got, want)
		}
	}
}

// explainPlan returns the EXPLAIN PLAN text for q.
func explainPlan(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+q)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		for _, v := range vals {
			switch s := v.(type) {
			case string:
				plan.WriteString(s)
			case []byte:
				plan.Write(s)
			}
			plan.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows.Err: %v", err)
	}
	return plan.String()
}

// ---------------------------------------------------------------------------
// §4.1 — the repro: derived-table LIMIT under outer WHERE + ORDER BY.
// ---------------------------------------------------------------------------

func TestFDB_DerivedTableLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "repro")

	// inner: ORDER BY id OFFSET 2 LIMIT 5 → [3,4,5,6,7]; outer WHERE id>4 → [5,6,7].
	got := getInts(t, ctx, db,
		"SELECT id FROM (SELECT id FROM t ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id")
	wantInts(t, got, []int64{5, 6, 7}, "derived-table LIMIT under WHERE (was [7,8,9,10] pre-fix)")
}

// ---------------------------------------------------------------------------
// §4.2 — derived LIMIT under outer ORDER BY + outer LIMIT: both apply.
// ---------------------------------------------------------------------------

func TestFDB_DerivedLimit_OuterOrderAndLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "doubleouter")

	// inner [3..7]; outer ORDER BY id LIMIT 3 OFFSET 1 → drop 3 → [4,5,6].
	got := getInts(t, ctx, db,
		"SELECT id FROM (SELECT id FROM t ORDER BY id LIMIT 5 OFFSET 2) AS s ORDER BY id LIMIT 3 OFFSET 1")
	wantInts(t, got, []int64{4, 5, 6}, "derived LIMIT + outer ORDER BY + outer LIMIT")
}

// ---------------------------------------------------------------------------
// §4.3 — derived LIMIT, no outer shaping (guards over-correction).
// ---------------------------------------------------------------------------

func TestFDB_DerivedLimit_NoOuterShaping_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "noouter")

	got := getInts(t, ctx, db,
		"SELECT id FROM (SELECT id FROM t ORDER BY id LIMIT 3) AS s ORDER BY id")
	wantInts(t, got, []int64{1, 2, 3}, "derived LIMIT with no outer shaping")
}

// ---------------------------------------------------------------------------
// §4.4 — plain top-level LIMIT unaffected.
// ---------------------------------------------------------------------------

func TestFDB_PlainTopLevelLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "plain")

	got := getInts(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2")
	wantInts(t, got, []int64{3, 4, 5}, "plain top-level LIMIT/OFFSET")

	// LIMIT with no OFFSET.
	got = getInts(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 4")
	wantInts(t, got, []int64{1, 2, 3, 4}, "plain top-level LIMIT, no offset")
}

// Qualified-star (a.*) + LIMIT regression (code-review finder on the RFC-128 impl).
// The a.* projection forces VisitSimpleTable's needRebuild path, which REPLACES op
// via buildLogicalPlanForSelect(sq); sq came from selectQueryFromClassification with
// limit:-1, so before the fix the LIMIT operator was never re-applied and
// `SELECT a.* FROM t a LIMIT 5` returned ALL rows (the post-execution hoist used to
// mask this; RFC-128 removed it). Revert-proven: without the parseLimitClause carry
// in the rebuild branch this returns 10 rows.
func TestFDB_QualifiedStarLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "qstar")

	got := getInts(t, ctx, db, "SELECT a.* FROM t a ORDER BY id LIMIT 5")
	wantInts(t, got, []int64{1, 2, 3, 4, 5}, "qualified-star a.* + LIMIT")

	got = getInts(t, ctx, db, "SELECT a.* FROM t a ORDER BY id LIMIT 3 OFFSET 2")
	wantInts(t, got, []int64{3, 4, 5}, "qualified-star a.* + LIMIT OFFSET")
}

// ---------------------------------------------------------------------------
// §4.5 — SELECT DISTINCT … LIMIT (LIMIT applies AFTER distinct).
// ---------------------------------------------------------------------------

func TestFDB_SelectDistinctLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "distinct")

	// distinct v over t2 = {10,20,30,40}; ORDER BY v LIMIT 3 OFFSET 1 → [20,30,40].
	got := getInts(t, ctx, db, "SELECT DISTINCT v FROM t2 ORDER BY v LIMIT 3 OFFSET 1")
	wantInts(t, got, []int64{20, 30, 40}, "SELECT DISTINCT ... LIMIT")
}

// ---------------------------------------------------------------------------
// §4.6 — top-level CTE + LIMIT, and a LIMIT inside the CTE body.
// (the LogicalCTE.Children()[0]==Body mis-walk class.)
// ---------------------------------------------------------------------------

func TestFDB_CTELimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "cte")

	// Outer LIMIT over a CTE main: pre-fix the children[0]==Body walk missed it.
	got := getInts(t, ctx, db,
		"WITH c AS (SELECT id FROM t) SELECT id FROM c ORDER BY id LIMIT 3")
	wantInts(t, got, []int64{1, 2, 3}, "top-level CTE + outer LIMIT")

	// LIMIT inside the CTE body: must NOT be mis-hoisted to the top level.
	got = getInts(t, ctx, db,
		"WITH c AS (SELECT id FROM t ORDER BY id LIMIT 2) SELECT id FROM c ORDER BY id")
	wantInts(t, got, []int64{1, 2}, "LIMIT inside CTE body")
}

// ---------------------------------------------------------------------------
// §4.7 — union: per-branch LIMIT and a trailing LIMIT over a union.
// ---------------------------------------------------------------------------

func TestFDB_UnionLimit_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "union")

	// Per-branch LIMIT: each branch capped at 2 → 2 from t (ids 1,2) + 2 from
	// t2 (ids 1,2). UNION ALL keeps duplicates; sort the result for a stable
	// assertion.
	got := getInts(t, ctx, db,
		"SELECT id FROM ((SELECT id FROM t ORDER BY id LIMIT 2) UNION ALL (SELECT id FROM t2 ORDER BY id LIMIT 2)) AS u ORDER BY id")
	wantInts(t, got, []int64{1, 1, 2, 2}, "union with per-branch LIMIT")

	// Trailing LIMIT over a union: union of t(1,2,3) and t2(1,2,3) = {1,2,3}
	// (UNION ALL → 6 rows: 1,1,2,2,3,3) ORDER BY id LIMIT 3 → [1,1,2].
	got = getInts(t, ctx, db,
		"SELECT id FROM ((SELECT id FROM t WHERE id<=3) UNION ALL (SELECT id FROM t2 WHERE id<=3)) AS u ORDER BY id LIMIT 3")
	wantInts(t, got, []int64{1, 1, 2}, "union with trailing LIMIT")
}

// ---------------------------------------------------------------------------
// §4.8 — multi-page rollover: a tiny scanned-rows page budget splits the LIMIT
// window across ≥2 fetchPage transactions. Without the §3.3 envelope the resume
// re-skips offset / resets limit and returns the wrong rows. THIS is the test
// that proves the re-skip is fixed, not merely shielded.
// ---------------------------------------------------------------------------

func TestFDB_MultiPageLimitRollover_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "rollover")

	// Pin a connection with a tiny per-page scanned-rows budget (3) so the
	// LIMIT 4 OFFSET 3 window (rows 4,5,6,7) straddles several pages.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 3). // paginate every 3 scanned rows
			Build())
	})

	got, err := getIntsConn(t, ctx, conn,
		"SELECT id FROM t ORDER BY id LIMIT 4 OFFSET 3")
	if err != nil {
		t.Fatalf("rollover query: %v", err)
	}
	wantInts(t, got, []int64{4, 5, 6, 7}, "multi-page LIMIT/OFFSET rollover (envelope resume)")

	// The derived-table repro under the same tiny page budget: the inner LIMIT
	// window must still page correctly across transactions.
	got, err = getIntsConn(t, ctx, conn,
		"SELECT id FROM (SELECT id FROM t ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id")
	if err != nil {
		t.Fatalf("rollover derived query: %v", err)
	}
	wantInts(t, got, []int64{5, 6, 7}, "multi-page derived LIMIT rollover")
}

// getIntsConn is getInts over a pinned *sql.Conn (for connection-local options).
func getIntsConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string) ([]int64, error) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// §4.9 — EXPLAIN: the top-level LIMIT now shows a Limit node (previously
// invisible because the translator skipped it).
// ---------------------------------------------------------------------------

func TestFDB_ExplainShowsLimitNode_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "explainlimit")

	plan := explainPlan(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2")
	if !strings.Contains(plan, "Limit(") {
		t.Fatalf("EXPLAIN does not show a Limit node: %q", plan)
	}
	// The query still returns correct rows alongside the visible operator.
	got := getInts(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2")
	wantInts(t, got, []int64{3, 4, 5}, "explain-limit query rows")
}

// ---------------------------------------------------------------------------
// §4.10 — ordering gate: ORDER BY x LIMIT n plans as Limit(Sort(...)) — the
// limit ABOVE the sort (applied after ordering), NOT Sort(Limit(...)). The
// sort is on a NON-PK column so a physical Sort node is actually present.
// ---------------------------------------------------------------------------

func TestFDB_LimitAboveSort_OrderingGate_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "limitsort")

	// ORDER BY a non-PK column (v) forces a Sort; LIMIT must wrap it.
	plan := explainPlan(t, ctx, db, "SELECT v FROM t2 ORDER BY v LIMIT 3")
	limitIdx := strings.Index(plan, "Limit(")
	sortIdx := strings.Index(plan, "Sort(")
	if limitIdx < 0 {
		t.Fatalf("no Limit node in plan: %q", plan)
	}
	if sortIdx < 0 {
		t.Fatalf("no Sort node in plan (ORDER BY non-PK should sort): %q", plan)
	}
	if limitIdx > sortIdx {
		t.Fatalf("Limit must be ABOVE Sort (Limit(Sort(...))), got %q", plan)
	}
	// Correctness alongside the shape: v sorted ascending, first 3 distinct-ish
	// values of v are 10,10,20.
	got := getInts(t, ctx, db, "SELECT v FROM t2 ORDER BY v LIMIT 3")
	wantInts(t, got, []int64{10, 10, 20}, "limit-above-sort rows")
}

// ---------------------------------------------------------------------------
// §4.11 — plan-cache reuse: a LIMIT query run twice reuses the cached physical
// plan (now legal post-hoist-removal) and returns the same correct rows.
// ---------------------------------------------------------------------------

func TestFDB_LimitPlanCacheReuse_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "cache")

	q := "SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 1"
	got1 := getInts(t, ctx, db, q)
	wantInts(t, got1, []int64{2, 3, 4}, "limit cache run 1")
	got2 := getInts(t, ctx, db, q)
	wantInts(t, got2, []int64{2, 3, 4}, "limit cache run 2 (cached plan)")
}

// ---------------------------------------------------------------------------
// §4.12 — scan-bound non-regression: a plain LIMIT k over the table bounds the
// scan via the operator's ReturnedRowLimit = offset+limit (executor.go), so the
// EXPLAIN carries the limit and the result is correct. (The retired pageRowBudget
// LIMIT arm no longer does this; the operator does.)
// ---------------------------------------------------------------------------

func TestFDB_LimitScanBound_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "scanbound")

	plan := explainPlan(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 2")
	if !strings.Contains(plan, "Limit(") {
		t.Fatalf("LIMIT not visible in plan (scan-bound regressed): %q", plan)
	}
	got := getInts(t, ctx, db, "SELECT id FROM t ORDER BY id LIMIT 2")
	wantInts(t, got, []int64{1, 2}, "scan-bound LIMIT rows")
}

// ---------------------------------------------------------------------------
// §4.13 — shared-combinator resume: an operator that uses applySkipLimit's
// props.Skip / ReturnedRowLimit (here a plain scan with a MAX_ROWS returned-row
// cap forcing pagination) still resumes correctly across a page — proving the
// envelope was confined to RecordQueryLimitPlan and did not disturb the shared
// SkipCursor / RowLimitedCursor.
// ---------------------------------------------------------------------------

func TestFDB_SharedCombinatorResume_RFC128(t *testing.T) {
	t.Parallel()
	db, ctx := rfc128DB(t, "sharedcombo")

	// MAX_ROWS=4 with a tiny scan page (2) over a 10-row scan with NO SQL LIMIT:
	// this drives applySkipLimit's ReturnedRowLimit / SkipCursor path across
	// pages, independent of the LIMIT envelope. Exactly 4 rows must come back.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxRows, 4).
			Set(api.OptExecutionScannedRowsLimit, 2).
			Build())
	})
	got, err := getIntsConn(t, ctx, conn, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("shared-combinator query: %v", err)
	}
	wantInts(t, got, []int64{1, 2, 3, 4}, "shared-combinator MAX_ROWS resume")
}
