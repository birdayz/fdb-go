package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A join comparing two rows' __ROW_VERSION reads the outer row's version as
// its serialized bytes; a version index scan probing with it must bind the
// tuple versionstamp the index stores (versions-tests.yamsql, versions-queries).
func TestFDB_VersionComparisonJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_vcj")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_vcj")
	testkit.MustExecCtx(t, setup, ctx, `CREATE SCHEMA TEMPLATE vcj_tpl
		CREATE TABLE t2(id BIGINT, col1 BIGINT, col2 STRING, PRIMARY KEY (id))
		CREATE INDEX t2_col2 AS SELECT col2 FROM t2
		CREATE TABLE t3(id BIGINT, col1 STRING, col2 BIGINT, PRIMARY KEY (id))
		CREATE INDEX t3_version_with_col1 AS SELECT "__ROW_VERSION", col1 FROM t3 ORDER BY "__ROW_VERSION"
		WITH OPTIONS(store_row_versions=true)`)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_vcj/s WITH TEMPLATE vcj_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_VCJ?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Each statement commits at its own version, in this order.
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t2 VALUES (1, 1, 'b')")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t3 VALUES (10, 'b', 1)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t2 VALUES (2, 2, 'b')")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t3 VALUES (11, 'b', 2), (12, 'a', 3)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t2 VALUES (3, 3, 'b')")

	rows, err := db.QueryContext(ctx, `SELECT t2.id, t3.id FROM t2, t3
		WHERE t2.col2 = 'b' AND t3.col1 = 'b' AND t2."__ROW_VERSION" > t3."__ROW_VERSION"`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[[2]int64]bool{}
	for rows.Next() {
		var pair [2]int64
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			t.Fatal(err)
		}
		got[pair] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := map[[2]int64]bool{{2, 10}: true, {3, 10}: true, {3, 11}: true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pairs = %v, want %v", got, want)
	}
}
