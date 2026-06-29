package sqldriver_test

// `<DML> ... OPTIONS (DRY RUN)` PREVIEWS the would-be-affected rows without committing,
// matching Java (AstNormalizer.visitQueryOptions → Options.DRY_RUN →
// ExecuteProperties.setDryRun → the DML plans branch to dryRunSave/DeleteRecord). RFC-158.
//
// This replaces the former fail-closed reject (the data-loss stopgap, RFC-158 §Problem):
// the option is now honored, not rejected. The flag is STATEMENT-scoped — parsed from the
// typed OPTIONS clause and carried on the cascadesPlan → paginatingRows.dryRun →
// ExecuteProperties.DryRun — so a DRY RUN statement can NEVER leak to a later plain
// statement on the same (pooled) connection. The no-sticky subtest is the data-loss
// regression sentinel.
//
// Each subtest runs with t.Parallel() against its OWN isolated schema (own table instance)
// so mutating subtests cannot interfere; the per-subtest schema is created sequentially
// (before t.Parallel()) to avoid catalog write-contention.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DmlDryRun(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dryrun")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dryrun")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dryrun CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	// newDB creates an ISOLATED schema (its own table instance) seeded with
	// (1,10),(2,20),(3,30). Called before t.Parallel() in each subtest so the schema
	// creation runs sequentially (no catalog write-contention), letting the subtest body
	// run in parallel against private state.
	newDB := func(t *testing.T, schema string) *sql.DB {
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dryrun/"+schema+" WITH TEMPLATE dryrun")
		db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dryrun?cluster_file=%s&schema=%s", clusterFilePath, schema))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")
		return db
	}
	countOf := func(db *sql.DB) int {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n)
		return n
	}
	maxAOf := func(db *sql.DB) int64 {
		var m sql.NullInt64
		_ = db.QueryRowContext(ctx, "SELECT MAX(a) FROM t").Scan(&m)
		return m.Int64
	}

	// DELETE: previews the would-be-deleted count, deletes nothing.
	t.Run("delete_previews_no_mutation", func(t *testing.T) {
		db := newDB(t, "s_del")
		t.Parallel()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE a > 0 OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("DELETE DRY RUN: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3 {
			t.Errorf("DELETE DRY RUN RowsAffected = %d, want 3 (would-delete all)", n)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after DELETE DRY RUN, count = %d, want 3 (NO mutation)", c)
		}
	})

	// UPDATE: previews the would-be-updated count, mutates nothing.
	t.Run("update_previews_no_mutation", func(t *testing.T) {
		db := newDB(t, "s_upd")
		t.Parallel()
		res, err := db.ExecContext(ctx, "UPDATE t SET a = 999 OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("UPDATE DRY RUN: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3 {
			t.Errorf("UPDATE DRY RUN RowsAffected = %d, want 3", n)
		}
		if m := maxAOf(db); m != 30 {
			t.Errorf("after UPDATE DRY RUN, MAX(a) = %d, want 30 (NO mutation)", m)
		}
	})

	// INSERT: previews 1, inserts nothing.
	t.Run("insert_previews_no_mutation", func(t *testing.T) {
		db := newDB(t, "s_ins")
		t.Parallel()
		res, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (99, 99) OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("INSERT DRY RUN: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("INSERT DRY RUN RowsAffected = %d, want 1", n)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after INSERT DRY RUN, count = %d, want 3 (row absent)", c)
		}
	})

	// Existence-check parity: an INSERT of an existing PK under DRY RUN still raises the
	// duplicate-key error (Java's dryRunSaveRecordAsync runs the same validation, just
	// dryRun=true). DRY RUN previews — it does not suppress validation.
	t.Run("insert_existing_pk_still_raises_23505", func(t *testing.T) {
		db := newDB(t, "s_exist")
		t.Parallel()
		_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 99) OPTIONS (DRY RUN)")
		if err == nil {
			t.Fatalf("INSERT existing PK DRY RUN: want duplicate-key error, got nil")
		}
		if !strings.Contains(err.Error(), "23505") {
			t.Errorf("INSERT existing PK DRY RUN error = %v, want 23505 (validation parity)", err)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after errored INSERT DRY RUN, count = %d, want 3", c)
		}
	})

	// (Graefe G1) DELETE echo keeps the `if deleted` filter (Java's
	// .filter(isDeleted -> isDeleted)): a mixed predicate matching one existing and one
	// absent PK previews exactly the existing one.
	t.Run("delete_echo_counts_existing_only", func(t *testing.T) {
		db := newDB(t, "s_echo")
		t.Parallel()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE id = 1 OR id = 999 OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("DELETE DRY RUN mixed PKs: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("DELETE DRY RUN echo = %d, want 1 (only the existing PK counts)", n)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after mixed-PK DELETE DRY RUN, count = %d, want 3", c)
		}
	})

	// (Torvalds, DATA-LOSS REGRESSION SENTINEL) The flag is per-statement, NOT sticky on
	// the connection: a DRY RUN DELETE followed by a PLAIN DELETE on the SAME connection —
	// the plain one MUST actually mutate. If DRY RUN leaked to the connection, the plain
	// DELETE would silently no-op (the resurrected data-loss bug). Pinned to ONE db.Conn so
	// the two statements are guaranteed to share a connection (Torvalds impl review).
	t.Run("not_sticky_plain_delete_after_dry_run_mutates", func(t *testing.T) {
		db := newDB(t, "s_sticky")
		t.Parallel()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn: %v", err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "DELETE FROM t WHERE id = 1 OPTIONS (DRY RUN)"); err != nil {
			t.Fatalf("DRY RUN DELETE: %v", err)
		}
		if c := countOf(db); c != 3 {
			t.Fatalf("after DRY RUN DELETE, count = %d, want 3", c)
		}
		res, err := conn.ExecContext(ctx, "DELETE FROM t WHERE id = 2")
		if err != nil {
			t.Fatalf("plain DELETE after DRY RUN: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("plain DELETE after DRY RUN RowsAffected = %d, want 1 (NOT sticky)", n)
		}
		if c := countOf(db); c != 2 {
			t.Errorf("after plain DELETE on same conn, count = %d, want 2 (plain DELETE MUST mutate)", c)
		}
	})

	// (Torvalds, EXPLAIN) EXPLAIN <DML> OPTIONS (DRY RUN) renders a plan with a live DB —
	// no reject, no mutation (EXPLAIN never invokes the executor).
	t.Run("explain_renders_plan_no_mutation", func(t *testing.T) {
		db := newDB(t, "s_explain")
		t.Parallel()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN DELETE FROM t WHERE a > 0 OPTIONS (DRY RUN)").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN DML DRY RUN: %v", err)
		}
		if plan == "" {
			t.Errorf("EXPLAIN DML DRY RUN returned an empty plan")
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after EXPLAIN DML DRY RUN, count = %d, want 3 (EXPLAIN never mutates)", c)
		}
	})

	// (Torvalds T1) DRY RUN inside an explicit BeginTx/COMMIT stages nothing across COMMIT:
	// the dry-run primitives write nothing even when the DML joins the user's transaction
	// (respectActiveTx).
	t.Run("in_explicit_tx_stages_nothing_across_commit", func(t *testing.T) {
		db := newDB(t, "s_tx")
		t.Parallel()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM t WHERE a > 0 OPTIONS (DRY RUN)"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("DRY RUN DELETE in tx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after committed DRY RUN DELETE in tx, count = %d, want 3 (stages nothing)", c)
		}
	})

	// Multi-option list: DRY RUN mixed with a harmless option is still detected (the helper
	// walks every option) and previews.
	t.Run("multi_option_nocache_and_dry_run_previews", func(t *testing.T) {
		db := newDB(t, "s_multi")
		t.Parallel()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE a > 0 OPTIONS (NOCACHE, DRY RUN)")
		if err != nil {
			t.Fatalf("DELETE OPTIONS (NOCACHE, DRY RUN): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3 {
			t.Errorf("mixed-option DRY RUN echo = %d, want 3", n)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after mixed-option DRY RUN, count = %d, want 3 (no mutation)", c)
		}
	})

	// (codex P1, DATA-LOSS) INSERT … SELECT … OPTIONS (DRY RUN): the grammar attaches the
	// OPTIONS to the inner SELECT's queryTerm, so insertStatement.queryOptions is nil. DRY
	// RUN must still be honored (detection walks the whole DML subtree). A missed DRY RUN
	// here would COMMIT — the resurrected data-loss bug. This is the data-loss sentinel for
	// the INSERT…SELECT spelling.
	t.Run("insert_select_previews_no_mutation", func(t *testing.T) {
		db := newDB(t, "s_inssel")
		t.Parallel()
		res, err := db.ExecContext(ctx, "INSERT INTO t SELECT id + 100, a FROM t WHERE id = 1 OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("INSERT … SELECT DRY RUN: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("INSERT … SELECT DRY RUN RowsAffected = %d, want 1 (previews one row)", n)
		}
		if c := countOf(db); c != 3 {
			t.Errorf("after INSERT … SELECT DRY RUN, count = %d, want 3 (NO mutation — data-loss sentinel)", c)
		}
	})

	// Control: the same INSERT … SELECT WITHOUT DRY RUN actually inserts — guards against the
	// subtree walk over-matching and suppressing a real INSERT … SELECT.
	t.Run("insert_select_without_dry_run_mutates", func(t *testing.T) {
		db := newDB(t, "s_inssel_real")
		t.Parallel()
		res, err := db.ExecContext(ctx, "INSERT INTO t SELECT id + 100, a FROM t WHERE id = 1")
		if err != nil {
			t.Fatalf("INSERT … SELECT: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("INSERT … SELECT RowsAffected = %d, want 1", n)
		}
		if c := countOf(db); c != 4 {
			t.Errorf("after INSERT … SELECT, count = %d, want 4 (real insert)", c)
		}
	})

	// Control: a harmless option (NOCACHE alone) still executes the real mutation — guards
	// against an over-broad parse that treats any OPTIONS clause as DRY RUN.
	t.Run("nocache_alone_still_mutates", func(t *testing.T) {
		db := newDB(t, "s_nocache")
		t.Parallel()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE id = 1 OPTIONS (NOCACHE)")
		if err != nil {
			t.Fatalf("DELETE OPTIONS (NOCACHE): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("NOCACHE DELETE RowsAffected = %d, want 1", n)
		}
		if c := countOf(db); c != 2 {
			t.Errorf("after NOCACHE DELETE, count = %d, want 2 (executes normally)", c)
		}
	})
}

// Java's dry-run save (saveTypedRecord, isDryRun=true) EARLY-RETURNS at
// FDBRecordStore.java:578 — before serializeAndSaveRecord (staging) and
// updateSecondaryIndexes (secondary-index validation). So Java's DRY RUN validates only
// the primary-key existence check against the PRE-statement state; it does NOT catch a
// secondary-UNIQUE conflict, nor an intra-statement duplicate PK (nothing is staged
// between rows). Go's DryRunSaveRecord matches this deliberately-lightweight design.
// Detecting either in dry-run would make Go STRICTER than Java — a conformance divergence
// (Go would reject a DRY RUN that Java previews as success). These pins lock the
// Java-faithful boundary so it cannot silently drift into a divergence.
func TestFDB_DmlDryRun_MatchesJavaLightweightValidation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dryrun_lw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dryrun_lw")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dryrun_lw"+
			" CREATE TABLE emp (id BIGINT, email STRING, PRIMARY KEY (id))"+
			" CREATE UNIQUE INDEX by_email ON emp (email)")

	newDB := func(t *testing.T, schema string) *sql.DB {
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dryrun_lw/"+schema+" WITH TEMPLATE dryrun_lw")
		db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dryrun_lw?cluster_file=%s&schema=%s", clusterFilePath, schema))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	countOf := func(db *sql.DB) int {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM emp").Scan(&n)
		return n
	}

	// Secondary-UNIQUE conflict: Java's dry-run skips updateSecondaryIndexes, so a DRY RUN
	// INSERT colliding on the unique email previews SUCCESS while the REAL INSERT raises
	// 23505. Go matches Java.
	t.Run("secondary_unique_conflict_not_caught_matches_java", func(t *testing.T) {
		db := newDB(t, "s_uniq")
		t.Parallel()
		mwjoMustExec(t, db, ctx, "INSERT INTO emp VALUES (1, 'a@x.com')")
		res, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (2, 'a@x.com') OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("DRY RUN INSERT w/ unique conflict = %v; want success (Java's dry-run skips secondary-index validation, FDBRecordStore.java:578)", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("DRY RUN preview = %d, want 1 (Java-faithful: secondary unique not checked)", n)
		}
		if c := countOf(db); c != 1 {
			t.Errorf("after DRY RUN, count = %d, want 1 (no mutation)", c)
		}
		// Control: the REAL insert with the same email DOES raise 23505 — the validation
		// dry-run intentionally skips (matching Java).
		if _, rerr := db.ExecContext(ctx, "INSERT INTO emp VALUES (3, 'a@x.com')"); rerr == nil || !strings.Contains(rerr.Error(), "23505") {
			t.Errorf("real INSERT unique conflict = %v, want 23505 (the path dry-run skips)", rerr)
		}
	})

	// Intra-statement duplicate PK: Java's dry-run stages nothing between rows, so two new
	// rows with the same (absent) PK both pass dry-run's PK check while the REAL multi-row
	// INSERT raises 23505 on the second row. Go matches Java.
	t.Run("intra_statement_dup_pk_not_caught_matches_java", func(t *testing.T) {
		db := newDB(t, "s_intra")
		t.Parallel()
		res, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (10, 'p@x.com'),(10, 'q@x.com') OPTIONS (DRY RUN)")
		if err != nil {
			t.Fatalf("DRY RUN intra-statement dup PK = %v; want success (Java's dry-run stages nothing between rows)", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Errorf("DRY RUN preview = %d, want 2 (Java-faithful: no intra-statement staging)", n)
		}
		if c := countOf(db); c != 0 {
			t.Errorf("after DRY RUN, count = %d, want 0 (no mutation)", c)
		}
	})
}
