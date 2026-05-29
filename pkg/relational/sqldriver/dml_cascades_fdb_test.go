package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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

// TestFDB_DMLCascades_Update pins UPDATE through Cascades: arithmetic SET
// (RHS resolved to a Value, not text), WHERE-scoped vs all-rows, correct
// RowsAffected, SET to NULL on a nullable column clears it, and the two
// plan-time rejections (NOT NULL violation, unsupported function in SET).
func TestFDB_DMLCascades_Update(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := dmlCascadesDB(t, "upd")
	seedItems(t, db, ctx, 4) // prices 10,20,30,40

	// Arithmetic SET, WHERE-scoped: halve prices > 20 (rows 3,4).
	res, err := db.ExecContext(ctx, "UPDATE Item SET price = price / 2 WHERE price > 20")
	if err != nil {
		t.Fatalf("UPDATE arithmetic: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("UPDATE RowsAffected = %d, want 2", n)
	}
	prices := map[int64]int64{}
	rows, err := db.QueryContext(ctx, "SELECT id, price FROM Item ORDER BY id")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for rows.Next() {
		var id, p int64
		if err := rows.Scan(&id, &p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		prices[id] = p
	}
	rows.Close()
	// 1→10, 2→20 unchanged; 3: 30/2=15, 4: 40/2=20.
	if prices[1] != 10 || prices[2] != 20 || prices[3] != 15 || prices[4] != 20 {
		t.Fatalf("after UPDATE prices = %v, want {1:10,2:20,3:15,4:20}", prices)
	}

	// SET to NULL on a nullable column clears it (price is nullable).
	if _, err := db.ExecContext(ctx, "UPDATE Item SET price = NULL WHERE id = 1"); err != nil {
		t.Fatalf("UPDATE SET NULL nullable: %v", err)
	}
	var price sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT price FROM Item WHERE id = 1").Scan(&price); err != nil {
		t.Fatalf("read null price: %v", err)
	}
	if price.Valid {
		t.Fatalf("price after SET NULL = %v, want NULL", price)
	}

	// SET NULL on a NOT NULL column (id) → NOT NULL violation at plan time.
	if _, err := db.ExecContext(ctx, "UPDATE Item SET id = NULL WHERE id = 2"); err == nil {
		t.Fatal("UPDATE SET id=NULL on NOT NULL column did not error")
	}

	// Unsupported function in SET → rejected.
	if _, err := db.ExecContext(ctx, "UPDATE Item SET name = UPPER(name) WHERE id = 2"); err == nil {
		t.Fatal("UPDATE SET name=UPPER(name) was not rejected")
	}
}

// TestFDB_DMLCascades_InsertSelect pins INSERT … SELECT through Cascades:
// positional column mapping (the SELECT's i-th output feeds the target's
// i-th column regardless of name), computed expressions, and a WHERE
// filter — the row must be built from the projection, not the source record.
func TestFDB_DMLCascades_InsertSelect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/dmlc_inssel"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	// src(id, price, qty); dst(id, total) — distinct shapes to exercise
	// positional mapping (SELECT id, price*qty → dst.id, dst.total).
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE dmlc_inssel_tmpl"+
		" CREATE TABLE src (id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE dst (id BIGINT, total BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE dmlc_inssel_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1, 10, 5), (2, 20, 3), (3, 30, 7)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Positional INSERT … SELECT: dst(id, total) ← (id, price*qty) for
	// rows with price >= 20. The 2nd SELECT output (price*qty) maps to
	// dst.total by position, not name.
	res, err := db.ExecContext(ctx,
		"INSERT INTO dst SELECT id, price * qty FROM src WHERE price >= 20")
	if err != nil {
		t.Fatalf("INSERT...SELECT: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("INSERT...SELECT RowsAffected = %d, want 2", n)
	}

	// Explicit column list with SELECT is rejected (Java parity).
	if _, err := db.ExecContext(ctx,
		"INSERT INTO dst (id, total) SELECT id, price FROM src WHERE id = 1"); err == nil {
		t.Fatal("INSERT (cols) SELECT was not rejected")
	}

	// row 2: total=20*3=60; row 3: total=30*7=210.
	got := map[int64]int64{}
	rows, err := db.QueryContext(ctx, "SELECT id, total FROM dst ORDER BY id")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for rows.Next() {
		var id, total int64
		if err := rows.Scan(&id, &total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = total
	}
	rows.Close()
	if got[2] != 60 || got[3] != 210 {
		t.Fatalf("INSERT...SELECT persisted = %v, want {2:60, 3:210}", got)
	}

	// Same-table INSERT … SELECT must not re-scan its own inserts
	// (Halloween): src has 3 rows; insert id+1000 for price >= 20 → exactly
	// 2 new rows, not a cascade.
	res2, err := db.ExecContext(ctx,
		"INSERT INTO src SELECT id + 1000, price, qty FROM src WHERE price >= 20")
	if err != nil {
		t.Fatalf("same-table INSERT...SELECT: %v", err)
	}
	if n, _ := res2.RowsAffected(); n != 2 {
		t.Fatalf("same-table INSERT...SELECT RowsAffected = %d, want 2 (Halloween)", n)
	}
	var cnt int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM src").Scan(&cnt); err != nil {
		t.Fatalf("count src: %v", err)
	}
	if cnt != 5 {
		t.Fatalf("src count after same-table insert = %d, want 5", cnt)
	}
}

// TestFDB_DMLCascades_UniqueIndexViolation pins that a secondary UNIQUE
// index violation (distinct from a duplicate primary key) surfaces
// SQLSTATE 23505 through the Cascades INSERT path — translateFDBError must
// map RecordIndexUniquenessViolationError, which the deleted naive path
// did via wrapSaveRecordError.
func TestFDB_DMLCascades_UniqueIndexViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/dmlc_uniq"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE dmlc_uniq_tmpl"+
		" CREATE TABLE Emp (id BIGINT, email STRING, PRIMARY KEY (id))"+
		" CREATE UNIQUE INDEX by_email ON Emp (email)"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE dmlc_uniq_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx, "INSERT INTO Emp VALUES (1, 'a@x.com')"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Different PK, same unique email → secondary UNIQUE index violation.
	_, err = db.ExecContext(ctx, "INSERT INTO Emp VALUES (2, 'a@x.com')")
	if err == nil {
		t.Fatal("duplicate unique-index value did not error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *api.Error: %T %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("error code = %s, want %s (23505)", apiErr.Code, api.ErrCodeUniqueConstraintViolation)
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
