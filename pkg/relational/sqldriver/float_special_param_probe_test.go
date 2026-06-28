package sqldriver_test

// Regression: a non-finite float64 parameter (NaN / ±Inf) has no SQL literal form
// under the driver's text-interpolation param path. Previously it rendered as
// "NaN"/"+Inf" and the parser rejected it with a confusing 42601 syntax error;
// now substituteParams rejects it up front with a clear invalid-parameter error.
// Finite floats (incl. very large/small) still round-trip.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_FloatSpecialParamProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fspecialp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fspecialp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fspecialp CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fspecialp/s WITH TEMPLATE fspecialp")
	dsn := fmt.Sprintf("fdbsql:///testdb_fspecialp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rejected := func(name string, v float64) {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, "INSERT INTO t (id, d) VALUES (1, ?)", v)
			if err == nil {
				t.Fatalf("%s param unexpectedly accepted", name)
			}
			// clear, type-accurate rejection (not a confusing 42601 syntax error).
			if strings.Contains(err.Error(), "42601") {
				t.Errorf("%s gave a syntax error (%v); want a clear non-finite param rejection", name, err)
			}
		})
	}
	rejected("nan", math.NaN())
	rejected("pos_inf", math.Inf(1))
	rejected("neg_inf", math.Inf(-1))

	t.Run("finite_floats_roundtrip", func(t *testing.T) {
		for i, v := range []float64{0.0, -1.5, 1e300, -1e-300, math.MaxFloat64} {
			id := 100 + i
			if _, err := db.ExecContext(ctx, "INSERT INTO t (id, d) VALUES (?, ?)", int64(id), v); err != nil {
				t.Fatalf("finite %v insert: %v", v, err)
			}
			var got float64
			if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT d FROM t WHERE id = %d", id)).Scan(&got); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got != v {
				t.Errorf("finite float %v round-trip = %v", v, got)
			}
		}
	})
}
