package sqldriver_test

// A non-finite float64 parameter (NaN / ±Infinity) has no BARE SQL literal
// form: "%g" renders it as NaN/+Inf/-Inf, which the parser reads as an
// identifier and rejects with a confusing 42601. It does have a CAST form, and
// substituteParams emits it — 'NaN', 'Infinity' and '-Infinity' are exactly the
// strings both Go's strconv.ParseFloat and Java's Double.parseDouble accept.
//
// The earlier answer was to refuse the parameter with 22023, which turned a
// TRANSPORT limitation of this driver into a TYPE restriction: a DOUBLE column
// stores these values happily through INSERT … VALUES, UPDATE and
// INSERT … SELECT, and only the `?` path could not reach them.
//
// Each value gets its OWN primary key. The previous revision reused id=1 for
// all three, so the second and third writes were rejected as duplicate keys and
// the assertion "an error came back" held for a reason that had nothing to do
// with the parameter — two thirds of it was vacuous.

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
		"CREATE SCHEMA TEMPLATE fspecialp CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fspecialp/s WITH TEMPLATE fspecialp")
	dsn := fmt.Sprintf("fdbsql:///testdb_fspecialp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	roundTrips := func(id int64, name string, v float64, ok func(float64) bool) {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx,
				"INSERT INTO t (id, d) VALUES (?, ?)", id, v); err != nil {
				if strings.Contains(err.Error(), "42601") {
					t.Fatalf("%s param produced a SYNTAX error (%v) — the interpolated form "+
						"is not parseable", name, err)
				}
				t.Fatalf("%s param rejected: %v — a DOUBLE column carries every IEEE-754 "+
					"value through every other syntax", name, err)
			}
			var got float64
			if err := db.QueryRowContext(ctx,
				fmt.Sprintf("SELECT d FROM t WHERE id = %d", id)).Scan(&got); err != nil {
				t.Fatalf("%s readback: %v", name, err)
			}
			if !ok(got) {
				t.Errorf("%s param round-tripped as %v (bits %#016x)", name, got, math.Float64bits(got))
			}
		})
	}
	roundTrips(1, "nan", math.NaN(), func(v float64) bool { return math.IsNaN(v) })
	roundTrips(2, "pos_inf", math.Inf(1), func(v float64) bool { return math.IsInf(v, 1) })
	roundTrips(3, "neg_inf", math.Inf(-1), func(v float64) bool { return math.IsInf(v, -1) })

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
