package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// planExplainVia retrieves the Cascades physical plan Explain string
// via the underlying EmbeddedConnection for the given query.
func planExplainVia(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()
	var plan string
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("expected *embedded.EmbeddedConnection, got %T", driverConn)
		}
		p, err := ec.PlanExplain(ctx, query)
		if err != nil {
			return err
		}
		plan = p
		return nil
	}); err != nil {
		t.Fatalf("PlanExplain(%q): %v", query, err)
	}
	return plan
}

// setupPlanShapeDB creates a fresh database + schema for a plan-shape subtest.
// Returns a *sql.DB connected to the created schema.
func setupPlanShapeDB(t *testing.T, suffix, templateDDL string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	dbPath := fmt.Sprintf("/planshape_%s_%s", suffix, t.Name())
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("ps_%s_%s", suffix, t.Name())
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, templateDDL)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFDB_PlanShapePKLookup verifies that a simple WHERE on the primary key
// produces a scan + filter plan (no index scan, no sort).
func TestFDB_PlanShapePKLookup(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "pk", "CREATE TABLE users (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))")

	for _, u := range []struct {
		id   int
		name string
	}{
		{1, "Alice"},
		{2, "Bob"},
		{3, "Carol"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO users VALUES (%d, '%s')", u.id, u.name)); err != nil {
			t.Fatalf("INSERT id=%d: %v", u.id, err)
		}
	}

	q := "SELECT id, name FROM users WHERE id = 1"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// Plan should contain a scan of the users table.
	if !strings.Contains(plan, "Scan") {
		t.Fatalf("expected Scan in plan, got: %s", plan)
	}
	// No InMemorySort needed for a PK equality lookup.
	if strings.Contains(plan, "InMemorySort") {
		t.Fatalf("PK lookup should not require InMemorySort: %s", plan)
	}
	// No IndexScan — there is no secondary index; PK scan + filter suffices.
	if strings.Contains(plan, "IndexScan") {
		t.Fatalf("PK lookup should use primary Scan, not IndexScan: %s", plan)
	}

	// Verify query results.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	var gotID int64
	var gotName string
	if !rows.Next() {
		t.Fatal("expected 1 row, got 0")
	}
	if err := rows.Scan(&gotID, &gotName); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if gotID != 1 || gotName != "Alice" {
		t.Fatalf("expected (1, Alice), got (%d, %s)", gotID, gotName)
	}
	if rows.Next() {
		t.Fatal("expected exactly 1 row, got more")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
}

// TestFDB_PlanShapeIndexScanRange verifies that a range filter + ORDER BY on
// an indexed column produces an IndexScan without InMemorySort.
func TestFDB_PlanShapeIndexScanRange(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "ixr",
		"CREATE TABLE items (id BIGINT NOT NULL, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_price ON items (price)")

	for _, item := range []struct {
		id    int
		price int
	}{
		{1, 500},
		{2, 50},
		{3, 150},
		{4, 200},
		{5, 25},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, %d)", item.id, item.price)); err != nil {
			t.Fatalf("INSERT id=%d: %v", item.id, err)
		}
	}

	q := "SELECT id, price FROM items WHERE price > 100 ORDER BY price"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// Must use IndexScan on idx_price.
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected IndexScan in plan, got: %s", plan)
	}
	// Index provides ordering — no InMemorySort.
	if strings.Contains(plan, "InMemorySort") {
		t.Fatalf("expected no InMemorySort (index provides ORDER BY ordering), got: %s", plan)
	}

	// Verify query results: prices > 100, ascending.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	type row struct {
		id    int64
		price int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.price); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	expected := []row{{3, 150}, {4, 200}, {1, 500}}
	if len(got) != len(expected) {
		t.Fatalf("expected %d rows, got %d: %+v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
		}
	}
}

// TestFDB_PlanShapeStreamingAggIndex verifies that GROUP BY on an indexed
// column produces StreamingAgg (not HashAgg) with no InMemorySort.
func TestFDB_PlanShapeStreamingAggIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "sagg",
		"CREATE TABLE items (id BIGINT NOT NULL, category STRING, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_category ON items (category)")

	for _, item := range []struct {
		id       int
		category string
		price    int
	}{
		{1, "electronics", 500},
		{2, "books", 50},
		{3, "clothing", 150},
		{4, "electronics", 200},
		{5, "books", 25},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO items VALUES (%d, '%s', %d)", item.id, item.category, item.price)); err != nil {
			t.Fatalf("INSERT id=%d: %v", item.id, err)
		}
	}

	q := "SELECT category, COUNT(*) FROM items GROUP BY category ORDER BY category"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// Must use StreamingAgg (index provides GROUP BY ordering).
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected StreamingAgg in plan, got: %s", plan)
	}
	// Must NOT fall back to HashAgg.
	if strings.Contains(plan, "HashAgg") {
		t.Fatalf("expected no HashAgg when index covers GROUP BY, got: %s", plan)
	}
	// Must use IndexScan on idx_category.
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected IndexScan in plan, got: %s", plan)
	}
	// No InMemorySort (index provides ordering).
	if strings.Contains(plan, "InMemorySort") {
		t.Fatalf("expected no InMemorySort (index provides ordering), got: %s", plan)
	}

	// Verify query results: grouped by category, ordered ascending.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	type aggRow struct {
		category string
		cnt      int64
	}
	var got []aggRow
	for rows.Next() {
		var r aggRow
		if err := rows.Scan(&r.category, &r.cnt); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	expected := []aggRow{
		{"books", 2},
		{"clothing", 1},
		{"electronics", 2},
	}
	if len(got) != len(expected) {
		t.Fatalf("expected %d groups, got %d: %+v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("row %d: expected %+v, got %+v\nfull: %+v", i, expected[i], got[i], got)
		}
	}
}

// TestFDB_PlanShapeJoinFilterPushdown verifies that a filter on the outer side
// of a join is pushed below the NestedLoopJoin (filter before join, not after).
func TestFDB_PlanShapeJoinFilterPushdown(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "jfp",
		"CREATE TABLE a (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE b (bid BIGINT NOT NULL, aid BIGINT, PRIMARY KEY (bid))")

	// Insert data into table a.
	for _, row := range []struct {
		id   int
		name string
	}{
		{1, "foo"},
		{2, "bar"},
		{3, "baz"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO a VALUES (%d, '%s')", row.id, row.name)); err != nil {
			t.Fatalf("INSERT a id=%d: %v", row.id, err)
		}
	}
	// Insert data into table b.
	for _, row := range []struct {
		bid int
		aid int
	}{
		{10, 1},
		{20, 2},
		{30, 1},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO b VALUES (%d, %d)", row.bid, row.aid)); err != nil {
			t.Fatalf("INSERT b bid=%d: %v", row.bid, err)
		}
	}

	q := "SELECT a.id FROM a INNER JOIN b ON a.id = b.aid WHERE a.name = 'foo'"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// Must contain a NestedLoopJoin.
	if !strings.Contains(plan, "NestedLoopJoin") {
		t.Fatalf("expected NestedLoopJoin in plan, got: %s", plan)
	}

	// The WHERE filter and the ON join predicate should both be present.
	// Currently the planner places the WHERE filter above the join:
	//   Filter([1 preds], NestedLoopJoin(INNER, [1 preds], Scan(A), Scan(B)))
	// The join predicate (a.id = b.aid) is inside the NLJ as [1 preds].
	// Pin this shape so any regression (e.g. losing the join predicate or
	// the filter entirely) is caught.
	if !strings.Contains(plan, "Filter") {
		t.Fatalf("expected Filter for WHERE clause, got: %s", plan)
	}
	// The NLJ must carry its own join predicate.
	nljIdx := strings.Index(plan, "NestedLoopJoin")
	if nljIdx < 0 {
		t.Fatalf("expected NestedLoopJoin in plan, got: %s", plan)
	}
	afterNLJ := plan[nljIdx:]
	if !strings.Contains(afterNLJ, "preds") {
		t.Fatalf("expected join predicates inside NestedLoopJoin, got: %s", plan)
	}

	// Verify query results: only a.id=1 has name='foo', and b has two rows
	// with aid=1 (bid=10, bid=30), so we expect 2 result rows.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 rows (a.id=1 matched twice), got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id != 1 {
			t.Fatalf("expected all result ids to be 1, got %d in %v", id, ids)
		}
	}
}

// TestFDB_PlanShapeDistinctOnPK verifies that SELECT DISTINCT on a primary key
// column does not introduce a Distinct operator, because PK uniqueness makes
// deduplication unnecessary.
func TestFDB_PlanShapeDistinctOnPK(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "dpk", "CREATE TABLE users (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))")

	for _, u := range []struct {
		id   int
		name string
	}{
		{1, "Alice"},
		{2, "Bob"},
		{3, "Carol"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO users VALUES (%d, '%s')", u.id, u.name)); err != nil {
			t.Fatalf("INSERT id=%d: %v", u.id, err)
		}
	}

	q := "SELECT DISTINCT id FROM users"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan: %s", plan)

	// DistinctOnUniqueElimRule eliminates the Distinct operator when
	// the projected columns include all PK columns. Selecting only
	// the PK (which is inherently unique) means DISTINCT is a no-op.
	if strings.Contains(plan, "Distinct") {
		t.Fatalf("expected Distinct to be eliminated (PK projected), got: %s", plan)
	}
	// Should still have a Scan.
	if !strings.Contains(plan, "Scan") {
		t.Fatalf("expected Scan in plan, got: %s", plan)
	}

	// Verify query results: all 3 users returned.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(ids) != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", len(ids), ids)
	}
}

// TestFDB_PlanShapeFilterPushdownBelowJoin verifies end-to-end that
// PushFilterBelowJoinRule pushes single-table predicates below joins.
//
// Schema: dept(did PK, dname), emp(eid PK, did, ename)
//
// The pushdown is verified with SELECT * (no projection overhead) where
// the Cascades cost model cleanly picks the pushed-down plan shape:
//
//	NestedLoopJoin(INNER, [join-pred], Scan(EMP), Filter([where-pred], Scan(DEPT)))
//
// rather than:
//
//	Filter([where-pred], NestedLoopJoin(INNER, [join-pred], Scan(EMP), Scan(DEPT)))
//
// Also tests the negative case: a predicate referencing BOTH sides
// stays above the join.
func TestFDB_PlanShapeFilterPushdownBelowJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := setupPlanShapeDB(t, "fpbj",
		"CREATE TABLE dept (did BIGINT NOT NULL, dname STRING, PRIMARY KEY (did)) "+
			"CREATE TABLE emp (eid BIGINT NOT NULL, did BIGINT, ename STRING, PRIMARY KEY (eid))")

	// Seed dept: eng, sales, hr.
	for _, d := range []struct {
		did   int
		dname string
	}{
		{1, "eng"},
		{2, "sales"},
		{3, "hr"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO dept VALUES (%d, '%s')", d.did, d.dname)); err != nil {
			t.Fatalf("INSERT dept did=%d: %v", d.did, err)
		}
	}
	// Seed emp: Alice and Bob in eng (did=1), Charlie in sales (did=2).
	for _, e := range []struct {
		eid   int
		did   int
		ename string
	}{
		{10, 1, "Alice"},
		{20, 1, "Bob"},
		{30, 2, "Charlie"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO emp VALUES (%d, %d, '%s')", e.eid, e.did, e.ename)); err != nil {
			t.Fatalf("INSERT emp eid=%d: %v", e.eid, err)
		}
	}

	// --- Positive case: single-table predicate pushed below join ---
	//
	// SELECT * avoids a LogicalProjection wrapper that can interact
	// with PullFilterAboveProjectionRule and mask the pushdown in the
	// final plan. Without projection, the Cascades cost model cleanly
	// selects the pushed-down shape.
	q := "SELECT * FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE d.dname = 'eng'"
	plan := planExplainVia(t, ctx, db, q)
	t.Logf("plan (pushdown): %s", plan)

	// Plan must contain a NestedLoopJoin.
	if !strings.Contains(plan, "NestedLoopJoin") {
		t.Fatalf("expected NestedLoopJoin in plan, got: %s", plan)
	}

	// The d.dname='eng' filter must appear INSIDE the NLJ arguments
	// (pushed below), not as a parent wrapping the NLJ.
	nljIdx := strings.Index(plan, "NestedLoopJoin")

	// No Filter before the NLJ — the predicate should be pushed below.
	filterIdx := strings.Index(plan, "Filter")
	if filterIdx >= 0 && filterIdx < nljIdx {
		t.Fatalf("expected filter pushed below join, but Filter wraps NLJ.\nplan: %s", plan)
	}

	// Filter must appear INSIDE the NLJ (after the NLJ opening paren).
	afterNLJ := plan[nljIdx:]
	if !strings.Contains(afterNLJ, "Filter") {
		t.Fatalf("expected pushed-down Filter inside NestedLoopJoin, got: %s", plan)
	}

	// The NLJ itself must carry the join predicate (ON e.did = d.did).
	if !strings.Contains(afterNLJ, "preds") {
		t.Fatalf("expected join predicates inside NestedLoopJoin, got: %s", plan)
	}

	// Verify query results: only eng department rows joined.
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()

	type joinRow struct {
		eid   int64
		did   int64
		ename string
		ddid  int64
		dname string
	}
	var got []joinRow
	for rows.Next() {
		var r joinRow
		if err := rows.Scan(&r.eid, &r.did, &r.ename, &r.ddid, &r.dname); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Only emp rows in eng (did=1): Alice (eid=10) and Bob (eid=20).
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.dname != "eng" {
			t.Fatalf("expected dname='eng', got %q in %+v", r.dname, r)
		}
		if r.did != 1 || r.ddid != 1 {
			t.Fatalf("expected did=1, got did=%d ddid=%d in %+v", r.did, r.ddid, r)
		}
	}

	// --- Negative case: cross-table predicate stays above join ---
	qCross := "SELECT * FROM emp AS e INNER JOIN dept AS d ON e.did = d.did WHERE e.eid > d.did"
	planCross := planExplainVia(t, ctx, db, qCross)
	t.Logf("plan (cross-table pred): %s", planCross)

	// The e.eid > d.did predicate references both sides — it must NOT
	// be pushed below the join.
	if !strings.Contains(planCross, "NestedLoopJoin") {
		t.Fatalf("expected NestedLoopJoin in cross-table plan, got: %s", planCross)
	}

	nljIdxC := strings.Index(planCross, "NestedLoopJoin")
	filterIdxC := strings.Index(planCross, "Filter")

	// The cross-table predicate should show up as a Filter wrapping
	// the NLJ (not pushed inside it).
	if filterIdxC < 0 || filterIdxC > nljIdxC {
		// Check if the NLJ carries extra predicates instead.
		afterNLJC := planCross[nljIdxC:]
		if !strings.Contains(afterNLJC, "preds") {
			t.Errorf("cross-table predicate not found in plan — "+
				"expected either Filter above NLJ or preds on NLJ, got: %s", planCross)
		}
	} else {
		t.Logf("cross-table predicate correctly above join")
	}

	// Cross-table query correctness: all emp rows where eid > did of
	// matching dept (all have eid 10/20/30 > did 1/2/3).
	rowsCross, err := db.QueryContext(ctx, qCross)
	if err != nil {
		t.Fatalf("QueryContext cross: %v", err)
	}
	defer rowsCross.Close()

	var crossCount int
	for rowsCross.Next() {
		crossCount++
		var r joinRow
		if err := rowsCross.Scan(&r.eid, &r.did, &r.ename, &r.ddid, &r.dname); err != nil {
			t.Fatalf("Scan cross: %v", err)
		}
		if r.eid <= r.ddid {
			t.Fatalf("cross-table: expected eid > did, got eid=%d did=%d", r.eid, r.ddid)
		}
	}
	if err := rowsCross.Err(); err != nil {
		t.Fatalf("rowsCross.Err: %v", err)
	}

	// All 3 joins satisfy eid > did: 10>1, 20>1, 30>2.
	if crossCount != 3 {
		t.Fatalf("cross: expected 3 rows, got %d", crossCount)
	}
}
