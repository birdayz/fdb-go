package sqldriver_test

// The aggregate pin matrix over mixed-nesting outer-join clusters: a GROUP BY
// / aggregate whose input is a LEFT (or RIGHT) box NESTED inside a larger
// join cluster, where the box's projection merges into the enclosing select
// and a box-level reference must still bind against the right leg's type —
// this is the dimension a plain single-box aggregate pin never exercises.
// The matrix crosses both nesting classes (joined-preserved: `d LEFT e`
// under an enclosing INNER join; clustered leg: `(d ⋈ c) LEFT e` — the box's
// preserved leg itself a cluster) × aggregate argument residence
// (null-supplying leg / preserved leg / buried leg) × both orientations
// (LEFT and the RIGHT mirror). Row-correct GROUP BY results are the
// assertion — COUNT(col) skips NULLs, so the padded rows discriminate a
// dropped/duplicated pad or a wrong-column read.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AggregateOverMixedNestingOuterJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/i3agg"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE i3agg_tmpl"+
		" CREATE TABLE dept (id BIGINT, cid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE emp (id BIGINT, did BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE cat (id BIGINT, rank BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE i3agg_tmpl"); err != nil {
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

// TestFDB_NullSupplyingLegMetadataNullable pins the column-metadata contract
// for a NULL-SUPPLYING clustered window (mirrors Java's
// pullUpResultColumnsWithNullability(true)): a NOT NULL column served
// through a NULL-SUPPLYING clustered window must report NULLABLE in the
// driver's column metadata (its slot serves NULL on padded rows), while the
// preserved side's NOT NULL column keeps nullable=false.
func TestFDB_NullSupplyingLegMetadataNullable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/i3meta"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE i3meta_tmpl"+
		" CREATE TABLE ma (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE mb (bid BIGINT NOT NULL, ref BIGINT, PRIMARY KEY (bid))"+
		" CREATE TABLE mc (cid BIGINT NOT NULL, w BIGINT, PRIMARY KEY (cid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE i3meta_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO ma VALUES (1, 10)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The clustered box (mb ⋈ mc) is the NULL-SUPPLYING leg; ma preserved.
	rows, err := db.QueryContext(ctx,
		"SELECT ma.id, b.bid, c.cid FROM mb AS b JOIN mc AS c ON b.ref = c.cid RIGHT JOIN ma ON ma.id = b.bid")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("column types: %v", err)
	}
	if len(cts) != 3 {
		t.Fatalf("columns = %d, want 3", len(cts))
	}
	if n, ok := cts[0].Nullable(); !ok || n {
		t.Errorf("ma.id (preserved, NOT NULL) nullable = (%v, %v), want (false, true)", n, ok)
	}
	if n, ok := cts[1].Nullable(); !ok || !n {
		t.Errorf("b.bid (null-supplying window, declared NOT NULL) nullable = (%v, %v), want (true, true) — padded rows serve NULL here", n, ok)
	}
	if n, ok := cts[2].Nullable(); !ok || !n {
		t.Errorf("c.cid (null-supplying window, declared NOT NULL) nullable = (%v, %v), want (true, true)", n, ok)
	}
}

// TestFDB_WhereConjunctOverBuriedSource is the ROW-level regression net for
// registering a BURIED leg reference found inside a WHERE conjunct: a
// cross-leg conjunct spanning a BURIED source of a clustered box leg and the
// other leg (`WHERE d.id = e.did` over `(dept ⋈ cat) LEFT emp`) must keep
// answering correct rows — the duplicate column name ID across all three
// tables makes this shape a standing first-match trap if that registration
// ever narrows. A companion white-box test cross-checks that the WHERE-side
// leg-type map registers buried windows exactly like the join seed's own map.
func TestFDB_WhereConjunctOverBuriedSource(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/i3wcb"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE i3wcb_tmpl"+
		" CREATE TABLE wd (id BIGINT, cid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE wc (id BIGINT, tag BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE we (id BIGINT, did BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE i3wcb_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// wd 1 (cat 5), wd 2 (cat 6). we 10 → did 1; we 20 → did 99 (no dept).
	if _, err := db.ExecContext(ctx, "INSERT INTO wd VALUES (1, 5), (2, 6)"); err != nil {
		t.Fatalf("seed wd: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO wc VALUES (5, 50), (6, 60)"); err != nil {
		t.Fatalf("seed wc: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO we VALUES (10, 1), (20, 99)"); err != nil {
		t.Fatalf("seed we: %v", err)
	}

	// Buried d (inside the preserved cluster d⋈c) crossed with the
	// null-supplying leg e in WHERE: only wd 1 has a matching emp.
	rows, err := db.QueryContext(ctx,
		"SELECT d.id, c.tag, e.id FROM wd AS d JOIN wc AS c ON c.id = d.cid LEFT JOIN we AS e ON e.did = d.id WHERE d.id = e.did")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got [][3]int64
	for rows.Next() {
		var d, c, e int64
		if err := rows.Scan(&d, &c, &e); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, [3]int64{d, c, e})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 1 || got[0] != [3]int64{1, 50, 10} {
		t.Fatalf("rows = %v, want exactly [[1 50 10]] (buried d crossed with e in WHERE)", got)
	}
}

// TestFDB_AggregateOverClusteredNullSupplyingLeg pins a correct-or-loud
// regression: the clustered NULL-SUPPLYING leg (emp ⋈ cat2, via RIGHT) under
// an ENCLOSING inner join, with the aggregate argument INSIDE the null
// cluster. This shape once diverged silently: the merged row's key
// registration used run-level keys for whole box spans, so a qualified
// aggregate operand ("C2.RANK") read a key that was never written and
// served COUNT of zeros instead of the correct count. The row-level and
// unenclosed shapes ride a different plan (materialized NLJ) and were
// correct throughout — pinned here as the tripwire against plan-shape drift.
func TestFDB_AggregateOverClusteredNullSupplyingLeg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/i3aggns"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE i3aggns_tmpl"+
		" CREATE TABLE dept (id BIGINT, cid BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE emp (id BIGINT, did BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE cat (id BIGINT, rank BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE i3aggns_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Same fixture as the mixed-nesting matrix above: cat 1 (rank 7) ← depts 1,2; cat 2
	// (rank 8) ← dept 3. dept 1: emps 10,11 (both did=1 → cat2 join on
	// did hits cat 1); dept 2: none (padded); dept 3: emp 12 (did=3, no cat).
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

	// The inversion cell: BURIED argument (c2.rank) in the null cluster,
	// enclosed. emp 10,11 (did=1) join cat2 1 → 2 rows under dept 1 → cat 1
	// counts 2; emp 12 (did=3) has no cat2 → the cluster is empty for dept 3
	// → padded → COUNT 0; dept 2 padded → 0. Want {1:2, 2:0}.
	t.Run("enclosed_count_buried_arg", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(c2.rank) FROM emp e JOIN cat c2 ON c2.id = e.did RIGHT JOIN dept d ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 0}})
	})
	// LEAF argument (e.id — the null cluster's other leg), enclosed.
	t.Run("enclosed_count_leaf_arg", func(t *testing.T) {
		check(t,
			"SELECT c.id, COUNT(e.id) FROM emp e JOIN cat c2 ON c2.id = e.did RIGHT JOIN dept d ON e.did = d.id JOIN cat c ON c.id = d.cid GROUP BY c.id",
			[]aggRow{{1, 2}, {2, 0}})
	})
	// UNENCLOSED aggregate over the same null cluster (rides the NLJ plan
	// today — the tripwire if the plan shape drifts onto the dissolved path).
	t.Run("unenclosed_count_buried_arg", func(t *testing.T) {
		check(t,
			"SELECT d.id, COUNT(c2.rank) FROM emp e JOIN cat c2 ON c2.id = e.did RIGHT JOIN dept d ON e.did = d.id GROUP BY d.id",
			[]aggRow{{1, 2}, {2, 0}, {3, 0}})
	})
	// NULL group key: GROUP BY a null-cluster column — padded rows form the
	// NULL group. COUNT(d.id) counts every dept row in each group: c2.rank 7
	// ← emps 10,11 → depts 1,1 → 2; NULL group ← depts 2,3 → 2.
	t.Run("group_by_null_cluster_column", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT c2.rank, COUNT(d.id) FROM emp e JOIN cat c2 ON c2.id = e.did RIGHT JOIN dept d ON e.did = d.id GROUP BY c2.rank")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var k sql.NullInt64
			var n int64
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			key := "<NULL>"
			if k.Valid {
				key = fmt.Sprintf("%d", k.Int64)
			}
			got[key] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := map[string]int64{"7": 2, "<NULL>": 2}
		if len(got) != len(want) || got["7"] != 2 || got["<NULL>"] != 2 {
			t.Errorf("groups = %v, want %v", got, want)
		}
	})
	// Row-level enclosed (probe D): the null cluster's columns project
	// correctly under the enclosing join.
	t.Run("row_level_enclosed", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT d.id, c2.rank FROM emp e JOIN cat c2 ON c2.id = e.did RIGHT JOIN dept d ON e.did = d.id JOIN cat c ON c.id = d.cid ORDER BY d.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var d int64
			var r sql.NullInt64
			if err := rows.Scan(&d, &r); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if r.Valid {
				got = append(got, fmt.Sprintf("%d|%d", d, r.Int64))
			} else {
				got = append(got, fmt.Sprintf("%d|<NULL>", d))
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := []string{"1|7", "1|7", "2|<NULL>", "3|<NULL>"}
		if len(got) != len(want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("rows = %v, want %v", got, want)
			}
		}
	})
}
