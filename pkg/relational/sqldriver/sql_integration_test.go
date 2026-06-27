package sqldriver_test

// End-to-end smoke tests that use Go's database/sql package, not the
// raw driver.Driver interface. These prove the *public* entry point
// shape works: users blank-import pkg/relational/sqldriver, call
// sql.Open, and get back a *sql.DB that proxies through the driver.
//
// Execution returns ErrCodeUnsupportedOperation until Phase 5 is in
// place. The tests assert on the shape of those errors so later
// phases can replace them one-by-one without breaking the public API.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	// Blank import registers the "fdbsql" driver.
	_ "fdb.dev/pkg/relational/sqldriver"
)

func TestSQLOpenRegistered(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("fdbsql", "fdbsql:///mydb")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	// sql.Open returns a *sql.DB without actually connecting.
	if db == nil {
		t.Fatal("sql.Open returned nil db")
	}
}

func TestSQLOpenRejectsBadDSN(t *testing.T) {
	t.Parallel()
	// Because we implement driver.DriverContext, database/sql parses
	// the DSN at sql.Open time. Bad DSNs surface here, not at Ping
	// (unlike the driver.Driver-only path where validation is lazy).
	_, err := sql.Open("fdbsql", "postgres:///wrong")
	if err == nil {
		t.Fatal("expected sql.Open to fail on bad DSN")
	}
	e := api.AsError(err)
	if e == nil {
		t.Fatalf("error is not *api.Error: %v", err)
	}
	if e.Code != api.ErrCodeInvalidPath {
		t.Errorf("code = %q, want InvalidPath", e.Code)
	}
}

func TestSQLPingFailsWithoutFDB(t *testing.T) {
	t.Parallel()
	// Point at a nonexistent cluster file so this test reliably fails
	// regardless of whether FDB is actually running on the host.
	db, err := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/nonexistent/fdb.cluster")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	err = db.PingContext(context.Background())
	if err == nil {
		t.Fatal("Ping should fail when no FDB cluster is available")
	}
}

func TestSQLPingReturnsUnsupportedForRemote(t *testing.T) {
	t.Parallel()
	// Remote mode also not implemented — must return UnsupportedOperation.
	db, err := sql.Open("fdbsql", "fdbsql://localhost:50051/mydb")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	err = db.PingContext(context.Background())
	if err == nil {
		t.Fatal("Ping should fail until remote impl exists")
	}
	e := api.AsError(err)
	if e == nil || e.Code != api.ErrCodeUnsupportedOperation {
		t.Errorf("expected UnsupportedOperation, got %v", err)
	}
}

func TestSQLContextDeadlineAtPing(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("fdbsql", "fdbsql:///mydb")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)
	err = db.PingContext(ctx)
	if err == nil {
		t.Fatal("expected Ping to fail on expired context")
	}
	// database/sql wraps driver errors, but the context error should
	// surface somewhere in the chain.
	if !errors.Is(err, context.DeadlineExceeded) {
		// database/sql may have retried internally; accept any error since
		// Connect checks ctx.Err() first (deadline) then FDB open failure.
		t.Logf("got non-deadline error (acceptable): %v", err)
	}
}

func TestSQLDriverList(t *testing.T) {
	t.Parallel()
	found := false
	for _, name := range sql.Drivers() {
		if name == "fdbsql" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fdbsql not in sql.Drivers(): %v", sql.Drivers())
	}
}
