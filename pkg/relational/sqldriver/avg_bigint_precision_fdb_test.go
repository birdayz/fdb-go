package sqldriver_test

// Probes AVG(BIGINT) precision above 2^53: the running sum must stay an EXACT
// int64 (Java AverageAccumulatorState.longAverageState — LongState SUM via
// Math.addExact) and convert to double ONCE at finish
// (total.doubleValue()/count; Cascades AVG_L is identical). Incremental
// float64 accumulation loses each +1 once the sum reaches 2^53
// (2^53 + 1 + 1 stays 2^53), yielding 3002399751580330.5 instead of the
// correct float64(2^53+2)/3 = 3002399751580331.5. Assertions are EXACT
// float64 equality — the defect is in the low-order bits, an epsilon would
// mask it.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AvgBigintPrecisionAbove2p53(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_avgbigprec")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_avgbigprec")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE avgbigprec CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_avgbigprec/s WITH TEMPLATE avgbigprec")
	dsn := fmt.Sprintf("fdbsql:///testdb_avgbigprec?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 9007199254740992 = 2^53; the two +1 rows are each absorbed (rounded away)
	// by an incremental float64 sum but preserved by the exact int64 sum.
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, v) VALUES (1, 9007199254740992), (2, 1), (3, 1)")

	var f float64
	if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM t").Scan(&f); err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := float64(int64(9007199254740994)) / 3 // 3002399751580331.5
	if f != want {
		t.Errorf("AVG over {2^53, 1, 1} = %v, want %v (exact int64 sum converted to double once); the lossy incremental-float64 result is %v",
			f, want, (float64(int64(1)<<53)+1+1)/3)
	}
}
