package sqldriver_test

// RFC-202 S6 retired the INCLUDE fail-closed rejection: `CREATE INDEX … ON t(…)
// INCLUDE (cols)` now builds a COVERING (KeyWithValue) index through the same
// generator the AS-SELECT form uses — exactly Java's DdlVisitor path
// (visitIndexOnSourceDefinition → OnSourceIndexGenerator → addValueColumn).
// This probe pins the driver-level acceptance both ways: the covering form
// creates, and the plain form keeps creating. The entry-byte proof lives in
// TestFDB_OnSourceIndex_WireEntries; the metadata golden in
// embedded's TestOnSourceIndex_IncludeBuildsKeyWithValue.

import (
	"context"
	"testing"
)

func TestFDB_IncludeClauseAcceptedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/testdb_incr")
	mwjoMustExec(t, db, ctx, "CREATE DATABASE /testdb_incr")

	t.Run("create_index_include_builds_covering", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE incr_a CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
				"CREATE INDEX t_a ON t (a) INCLUDE (b)"); err != nil {
			t.Fatalf("CREATE INDEX ... INCLUDE must build a covering index through the generator (RFC-202 S6): %v", err)
		}
	})
	t.Run("plain_index_without_include_still_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			"CREATE SCHEMA TEMPLATE incr_b CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) "+
				"CREATE INDEX t_a ON t (a)"); err != nil {
			t.Fatalf("plain CREATE INDEX (no INCLUDE) should work: %v", err)
		}
	})
}
