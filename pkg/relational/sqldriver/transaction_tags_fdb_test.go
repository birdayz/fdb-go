package sqldriver_test

// End-to-end proof that ?transaction_tags= drives real SQL execution: the tags
// are applied to every transaction the connection opens (the single seam in
// EmbeddedConnection.beginTransaction covers autocommit and explicit BEGIN
// alike), and both reads and writes still work with the tags on the wire.
//
// The tag bytes themselves are pinned against real C++ ObjectWriter output at
// the client layer; what this test adds is that the SQL layer actually reaches
// that path rather than dropping the option on the floor.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_TransactionTags_TaggedConnectionReadsAndWrites(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_txtags")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txtags")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txtags CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txtags/s WITH TEMPLATE txtags")

	dsn := fmt.Sprintf("fdbsql:///testdb_txtags?cluster_file=%s&schema=s&transaction_tags=tenant-a,bulk",
		clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Autocommit write through a tagged connection.
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("tagged autocommit INSERT: %v", err)
	}

	// Explicit transaction through the same tagged connection — a different
	// code path into beginTransaction than autocommit.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("tagged BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES (3, 30)"); err != nil {
		t.Fatalf("tagged explicit INSERT: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tagged commit: %v", err)
	}

	var sum int64
	if err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM t").Scan(&sum); err != nil {
		t.Fatalf("tagged read: %v", err)
	}
	if sum != 60 {
		t.Errorf("SUM(v) = %d, want 60", sum)
	}
}

// The load-bearing assertion: the tags must actually be ON the transaction.
// Proving only that a tagged connection still reads and writes is worthless —
// deleting the tag-application loop entirely leaves that green, because
// untagged transactions work fine. This reads the tags back off the open
// transaction, so dropping the loop turns it red.
func TestFDB_TransactionTags_TagsReachTheTransaction(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_txtags2")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txtags2")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txtags2 CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txtags2/s WITH TEMPLATE txtags2")

	dsn := fmt.Sprintf("fdbsql:///testdb_txtags2?cluster_file=%s&schema=s&transaction_tags=gamma,alpha",
		clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // asserted below via the tag read

	var got []string
	if err := conn.Raw(func(dc any) error {
		type tagReporter interface{ ActiveTransactionTags() []string }
		rep, ok := dc.(tagReporter)
		if !ok {
			t.Fatalf("driver conn %T does not report transaction tags", dc)
		}
		got = rep.ActiveTransactionTags()
		return nil
	}); err != nil {
		t.Fatalf("conn.Raw: %v", err)
	}

	// DSN tags are sorted, so the transaction carries them in sorted order.
	if want := "alpha,gamma"; strings.Join(got, ",") != want {
		t.Errorf("transaction tags = %q, want %q", strings.Join(got, ","), want)
	}
}

// A tag set the record layer rejects must fail when the DSN is opened, not on
// some later statement — the whole point of validating at parse time.
func TestFDB_TransactionTags_InvalidTagFailsAtOpen(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dsn := fmt.Sprintf("fdbsql:///testdb_txtags_bad?cluster_file=%s&transaction_tags=%s",
		clusterFilePath, strings.Repeat("x", 17))
	// sql.Open defers driver work, so the error may surface here or on first
	// use — but it MUST surface, and it must carry the record layer's wording.
	// An early `return` on the sql.Open error would let this pass vacuously.
	db, openErr := sql.Open("fdbsql", dsn)
	err := openErr
	if err == nil {
		t.Cleanup(func() { db.Close() })
		_, err = db.ExecContext(ctx, "SELECT 1")
	}
	if err == nil {
		t.Fatal("an over-long tag must fail rather than silently dropping the tag")
	}
	if !strings.Contains(err.Error(), "Tag must be 16 characters or shorter") {
		t.Errorf("error = %q, want the record layer's wording", err)
	}
}
