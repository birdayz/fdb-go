package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

var scalarZeroFixtureID atomic.Uint64

func TestFDB_ScalarMathSignedZero(t *testing.T) {
	t.Parallel()
	name := fmt.Sprintf("scalarzero_%d", scalarZeroFixtureID.Add(1))
	db := setupErrorTestDB(t, "/testdb_"+name, name,
		"CREATE TABLE t (id BIGINT, z DOUBLE, f FLOAT, d DOUBLE, PRIMARY KEY (id)) "+
			"CREATE TABLE p (id BIGINT, v DOUBLE, PRIMARY KEY (id)) "+
			"CREATE TABLE q (id BIGINT, n BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE ints (id BIGINT, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_ints_k ON ints (k)")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	// The setup deadline is canceled before parallel children resume. Each
	// child owns its deadline so waiting for a test slot cannot consume it.
	defer cancel()
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (1, -0.0, CAST(-0.0 AS FLOAT), -0.25), (2, 0.0, CAST(0.0 AS FLOAT), 0.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO q VALUES (9, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO ints VALUES (1, 5), (2, 6), (3, 5), (4, NULL), (5, 7)")
	var z, f float64
	if err := db.QueryRowContext(ctx, "SELECT z, f FROM t WHERE id = 1").Scan(&z, &f); err != nil {
		t.Fatal(err)
	}
	if z != 0 || f != 0 || !math.Signbit(z) || !math.Signbit(f) {
		t.Fatalf("stored operands must carry negative zero: z=%v, f=%v", z, f)
	}
	for _, expr := range []string{
		"FLOOR(z)", "FLOOR(f)", "FLOOR(-0.0)",
		"CEIL(z)", "CEIL(f)", "CEIL(d)", "CEIL(-0.25)",
		"CEILING(z)", "CEILING(f)", "CEILING(d)", "CEILING(-0.25)",
		"ROUND(z)", "ROUND(f)", "ROUND(d)", "ROUND(-0.25)",
		"ROUND(d, 0)", "ROUND(d, -1)", "ROUND(-0.001, 2)",
		"ROUND(CAST(d AS FLOAT), -1)", "ROUND(d, -9223372036854775808)",
		"POWER(z, 3)", "POWER(f, 3)", "POWER(-0.0, 3)",
		"POW(z, 3)", "POW(f, 3)", "POW(-0.0, 3)",
		"POWER(-0.000000000000000000000000000001, 13)",
	} {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var result, reciprocal float64
			query := "SELECT " + expr + ", 1.0 / " + expr + " FROM t WHERE id = 1"
			if err := db.QueryRowContext(ctx, query).Scan(&result, &reciprocal); err != nil {
				t.Fatal(err)
			}
			if result != 0 || !math.Signbit(result) {
				t.Errorf("%s = %v, want -0 (bitwise)", expr, result)
			}
			if !math.IsInf(reciprocal, -1) {
				t.Errorf("1.0 / %s = %v, want -Infinity", expr, reciprocal)
			}
		})
	}
	for _, query := range []string{
		"SELECT s, COUNT(*) FROM (SELECT CEIL(d) AS s FROM t) a GROUP BY s",
		"SELECT DISTINCT CEIL(d), 1 FROM t",
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			signs := map[bool]int{}
			for rows.Next() {
				var value float64
				var count int64
				if err := rows.Scan(&value, &count); err != nil {
					t.Fatal(err)
				}
				if value != 0 || count != 1 {
					t.Errorf("row = (%v, %d), want signed zero and count 1", value, count)
				}
				signs[math.Signbit(value)]++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if signs[false] != 1 || signs[true] != 1 {
				t.Errorf("zero sign populations = %v, want one +0 and one -0", signs)
			}
		})
	}
	t.Run("bound parameter", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, input := range []float64{math.Copysign(0, -1), 0, 3, 3.5} {
			var got any
			if err := db.QueryRowContext(ctx, "SELECT FLOOR(?) FROM t WHERE id = 1", input).Scan(&got); err != nil {
				t.Fatal(err)
			}
			value, ok := got.(float64)
			if !ok || math.Float64bits(value) != math.Float64bits(math.Floor(input)) {
				t.Errorf("FLOOR(%v) = %T(%v), want floating floor with sign preserved", input, got, got)
			}
		}
	})
	t.Run("parameter arithmetic and storage", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for id, input := range []float64{math.Copysign(0, -1), 0, 3, -3, 3.5, math.SmallestNonzeroFloat64, math.MaxFloat64} {
			var result, quotient any
			if err := db.QueryRowContext(ctx, "SELECT ?, ? / 2 FROM t WHERE id = 1", input, input).Scan(&result, &quotient); err != nil {
				t.Fatal(err)
			}
			for _, pair := range []struct {
				got  any
				want float64
			}{{result, input}, {quotient, input / 2}} {
				f, ok := pair.got.(float64)
				if !ok || math.Float64bits(f) != math.Float64bits(pair.want) {
					t.Errorf("parameter %v: got %T(%v), want float64(%v) bitwise", input, pair.got, pair.got, pair.want)
				}
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO p VALUES (?, ?)", int64(id), input); err != nil {
				t.Fatal(err)
			}
			var stored float64
			if err := db.QueryRowContext(ctx, "SELECT v FROM p WHERE id = ?", int64(id)).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if math.Float64bits(stored) != math.Float64bits(input) {
				t.Errorf("stored parameter %v = %v, want identical bits", input, stored)
			}
		}
		var integer any
		if err := db.QueryRowContext(ctx, "SELECT FLOOR(?) FROM t WHERE id = 1", int64(9007199254740993)).Scan(&integer); err != nil || integer != int64(9007199254740993) {
			t.Errorf("integer FLOOR parameter = (%v, %v), want exact int64", integer, err)
		}
	})
	t.Run("integer positions", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, tc := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO q VALUES (1, ?)", []any{float64(3)}},
			{"INSERT INTO q VALUES (2, 3.0)", nil},
			{"UPDATE q SET n = ? WHERE id = 9", []any{float64(3)}},
			{"UPDATE q SET n = 3.0 WHERE id = 9", nil},
		} {
			_, err := db.ExecContext(ctx, tc.query, tc.args...)
			var sqlErr *api.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeCannotConvertType {
				t.Errorf("integer assignment %s => %v, want 22000 like a DOUBLE literal", tc.query, err)
			}
		}
		for _, tc := range []struct {
			query string
			args  []any
			code  api.ErrorCode
			want  any
		}{
			{"SELECT id FROM t ORDER BY id LIMIT ?", []any{float64(3)}, api.ErrCodeSyntaxError, nil},
			{"SELECT id FROM t ORDER BY id LIMIT 3.0", nil, api.ErrCodeSyntaxError, nil},
			{"SELECT id FROM t ORDER BY id LIMIT ?", []any{int64(3)}, "", int64(1)},
			{"SELECT id FROM t ORDER BY id LIMIT 1 OFFSET ?", []any{float64(3)}, api.ErrCodeSyntaxError, nil},
			{"SELECT id FROM t ORDER BY id LIMIT 1 OFFSET 3.0", nil, api.ErrCodeSyntaxError, nil},
			{"SELECT id FROM t ORDER BY id LIMIT 1 OFFSET ?", []any{int64(1)}, "", int64(2)},
			{"SELECT ROUND(1234, ?) FROM t WHERE id = 1", []any{float64(3)}, "", int64(1234)},
			{"SELECT ROUND(1234, 3.0) FROM t WHERE id = 1", nil, "", int64(1234)},
		} {
			var result any
			err := db.QueryRowContext(ctx, tc.query, tc.args...).Scan(&result)
			var sqlErr *api.Error
			var code api.ErrorCode
			if errors.As(err, &sqlErr) {
				code = sqlErr.Code
			}
			if code != tc.code || result != tc.want || (tc.code == "" && err != nil) {
				t.Errorf("integer position %s => %T(%v), code=%s, err=%v; want %T(%v), code=%s", tc.query, result, result, code, err, tc.want, tc.want, tc.code)
			}
		}
	})
	t.Run("indexed integer predicates", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		isPointLookup := func(plan string) bool {
			return strings.Contains(plan, "IndexScan(IDX_INTS_K, [=]") && !strings.Contains(plan, "[*]")
		}
		for _, broad := range []string{
			"PredicatesFilter(IndexScan(IDX_INTS_K, [*]))",
			"Union(IndexScan(IDX_INTS_K, [=]), IndexScan(IDX_INTS_K, [*]))",
		} {
			if isPointLookup(broad) {
				t.Fatalf("broad index scan passed the point-lookup check: %s", broad)
			}
		}
		var control string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT id FROM ints WHERE k + 0 = ? ORDER BY k", float64(5)).Scan(&control); err != nil {
			t.Fatal(err)
		}
		t.Logf("full-index-scan negative control: %s", control)
		if !strings.Contains(control, "IndexScan(IDX_INTS_K, [*]") {
			t.Fatalf("negative control must use the same index without equality bounds: %s", control)
		}
		if isPointLookup(control) {
			t.Fatalf("non-sargable control passed the point-lookup check: %s", control)
		}
		plans := map[string]string{}
		for _, tc := range []struct {
			family string
			query  string
			args   []any
			want   []int64
		}{
			{"eq", "SELECT id FROM ints WHERE k = ?", []any{float64(5)}, []int64{1, 3}},
			{"eq", "SELECT id FROM ints WHERE k = 5.0", nil, []int64{1, 3}},
			{"eq", "SELECT id FROM ints WHERE k = ?", []any{int64(5)}, []int64{1, 3}},
			{"in", "SELECT id FROM ints WHERE k IN (?, ?)", []any{float64(5), float64(6)}, []int64{1, 2, 3}},
			{"in", "SELECT id FROM ints WHERE k IN (5.0, 6.0)", nil, []int64{1, 2, 3}},
			{"in", "SELECT id FROM ints WHERE k IN (?, ?)", []any{int64(5), int64(6)}, []int64{1, 2, 3}},
		} {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query, tc.args...).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			if !isPointLookup(plan) {
				t.Errorf("%s: lost secondary BIGINT equality bounds: %s", tc.query, plan)
			}
			if expected, ok := plans[tc.family]; ok {
				if plan != expected {
					t.Errorf("%s: literal types changed the plan:\ngot  %s\nwant %s", tc.query, plan, expected)
				}
			} else {
				plans[tc.family] = plan
			}
			rows, err := db.QueryContext(ctx, tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[int64]int{}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				seen[id]++
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(seen) != len(tc.want) {
				t.Errorf("%s: got %v, want %v", tc.query, seen, tc.want)
			}
			for _, id := range tc.want {
				if seen[id] != 1 {
					t.Errorf("%s: id %d appeared %d times, want once", tc.query, id, seen[id])
				}
			}
		}
	})
	t.Run("floating order key is not a position", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, tc := range []struct {
			query string
			args  []any
		}{
			{"SELECT id FROM t ORDER BY ?", []any{float64(2)}},
			{"SELECT id FROM t ORDER BY 2.0", nil},
			{"SELECT id FROM t ORDER BY ?", []any{int64(1)}},
		} {
			rows, err := db.QueryContext(ctx, tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[int64]int{}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				seen[id]++
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			// A floating 2 is a constant key, not a nonexistent second
			// SELECT-list position. Tie order is deliberately unconstrained.
			if len(seen) != 2 || seen[1] != 1 || seen[2] != 1 {
				t.Errorf("%s returned %v, want each source row once", tc.query, seen)
			}
		}
	})
	t.Run("integer order key retains position interpretation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var got any
		err := db.QueryRowContext(ctx, "SELECT id FROM t ORDER BY ?", int64(2)).Scan(&got)
		var sqlErr *api.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != api.ErrCodeInvalidParameter || sqlErr.Message != "ORDER BY position 2 is out of range: SELECT list has 1 entries" {
			t.Errorf("integer ORDER BY 2 = (%v, %v), want positional 22023", got, err)
		}
	})
	t.Run("sequential constants", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		for _, tc := range []struct {
			expr     string
			negative bool
		}{
			{"FLOOR(-0.0)", true},
			{"FLOOR(0.0)", false},
			{"FLOOR(-0.0)", true},
			{"CEIL(-0.25)", true},
			{"CEIL(0.0)", false},
			{"CEIL(-0.25)", true},
			{"POWER(-0.0, 3)", true},
			{"POWER(-0.0, 2)", false},
			{"POWER(-0.0, 3)", true},
		} {
			var got float64
			if err := conn.QueryRowContext(ctx, "SELECT "+tc.expr+" FROM t WHERE id = 1").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != 0 || math.Signbit(got) != tc.negative {
				t.Errorf("%s = %v; want zero with negative=%v", tc.expr, got, tc.negative)
			}
		}
	})
}
