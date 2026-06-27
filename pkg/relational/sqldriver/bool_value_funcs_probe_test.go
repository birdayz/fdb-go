package sqldriver_test

// Probes for boolean expressions in projection (another potential predicateValue
// site) and value functions (COALESCE / NULLIF) used as cross-table comparison
// operands over a join — the same correlation-hiding class as the CASE fix.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_BoolValueFuncsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_boolvf")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_boolvf")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE boolvf "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_boolvf/s WITH TEMPLATE boolvf")
	dsn := fmt.Sprintf("fdbsql:///testdb_boolvf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 5), (2, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, y) VALUES (50, 5), (51, 10)")

	pairs := func(q string) []string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return []string{"ERR:" + err.Error()}
		}
		return siScanRows(t, rows)
	}

	// COALESCE as a cross-table comparison operand: COALESCE(a.x, 0) = c.y.
	t.Run("coalesce_cross_table", func(t *testing.T) {
		got := pairs("SELECT a.id, c.id FROM a JOIN c ON COALESCE(a.x, 0) = c.y")
		want := []string{"1|50", "2|51"} // a1.x=5=c50.y; a2.x=10=c51.y
		if !eqStrSlices(got, want) {
			t.Errorf("COALESCE cross-table = %v, want %v", got, want)
		}
	})

	// NULLIF cross-table: NULLIF(a.x, 5) = c.y. a1→NULLIF(5,5)=NULL→NULL=c.y none;
	// a2→NULLIF(10,5)=10→10=c51.y(10) → (2,51).
	t.Run("nullif_cross_table", func(t *testing.T) {
		got := pairs("SELECT a.id, c.id FROM a JOIN c ON NULLIF(a.x, 5) = c.y")
		want := []string{"2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("NULLIF cross-table = %v, want %v", got, want)
		}
	})

	// arithmetic cross-table operand: a.x + 0 = c.y (control, must work).
	t.Run("arith_cross_table", func(t *testing.T) {
		got := pairs("SELECT a.id, c.id FROM a JOIN c ON a.x + 0 = c.y")
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("arith cross-table = %v, want %v", got, want)
		}
	})
}
