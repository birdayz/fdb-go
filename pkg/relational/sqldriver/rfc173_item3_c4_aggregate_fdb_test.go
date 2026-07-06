package sqldriver_test

// RFC-173 item 3 commit 4 — the aggregate pin matrix over mixed-nesting
// outer-join clusters. The unprobed dimension that let the
// divergent-baked-types panic ship: every aggregate pin before this file ran
// over a PLAIN gated box; none composed GROUP BY with a LEFT box INSIDE a
// larger cluster, where the dissolved box's select merges into the enclosing
// select and stranded box-level baked references re-bound by name with a
// LEG's narrower type. The matrix crosses both gate-arm classes
// (joined-preserved: `d LEFT e` under an enclosing INNER join; clustered
// leg: `(d ⋈ c) LEFT e` — the box's preserved leg itself a cluster) ×
// aggregate argument residence (null-supplying leg / preserved leg / buried
// leg) × both orientations (LEFT and the RIGHT mirror). Row-correct GROUP BY
// results are the assertion — COUNT(col) skips NULLs, so the padded rows
// discriminate a dropped/duplicated pad or a wrong-column read.

import (
	"context"
	"database/sql"
	"testing"
)

func TestFDB_RFC173Item3_AggregateOverMixedNesting(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173i3agg"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173i3agg_tmpl"+
		" CREATE TABLE dept (id BIGINT, cid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE emp (id BIGINT, did BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE cat (id BIGINT, rank BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173i3agg_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// cat 1 (rank 7): depts 1,2. cat 2 (rank 8): dept 3.
	// dept 1: emps 10,11. dept 2: none (padded). dept 3: emp 12.
	if _, err := db.ExecContext(ctx, "INSERT INTO cat VALUES (1, 7), (2, 8)"); err != nil {
		t.Fatalf("seed cat: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO dept VALUES (1, 1), (2, 1), (3, 2)"); err != nil {
		t.Fatalf("seed dept: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO emp VALUES (10, 1), (11, 1), (12, 3)"); err != nil {
		t.Fatalf("seed emp: %v", err)
	}

	type aggRow struct{ key, cnt int64 }
	check := func(t *testing.T, q string, want []aggRow) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v\n  sql: %s", err, q)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var r aggRow
			if err := rows.Scan(&r.key, &r.cnt); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[r.key] = r.cnt
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("groups = %v, want %v\n  sql: %s", got, want, q)
		}
		for _, w := range want {
			if got[w.key] != w.cnt {
				t.Errorf("group %d = %d, want %d\n  sql: %s", w.key, got[w.key], w.cnt, q)
			}
		}
	}

	// ---- Class 1: joined-preserved — `dept LEFT emp` box under an
	// enclosing INNER join to cat, GROUP BY the OTHER (non-box) leg.

	// COUNT over the NULL-SUPPLYING leg: dept 2's padded row contributes 0.
	// cat 1 → dept1{10,11} + dept2{pad} = 2; cat 2 → dept3{12} = 1.
	t.Run("joined_preserved_count_null_supplying", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(e.id) FROM dept d LEFT JOIN emp e ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 1}})
	})
	// COUNT over the PRESERVED leg: every dept row (incl. the pad) counts.
	// cat 1 → 3 rows (dept1 ×2 + dept2 pad ×1); cat 2 → 1.
	t.Run("joined_preserved_count_preserved", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(d.id) FROM dept d LEFT JOIN emp e ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 3}, {2, 1}})
	})
	// RIGHT mirror of the null-supplying count.
	t.Run("joined_preserved_count_right_mirror", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(e.id) FROM emp e RIGHT JOIN dept d ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 1}})
	})

	// ---- Class 2: clustered preserved leg — `(dept ⋈ cat) LEFT emp`,
	// GROUP BY a BURIED preserved source, COUNT over the null side.
	t.Run("clustered_leg_count_group_by_buried", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(e.id) FROM dept d JOIN cat c ON c.id = d.cid LEFT JOIN emp e ON e.did = d.id GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 1}})
	})
	// GROUP BY the buried leg, COUNT over the OTHER buried leg (both args
	// inside the preserved cluster; the pad only NULLs the emp window).
	t.Run("clustered_leg_count_buried_arg", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(d.id) FROM dept d JOIN cat c ON c.id = d.cid LEFT JOIN emp e ON e.did = d.id GROUP BY c.id",
			[]aggRow{{1, 3}, {2, 1}})
	})
	// RIGHT mirror of the clustered-leg class: emp RIGHT (dept ⋈ cat).
	t.Run("clustered_leg_right_mirror", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(e.id) FROM emp e RIGHT JOIN dept d ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 1}})
	})
	// SUM over a buried preserved column with the pad present: dept ranks
	// via cat — cat 1 rank 7 counts once per surviving row (3 rows = 21),
	// cat 2 rank 8 once (8). Exercises a non-COUNT aggregate reading a
	// buried column through the widened window.
	t.Run("clustered_leg_sum_buried", func(t *testing.T) {
		check(t,
			"SELECT c.id, SUM(c.rank) FROM dept d JOIN cat c ON c.id = d.cid LEFT JOIN emp e ON e.did = d.id GROUP BY c.id",
			[]aggRow{{1, 21}, {2, 8}})
	})
}
