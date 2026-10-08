package sqltest

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// TestFDB_ExistsOverANullSuppliedRowKeepsTheRow: an EXISTS correlated to the
// null-supplied side of a LEFT JOIN may plan as FlatMap(DefaultOnEmpty(h),
// exists) passing h's row through. When h has no match that row is the absent
// (null-extended) record, and it must flow on as one rather than vanish.
func TestFDB_ExistsOverANullSuppliedRowKeepsTheRow(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := testkit.DgcOpen(t, "/FRL/testdb_nsexists", "nsexists",
		"CREATE TABLE w (id BIGINT, f BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE h (id BIGINT, f BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) "+
			"CREATE TABLE q (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX hf AS SELECT f FROM h")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO w VALUES (1, 10), (2, 20), (3, 30)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO h VALUES (1, 10, [1]), (2, 20, [2])")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO q VALUES (1)")

	for _, tc := range []struct{ sql, want string }{
		{
			"SELECT w.id, h.id FROM w LEFT JOIN h ON h.f = w.f WHERE NOT EXISTS (SELECT 1 FROM h.arr x WHERE x = 2)",
			"[1 1] [3 NULL]",
		},
		{
			"SELECT w.id, h.id FROM w LEFT JOIN h ON h.id = w.id WHERE NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)",
			"[2 2] [3 NULL]",
		},
	} {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan); err != nil {
			t.Fatalf("explain %s: %v", tc.sql, err)
		}
		if !strings.Contains(plan, "FlatMap(outer=DefaultOnEmpty(") {
			t.Fatalf("%s planned %s: no longer passes the null-supplied row through a FlatMap", tc.sql, plan)
		}
		rows, err := db.QueryContext(ctx, tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		var got []string
		for rows.Next() {
			var id int64
			var hid sql.NullInt64
			if err := rows.Scan(&id, &hid); err != nil {
				t.Fatal(err)
			}
			cell := "NULL"
			if hid.Valid {
				cell = fmt.Sprint(hid.Int64)
			}
			got = append(got, fmt.Sprintf("[%d %s]", id, cell))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		rows.Close()
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s\n got %s\nwant %s", tc.sql, strings.Join(got, " "), tc.want)
		}
	}
}

// TestFDB_ExistsOverScalarElementsFindingNoneUnderAJoin: an EXISTS over a
// correlated array of scalars plans as FirstOrDefault over the elements. When
// no element qualifies, the default is a NULL scalar standing for "no row", and
// the existence check reading it must see that absence rather than a NULL in a
// NOT NULL element column.
func TestFDB_ExistsOverScalarElementsFindingNoneUnderAJoin(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	db, ctx := testkit.DgcOpen(t, "/FRL/testdb_scalarexists", "scalarexists",
		"CREATE TABLE t3 (id BIGINT, col2 BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t4 (id BIGINT, col2 BIGINT, col4 BIGINT ARRAY, PRIMARY KEY (id)) "+
			"CREATE INDEX t4_col2 AS SELECT col2 FROM t4")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t3 VALUES (3, 1), (4, 2)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO t4 VALUES (5, 1, [1]), (7, 2, [1, 2]), (15, 1, [1, 2, 3])")

	const q = "SELECT t4.id FROM t3, t4 WHERE t4.col2 = t3.col2 AND EXISTS (SELECT 1 FROM t4.col4 x WHERE x = 2)"
	got := testkit.DgcInts(t, db, ctx, q, true)
	if !testkit.DgcEq(got, []int64{7, 15}) {
		t.Errorf("%s = %v, want [7 15]", q, got)
	}
	const notExists = "SELECT t4.id FROM t3, t4 WHERE t4.col2 = t3.col2 AND NOT EXISTS (SELECT 1 FROM t4.col4 x WHERE x = 2)"
	if got := testkit.DgcInts(t, db, ctx, notExists, true); !testkit.DgcEq(got, []int64{5}) {
		t.Errorf("%s = %v, want [5]", notExists, got)
	}
}
