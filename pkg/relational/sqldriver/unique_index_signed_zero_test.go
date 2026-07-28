package sqldriver_test

// A UNIQUE index holds BOTH signed zeros, and every proof derived from it stays
// sound. This is the coherence check for the decision that DISTINCT uses tuple
// identity rather than IEEE equality.
//
// -0.0 and +0.0 are IEEE-equal but pack to distinct FDB tuple keys, so a unique
// index accepts both. Two consequences have to agree, and they are proved by
// different layers:
//
//   - uniqueness enforcement scans the raw packed prefix, so it permits both;
//   - DISTINCT elision reasons "a unique index gives one row per value, so the
//     dedup is a no-op" and removes the Distinct operator entirely.
//
// Those agree ONLY because DISTINCT counts the two zeros as two values. Had
// DISTINCT merged them (the IEEE reading), the elision would be UNSOUND: the
// index would hand back two rows for what DISTINCT calls one value, with no
// dedup left in the plan to collapse them. The merge was attempted and reverted
// earlier for a different reason; this test pins the interaction that also
// requires the split, so a future attempt to merge cannot pass silently here.
//
// It also guards the widening: `v = 0` spans two keys, so an equality on a
// UNIQUE column legitimately returns TWO rows. Any cardinality proof that says
// "unique equality implies at most one row" is wrong for a zero comparand, and
// a LIMIT or single-row optimisation resting on it would drop a row.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_UniqueIndexSignedZero(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uiz")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uiz")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uiz "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE UNIQUE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uiz/s WITH TEMPLATE uiz")
	dsn := fmt.Sprintf("fdbsql:///testdb_uiz?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 5)")
	// The load-bearing insert: a UNIQUE index must accept the OTHER signed zero,
	// because they are different keys. If this ever starts failing, uniqueness
	// has moved to IEEE equality and the DISTINCT-elision reasoning below has to
	// be revisited with it.
	if _, err := db.ExecContext(ctx, "INSERT INTO t (id, v, w) VALUES (2, 0.0, 9)"); err != nil {
		t.Fatalf("inserting +0.0 alongside -0.0 into a UNIQUE index was REJECTED: %v\n"+
			"the two signed zeros pack to distinct keys, so uniqueness must permit both; "+
			"if this changed deliberately, DISTINCT elision over a unique index needs "+
			"rechecking because it assumes one row per value", err)
	}
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (3, 5.0, 1)")

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
	scalar := func(t *testing.T, q string) int64 {
		t.Helper()
		var n int64
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return n
	}

	if got := ids(t, "SELECT id FROM t WHERE v = 0"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("v = 0 on a UNIQUE index = %v, want [1 2] — the equality widens across both "+
			"signed-zero keys, so a unique-column equality legitimately returns TWO rows; any "+
			"'at most one row' proof or single-row optimisation resting on uniqueness would "+
			"drop one", got)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM t WHERE v = 0"); n != 2 {
		t.Fatalf("COUNT(*) WHERE v = 0 = %d, want 2", n)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM (SELECT DISTINCT v FROM t WHERE v = 0) AS s"); n != 2 {
		t.Fatalf("DISTINCT v WHERE v = 0 = %d, want 2 — the two signed zeros are two values "+
			"under tuple identity, which is exactly what makes eliding the Distinct over a "+
			"unique index sound. If DISTINCT ever merges them, that elision becomes unsound "+
			"and this must be fixed together with it", n)
	}
	if got := ids(t, "SELECT id FROM t WHERE v = 0 LIMIT 5"); len(got) != 2 {
		t.Fatalf("v = 0 LIMIT 5 = %v, want both rows", got)
	}
	// Control: an ordinary unique probe really does yield one row.
	if got := ids(t, "SELECT id FROM t WHERE v = 5.0"); len(got) != 1 || got[0] != 3 {
		t.Fatalf("v = 5.0 = %v, want [3]", got)
	}
}
