package sqltest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Go-only: raw vector bytes are written, and bytes of another shape fail 22000 at
// execution (DIVERGENCES.md, "Raw bytes assigned to a VECTOR column").
func TestFDB_VectorAssignmentDimensionMismatchIs22000(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "vecdim",
		"CREATE TABLE dst (id BIGINT, v VECTOR(4, FLOAT), PRIMARY KEY (id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dst VALUES (1, CAST([1.0, 0.0, 0.0, 0.0] AS VECTOR(4, FLOAT)))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// Well-formed bytes of the column's shape are written as given.
	if _, err := db.ExecContext(ctx, "INSERT INTO dst VALUES (2, CAST([0.0, 0.0, 0.0, 1.0] AS VECTOR(4, FLOAT)))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var stored []byte
	if err := db.QueryRowContext(ctx, "SELECT v FROM dst WHERE id = 2").Scan(&stored); err != nil {
		t.Fatalf("read vector bytes: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE dst SET v = ? WHERE id = 1", stored); err != nil {
		t.Fatalf("UPDATE with well-formed VECTOR(4) bytes: %v", err)
	}
	var written []byte
	if err := db.QueryRowContext(ctx, "SELECT v FROM dst WHERE id = 1").Scan(&written); err != nil || !bytes.Equal(written, stored) {
		t.Fatalf("row 1 vector = %x, %v; want the bytes written, %x", written, err, stored)
	}

	_, err := db.ExecContext(ctx, "UPDATE dst SET v = ? WHERE id = 1", []byte{1, 2, 3})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCannotConvertType ||
		apiErr.Message != "Vector precision or dimensions do not match the target column" {
		t.Fatalf("UPDATE of VECTOR(4) to 3 raw bytes: err = %v, want 22000 with the executor's message", err)
	}
}
