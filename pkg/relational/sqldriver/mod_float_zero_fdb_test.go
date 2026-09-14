package sqldriver_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

// Go's MOD() function follows the infix remainder operator: approximate
// numeric operands yield NaN for a zero divisor, not an integer 22012 error.
func TestFDB_ModFloatZero(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_modfloatzero", "modfloatzero",
		"CREATE TABLE t (id BIGINT, n BIGINT, f FLOAT, d DOUBLE, PRIMARY KEY (id))")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, 7, CAST(7 AS FLOAT), 7.0)")
	for _, expr := range []string{"-0.0", "CAST(-0.0 AS FLOAT)"} {
		var got float64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 0 || !math.Signbit(got) {
			t.Fatalf("signed-zero operand %s = %v, want -0", expr, got)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  float64
	}{
		{"SELECT MOD(7.0, 0.0) FROM t", math.NaN()},
		{"SELECT MOD(7.0, 2.0) FROM t", 1},
		{"SELECT MOD(7.0, 0.0) FROM t", math.NaN()},
	} {
		var got float64
		if err := conn.QueryRowContext(ctx, tc.query).Scan(&got); err != nil {
			t.Error(err)
		} else if (math.IsNaN(tc.want) && !math.IsNaN(got)) || (!math.IsNaN(tc.want) && got != tc.want) {
			t.Errorf("sequential %s = %v, want %v", tc.query, got, tc.want)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{
		"MOD(d, 0.0)", "MOD(d, -0.0)", "MOD(n, 0.0)", "MOD(d, 0)",
		"MOD(f, CAST(0 AS FLOAT))", "MOD(f, CAST(-0.0 AS FLOAT))", "MOD(f, 0)",
		"MOD(7.0, 0.0)", "MOD(0.0, 0.0)",
		"MOD(CAST('Infinity' AS DOUBLE), d)", "MOD(d, CAST('NaN' AS DOUBLE))",
		"d % 0.0", "f % CAST(0 AS FLOAT)", "d MOD 0.0",
	} {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			var got float64
			if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&got); err != nil {
				t.Fatalf("%s: %v; want NaN without an error", expr, err)
			}
			if !math.IsNaN(got) {
				t.Fatalf("%s = %v, want NaN", expr, got)
			}
		})
	}
	for _, tc := range []struct {
		expr string
		want any
	}{
		{"MOD(n, 3)", int64(1)},
		{"MOD(d, 2.5)", float64(2)},
		{"MOD(0.0 - d, 2.5)", float64(-2)},
		{"MOD(d, CAST('Infinity' AS DOUBLE))", float64(7)},
		{"MOD(-0.0, d)", math.Copysign(0, -1)},
		{"MOD(0.0 - d, d)", math.Copysign(0, -1)},
		{"MOD(CAST(NULL AS DOUBLE), 0.0)", nil},
		{"MOD(d, CAST(NULL AS DOUBLE))", nil},
		{"MOD(CAST(16777217 AS FLOAT), CAST(2 AS FLOAT))", float64(0)},
		{"MOD(CAST(16777216 AS FLOAT), CAST(16777217 AS BIGINT))", float64(0)},
		{"MOD(CAST(16777217 AS BIGINT), CAST(16777216 AS FLOAT))", float64(0)},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			var got any
			if err := db.QueryRowContext(ctx, "SELECT "+tc.expr+" FROM t WHERE id = 1").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want, ok := tc.want.(float64); ok {
				f, ok := got.(float64)
				if !ok || math.Float64bits(f) != math.Float64bits(want) {
					t.Fatalf("got %T(%v), want %v (bitwise)", got, got, want)
				}
			} else if got != tc.want {
				t.Fatalf("got %T(%v), want %T(%v)", got, got, tc.want, tc.want)
			}
		})
	}
	for _, expr := range []string{"MOD(n, 0)", "MOD(CAST(n AS INTEGER), CAST(0 AS INTEGER))", "n % 0"} {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			var got any
			err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&got)
			var sqlErr *api.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeDivisionByZero {
				t.Fatalf("%s: got (%v, %v), want integral 22012", expr, got, err)
			}
		})
	}
}

func TestFDB_ModFunctionIndexRemainsRejected(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_modindexboundary", "modindexboundary",
		"CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := db.ExecContext(ctx, `CREATE SCHEMA TEMPLATE modindexfunction
		CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))
		CREATE INDEX i_mod AS SELECT MOD(n, 3) FROM t`)
	var sqlErr *api.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeUnsupportedOperation ||
		sqlErr.Message != "unable to construct expression" {
		t.Fatalf("MOD() index: %v; want unsupported operation, not new persisted index-expression admission", err)
	}
	mwjoMustExec(t, db, ctx, `CREATE SCHEMA TEMPLATE modindexoperator
		CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))
		CREATE INDEX i_mod AS SELECT n % 3 FROM t`)
}
