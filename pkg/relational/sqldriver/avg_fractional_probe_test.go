package sqldriver_test

// Probes AVG return type/value: AVG over an integer column returns a DOUBLE true
// average (1.5), NOT an integer-truncated value (1). Matches Java —
// NumericAggregationValue's AVG_I/AVG_L/AVG_F/AVG_D all produce TypeCode.DOUBLE.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
)

func TestFDB_AvgFractionalProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_avgfracp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_avgfracp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE avgfracp CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_avgfracp/s WITH TEMPLATE avgfracp")
	dsn := fmt.Sprintf("fdbsql:///testdb_avgfracp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 1), (2, 2)")

	t.Run("avg_int_is_fractional_double", func(t *testing.T) {
		var f float64
		if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM t").Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(f-1.5) > 1e-9 {
			t.Errorf("AVG({1,2}) = %v, want 1.5 (true average, not integer-truncated 1)", f)
		}
	})
	t.Run("avg_column_type_is_double", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT AVG(v) FROM t")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cts, _ := rows.ColumnTypes()
		if len(cts) != 1 || cts[0].DatabaseTypeName() != "DOUBLE" {
			t.Errorf("AVG column type = %v, want DOUBLE", cts)
		}
	})
	t.Run("avg_whole_result", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (3, 3)")
		var f float64
		if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM t").Scan(&f); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if math.Abs(f-2.0) > 1e-9 {
			t.Errorf("AVG({1,2,3}) = %v, want 2.0", f)
		}
	})
}
