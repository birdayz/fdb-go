package sqldriver_test

// Probes DDL error paths — every invalid DDL is REJECTED (fail-closed; no bad
// schema/database created) with a clean, SPECIFIC SQLSTATE: CREATE DATABASE that exists →
// 42F04, DROP DATABASE that doesn't → 42F63, table without a PRIMARY KEY → 42601,
// duplicate column name → 42701, PRIMARY KEY over an unknown column → 42703,
// duplicate schema-template NAME → 42F59. The in-template object errors (42701/42703)
// surface their specific code as the OUTER SQLSTATE — the rejectsCode helper additionally
// asserts they do NOT carry the generic 42F59 "invalid schema template" wrapper (RFC-161:
// in-template index/column errors propagate their own code, matching Java's per-exception
// ExceptionUtil mapping rather than a blanket wrap).

import (
	"context"
	"strings"
	"testing"
)

func TestFDB_DDLErrorsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/testdb_ddlerrp")
	mwjoMustExec(t, db, ctx, "CREATE DATABASE /testdb_ddlerrp")

	rejectsCode := func(name, q, code string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, q)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			if !strings.Contains(err.Error(), code) {
				t.Errorf("%s error = %v, want %s", name, err, code)
			}
			// The specific code must be the OUTER SQLSTATE, NOT buried under the generic
			// 42F59 "invalid schema template" wrapper (RFC-161 — in-template index/column
			// errors propagate their own code). None of these rejections is an
			// invalid-template error, so 42F59 must not appear. Before the fix, the
			// duplicate-column / PK-over-unknown cases rendered as `42F59: table "T":
			// 42701: …` and a Contains("42701") check passed vacuously — this pins the
			// dimension that actually changed.
			if strings.Contains(err.Error(), "42F59") {
				t.Errorf("%s error = %v\n  must NOT carry the generic 42F59 wrapper; %s should be the outer SQLSTATE", name, err, code)
			}
		})
	}
	rejects := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, q); err == nil {
				t.Errorf("%s unexpectedly succeeded; invalid DDL must be rejected", name)
			}
		})
	}

	rejectsCode("create_database_exists", "CREATE DATABASE /testdb_ddlerrp", "42F04")
	rejectsCode("drop_nonexistent_database", "DROP DATABASE /testdb_nope_xyz_123", "42F63")
	rejectsCode("table_without_primary_key",
		"CREATE SCHEMA TEMPLATE de_nopk CREATE TABLE t (id BIGINT NOT NULL, x BIGINT)", "42601")
	// duplicate column → clean 42701 (validated in parseTableDefinition before the
	// proto-descriptor build that used to leak an XX000 internal error).
	rejectsCode("duplicate_column",
		"CREATE SCHEMA TEMPLATE de_dup CREATE TABLE t (id BIGINT NOT NULL, x BIGINT, x STRING, PRIMARY KEY (id))", "42701")
	// PK over an unknown column → clean 42703 (validated in parseTableDefinition
	// before the metadata build that used to leak an XX000 internal error).
	rejectsCode("pk_unknown_column",
		"CREATE SCHEMA TEMPLATE de_badpk CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (nope))", "42703")
	rejects("unknown_column_type",
		"CREATE SCHEMA TEMPLATE de_badtype CREATE TABLE t (id BIGINT NOT NULL, x FROBNICATE, PRIMARY KEY (id))")

	t.Run("duplicate_template_name", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "CREATE SCHEMA TEMPLATE de_ok CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
		_, err := db.ExecContext(ctx, "CREATE SCHEMA TEMPLATE de_ok CREATE TABLE u (id BIGINT NOT NULL, PRIMARY KEY (id))")
		if err == nil || !strings.Contains(err.Error(), "42F59") {
			t.Errorf("duplicate template error = %v, want 42F59", err)
		}
	})
}
