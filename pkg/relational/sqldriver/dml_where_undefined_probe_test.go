package sqldriver_test

// Regression (TODO 1066): a DELETE/UPDATE with a nonexistent WHERE column or a nonexistent
// (unqualified) target table must surface the SAME clean SQLSTATE the SELECT/INSERT paths
// already give — 42703 (undefined column) / 42F01 (undefined table) — not a generic 0AF00
// "DML Cascades translation failed". The SET-column case is already 42703
// (update_undefined_column_probe); this pins the WHERE-column and table cases.
//
// Each subtest runs with t.Parallel() against its OWN isolated schema (own table instance),
// created sequentially before t.Parallel() to avoid catalog write-contention.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DmlWhereUndefinedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dwu")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dwu")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dwu CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	newDB := func(t *testing.T, schema string) *sql.DB {
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dwu/"+schema+" WITH TEMPLATE dwu")
		db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_dwu?cluster_file=%s&schema=%s", clusterFilePath, schema))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 10)")
		return db
	}

	// wantCode pins that `stmt` fails with SQLSTATE `code`.
	wantCode := func(name, schema, stmt, code string) {
		t.Run(name, func(t *testing.T) {
			db := newDB(t, schema)
			t.Parallel()
			_, err := db.ExecContext(ctx, stmt)
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("%s\n  error = %v\n  want SQLSTATE %s", stmt, err, code)
			}
		})
	}

	// WHERE references a column that does not exist → 42703 (matches SELECT … WHERE nope).
	wantCode("delete_where_undefined_col_42703", "s_dwc", "DELETE FROM t WHERE nope = 1", "42703")
	wantCode("update_where_undefined_col_42703", "s_uwc", "UPDATE t SET a = 1 WHERE nope = 1", "42703")
	// (codex r4) An EXISTS atom BEFORE the bad column must not mask the 42703: the
	// subquery-aware walk sees past EXISTS to the undefined column.
	wantCode("delete_exists_then_undefined_col_42703", "s_exc", "DELETE FROM t WHERE EXISTS (SELECT 1 FROM t) AND nope = 1", "42703")
	// Nonexistent (unqualified) table → 42F01 (matches INSERT INTO notable).
	wantCode("delete_nonexistent_table_42F01", "s_dnt", "DELETE FROM notable WHERE id = 1", "42F01")
	wantCode("update_nonexistent_table_42F01", "s_unt", "UPDATE notable SET a = 1 WHERE id = 1", "42F01")
	// WHERE-independence (@claude): the table check fires even with NO WHERE clause.
	wantCode("delete_nonexistent_table_no_where_42F01", "s_dntn", "DELETE FROM notable", "42F01")
	wantCode("update_nonexistent_table_no_where_42F01", "s_untn", "UPDATE notable SET a = 1", "42F01")
	// Active-schema-qualified missing table (codex/Torvalds): a VALID qualifier (the
	// session schema) + a missing table must still get 42F01 — the existence check runs
	// after resolveQualifiedTableNames strips the (valid) qualifier.
	wantCode("delete_active_schema_qualified_missing_42F01", "s_aqd", "DELETE FROM s_aqd.notable WHERE id = 1", "42F01")
	wantCode("update_active_schema_qualified_missing_42F01", "s_aqu", "UPDATE s_aqu.notable SET a = 1 WHERE id = 1", "42F01")

	// Precedence (codex): a schema-qualified target with a bad qualifier must NOT be
	// preempted by the bare-table 42F01 — the qualifier validation downstream owns the
	// error. The bare-table existence check only fires for an unqualified name.
	t.Run("qualified_bad_schema_not_preempted_by_42F01", func(t *testing.T) {
		db := newDB(t, "s_qual")
		t.Parallel()
		_, err := db.ExecContext(ctx, "DELETE FROM other.missing WHERE id = 1")
		if err == nil {
			t.Fatalf("DELETE FROM other.missing: want an error, got nil")
		}
		// The bare-table check must have deferred: the error is NOT our 42F01 "Unknown
		// table MISSING". (Downstream qualifier validation reports the real cause.)
		if strings.Contains(err.Error(), "42F01") && strings.Contains(strings.ToUpper(err.Error()), "MISSING") {
			t.Errorf("schema-qualified bad target preempted by bare-table 42F01: %v\n  (want the qualifier error to take precedence)", err)
		}
	})

	// Precedence (codex r3): a BAD qualifier + an EXISTING bare table + a BAD WHERE column
	// must surface the qualifier error (42F00), NOT the WHERE-column 42703 — the qualifier
	// is validated before the WHERE walk error is classified.
	t.Run("bad_qualifier_existing_table_bad_col_not_42703", func(t *testing.T) {
		db := newDB(t, "s_bqc")
		t.Parallel()
		// "t" exists in s_bqc; "other" is a bad schema qualifier; "nope" is a bad column.
		_, err := db.ExecContext(ctx, "DELETE FROM other.t WHERE nope = 1")
		if err == nil {
			t.Fatalf("DELETE FROM other.t WHERE nope=1: want an error, got nil")
		}
		if strings.Contains(err.Error(), "42703") {
			t.Errorf("bad qualifier preempted by WHERE-column 42703: %v\n  (want the qualifier error 42F00 to take precedence)", err)
		}
	})

	// Controls: a valid WHERE column still plans + executes (no over-rejection).
	t.Run("valid_where_delete_works", func(t *testing.T) {
		db := newDB(t, "s_vwd")
		t.Parallel()
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (2, 20)")
		if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE a = 20"); err != nil {
			t.Fatalf("valid DELETE WHERE: %v", err)
		}
	})
	t.Run("valid_where_update_works", func(t *testing.T) {
		db := newDB(t, "s_vwu")
		t.Parallel()
		if _, err := db.ExecContext(ctx, "UPDATE t SET a = 11 WHERE id = 1"); err != nil {
			t.Fatalf("valid UPDATE WHERE: %v", err)
		}
	})
}
