package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// dmlCascadesDB creates an isolated db+schema with an Item table and
// returns a schema-scoped *sql.DB. DML (INSERT VALUES, DELETE) routes
// through the Cascades path.
func dmlCascadesDB(t *testing.T, tag string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/dmlc_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := "dmlc_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE Item (id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE Flag (name STRING NOT NULL, PRIMARY KEY (name))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func itemIDs(t *testing.T, db *sql.DB, ctx context.Context) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT id FROM Item ORDER BY id")
	if err != nil {
		t.Fatalf("scan items: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func seedItems(t *testing.T, db *sql.DB, ctx context.Context, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO Item (id, price) VALUES (?, ?)", int64(i), int64(i*10)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// TestFDB_DMLCascades_DeleteRowsAffected pins that a Cascades DELETE
// reports the exact number of rows removed (the countAll drain), incl.
// the zero-match case.
func TestFDB_DMLCascades_DeleteRowsAffected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := dmlCascadesDB(t, "del_ra")
	seedItems(t, db, ctx, 5)

	res, err := db.ExecContext(ctx, "DELETE FROM Item WHERE price >= 30")
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 3 {
		t.Fatalf("DELETE RowsAffected = %d, want 3", n)
	}
	if got := itemIDs(t, db, ctx); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("remaining = %v, want [1 2]", got)
	}

	// Zero-match DELETE → RowsAffected 0, nothing removed.
	res, err = db.ExecContext(ctx, "DELETE FROM Item WHERE price = 99999")
	if err != nil {
		t.Fatalf("DELETE zero: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("zero-match RowsAffected = %d, want 0", n)
	}
	if got := itemIDs(t, db, ctx); len(got) != 2 {
		t.Fatalf("after zero-match remaining = %v, want 2 rows", got)
	}
}

// TestFDB_DMLCascades_DeleteNonCorrelatedNotExists pins non-correlated
// (NOT) EXISTS DELETE — the case that exposed the FirstOrDefault semi-join
// bug. Empty subquery: NOT EXISTS deletes all; EXISTS deletes none.
func TestFDB_DMLCascades_DeleteNonCorrelatedNotExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := dmlCascadesDB(t, "del_nce")
	seedItems(t, db, ctx, 3)
	if _, err := db.ExecContext(ctx, "INSERT INTO Flag (name) VALUES ('present')"); err != nil {
		t.Fatalf("seed flag: %v", err)
	}

	// NOT EXISTS over an empty subquery → true for all → delete everything.
	res, err := db.ExecContext(ctx, "DELETE FROM Item WHERE NOT EXISTS (SELECT name FROM Flag WHERE name = 'absent')")
	if err != nil {
		t.Fatalf("DELETE NOT EXISTS(empty): %v", err)
	}
	if n, _ := res.RowsAffected(); n != 3 {
		t.Fatalf("NOT EXISTS(empty) deleted %d, want 3", n)
	}
	if got := itemIDs(t, db, ctx); len(got) != 0 {
		t.Fatalf("after NOT EXISTS delete remaining = %v, want []", got)
	}

	// Re-seed; EXISTS over a non-empty subquery → true for all → delete all.
	seedItems(t, db, ctx, 2)
	res, err = db.ExecContext(ctx, "DELETE FROM Item WHERE EXISTS (SELECT name FROM Flag WHERE name = 'present')")
	if err != nil {
		t.Fatalf("DELETE EXISTS(non-empty): %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("EXISTS(non-empty) deleted %d, want 2", n)
	}
}

// TestFDB_DMLCascades_ExplicitTxRollback pins Gap B (runInTx): DML inside
// an explicit transaction joins that transaction and is discarded on
// ROLLBACK / persisted on COMMIT, for both INSERT VALUES and DELETE.
func TestFDB_DMLCascades_ExplicitTxRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := dmlCascadesDB(t, "tx")
	seedItems(t, db, ctx, 2) // ids 1,2

	// INSERT inside a tx, then ROLLBACK → row must not persist.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO Item (id, price) VALUES (99, 990)"); err != nil {
		t.Fatalf("tx INSERT: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := itemIDs(t, db, ctx); len(got) != 2 {
		t.Fatalf("after rollback got %v, want [1 2] (insert discarded)", got)
	}

	// DELETE inside a tx, then COMMIT → removal must persist.
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx2: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, "DELETE FROM Item WHERE id = 1"); err != nil {
		t.Fatalf("tx DELETE: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := itemIDs(t, db, ctx); len(got) != 1 || got[0] != 2 {
		t.Fatalf("after commit got %v, want [2]", got)
	}
}

// TestFDB_DMLCascades_ExplainPlanShapes pins that DELETE and INSERT VALUES
// plan through Cascades with the expected physical plan shapes (Delete over
// a scan/filter; Insert over Explode), proving the Cascades path fired
// rather than a naive fallback.
func TestFDB_DMLCascades_ExplainPlanShapes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := dmlCascadesDB(t, "explain")
	logger := &syncCaptureLogger{}
	conn := installLogger(t, db, logger)

	if _, err := conn.ExecContext(ctx, "INSERT INTO Item (id, price) VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM Item WHERE price = 10"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	events := logger.snapshot()
	if len(events) < 2 {
		t.Fatalf("want >=2 planning events, got %d", len(events))
	}
	ins, del := events[0].PlanExplain, events[1].PlanExplain
	if !strings.Contains(ins, "Insert(") || !strings.Contains(strings.ToLower(ins), "explode") {
		t.Fatalf("INSERT plan %q is not Insert-over-Explode", ins)
	}
	if !strings.Contains(del, "Delete(") {
		t.Fatalf("DELETE plan %q is not a Delete plan", del)
	}
	for _, ev := range events {
		if ev.Cache != embedded.PlanCacheSkip {
			// DML is never cached.
			t.Errorf("DML cache event = %v, want skip", ev.Cache)
		}
	}
}
