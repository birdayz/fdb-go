package sqldriver_test

// Pins GROUP BY grouping validation: a SELECT-list column that is neither a
// grouping key nor inside an aggregate is rejected with 42803 ("must appear in the
// GROUP BY clause or be used in an aggregate function"), including `SELECT *` with
// GROUP BY and a stray column alongside an otherwise-valid aggregate. Valid shapes
// (key only; key + aggregate) succeed.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_GroupByValidationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gv")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gv")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gv CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gv/s WITH TEMPLATE gv")
	dsn := fmt.Sprintf("fdbsql:///testdb_gv?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, v) VALUES (1,7,10),(2,7,20),(3,8,30)")

	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "42803") {
				t.Errorf("%s error = %v, want 42803 (grouping error)", name, err)
			}
		})
	}
	rejected("nongrouped_column", "SELECT a, v FROM t GROUP BY a")
	rejected("star_with_groupby", "SELECT * FROM t GROUP BY a")
	rejected("stray_column_beside_aggregate", "SELECT a, SUM(v), id FROM t GROUP BY a")

	ok := func(name, q string, wantRows int) {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				n++
			}
			if n != wantRows {
				t.Errorf("%s returned %d rows, want %d", name, n, wantRows)
			}
		})
	}
	ok("key_only", "SELECT a FROM t GROUP BY a", 2) // groups a=7, a=8
	ok("key_and_aggregate", "SELECT a, SUM(v) FROM t GROUP BY a", 2)
	ok("aggregate_only", "SELECT SUM(v) FROM t GROUP BY a", 2) // agg without selecting the key is fine
}
