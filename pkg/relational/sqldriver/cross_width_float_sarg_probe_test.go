package sqldriver_test

// Cross-WIDTH float SARG: FLOAT (binary32) and DOUBLE (binary64) are distinct
// types with distinct FDB tuple type codes (0x20 single vs 0x21 double), and a
// comparand only finds an index entry if it packs under that entry's code.
//
// Three defects met here, and each was invisible to the others:
//
//   - The walker mapped BOTH `CAST(x AS FLOAT)` and `CAST(x AS DOUBLE)` to one
//     DOUBLE-coded type, so a FLOAT cast never rounded. `d = CAST(0.1 AS FLOAT)`
//     matched a DOUBLE 0.1 that the binary32 value is NOT equal to, and
//     `f = CAST(0.1 AS FLOAT)` MISSED the FLOAT row that it IS equal to — one
//     collapsed type producing wrong rows in both directions at once.
//   - CastValue's FLOAT arm was shared with DOUBLE and so never rounded to
//     binary32, never rejected NaN/Infinite, and never range-checked — all three
//     of which Java's DOUBLE_TO_FLOAT does (CastValue.java:166-175).
//   - Once the first two were fixed and a FLOAT-typed constant could finally
//     reach an index probe, it packed 0x20 against a DOUBLE column's 0x21
//     entries: `d = CAST(1.5 AS FLOAT)` returned NOTHING even though binary32
//     1.5 IS 1.5, and `d > CAST(1.0 AS FLOAT)` returned EVERY row because a
//     0x20 bound sorts below every 0x21 entry.
//
// 0.1 is the load-bearing value: it is not representable in binary32, so it
// distinguishes a real rounding fix from a type-code-only one. 1.5 is exactly
// representable in both and cannot tell them apart — a suite that only ever
// tests values like 1.5 passes with every one of the three defects present.
//
// The plan assertions are not decoration: a shape that silently stops using the
// index still returns the right rows through a residual filter, which would let
// the SARG defect come back with the row checks all green.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// ORDER BY with two sort keys that differ ONLY in float width. Sort-key dedup
// (dedupSortKeys) keys identity on EXPLAIN text, so while that text rendered
// FLOAT and DOUBLE identically, the second key read as a duplicate of the
// first. It was harmless only while both CAST targets resolved to the same
// type; once they stopped being the same operation, dropping the DOUBLE key
// would discard the tie-breaker that orders values binary32 cannot separate.
//
// The three values below all sit within one binary32 step of 1.0: ids 1 and 2
// round to the SAME float32, so the second key is the only thing that orders
// them, and id 3 rounds up so it sorts last under either key.
func TestFDB_CrossWidthFloatSortKeys(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cwfsort")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cwfsort")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cwfsort "+
		"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cwfsort/s WITH TEMPLATE cwfsort")
	dsn := fmt.Sprintf("fdbsql:///testdb_cwfsort?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d) VALUES "+
		"(1, 1.0000000596046448), (2, 1.0000000298023224), (3, 1.0000000894069672)")

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		// float32 ties ids 1 and 2; the DOUBLE key breaks the tie ascending.
		{"SELECT id FROM t ORDER BY CAST(d AS FLOAT), CAST(d AS DOUBLE)", []int64{2, 1, 3}},
		// Same, tie broken the other way — proves the second key is REALLY
		// consulted rather than the result coinciding with the first key.
		{"SELECT id FROM t ORDER BY CAST(d AS FLOAT), CAST(d AS DOUBLE) DESC", []int64{1, 2, 3}},
		{"SELECT id FROM t ORDER BY d", []int64{2, 1, 3}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var v int64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			// Order is the assertion here — do NOT sort.
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v — the width-differing second sort key was dropped "+
						"or ignored, losing the tie-breaker", got, tc.want)
				}
			}
		})
	}
}

func TestFDB_CrossWidthFloatSargProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cwfsarg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cwfsarg")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cwfsarg "+
		"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, f FLOAT, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT NOT NULL, uf FLOAT, ud DOUBLE, PRIMARY KEY (id)) "+
		"CREATE INDEX t_d ON t (d) "+
		"CREATE INDEX t_f ON t (f)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cwfsarg/s WITH TEMPLATE cwfsarg")
	dsn := fmt.Sprintf("fdbsql:///testdb_cwfsarg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Row 3 carries 0.1 in both columns: d holds binary64 0.1, f holds binary32
	// 0.1, and those two are NOT equal once f is widened for comparison. Every
	// assertion below that involves row 3 turns on that inequality.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d, f) VALUES (1, 1.5, 1.5), (2, 2.5, 2.5), (3, 0.1, 0.1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO u (id, uf, ud) VALUES (10, 1.5, 1.5), (20, 2.5, 2.5), (30, 0.1, 0.1)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	cases := []struct {
		query string
		want  []int64
		// plan, when non-empty, must appear in EXPLAIN — it pins that the
		// predicate is still answered by the index rather than by a filter.
		plan string
		why  string
	}{
		// binary32 1.5 IS binary64 1.5, so a FLOAT cast against a DOUBLE index
		// must still find the row. Returned nothing while the FLOAT constant
		// packed 0x20 against 0x21 entries.
		{"SELECT id FROM t WHERE d = CAST(1.5 AS FLOAT)", []int64{1}, "IndexScan(T_D", "exact in both widths"},
		{"SELECT id FROM t WHERE d = CAST(1.5 AS DOUBLE)", []int64{1}, "IndexScan(T_D", "same-width control"},
		{"SELECT id FROM t WHERE d = 1.5", []int64{1}, "IndexScan(T_D", "bare literal control"},
		{"SELECT id FROM t WHERE f = CAST(1.5 AS FLOAT)", []int64{1}, "IndexScan(T_F", "FLOAT cast, FLOAT index"},
		{"SELECT id FROM t WHERE f = 1.5", []int64{1}, "IndexScan(T_F", "exact literal narrows to the FLOAT column"},
		// Row 3 (d = 0.1) is NOT > 1.0. A 0x20 bound against 0x21 entries sorts
		// below everything and returned all three rows.
		{"SELECT id FROM t WHERE d > CAST(1.0 AS FLOAT)", []int64{1, 2}, "IndexScan(T_D", "FLOAT bound on a DOUBLE index"},

		// binary32 0.1 != binary64 0.1 — the pair that only a real rounding fix
		// gets right, and it must come out wrong in OPPOSITE directions.
		{"SELECT id FROM t WHERE d = CAST(0.1 AS FLOAT)", nil, "IndexScan(T_D", "rounded constant misses the binary64 row"},
		{"SELECT id FROM t WHERE f = CAST(0.1 AS FLOAT)", []int64{3}, "IndexScan(T_F", "rounded constant finds the binary32 row"},
		{"SELECT id FROM t WHERE d = 0.1", []int64{3}, "IndexScan(T_D", "unrounded literal finds the binary64 row"},
		{"SELECT id FROM t WHERE f = 0.1", nil, "IndexScan(T_F", "binary64 literal misses the binary32 row"},

		// A binary64 comparand against a binary32 index, across every ordering
		// operator. f holds 1.5, 2.5 and binary32 0.1 (= 0.10000000149…, which
		// IS greater than binary64 0.1).
		{"SELECT id FROM t WHERE f > 0.1", []int64{1, 2, 3}, "IndexScan(T_F", "widened binary32 0.1 exceeds binary64 0.1"},
		{"SELECT id FROM t WHERE f < 2.0", []int64{1, 3}, "IndexScan(T_F", "binary64 upper bound"},
		{"SELECT id FROM t WHERE f >= 1.5", []int64{1, 2}, "IndexScan(T_F", "inclusive bound"},
		{"SELECT id FROM t WHERE f > 1.0", []int64{1, 2}, "IndexScan(T_F", "exclusive bound"},

		// Column-vs-column across widths. The comparand is promoted to the
		// indexed column's type, so these stay index probes.
		{"SELECT t.id FROM t, u WHERE t.d = u.uf", []int64{1, 2}, "IndexScan(T_D", "FLOAT column probing a DOUBLE index"},
		{"SELECT t.id FROM t, u WHERE t.d = CAST(u.uf AS FLOAT)", []int64{1, 2}, "IndexScan(T_D", "FLOAT cast of a column"},
		{"SELECT t.id FROM t, u WHERE t.d = CAST(u.ud AS FLOAT)", []int64{1, 2}, "IndexScan(T_D", "DOUBLE column rounded to FLOAT"},
		{"SELECT t.id FROM t, u WHERE t.f = u.ud", []int64{1, 2}, "", "DOUBLE column against a FLOAT column"},
		{"SELECT id FROM t WHERE d = f", []int64{1, 2}, "", "same-table cross width; row 3's two 0.1s differ"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if tc.plan != "" {
				if plan := explainOnConn(t, ctx, conn, tc.query); !strings.Contains(plan, tc.plan) {
					t.Fatalf("%s\nplan = %s\nwant it to contain %q — the rows may still be right via a\n"+
						"residual filter, which would hide a SARG regression", tc.why, plan, tc.plan)
				}
			}
			rows, err := conn.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var v int64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if len(got) != len(tc.want) {
				t.Fatalf("%s\ngot %v, want %v", tc.why, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s\ngot %v, want %v", tc.why, got, tc.want)
				}
			}
		})
	}
}
