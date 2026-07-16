package sqldriver_test

import (
	"context"
	"database/sql"
	"testing"
)

// Regression pin: a computed UNALIASED projection in a JOIN-bodied recursive
// leg resolves through the ordinal read (its rendering carries a dot; the
// remap must never first-dot-split it into a garbage correlation).
func TestFDB_RecursiveCTEComputedLegProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/rcte_comp"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rcte_comp_tmpl"+
		" CREATE TABLE t (id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rcte_comp_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1), (2), (3), (4)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The recursive leg is a PROJECTION over a JOIN (r, t b) whose single
	// output is an unaliased COMPUTED column with a qualified ref inside —
	// its rendering "(B.ID + 1)"-style contains a dot.
	for _, tc := range []struct{ name, sql string }{
		{"unaliased_no_orderby", "WITH RECURSIVE r AS (" +
			"SELECT id FROM t WHERE id = 1" +
			" UNION ALL SELECT b.id + 1 FROM r, t b WHERE b.id = r.id AND b.id < 4" +
			") SELECT * FROM r"},
		{"aliased_leg", "WITH RECURSIVE r AS (" +
			"SELECT id FROM t WHERE id = 1" +
			" UNION ALL SELECT b.id + 1 AS id FROM r, t b WHERE b.id = r.id AND b.id < 4" +
			") SELECT * FROM r"},
		{"cte_column_list", "WITH RECURSIVE r(n) AS (" +
			"SELECT id FROM t WHERE id = 1" +
			" UNION ALL SELECT b.id + 1 FROM r, t b WHERE b.id = r.n AND b.id < 4" +
			") SELECT * FROM r"},
	} {
		rows, err := db.QueryContext(ctx, tc.sql)
		if err != nil {
			t.Errorf("%s: query err: %v", tc.name, err)
			continue
		}
		var got []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", tc.name, err)
			}
			if v.Valid {
				got = append(got, v.Int64)
			} else {
				got = append(got, -999)
			}
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			t.Errorf("%s: rows err: %v", tc.name, rerr)
			continue
		}
		if len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 3 || got[3] != 4 {
			t.Errorf("%s: rows = %v, want [1 2 3 4]", tc.name, got)
		}
	}
}
