package sqldriver_test

// CONFORMANCE pin — UPDATE of a PRIMARY KEY column returns XXXXX in BOTH engines (RFC-160).
//
// `UPDATE t SET id = <new> WHERE id = <old>` does not relocate the record; the executor
// applies the SET to the proto (including the PK field) then calls SaveRecordWithOptions(
// .., ErrorIfNotExistsOrTypeChanged), which targets the NEW pk and fails the existence
// check. JAVA IS IDENTICAL: RecordQueryUpdatePlan.saveRecordAsync saves with
// ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED, and ExceptionUtil.recordCoreToRelationalException
// does NOT special-case the resulting RecordDoesNotExistException → it falls to the DEFAULT
// ErrorCode.UNKNOWN, which is "XXXXX" — byte-identical to Go's ErrCodeUnknown ("XXXXX").
// So the SQLSTATE matches Java; neither engine relocates and both fail-CLOSED (table
// UNCHANGED). The original TODO (item 1085) framed the XXXXX as a Go "leak to fix" with a
// clean "cannot update primary key" code — but that would DIVERGE from Java; XXXXX is the
// conformant result. This test pins: (1) XXXXX AND the "record does not exist" path message
// (Java-faithful, not the leaky `executor:` prefix), (2) no corruption, (3) non-PK UPDATE works.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_UpdatePrimaryKeyProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_upkp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_upkp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE upkp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_upkp/s WITH TEMPLATE upkp")
	dsn := fmt.Sprintf("fdbsql:///testdb_upkp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20)")

	snapshot := func() []string {
		rows, err := db.QueryContext(ctx, "SELECT id, a FROM t")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		var o []string
		for rows.Next() {
			var id, a int64
			_ = rows.Scan(&id, &a)
			o = append(o, fmt.Sprintf("%d:%d", id, a))
		}
		sort.Strings(o)
		return o
	}
	before := snapshot()

	t.Run("pk_update_rejected_xxxxx_matches_java", func(t *testing.T) {
		// Changing the PK retargets the save to the NEW pk (no record) → the existence
		// check fails. Java is IDENTICAL: RecordQueryUpdatePlan saves with
		// ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED and ExceptionUtil maps the resulting
		// RecordDoesNotExistException to the DEFAULT ErrorCode.UNKNOWN = "XXXXX" (it is not
		// in Java's RecordCoreException switch). Go's ErrCodeUnknown is also "XXXXX", so the
		// SQLSTATE matches Java. A clean Go-only "cannot update primary key" code would
		// DIVERGE — so XXXXX is the conformant result, pinned here (RFC-160).
		_, err := db.ExecContext(ctx, "UPDATE t SET id = 99 WHERE id = 1")
		if err == nil {
			t.Fatalf("UPDATE SET id (PK) unexpectedly succeeded; want XXXXX rejection (fail-closed)")
		}
		// Anchor BOTH axes: the SQLSTATE (XXXXX) AND the specific path message.
		// XXXXX is the catch-all default, so asserting it alone would stay green if some
		// OTHER failure (a planner blowup, a different RecordCoreException) regressed into
		// XXXXX. "record does not exist" is RecordDoesNotExistException's own message — the
		// Java-faithful one (not the leaky `executor:` prefix) — pinning THIS path.
		if !strings.Contains(err.Error(), "XXXXX") || !strings.Contains(err.Error(), "record does not exist") {
			t.Errorf("UPDATE SET id (PK) error = %v\n  want SQLSTATE XXXXX + \"record does not exist\" (matches Java's RecordDoesNotExistException → ErrorCode.UNKNOWN)", err)
		}
	})
	t.Run("no_data_corruption", func(t *testing.T) {
		after := snapshot()
		if len(after) != len(before) {
			t.Fatalf("row count changed: before %v, after %v", before, after)
		}
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("table mutated by failed PK update: before %v, after %v", before, after)
			}
		}
	})
	t.Run("non_pk_update_still_works", func(t *testing.T) {
		// sanity: a normal (non-PK) UPDATE is unaffected.
		if _, err := db.ExecContext(ctx, "UPDATE t SET a = 11 WHERE id = 1"); err != nil {
			t.Fatalf("non-PK update: %v", err)
		}
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
			t.Fatalf("read: %v", err)
		}
		if a != 11 {
			t.Errorf("non-PK update id=1 a = %d, want 11", a)
		}
	})
}
