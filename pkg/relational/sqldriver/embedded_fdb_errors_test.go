package sqldriver_test

// Error-path coverage for the embedded FDB driver — pinning the
// behaviour the user sees when DML is rejected. Each test sets up a
// minimal schema, executes a known-bad statement, and asserts the
// returned error's SQLSTATE matches the expected api.ErrCode*.
//
// Per TODO.md MEDIUM "Error-path coverage": separate file from the
// happy-path embedded_fdb_test.go so the error-shape diff lives
// together.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// setupErrorTestDB creates a fresh database + schema template + schema
// and returns a *sql.DB wired into that schema. Same shape as the
// happy-path tests' setup, factored so the error tests can share it.
func setupErrorTestDB(t *testing.T, dbPath, schemaName, ddl string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s_tmpl %s", schemaName, ddl)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s_tmpl", dbPath, schemaName, schemaName)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schemaName)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// asAPIError unwraps the err to *api.Error. Returns nil if the chain
// has no api.Error.
func asAPIError(err error) *api.Error {
	var e *api.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// assertErrorCode runs the SQL, expects an error, and asserts the
// returned error's api.ErrCode* matches `wantCode`.
func assertErrorCode(t *testing.T, db *sql.DB, sql string, wantCode api.ErrorCode) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), sql)
	if err == nil {
		t.Fatalf("expected error %q, got nil", wantCode)
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("error is not *api.Error: %v (%T)", err, err)
	}
	if got.Code != wantCode {
		t.Fatalf("error code = %q, want %q (full: %v)", got.Code, wantCode, err)
	}
}

func TestFDB_Errors_PKConflictDuplicateInsert(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_pk", "errs_pk",
		"CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))")

	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO Item (id, name) VALUES (1, 'first')"); err != nil {
		t.Fatalf("first INSERT: %v", err)
	}
	// Re-inserting the same PK should fail with UniqueConstraintViolation.
	_, err := db.ExecContext(context.Background(),
		"INSERT INTO Item (id, name) VALUES (1, 'duplicate')")
	if err == nil {
		t.Fatal("duplicate-PK INSERT did not error")
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("error is not *api.Error: %v", err)
	}
	if got.Code != api.ErrCodeUniqueConstraintViolation {
		t.Logf("warn: expected UniqueConstraintViolation, got %q (this surfaces the actual contract — adjust the assertion if the engine returns a different code)", got.Code)
		// Fail loudly with the actual code so the test pins WHATEVER
		// the engine returns; a future engine change that silently
		// alters this contract surfaces here.
		t.Fatalf("error code = %q, want %q (full: %v)", got.Code, api.ErrCodeUniqueConstraintViolation, err)
	}
}

// NULL into a PK column: pinned by null_pk_java_parity_test.go. A test
// that lived here (TestFDB_Errors_NotNullViolation_PKDoc) claimed to pin
// the shape but logged-and-returned on every path and asserted nothing —
// it stayed green whether the engine errored, stored 0, or stored tuple
// null, so it was deleted. Java raises NO error for this shape (a scalar
// column is never non-nullable in the relational layer; the PK stores a
// real tuple null), and Go matches — the parity file asserts each
// observable that distinguishes the candidate behaviours.

func TestFDB_Errors_TypeMismatchInsert(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_tm", "errs_tm",
		"CREATE TABLE Item (id BIGINT, qty BIGINT, PRIMARY KEY (id))")
	// Inserting a STRING into a BIGINT column.
	_, err := db.ExecContext(context.Background(),
		"INSERT INTO Item (id, qty) VALUES (1, 'not-a-number')")
	if err == nil {
		t.Fatal("string-into-BIGINT INSERT did not error")
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("error is not *api.Error: %v", err)
	}
	// Java's SemanticException for INSERT type mismatch → CANNOT_CONVERT_TYPE (22000).
	if got.Code != api.ErrCodeCannotConvertType {
		t.Fatalf("error code = %q, want %q (full: %v)", got.Code, api.ErrCodeCannotConvertType, err)
	}
}

func TestFDB_Errors_InvalidSQL(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_sql", "errs_sql",
		"CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))")
	_, err := db.ExecContext(context.Background(), "THIS IS NOT VALID SQL")
	if err == nil {
		t.Fatal("invalid SQL did not error")
	}
	// Pin: the parser's error must carry "syntax" or be SyntaxError-typed.
	if got := asAPIError(err); got != nil {
		if got.Code != api.ErrCodeSyntaxError {
			t.Fatalf("error code = %q, want %q (full: %v)", got.Code, api.ErrCodeSyntaxError, err)
		}
		return
	}
	// Some parse errors come through as plain errors with "syntax" in the message.
	if !strings.Contains(strings.ToLower(err.Error()), "syntax") {
		t.Fatalf("invalid-SQL error doesn't mention 'syntax': %v", err)
	}
}

func TestFDB_Errors_UndefinedTable(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_undef", "errs_undef",
		"CREATE TABLE Item (id BIGINT, PRIMARY KEY (id))")
	_, err := db.ExecContext(context.Background(),
		"INSERT INTO NoSuchTable (id) VALUES (1)")
	if err == nil {
		t.Fatal("INSERT into nonexistent table did not error")
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("error is not *api.Error: %v", err)
	}
	if got.Code != api.ErrCodeUndefinedTable {
		t.Fatalf("error code = %q, want %q (full: %v)", got.Code, api.ErrCodeUndefinedTable, err)
	}
}

// TestFDB_Errors_UndefinedColumn probes Java conformance: SELECT
// col_doesnt_exist FROM t must error with 42703 (undefined column).
func TestFDB_Errors_UndefinedColumn(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_undef_col", "errs_undef_col",
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10)"); err != nil {
		t.Fatalf("setup INSERT: %v", err)
	}

	tests := []struct {
		name string
		sql  string
	}{
		{"select_undef_col", "SELECT nonexistent FROM t"},
		{"where_undef_col", "SELECT id FROM t WHERE nonexistent = 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, tc.sql)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.sql)
			}
			got := asAPIError(err)
			if got == nil {
				t.Fatalf("expected api.Error, got non-API error: %v", err)
			}
			t.Logf("SQL: %s → code=%s msg=%s", tc.sql, got.Code, got.Message)
			if got.Code != api.ErrCodeUndefinedColumn {
				t.Errorf("error code = %q, want %q", got.Code, api.ErrCodeUndefinedColumn)
			}
		})
	}
}

// TestFDB_Errors_UnknownQualifier probes Java conformance: SELECT
// x.col FROM t (where x is not a valid table/alias) must error with
// 42703 (undefined column), matching Java's SemanticAnalyzer
// resolveIdentifier which returns UNDEFINED_COLUMN for unresolvable
// qualified references.
func TestFDB_Errors_UnknownQualifier(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_qual", "errs_qual",
		"CREATE TABLE t (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1, 'hello')"); err != nil {
		t.Fatalf("setup INSERT: %v", err)
	}

	tests := []struct {
		name string
		sql  string
	}{
		{"select_unknown_qual", "SELECT x.id FROM t"},
		{"where_unknown_qual", "SELECT id FROM t WHERE x.id = 1"},
		{"order_by_unknown_qual", "SELECT id FROM t ORDER BY x.id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, tc.sql)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.sql)
			}
			got := asAPIError(err)
			if got == nil {
				t.Logf("error is not *api.Error: %v (%T)", err, err)
				t.Fatalf("expected api.Error with code 42703, got non-API error")
			}
			t.Logf("SQL: %s → code=%s msg=%s", tc.sql, got.Code, got.Message)
			if got.Code != api.ErrCodeUndefinedColumn {
				t.Errorf("error code = %q, want %q (42703)", got.Code, api.ErrCodeUndefinedColumn)
			}
		})
	}
}

// TestFDB_Errors_UndefinedTableInJoin verifies that referencing a
// nonexistent table in a JOIN produces 42F01 (not 0AF00).
func TestFDB_Errors_UndefinedTableInJoin(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_errs_join_undef", "errs_join_undef",
		"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1, 10)"); err != nil {
		t.Fatalf("setup INSERT: %v", err)
	}

	_, err := db.QueryContext(ctx, "SELECT t.id FROM t, nonexistent WHERE t.id = nonexistent.id")
	if err == nil {
		t.Fatal("expected error for nonexistent table in JOIN, got nil")
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("expected api.Error, got: %v", err)
	}
	t.Logf("SQL → code=%s msg=%s", got.Code, got.Message)
	if got.Code != api.ErrCodeUndefinedTable {
		t.Errorf("error code = %q, want %q (42F01)", got.Code, api.ErrCodeUndefinedTable)
	}
}
