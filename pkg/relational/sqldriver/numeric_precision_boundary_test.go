package sqldriver_test

// Pins that widening an int constant to DOUBLE stays LOSSY above 2^53, because
// that is what SQL requires — and a well-meaning "fix" would break it.
//
// float64 has a 53-bit mantissa; int64 reaches ~9.2e18. So `double_col = 2^53+1`
// converts the constant to 2^53 and matches a stored 2^53. That looks like a
// precision bug and is not: SQL converts an exact numeric to approximate when
// the two are compared, so the conversion is the specified semantics. Postgres
// behaves the same way.
//
// The hazard this guards is the opposite direction. The FLOAT (binary32) sibling
// helper DOES check exactness before narrowing, because a non-exact float32
// constant probes a tuple type code that cannot match any index entry — it
// returns no rows rather than imprecise ones. Reading those two side by side
// invites "the DOUBLE path is missing an exactness check", and adding one there
// would decline a mandated conversion and turn correct rows into missing ones.
// An earlier comment in expr.go actively encouraged that by claiming int→double
// is "always exact for any realistic int64", which is false.
//
// So this test exists to make the lossy behaviour deliberate and visible rather
// than something a future reader "corrects".

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NumericPrecisionBoundary(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_npb")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_npb")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE npb "+
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, PRIMARY KEY (id)) "+
		"CREATE INDEX d_v ON d (v) "+
		"CREATE TABLE b (id BIGINT NOT NULL, n BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX b_n ON b (n)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_npb/s WITH TEMPLATE npb")
	dsn := fmt.Sprintf("fdbsql:///testdb_npb?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// 2^53 = 9007199254740992. 2^53+1 is NOT representable in float64.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, v) VALUES (1, 9007199254740992), (2, 9007199254740994)")
	// A BIGINT column holds 2^53+1 exactly.
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, n) VALUES "+
		"(1, 9007199254740992), (2, 9007199254740993), (3, 9007199254740994)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ids := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	for _, tc := range []struct {
		query string
		want  []int64
		why   string
	}{
		{
			"SELECT id FROM d WHERE v = 9007199254740993",
			[]int64{1},
			"2^53+1 converts to 2^53 against a DOUBLE column and matches the stored 2^53 — " +
				"SQL converts exact numerics to approximate, so this is correct, NOT a " +
				"precision bug to 'fix' with an exactness check",
		},
		{
			"SELECT id FROM d WHERE v = 9007199254740992",
			[]int64{1},
			"the representable neighbour matches the same row",
		},
		{
			"SELECT id FROM b WHERE n = 9007199254740993",
			[]int64{2},
			"a BIGINT column compared to an int constant stays EXACT — no conversion happens, " +
				"so 2^53+1 finds its own row and not 2^53's",
		},
		{
			"SELECT id FROM b WHERE n = 9007199254740992",
			[]int64{1},
			"exactness holds on both sides of the boundary",
		},
		{
			"SELECT id FROM d WHERE v > 9007199254740993",
			[]int64{2},
			"the same conversion applies to ordering: the bound becomes 2^53, so only the " +
				"strictly larger stored value qualifies",
		},
		{
			"SELECT id FROM b WHERE n > 9007199254740992",
			[]int64{2, 3},
			"BIGINT ordering stays exact across the boundary",
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := ids(t, tc.query); !eq(got, tc.want) {
				t.Fatalf("%s\n%s = %v, want %v", tc.why, tc.query, got, tc.want)
			}
		})
	}
}
