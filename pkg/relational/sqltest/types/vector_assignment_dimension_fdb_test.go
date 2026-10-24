package sqltest

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Go-only: raw vector bytes of another shape fail 22000 at execution (DIVERGENCES.md,
// "Raw bytes assigned to a VECTOR column"); Java refuses any bytes at planning.
func TestFDB_VectorAssignmentDimensionMismatchIs22000(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "vecdim",
		"CREATE TABLE dst (id BIGINT, v VECTOR(4, FLOAT), PRIMARY KEY (id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO dst VALUES (1, CAST([1.0, 0.0, 0.0, 0.0] AS VECTOR(4, FLOAT)))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	_, err := db.ExecContext(ctx, "UPDATE dst SET v = ? WHERE id = 1", []byte{1, 2, 3})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCannotConvertType ||
		apiErr.Message != "Vector precision or dimensions do not match the target column" {
		t.Fatalf("UPDATE of VECTOR(4) to 3 raw bytes: err = %v, want 22000 with the executor's message", err)
	}
}
