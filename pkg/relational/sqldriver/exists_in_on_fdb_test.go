package sqldriver_test

// RFC-154 §5 — EXISTS in a JOIN ON clause (Java parity).
//
// For an INNER join, `ON cond AND EXISTS(s)` is equivalent to `ON cond WHERE
// EXISTS(s)` (no null-extension): it lowers to a 2-ForEach + Existential
// SelectExpression that the NLJ rule's implementJoinWithExistential path turns
// into a semi-join. OUTER EXISTS-in-ON is deferred (RFC-154 §5.2b) and rejected
// fail-closed so it never returns wrong rows.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ExistsInOn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_on")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_on")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_on "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_on/s WITH TEMPLATE exists_on")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_on?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a={1,2}; c matches each a; d has only id=1, so EXISTS(d.id=a.id) is true
	// for a=1, false for a=2.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (20, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (1)")

	// INNER join, correlated EXISTS in ON: a=2's match is filtered out
	// (EXISTS(d.id=2) is false), a=1 survives.
	t.Run("inner_exists_in_on", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|50"}
		if !eqStrSlices(got, want) {
			t.Errorf("INNER EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// INNER join, NOT EXISTS in ON: complementary — a=1 filtered, a=2 survives.
	t.Run("inner_not_exists_in_on", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND NOT EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("INNER NOT-EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// INNER join, EXISTS as the SOLE ON conjunct (no equi-join): every (a,c)
	// pair where EXISTS(d.id=a.id) holds. Only a=1 qualifies → a=1 × {c50,c51}.
	t.Run("inner_sole_exists_in_on", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id, c.id FROM a JOIN c ON EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|50", "1|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("INNER sole-EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// OUTER EXISTS-in-ON is deferred (RFC-154 §5.2b) — must reject cleanly,
	// never silently null-extend wrongly.
	t.Run("left_exists_in_on_rejected", func(t *testing.T) {
		assertUnsupported(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
				"LEFT JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
	})
}
