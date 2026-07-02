package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// queryIDs runs q and returns the single BIGINT column of every row, sorted
// ascending. The callers assert on a row SET (membership/count), not the
// executor's emission order — none of these queries carry an ORDER BY — so the
// helper sorts to keep the assertions order-insensitive (a valid plan/ordering
// change must not flake them).
func queryIDs(t *testing.T, db *sql.DB, ctx context.Context, q string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Self-comparison regression: a self-comparison between two columns of the SAME scanned
// source must NOT SARG either column into a scan range. The commuted orientation
// of `b = a` (a indexed, b not) would bind the indexed column `a` and seek the
// circular range `a = <this row's b>`, which returns 0 rows. bindOriented
// comparison's comparand-side guard (comparandIndependentOfSource) rejects a
// comparand that is a per-row column of the matched source, so the predicate stays
// a residual filter and both rows survive. The as-written `a = b` is the
// pre-existing sibling with the identical root cause; both must return 2 rows.
func TestFDB_SelfComparisonNotSargedToCircularRange(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_selfcmp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_selfcmp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE selfcmp_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_selfcmp/s WITH TEMPLATE selfcmp_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_selfcmp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// rows 1,2: a==b (must match); row 3: a!=b (must not).
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 10, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (2, 20, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (3, 30, 99)")

	for _, q := range []string{
		"SELECT id FROM t WHERE b = a",
		"SELECT id FROM t WHERE a = b",
	} {
		got := queryIDs(t, db, ctx, q)
		if len(got) != 2 {
			t.Errorf("%q: got %d rows %v, want 2 (ids 1,2)", q, len(got), got)
		}
		// The plan must keep the self-comparison as a residual filter, never an
		// index range scan over T_A (which would seek the circular a=<b> range).
		plan := mwjoExplainer(t, db, ctx)(q)
		if strings.Contains(strings.ToUpper(plan), "INDEXSCAN(T_A") {
			t.Errorf("%q SARG'd self-comparison into circular index range: %s", q, plan)
		}
	}

	// Control: a genuine constant comparand MUST still SARG the index.
	planConst := mwjoExplainer(t, db, ctx)("SELECT id FROM t WHERE a = 20")
	if !strings.Contains(strings.ToUpper(planConst), "INDEXSCAN(T_A") {
		t.Errorf("constant equality lost its index SARG (regression): %s", planConst)
	}
	if got := queryIDs(t, db, ctx, "SELECT id FROM t WHERE a = 20"); len(got) != 1 || got[0] != 2 {
		t.Errorf("a=20: got %v, want [2]", got)
	}
}

// Probe-fed-residual regression: a 2-table join with TWO cross-correlation predicates, one
// of which is a primary-key probe (t.fk = u.id) and one a non-sargable secondary
// filter (t.a = u.c, u.c unindexed), must drive the SMALL/probe-able side and
// PK-probe the other — FlatMap(Scan(T), probe(U)) — not full-scan T per U row
// (O(N×M)). The drive-T inner U-leg SARGs u.id=t.fk into a PK probe and leaves
// u.c=t.a as an outer-correlated residual; compensationSafeForYield must allow
// that residual (its probe already feeds the T correlation) instead of rejecting
// it and forcing the U-driver full-scan-T plan.
func TestFDB_CompositeJoinDrivesProbeSide(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_compjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_compjoin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE compjoin_tmpl "+
			"CREATE TABLE t (id BIGINT NOT NULL, fk BIGINT, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE u (id BIGINT NOT NULL, c BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_compjoin/s WITH TEMPLATE compjoin_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_compjoin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO u VALUES (1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO u VALUES (2, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO u VALUES (3, 300)")
	// t.id 10: fk=1→u.id=1, a=100==u.c=100  MATCH
	// t.id 11: fk=2→u.id=2, a=999!=u.c=200  no
	// t.id 12: fk=3→u.id=3, a=300==u.c=300  MATCH
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (10, 1, 100)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (11, 2, 999)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (12, 3, 300)")

	const q = "SELECT t.id FROM t, u WHERE t.fk = u.id AND t.a = u.c"
	got := queryIDs(t, db, ctx, q)
	if len(got) != 2 || got[0] != 10 || got[1] != 12 {
		t.Errorf("%q: got %v, want [10 12]", q, got)
	}

	plan := mwjoExplainer(t, db, ctx)(q)
	t.Logf("PLAN: %s", plan)
	up := strings.ToUpper(plan)
	// The bad plan drives U and re-scans all of T per U row.
	if strings.Contains(up, "OUTER=SCAN(U)") {
		t.Errorf("REGRESSION: composite join drives off Scan(U), full-scanning T per row (O(N*M)): %s", plan)
	}
	// The good plan PK-probes U from the T driver: a restricted U scan appears.
	if !strings.Contains(up, "SCAN(U, [=]") {
		t.Errorf("expected a correlated PK probe Scan(U,[=...]) inner, got: %s", plan)
	}
}
