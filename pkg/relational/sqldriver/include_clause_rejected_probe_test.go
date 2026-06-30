package sqldriver_test

// Regression: `CREATE INDEX ... INCLUDE (cols)` (covering index) was SILENTLY accepted
// with the INCLUDE clause DROPPED — Go created a PLAIN index where Java creates a
// COVERING (KeyWithValue) one (DdlVisitor.java:249 → addValueColumn). The same DDL
// producing different index structures across engines is a wire/DDL-portability
// divergence. Go's record layer HAS covering support (KeyWithValueExpression), but the
// SQL DDL layer doesn't wire INCLUDE yet, so until it does (TODO.md) it is rejected
// (0A000 "INCLUDE clause (covering index) is not yet supported"), matching the vector
// index path's own INCLUDE rejection. A plain index (no INCLUDE) is unaffected.

import (
	"context"
	"strings"
	"testing"
)

func TestFDB_IncludeClauseRejectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/testdb_incr")
	mwjoMustExec(t, db, ctx, "CREATE DATABASE /testdb_incr")

	t.Run("create_index_include_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE incr_a CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
				"CREATE INDEX t_a ON t (a) INCLUDE (b)")
		if err == nil {
			t.Fatalf("CREATE INDEX ... INCLUDE unexpectedly succeeded (must reject, not silently build a plain index)")
		}
		// In-template index errors now PROPAGATE their specific SQLSTATE (RFC-161): the
		// OUTER SQLSTATE is the cause's own code — 0A000 (UNSUPPORTED_OPERATION) for an
		// unsupported INCLUDE / covering index — not the generic 42F59 "invalid schema
		// template" wrapper. Java does the same (its DdlVisitor doesn't wrap; ExceptionUtil
		// maps each exception to its specific ErrorCode). A `database/sql` caller doing
		// SQLSTATE extraction now sees the real cause.
		msg := err.Error()
		if !strings.Contains(msg, "0A000") {
			t.Errorf("INCLUDE error SQLSTATE = %v, want 0A000 (covering not yet supported, propagated — not the 42F59 wrapper)", err)
		}
		if strings.Contains(msg, "42F59") {
			t.Errorf("INCLUDE error should no longer carry the generic 42F59 wrapper: %v", err)
		}
		if !strings.Contains(strings.ToUpper(msg), "INCLUDE") {
			t.Errorf("error should mention INCLUDE: %v", err)
		}
	})
	t.Run("plain_index_without_include_still_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE incr_b CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id)) "+
				"CREATE INDEX t_a ON t (a)"); err != nil {
			t.Fatalf("plain CREATE INDEX (no INCLUDE) should work: %v", err)
		}
	})
}
