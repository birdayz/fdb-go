package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// Check bits, not IEEE equality: -0.0 == +0.0 would hide the regression.
func TestFDB_AggregateSumAvgSignedZero(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const path = "/testdb_aggregate_signed_zero"
	setup := openTestDB(t, path)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+path)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE aggregate_signed_zero "+
		"CREATE TABLE t (id BIGINT, g BIGINT, d DOUBLE, f FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+path+"/s WITH TEMPLATE aggregate_signed_zero")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", path, clusterFilePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES "+
		"(1,1,-0.0,-0.0),(2,1,-0.0,-0.0),"+
		"(3,2,0.0,0.0),(4,2,0.0,0.0),"+
		"(5,3,-0.0,-0.0),(6,3,0.0,0.0),"+
		"(7,4,-1.0,-1.0),(8,4,1.0,1.0),"+
		"(9,5,NULL,NULL),(10,5,NULL,NULL),"+
		"(11,6,NULL,NULL),(12,6,-0.0,-0.0),"+
		"(13,7,-0.0,-0.0)")
	var storedD, storedF float64
	if err := db.QueryRowContext(ctx, "SELECT d, f FROM t WHERE id = 1").Scan(&storedD, &storedF); err != nil {
		t.Fatal(err)
	}
	negZero := math.Copysign(0, -1)
	for _, v := range []float64{storedD, storedF} {
		if math.Float64bits(v) != math.Float64bits(negZero) {
			t.Fatalf("fixture did not store negative zero: bits=%016x", math.Float64bits(v))
		}
	}
	const grouped = "SELECT g, SUM(d), AVG(d), SUM(f), AVG(f) FROM t GROUP BY g ORDER BY g"
	plan := floatOrderingExplain(t, db, ctx, grouped)
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected streaming accumulation, got %s", plan)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := []sql.NullFloat64{
		{Float64: negZero, Valid: true},
		{Float64: 0, Valid: true},
		{Float64: 0, Valid: true},
		{Float64: 0, Valid: true},
		{},
		{Float64: negZero, Valid: true},
		{Float64: negZero, Valid: true},
	}
	check := func(query string, got []sql.NullFloat64, expected sql.NullFloat64) {
		t.Helper()
		for i, v := range got {
			if v.Valid != expected.Valid || (v.Valid && math.Float64bits(v.Float64) != math.Float64bits(expected.Float64)) {
				t.Errorf("%s column %d: got %v bits=%016x, want %v bits=%016x", query, i, v, math.Float64bits(v.Float64), expected, math.Float64bits(expected.Float64))
			}
		}
	}
	for _, budget := range []int{1_000_000, 1, 2, 3} {
		if err := conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("unexpected connection %T", dc)
			}
			ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, budget).Build())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := conn.QueryContext(ctx, grouped)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var g int64
			got := make([]sql.NullFloat64, 4)
			if err := rows.Scan(&g, &got[0], &got[1], &got[2], &got[3]); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if n >= len(want) || g != int64(n+1) {
				rows.Close()
				t.Fatalf("budget=%d group=%d at row=%d", budget, g, n)
			}
			check(fmt.Sprintf("group=%d budget=%d", g, budget), got, want[n])
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil || n != len(want) {
			t.Fatalf("budget=%d: groups=%d want=%d err=%v", budget, n, len(want), err)
		}
		for g, expected := range append(want, sql.NullFloat64{}) {
			query := fmt.Sprintf("SELECT SUM(d), AVG(d), SUM(f), AVG(f) FROM t WHERE g = %d", g+1)
			got := make([]sql.NullFloat64, 4)
			if err := conn.QueryRowContext(ctx, query).Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
				t.Fatalf("budget=%d %s: %v", budget, query, err)
			}
			check(fmt.Sprintf("%s budget=%d", query, budget), got, expected)
		}
		t.Logf("AGGREGATE-SIGNED-ZERO budget=%d groups=%d scalar_queries=%d", budget, n, len(want)+1)
	}
}
