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

func TestFDB_AggregateFloatPrecision(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_aggfloatprecision", "aggfloatprecision",
		"CREATE TABLE t (id BIGINT, g BIGINT, f FLOAT, d DOUBLE, PRIMARY KEY (g, id))")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES "+
		"(1, 1, CAST(16777216 AS FLOAT), 16777216.0), "+
		"(2, 1, CAST(1 AS FLOAT), 1.0), "+
		"(3, 1, CAST(-16777216 AS FLOAT), -16777216.0), "+
		"(4, 2, CAST(3.4028234663852886e38 AS FLOAT), 3.4028234663852886e38), "+
		"(5, 2, CAST(3.4028234663852886e38 AS FLOAT), 3.4028234663852886e38), "+
		"(6, 3, NULL, NULL), (7, 3, NULL, NULL), "+
		"(8, 4, NULL, NULL), (9, 4, CAST(-0.0 AS FLOAT), -0.0), (10, 4, CAST(-0.0 AS FLOAT), -0.0), "+
		"(11, 5, CAST(16777216 AS FLOAT), 16777216.0), "+
		"(12, 5, CAST(1 AS FLOAT), 1.0), (13, 5, CAST(1 AS FLOAT), 1.0)")
	// Verify stored FLOATs reach the driver as widened doubles with their
	// values/sign intact, rather than blaming arithmetic for a storage change.
	for id, want := range map[int]float64{1: 1 << 24, 4: math.MaxFloat32, 9: math.Copysign(0, -1)} {
		var got float64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT f FROM t WHERE id = %d", id)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("stored FLOAT id=%d: got %v, want %v (bitwise)", id, got, want)
		}
	}
	const columns = "SUM(f), AVG(f), SUM(d), AVG(d), SUM(CAST(f AS DOUBLE)), AVG(CAST(d AS FLOAT)), " +
		"SUM(CASE WHEN id > 0 THEN f ELSE CAST(0 AS FLOAT) END)"
	const grouped = "SELECT g, " + columns + " FROM t GROUP BY g ORDER BY g"
	for _, q := range []string{grouped, "SELECT " + columns + " FROM t WHERE g = 1"} {
		plan := planExplainVia(t, ctx, db, q)
		if !strings.Contains(plan, "StreamingAgg") || !strings.Contains(plan, "Scan") || strings.Contains(plan, "Sort") {
			t.Fatalf("require streaming aggregation over primary-key-ordered input without a sort: %s", plan)
		}
	}
	negZero := math.Copysign(0, -1)
	wants := map[int64][]any{
		1:  {float64(0), float64(0), float64(1), 1.0 / 3, float64(1), float64(0), float64(0)},
		2:  {math.Inf(1), math.Inf(1), float64(math.MaxFloat32) * 2, float64(math.MaxFloat32), float64(math.MaxFloat32) * 2, math.Inf(1), math.Inf(1)},
		3:  {nil, nil, nil, nil, nil, nil, nil},
		4:  {negZero, negZero, negZero, negZero, negZero, negZero, negZero},
		5:  {float64(1 << 24), float64(1<<24) / 3, float64(1<<24 + 2), float64(1<<24+2) / 3, float64(1<<24 + 2), float64(1<<24) / 3, float64(1 << 24)},
		99: {nil, nil, nil, nil, nil, nil, nil},
	}
	for _, budget := range []int{1_000_000, 1, 2, 3, 4} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			conn := pagedConn(t, db, budget)
			check := func(q string, groups []int64, grouped bool) {
				t.Helper()
				rows, err := conn.QueryContext(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				n := 0
				for rows.Next() {
					if n >= len(groups) {
						t.Fatalf("%s: extra row", q)
					}
					g := groups[n]
					got := make([]sql.NullFloat64, 7)
					args := make([]any, 0, 8)
					if grouped {
						args = append(args, &g)
					}
					for i := range got {
						args = append(args, &got[i])
					}
					if err := rows.Scan(args...); err != nil {
						t.Fatal(err)
					}
					if g != groups[n] {
						t.Fatalf("%s: group %d, want %d", q, g, groups[n])
					}
					for i, want := range wants[g] {
						if want == nil {
							if got[i].Valid {
								t.Errorf("group %d slot %d: got %v, want NULL", g, i, got[i])
							}
						} else if !got[i].Valid || math.Float64bits(got[i].Float64) != math.Float64bits(want.(float64)) {
							t.Errorf("group %d slot %d: got %+v, want %v (bitwise); query=%s", g, i, got[i], want, q)
						}
					}
					n++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if n != len(groups) {
					t.Errorf("%s: got %d rows, want %d", q, n, len(groups))
				}
			}
			check(grouped, []int64{1, 2, 3, 4, 5}, true)
			for _, g := range []int64{1, 2, 3, 4, 5, 99} {
				check(fmt.Sprintf("SELECT %s FROM t WHERE g = %d", columns, g), []int64{g}, false)
			}
			t.Logf("AGGREGATE-FLOAT-PRECISION budget=%d: grouped/scalar, CAST, CASE, NULL, overflow and signed zero checked", budget)
		})
	}
}
