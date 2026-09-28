package sqldriver_test

// Statement OPTIONS and the plan-cache key text, as Java 4.14.2.0 has them:
// OPTIONS is statement-level only (NOCACHE, LOG QUERY, DRY RUN, PLAN RIGHT
// DEEP, ISOLATION LEVEL SNAPSHOT), and the cache key is rendered from tokens,
// so comments never split an entry and literals are never case-folded.

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_StatementOptions_PlanCache(t *testing.T) {
	t.Parallel()
	_, db := setupCascadesTestDB(t)
	ctx := context.Background()
	logger := &syncCaptureLogger{}
	conn := installLogger(t, db, logger)

	run := func(q string) []byte {
		t.Helper()
		var out []byte
		if err := conn.QueryRowContext(ctx, q).Scan(&out); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}
	lastCache := func() embedded.PlanCacheEvent {
		events := logger.snapshot()
		return events[len(events)-1].Cache
	}

	// NOCACHE neither reads nor populates the cache.
	for i := 0; i < 2; i++ {
		run("SELECT B64'YWJj' FROM Item WHERE item_id = 1 OPTIONS (NOCACHE)")
		if c := lastCache(); c != embedded.PlanCacheSkip {
			t.Fatalf("NOCACHE run %d: cache %v, want skip", i, c)
		}
	}
	if got := run("SELECT B64'YWJj' FROM Item WHERE item_id = 1"); string(got) != "abc" {
		t.Fatalf("B64'YWJj' = %q", got)
	}
	if c := lastCache(); c != embedded.PlanCacheMiss {
		t.Fatalf("first plain run: cache %v, want miss (NOCACHE populated it)", c)
	}
	// A comment and keyword case do not split the entry.
	run("select /* note */ B64'YWJj' FROM Item -- trailing\n WHERE item_id = 1")
	if c := lastCache(); c != embedded.PlanCacheHit {
		t.Fatalf("commented spelling: cache %v, want hit", c)
	}
	// A literal differing only in case is another value and another entry.
	if got := run("SELECT B64'ywjj' FROM Item WHERE item_id = 1"); string(got) == "abc" {
		t.Fatalf("B64'ywjj' returned the cached B64'YWJj' value %q", got)
	}
	if c := lastCache(); c != embedded.PlanCacheMiss {
		t.Fatalf("B64'ywjj': cache %v, want miss", c)
	}
	// PLAN RIGHT DEEP plans under another configuration and so another entry.
	run("SELECT B64'YWJj' FROM Item WHERE item_id = 1 OPTIONS (PLAN RIGHT DEEP)")
	if c := lastCache(); c != embedded.PlanCacheMiss {
		t.Fatalf("PLAN RIGHT DEEP: cache %v, want miss", c)
	}
	// LOG QUERY and SNAPSHOT parse and share the plain entry.
	run("SELECT B64'YWJj' FROM Item WHERE item_id = 1 OPTIONS (LOG QUERY, ISOLATION LEVEL SNAPSHOT)")
	if c := lastCache(); c != embedded.PlanCacheHit {
		t.Fatalf("LOG QUERY, SNAPSHOT: cache %v, want hit", c)
	}
}

func TestFDB_StatementOptions_Placement(t *testing.T) {
	t.Parallel()
	_, db := setupCascadesTestDB(t)
	ctx := context.Background()
	for _, q := range []string{
		"SELECT item_id FROM (SELECT item_id FROM Item OPTIONS (NOCACHE)) AS X",
		"SELECT item_id FROM Item OPTIONS (EF_SEARCH 10)",
	} {
		var apiErr *api.Error
		if _, err := db.QueryContext(ctx, q); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
			t.Errorf("%s: want 42601, got %v", q, err)
		}
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN SELECT item_id FROM Item WHERE item_id = 1 OPTIONS (ISOLATION LEVEL SNAPSHOT)")
	if err != nil {
		t.Fatalf("EXPLAIN with OPTIONS: %v", err)
	}
	rows.Close()
}

// TestFDB_StatementOptions_SnapshotRead: a read under ISOLATION LEVEL SNAPSHOT
// takes no read-conflict range, so a concurrent committed write to the rows it
// read does not abort the reader's commit; the same read without the option
// does.
func TestFDB_StatementOptions_SnapshotRead(t *testing.T) {
	t.Parallel()
	_, db := setupCascadesTestDB(t)
	ctx := context.Background()

	attempt := func(opts string, id int) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback() //nolint:errcheck
		rows, err := tx.QueryContext(ctx, "SELECT item_id, price FROM Item"+opts)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read rows: %v", err)
		}
		rows.Close()
		if _, err := db.ExecContext(ctx, "UPDATE Item SET price = price + 1 WHERE item_id = 1"); err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO Item VALUES (?, 'x', 1)", id); err != nil {
			t.Fatalf("own write: %v", err)
		}
		return tx.Commit()
	}
	if err := attempt(" OPTIONS (ISOLATION LEVEL SNAPSHOT)", 900); err != nil {
		t.Fatalf("snapshot read: commit %v, want success", err)
	}
	if err := attempt("", 901); err == nil {
		t.Fatal("serializable read: commit succeeded, want a conflict")
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item WHERE item_id >= 900").Scan(&n); err != nil || n != 1 {
		t.Fatalf("committed own writes = %d (%v), want 1", n, err)
	}
}

// TestFDB_StatementOptions_SnapshotRefusedOnDML: SNAPSHOT is for reads only,
// whether set on the statement or the connection; DRY_RUN on the connection
// previews every DML statement, as Java merges connection and statement
// options.
func TestFDB_StatementOptions_SnapshotRefusedOnDML(t *testing.T) {
	t.Parallel()
	_, db := setupCascadesTestDB(t)
	ctx := context.Background()
	unsupported := func(err error) bool {
		var apiErr *api.Error
		return errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUnsupportedOperation
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO Item VALUES (500, 'x', 1) OPTIONS (ISOLATION LEVEL SNAPSHOT)"); !unsupported(err) {
		t.Fatalf("INSERT with SNAPSHOT: want 0A000, got %v", err)
	}

	snap := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptIsolationLevelSnapshot, true).Build())
	})
	if _, err := snap.ExecContext(ctx, "UPDATE Item SET price = 2 WHERE item_id = 1"); !unsupported(err) {
		t.Fatalf("UPDATE on a SNAPSHOT connection: want 0A000, got %v", err)
	}
	var n int
	if err := snap.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item").Scan(&n); err != nil {
		t.Fatalf("SELECT on a SNAPSHOT connection: %v", err)
	}

	dry := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptDryRun, true).Build())
	})
	if _, err := dry.ExecContext(ctx, "INSERT INTO Item VALUES (501, 'x', 1)"); err != nil {
		t.Fatalf("INSERT on a DRY_RUN connection: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Item WHERE item_id = 501").Scan(&n); err != nil || n != 0 {
		t.Fatalf("DRY_RUN connection committed the INSERT: %d rows, %v", n, err)
	}
}
