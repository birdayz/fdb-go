package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_DMLCascades_ParamsIndexAndPK pins three DML dimensions that are
// easy to regress with CI green because little else exercises them through
// the Cascades path: parameterized DML, secondary-index consistency after
// UPDATE/DELETE, and UPDATE of a primary-key column (rejected with a typed
// error, matching the deleted naive path).
func TestFDB_DMLCascades_ParamsIndexAndPK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/dml_probe"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE dml_probe_tmpl"+
		" CREATE TABLE T (id BIGINT, email STRING, age BIGINT, PRIMARY KEY (id))"+
		" CREATE INDEX by_age ON T (age)"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE dml_probe_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// (1) Parameterized INSERT / UPDATE — the common real-world shape.
	if _, err := db.ExecContext(ctx, "INSERT INTO T VALUES (?, ?, ?)", int64(1), "a@x.com", int64(30)); err != nil {
		t.Fatalf("param INSERT: %v", err)
	}
	var email string
	if err := db.QueryRowContext(ctx, "SELECT email FROM T WHERE id = 1").Scan(&email); err != nil || email != "a@x.com" {
		t.Fatalf("param INSERT readback: email=%q err=%v", email, err)
	}
	res, err := db.ExecContext(ctx, "UPDATE T SET age = ? WHERE id = ?", int64(31), int64(1))
	if err != nil {
		t.Fatalf("param UPDATE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("param UPDATE affected=%d, want 1", n)
	}

	// (2) Secondary-index consistency: querying via by_age after the UPDATE
	// must reflect the new value (the index is maintained by the save).
	var cnt30, cnt31 int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE age = 30").Scan(&cnt30); err != nil {
		t.Fatalf("count age=30: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE age = 31").Scan(&cnt31); err != nil {
		t.Fatalf("count age=31: %v", err)
	}
	if cnt30 != 0 || cnt31 != 1 {
		t.Fatalf("index inconsistent after UPDATE: age=30 count=%d (want 0), age=31 count=%d (want 1)", cnt30, cnt31)
	}

	// (3) UPDATE of a PK column fails at execution with a typed *api.Error.
	// This is Java's exact behavior: no plan-time PK guard — the in-place
	// save targets the new key, throws RecordDoesNotExistException, which
	// Java's ExceptionUtil leaves unmapped → ErrorCode.UNKNOWN (Go:
	// ErrCodeUnknown). Pin the code, not just the type — a future
	// translateFDBError change must be caught (this class of bug has
	// already shipped CI-green once).
	_, errPK := db.ExecContext(ctx, "UPDATE T SET id = 99 WHERE id = 1")
	if errPK == nil {
		t.Fatal("UPDATE of a PK column was not rejected")
	}
	var apiErr *api.Error
	if !errors.As(errPK, &apiErr) {
		t.Fatalf("UPDATE-PK error is not *api.Error: %T %v", errPK, errPK)
	}
	if apiErr.Code != api.ErrCodeUnknown {
		t.Fatalf("UPDATE-PK error code = %s, want %s (Java ErrorCode.UNKNOWN)", apiErr.Code, api.ErrCodeUnknown)
	}
	var c1, c99 int64
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE id = 1").Scan(&c1)
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE id = 99").Scan(&c99)
	if c1 != 1 || c99 != 0 {
		t.Fatalf("UPDATE-PK left inconsistent state: id=1 count=%d (want 1), id=99 count=%d (want 0)", c1, c99)
	}

	// (4) DELETE by an indexed column, then confirm the row is gone.
	if _, err := db.ExecContext(ctx, "DELETE FROM T WHERE age = 31"); err != nil {
		t.Fatalf("DELETE by indexed col: %v", err)
	}
	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T").Scan(&total); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if total != 0 {
		t.Fatalf("after DELETE age=31: total rows=%d, want 0", total)
	}
}
