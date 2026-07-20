package sqldriver_test

// Pins the NULLIF boundary discovered while adding COALESCE to the RFC-182
// rowdiff harness: the engine supports COALESCE but rejects NULLIF with
// `0AF00: Unsupported operator NULLIF`. NULLIF is a capability the engine lacks,
// not a soundness gap, so the harness generates only COALESCE. This locks in the
// LOUD rejection so a future NULLIF implementation can't regress to silently
// wrong results unnoticed; flip the reject arm to a row assertion when it lands.
import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_Nullif_Unsupported(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nullif")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nullif")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nulliftpl "+
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nullif/s WITH TEMPLATE nulliftpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_nullif?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,a) VALUES (1,7),(2,3)")

	// COALESCE is supported and NULL-absorbing (a non-NULL row → its value).
	t.Run("coalesce_ok", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE COALESCE(a, 5) = 7")
		if err != nil {
			t.Fatalf("COALESCE must be supported, got: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if n != 1 {
			t.Errorf("COALESCE(a,5)=7 = %d rows, want 1", n)
		}
	})

	// NULLIF must decline LOUDLY (0AF00), never silently.
	t.Run("nullif_rejected", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE NULLIF(a, 3) = 7")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
			}
			err = rows.Err()
		}
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("NULLIF error = %v, want 0AF00 (unsupported operator)", err)
		}
	})
}
