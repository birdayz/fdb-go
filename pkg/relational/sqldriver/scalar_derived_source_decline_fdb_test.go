package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_ScalarDerivedSourceDeclines pins the correlated-SCALAR-subquery twin of
// the derived-inner correlated-EXISTS fix. A correlated scalar subquery
// (`SELECT …, (SELECT … WHERE inner.k = outer.k) FROM …`) whose inner FROM is a
// DERIVED TABLE routes through buildCorrelatedScalar. Unlike the EXISTS fast path,
// buildCorrelatedScalar builds its inner SCOPE first (analyzer.ResolveTable /
// addCorrelatedJoinScopeSource) BEFORE the operator tree, and a derived source is
// not a catalog table nor WITH-registered — so ResolveTable("d") misses and the
// query DECLINES LOUDLY (0A000) rather than degrading to the empty bare scan.
//
// Correct-or-conservative: the derived scalar shape is a reach gap, NOT a
// silent-wrong. This test guards the loud decline so a future change to the scalar
// path can never regress it into the same silent-wrong the EXISTS path had (a bare
// NewScan("d") over a non-existent table → the scalar reads an empty relation and
// answers a wrong / silently-NULL value). Both the derived-PRIMARY and the
// derived-LEG positions are pinned.
func TestFDB_ScalarDerivedSourceDeclines(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_scalar_derived_decline"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sdd_tmpl "+
		"CREATE TABLE ord (order_id BIGINT NOT NULL, cust_id BIGINT, PRIMARY KEY (order_id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE sdd_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO ord VALUES (1, 10), (2, 20)")

	mustDecline := func(t *testing.T, q string) {
		t.Helper()
		rows, qerr := db.QueryContext(ctx, q)
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("expected a LOUD decline (0A000) for a derived source in a correlated scalar subquery — never a silent bare scan\n  sql: %s", q)
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUnsupportedOperation)
	}

	// Derived PRIMARY source of the scalar subquery.
	t.Run("derived_primary", func(t *testing.T) {
		mustDecline(t, "SELECT o.order_id, (SELECT d.cust_id FROM "+
			"(SELECT order_id, cust_id FROM ord) AS d WHERE d.order_id = o.order_id) FROM ord AS o")
	})
	// Real primary + derived comma LEG of the scalar subquery.
	t.Run("derived_leg", func(t *testing.T) {
		mustDecline(t, "SELECT o.order_id, (SELECT a.cust_id FROM ord a, "+
			"(SELECT order_id FROM ord) AS d WHERE a.order_id = o.order_id) FROM ord AS o")
	})
}
