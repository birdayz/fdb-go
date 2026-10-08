package testkit

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
	"sync"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// errorTestTemplates is every schema-template name setupErrorTestDB has
// created in this test binary. The template is named after the schema alone
// and lives in the one catalog every parallel test shares, so two tests
// passing the same schema name race to create it and the loser fails
// "Schema template already exists" (42F62), whichever runs second.
var errorTestTemplates sync.Map

// setupErrorTestDB creates a fresh database + schema template + schema
// and returns a *sql.DB wired into that schema. Same shape as the
// happy-path tests' setup, factored so the error tests can share it.
func SetupErrorDB(t *testing.T, dbPath, schemaName, ddl string) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	template := strings.ToUpper(schemaName) + "_TMPL"
	if other, taken := errorTestTemplates.LoadOrStore(template, t.Name()); taken {
		t.Fatalf("setupErrorTestDB: schema name %q is also %s's; its template %s is catalog-global, so pick another", schemaName, other, template)
	}
	setup := OpenDB(t, dbPath)
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
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", strings.ToUpper(dbPath), clusterFilePath, strings.ToUpper(schemaName))
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// asAPIError unwraps the err to *api.Error. Returns nil if the chain
// has no api.Error.
func AsAPIError(err error) *api.Error {
	var e *api.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// assertErrorCode runs the SQL, expects an error, and asserts the
// returned error's api.ErrCode* matches `wantCode`.
func AssertErrorCode(t *testing.T, db *sql.DB, sql string, wantCode api.ErrorCode) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), sql)
	if err == nil {
		t.Fatalf("expected error %q, got nil", wantCode)
	}
	got := AsAPIError(err)
	if got == nil {
		t.Fatalf("error is not *api.Error: %v (%T)", err, err)
	}
	if got.Code != wantCode {
		t.Fatalf("error code = %q, want %q (full: %v)", got.Code, wantCode, err)
	}
}
