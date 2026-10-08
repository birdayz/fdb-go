package testkit

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// collectRows runs a query and returns rows as [][]any.
func CollectRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}

	var result [][]any
	for rows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		result = append(result, dest)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return result
}

func ExpectError(t *testing.T, db *sql.DB, query string) error {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return err
	}
	t.Fatalf("expected error for %q, got success", query)
	return nil
}

// requireSQLSTATE unwraps err to *api.Error and asserts that the SQLSTATE
// code matches want. Use after expectError for Java-conformance tests where
// the exact SQLSTATE is known.
func RequireSQLSTATE(t *testing.T, err error, want api.ErrorCode) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Code != want {
		t.Errorf("SQLSTATE: got %s, want %s (err: %v)", apiErr.Code, want, err)
	}
}
