package sqldriver_test

// Regression pin for a signed-zero DISTINCT/dedup bug found reviewing CQ-27
// (TODO.md): packedDedupKey (pkg/recordlayer/query/executor/executor.go),
// the single encoder every unordered dedup path routes through (DISTINCT,
// UNION DISTINCT, CTE dedup), tuple-packed a FLOAT/DOUBLE slot verbatim.
// FDB tuple encoding preserves the IEEE sign bit, so a stored -0.0 and a
// stored +0.0 pack to two DIFFERENT keys — meaning `SELECT DISTINCT v`
// would silently return BOTH as separate rows, disagreeing with `=`
// (cmpAny, RFC-082, follows IEEE numeric equality: -0.0 == +0.0) and with
// Go's own map[float64] key semantics.
//
// DISTINCT is an EQUALITY concept (SQL `SELECT DISTINCT x` groups by `x`),
// not an ORDERING one — it must agree with cmpAny's `=`, not with the SORT
// total order (which stays sign-preserving on purpose per RFC-082, and
// rightly keeps -0.0 and +0.0 apart under ORDER BY: see
// negative_zero_index_sarg_probe_test.go's doc comment for that split).
// Fixed by canonicalizing a zero-valued FLOAT/DOUBLE slot to +0.0 before
// tuple-packing the dedup key.
import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NegativeZeroDistinctDedupProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nzdedup")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nzdedup")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nzdedup "+
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, PRIMARY KEY (id)) "+
		"CREATE TABLE f (id BIGINT NOT NULL, v FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzdedup/s WITH TEMPLATE nzdedup")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzdedup?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Both signed zeros AND a distinct nonzero control, twice each — proves
	// the fix collapses zero (regardless of which row's sign wrote the
	// canonical dedup key first) while still deduping/keeping everything
	// else normally.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, v) VALUES (1, -0.0), (2, 0.0), (3, 5.0), (4, 5.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id, v) VALUES (1, -0.0), (2, 0.0), (3, 5.0), (4, 5.0)")

	for _, tbl := range []string{"d", "f"} {
		tbl := tbl
		t.Run(tbl, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT DISTINCT v FROM %s", tbl))
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []float64
			for rows.Next() {
				var v float64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, v)
			}
			sort.Float64s(got)
			want := []float64{0, 5}
			if len(got) != len(want) {
				t.Fatalf("%s: SELECT DISTINCT v = %v (len %d), want %v (len %d) — signed zero "+
					"was not collapsed into one DISTINCT row", tbl, got, len(got), want, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: SELECT DISTINCT v = %v, want %v", tbl, got, want)
				}
			}
		})
	}
}
