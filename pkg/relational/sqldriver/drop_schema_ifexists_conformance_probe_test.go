package sqldriver_test

// Conformance sentinel: DROP SCHEMA does NOT honor IF EXISTS, and that MATCHES Java.
// Java's DdlVisitor.visitDropSchemaStatement (DdlVisitor.java:472) never reads
// ctx.ifExists() — it builds getDropSchemaConstantAction(db, schema, Options.NONE), so
// `DROP SCHEMA IF EXISTS <nonexistent>` errors exactly like the bare form. The sibling
// statements DO honor it (visitDropDatabaseStatement:466, visitDropSchemaTemplateStatement:483
// both thread throwIfDoesNotExist from ifExists()). Go mirrors this split. This pins the
// (Java-conformant) split so nobody "fixes" DROP SCHEMA to honor IF EXISTS and diverges.

import (
	"context"
	"strings"
	"testing"
)

func TestFDB_DropSchemaIfExistsConformance(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openTestDB(t, "/testdb_dsiec")
	mwjoMustExec(t, db, ctx, "CREATE DATABASE /testdb_dsiec")
	mwjoMustExec(t, db, ctx, "CREATE SCHEMA TEMPLATE dsiec CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx, "CREATE SCHEMA /testdb_dsiec/real WITH TEMPLATE dsiec")

	errs := func(q string) error { _, err := db.ExecContext(ctx, q); return err }

	t.Run("drop_schema_IF_EXISTS_nonexistent_still_errors_matches_java", func(t *testing.T) {
		// IF EXISTS is IGNORED for DROP SCHEMA (Java parity) → errors on non-existent.
		err := errs("DROP SCHEMA IF EXISTS /testdb_dsiec/ghost")
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("DROP SCHEMA IF EXISTS <nonexistent> = %v; want a 'does not exist' error "+
				"(Java ignores IF EXISTS here — DdlVisitor:472). If this now no-ops, Go has "+
				"DIVERGED from Java; do not 'fix' without changing Java too.", err)
		}
	})
	t.Run("drop_schema_bare_nonexistent_errors", func(t *testing.T) {
		if err := errs("DROP SCHEMA /testdb_dsiec/ghost2"); err == nil {
			t.Errorf("DROP SCHEMA <nonexistent> should error")
		}
	})
	t.Run("drop_schema_template_IF_EXISTS_nonexistent_noops_sibling", func(t *testing.T) {
		// Sibling DOES honor IF EXISTS (Java visitDropSchemaTemplateStatement:483).
		if err := errs("DROP SCHEMA TEMPLATE IF EXISTS ghosttmpl"); err != nil {
			t.Errorf("DROP SCHEMA TEMPLATE IF EXISTS <nonexistent> = %v, want no-op", err)
		}
	})
	t.Run("drop_database_IF_EXISTS_nonexistent_noops_sibling", func(t *testing.T) {
		// Sibling DOES honor IF EXISTS (Java visitDropDatabaseStatement:466).
		if err := errs("DROP DATABASE IF EXISTS /testdb_dsiec_ghostdb"); err != nil {
			t.Errorf("DROP DATABASE IF EXISTS <nonexistent> = %v, want no-op", err)
		}
	})
	t.Run("drop_schema_IF_EXISTS_existing_succeeds", func(t *testing.T) {
		if err := errs("DROP SCHEMA IF EXISTS /testdb_dsiec/real"); err != nil {
			t.Errorf("DROP SCHEMA IF EXISTS <existing> = %v, want success", err)
		}
	})
}
