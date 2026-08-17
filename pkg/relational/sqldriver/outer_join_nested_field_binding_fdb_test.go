package sqldriver_test

// A NESTED field read on an outer join's NULL-ABLE side must resolve at
// runtime. Under exact-ordinal resolution the projection carries the nested
// path as `L.N#2.B#1` — a FieldValue chain rooted at the leg's exact QOV — and
// the leg's QOV must therefore be bound in the row context the join hands its
// projection, not merely present in the plan.
//
// It was not, on both outer directions: the read failed with `resolution error
// 46 at qov.binding: exact QOV "L" (…) has no declared runtime binding` — the
// leg was bound under a DIFFERENT exact QOV than the one the projection holds.
// Six generated corpus scenarios caught it; this pins the shape directly, with
// both a preserved-side and a null-padded-side row so a fix that binds only the
// matched rows still fails.

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_OuterJoinNestedFieldBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/ojnfb"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ojnfb_tmpl"+
		" CREATE TYPE AS STRUCT nst (a BIGINT, b BIGINT)"+
		" CREATE TABLE t_rdn (id BIGINT, n nst, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE ojnfb_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t_rdn VALUES (1, (10, 11)), (2, (20, 22))"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type row struct {
		lID, lNB, rID, rNB sql.NullInt64
	}
	collect := func(t *testing.T, q string) []row {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.lID, &r.lNB, &r.rID, &r.rNB); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// `l.id = r.id - 1` pairs (1,2) and leaves one row unmatched on each side,
	// so every query below returns one matched row and one null-padded row.
	for _, tc := range []struct {
		name string
		sql  string
		want []row
	}{
		{
			name: "left-outer",
			sql: "SELECT l.id AS l_id, l.n.b AS l_n_b, r.id AS r_id, r.n.b AS r_n_b " +
				"FROM t_rdn AS l LEFT JOIN t_rdn AS r ON l.id = r.id - 1 ORDER BY l.id",
			want: []row{
				{lID: i64(1), lNB: i64(11), rID: i64(2), rNB: i64(22)},
				{lID: i64(2), lNB: i64(22)},
			},
		},
		{
			name: "right-outer",
			sql: "SELECT l.id AS l_id, l.n.b AS l_n_b, r.id AS r_id, r.n.b AS r_n_b " +
				"FROM t_rdn AS l RIGHT JOIN t_rdn AS r ON l.id = r.id - 1 ORDER BY r.id",
			want: []row{
				{rID: i64(1), rNB: i64(11)},
				{lID: i64(1), lNB: i64(11), rID: i64(2), rNB: i64(22)},
			},
		},
		{
			// Ordered on BOTH sides, with the NULL-ABLE side's key first.
			name: "right-outer-ordered-both",
			sql: "SELECT l.id AS l_id, l.n.b AS l_n_b, r.id AS r_id, r.n.b AS r_n_b " +
				"FROM t_rdn AS l RIGHT JOIN t_rdn AS r ON l.id = r.id - 1 ORDER BY l.id, r.id",
			want: []row{
				{rID: i64(1), rNB: i64(11)},
				{lID: i64(1), lNB: i64(11), rID: i64(2), rNB: i64(22)},
			},
		},
		{
			name: "left-outer-ordered-both",
			sql: "SELECT l.id AS l_id, l.n.b AS l_n_b, r.id AS r_id, r.n.b AS r_n_b " +
				"FROM t_rdn AS l LEFT JOIN t_rdn AS r ON l.id = r.id - 1 ORDER BY l.id, r.id",
			want: []row{
				{lID: i64(1), lNB: i64(11), rID: i64(2), rNB: i64(22)},
				{lID: i64(2), lNB: i64(22)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(t, tc.sql)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func i64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
