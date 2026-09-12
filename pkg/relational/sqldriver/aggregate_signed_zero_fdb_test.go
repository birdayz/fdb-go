package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// A negative zero is not the additive identity for +0. Java seeds SUM/AVG
// with the first non-null operand, so an all-negative-zero group stays -0.
func TestFDB_AggregateSignedZero(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_aggsignedzero", "aggsignedzero",
		"CREATE TABLE t (id BIGINT, g BIGINT, d DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX t_gid_d ON t (g, id, d)")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES "+
		"(1, 1, NULL), (2, 1, -0.0), (3, 1, NULL), (4, 1, -0.0), "+
		"(5, 2, 0.0), (6, 2, -0.0), (7, 3, NULL), (8, 3, NULL), "+
		"(9, 4, -1.0), (10, 4, 1.0), (11, 5, -0.0)")
	// Confirm the input itself retains the sign; otherwise an aggregate check
	// could blame the wrong boundary or pass over a non-discriminating fixture.
	var stored float64
	if err := db.QueryRowContext(ctx, "SELECT d FROM t WHERE id = 2").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 || !math.Signbit(stored) {
		t.Fatalf("stored d = %v bits=%016x, want negative zero", stored, math.Float64bits(stored))
	}

	const grouped = "SELECT g, SUM(d), AVG(d) FROM t GROUP BY g ORDER BY g"
	plan := planExplainVia(t, ctx, db, grouped)
	if !strings.Contains(plan, "StreamingAgg") || !strings.Contains(plan, "Index") || strings.Contains(plan, "Sort") {
		t.Fatalf("require a streaming aggregate over ordered index input, without a buffering sort: %s", plan)
	}

	for _, budget := range []int{1_000_000, 1, 2, 3, 4} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			conn := pagedConn(t, db, budget)
			check := func(q string, grouped bool, want []int64) {
				t.Helper()
				rows, err := conn.QueryContext(ctx, q)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				defer rows.Close()
				n := 0
				for rows.Next() {
					if n >= len(want) {
						t.Fatalf("%s: extra row", q)
					}
					g := want[n]
					var sum, avg sql.NullFloat64
					if grouped {
						err = rows.Scan(&g, &sum, &avg)
					} else {
						err = rows.Scan(&sum, &avg)
					}
					if err != nil {
						t.Fatal(err)
					}
					if g != want[n] {
						t.Errorf("row %d: group %d, want %d", n, g, want[n])
					}
					for name, v := range map[string]sql.NullFloat64{"SUM": sum, "AVG": avg} {
						wantNull := g == 3 || g == 99
						wantNegative := g == 1 || g == 5
						if v.Valid == wantNull || (v.Valid && (v.Float64 != 0 || math.Signbit(v.Float64) != wantNegative)) {
							t.Errorf("%s group %d: %s = %+v bits=%016x, want null=%v negative-zero=%v",
								q, g, name, v, math.Float64bits(v.Float64), wantNull, wantNegative)
						}
					}
					n++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if n != len(want) {
					t.Errorf("%s: %d rows, want %d", q, n, len(want))
				}
			}
			check(grouped, true, []int64{1, 2, 3, 4, 5})
			for _, g := range []int64{1, 2, 3, 4, 5, 99} {
				check(fmt.Sprintf("SELECT SUM(d), AVG(d) FROM t WHERE g = %d", g), false, []int64{g})
			}
			t.Logf("AGGREGATE-SIGNED-ZERO budget=%d: grouped and scalar results checked bitwise", budget)
		})
	}
}
