package sqltest

// Regression for the pre-existing materialized-NLJ bug: a compound JOIN ON clause
// whose conjunct is a subquery (IN-subquery or scalar-subquery) was silently
// DROPPED at translation — the ON resolver installs no SubqueryPlanner, so
// WalkPredicate declined the shape, a permissive `continue` dropped the entire ON
// predicate, and the join degraded to a CROSS PRODUCT (silent wrong rows,
// TODO.md "Known gaps").
//
// Go (like Java) does not support IN-subqueries or correlated scalar subqueries
// anywhere. The fix is fail-CLOSED: reject these ON shapes cleanly with
// ErrCodeUnsupportedQuery instead of dropping them. EXISTS-in-ON IS supported
// (Java parity) — pinned separately in exists_in_on_fdb_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_SubqueryInOn_RejectedCleanly(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_subq_on")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_subq_on")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE subq_on "+
			"CREATE TABLE a (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, a_id BIGINT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, b_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_subq_on/s WITH TEMPLATE subq_on")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_SUBQ_ON?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	testkit.MustExecCtx(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (20, 2)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO c (id, a_id, w) VALUES (50, 1, 999), (51, 2, 888)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO d (id, b_id) VALUES (1, 999), (2, 888)")

	// --- The bug: subquery in ON must be rejected cleanly, never a cross product.

	t.Run("left_in_subquery_on", func(t *testing.T) {
		testkit.AssertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("left_scalar_subquery_on", func(t *testing.T) {
		testkit.AssertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w > (SELECT MAX(d.b_id) FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("inner_in_subquery_on", func(t *testing.T) {
		testkit.AssertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("sole_in_subquery_on", func(t *testing.T) {
		testkit.AssertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id)")
	})

	// --- Controls: non-subquery compound ON clauses still work correctly.

	t.Run("ctrl_constant_conjunct", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w = 12345")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := testkit.ScanRowStrings(t, rows)
		want := []string{"1|NULL", "2|NULL"}
		if !testkit.EqualStrings(got, want) {
			t.Errorf("constant-conjunct LEFT JOIN rows = %v, want %v", got, want)
		}
	})
	t.Run("ctrl_single_eq", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := testkit.ScanRowStrings(t, rows)
		want := []string{"1|50", "2|51"}
		if !testkit.EqualStrings(got, want) {
			t.Errorf("single-eq LEFT JOIN rows = %v, want %v", got, want)
		}
	})
	// IN with a value LIST (not a subquery) must still work — the detector must
	// not over-reject `IN (a, b, c)`.
	t.Run("ctrl_in_value_list", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w IN (999, 888)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := testkit.ScanRowStrings(t, rows)
		want := []string{"1|50", "2|51"}
		if !testkit.EqualStrings(got, want) {
			t.Errorf("IN-value-list LEFT JOIN rows = %v, want %v", got, want)
		}
	})
}
