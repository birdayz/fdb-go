package sqldriver_test

// Probes DDL error paths — every invalid DDL is REJECTED (fail-closed; no bad
// schema/database created). Clean SQLSTATEs: CREATE DATABASE that exists → 42F04,
// DROP DATABASE that doesn't → 42F63, table without a PRIMARY KEY → 42601,
// duplicate schema-template name → 42F59. A duplicate column name and a PK over an
// unknown column are also rejected, but currently surface a leaky internal error
// (XX000 + raw protodesc message) rather than a clean 42-class user error — see
// TODO.md "DDL error classification". This test pins the rejection (the fail-closed
// behavior); tighten the code assertions when the messages are cleaned up.

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
