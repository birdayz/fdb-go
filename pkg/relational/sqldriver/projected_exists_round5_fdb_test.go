package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExists_Round5 pins the two round-5 review-found regressions:
//
//	P1 — `SELECT * FROM t1 WHERE EXISTS(...)` reported the INNER subquery's columns.
//	     The RFC-141 re-architecture plans a plain WHERE-EXISTS as an IDENTITY
//	     FlatMap (result value = the outer row's QuantifiedObjectValue, with a
//	     PredicatesFilter on top). deriveColumnsFromFlatMap only special-cased the
//	     PROJECTED-EXISTS RecordConstructor; the identity case fell through to
//	     merging outer+inner columns → the driver reported t1's columns AND t2's.
//	     The cursor emits ONLY the outer row, so the metadata was wrong (and a
//	     SELECT * scan into the wrong arity mis-binds). FIX: identity-over-outer
//	     returns ONLY the outer plan's columns.
//
//	P2 — qualified ORDER BY over a JOIN sorted by the WRONG leg. The round-4 fix
//	     strips `T2.ID`→`ID` for non-selected qualified sort keys. For a JOIN
//	     source the FlatMap outer row is a MERGED row where the bare `ID` key is
//	     LAST-LEG-WINS (the wrong leg); mergeRows writes BOTH bare last-leg keys
//	     AND authoritative qualified `LEG.COL` keys. So `ORDER BY t2.id DESC` over
//	     a join sorted by t1.id. FIX: strip-to-bare ONLY for single-table sources;
//	     for a JOIN source rebase the qualified ORDER BY key to the QUALIFIED
//	     merged-row key so it resolves the correct leg.
func TestFDB_ProjectedExists_Round5(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r5")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r5")
	// Both t1 and t2 carry a COLLIDING column name `sk` (the sort key) with
	// OPPOSITE orderings, so a wrong-leg resolution (stripping `t2.sk`→bare `SK`,
	// which is last-leg-wins on the merged join row) produces a DIFFERENT order
	// than the correct leg. `id` likewise collides (both PK), and t2.id ordering
	// is deliberately the INVERSE of t1.id ordering across the joined rows.
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r5_tmpl "+
		"CREATE TABLE t1(id BIGINT, col1 BIGINT, sk BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, sk BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r5/s WITH TEMPLATE projexists_r5_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r5?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1: ids 1..5; col1 = id*10 (ascending with id); sk = id (ascending with id).
	// t2: t1_id ∈ {1,3,5}; id and sk both chosen so their ordering across the
	//     joined rows is the INVERSE of t1.id's:
	//       t1_id=1 → t2.id=300, t2.sk=30
	//       t1_id=3 → t2.id=200, t2.sk=20
	//       t1_id=5 → t2.id=100, t2.sk=10
	//     So over the joined rows (t1.id ∈ {1,3,5}):
	//       t1.id  : 1, 3, 5   (t1.sk identical: 1,3,5)
	//       t2.id  : 300,200,100   (DESCENDING as t1.id ascends)
	//       t2.sk  : 30, 20, 10    (DESCENDING as t1.id ascends)
	//     Therefore `ORDER BY t1.id`, `ORDER BY t2.id`, `ORDER BY t1.sk`,
	//     `ORDER BY t2.sk` give DISTINGUISHABLE orders: a wrong-leg sort (bare
	//     `ID`/`SK` last-leg-wins) yields a different t1.id sequence and fails.
	// t3: references t1 ids {2,3} (the projected-EXISTS probe target for the join).
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10, 1), (2, 20, 2), (3, 30, 3), (4, 40, 4), (5, 50, 5)")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (300, 1, 30), (200, 3, 20), (100, 5, 10)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (1000, 2), (2000, 3)")

	// ════════════════════════════════════════════════════════════════════════
	// P1: SELECT * FROM t1 WHERE EXISTS(...) reports EXACTLY t1's columns.
	// ════════════════════════════════════════════════════════════════════════
	//
	// The plain WHERE-EXISTS plan is an identity FlatMap (outer row only) + a
	// PredicatesFilter. The cursor emits ONLY the outer t1 row, so the reported
	// columns MUST be exactly t1's (ID, COL1, SK) — never t1's + the inner t2's.
	// A regression here re-merges the inner columns: the metadata (and an
	// arity-checked Scan) breaks.
	t.Run("p1_select_star_where_exists_columns", func(t *testing.T) {
		q := "SELECT * FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		// EXACTLY t1's three columns, no inner t2 columns leaked.
		up := make([]string, len(cols))
		for i, c := range cols {
			up[i] = strings.ToUpper(c)
		}
		if len(up) != 3 {
			t.Fatalf("SELECT * WHERE EXISTS reported %d columns %v — expected exactly t1's (ID, COL1, SK); inner t2 columns leaked", len(up), cols)
		}
		// Must be t1's columns (ID, COL1, SK), in t1's declared order. Inner
		// columns of t2 (T1_ID and t2's own SK/ID) must NOT appear appended.
		want := []string{"ID", "COL1", "SK"}
		for i := range want {
			if up[i] != want[i] {
				t.Fatalf("column %d = %q, want %q (cols=%v) — inner columns leaked or order wrong", i, up[i], want[i], cols)
			}
		}
		// Scanning each row into exactly 3 targets must succeed and equal t1's data
		// filtered to the ids that have a t2 ({1,3,5}).
		got := map[int64][2]int64{}
		for rows.Next() {
			var id, col1, sk int64
			if err := rows.Scan(&id, &col1, &sk); err != nil {
				t.Fatalf("scan into 3 targets failed (column arity leak?): %v", err)
			}
			got[id] = [2]int64{col1, sk}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want2 := map[int64][2]int64{1: {10, 1}, 3: {30, 3}, 5: {50, 5}}
		if len(got) != len(want2) {
			t.Fatalf("SELECT * WHERE EXISTS returned %d rows %v, want %v", len(got), got, want2)
		}
		for id, c := range want2 {
			if got[id] != c {
				t.Errorf("id=%d: (col1,sk)=%v, want %v", id, got[id], c)
			}
		}
	})

	// P1 control: SELECT * FROM t1 WHERE NOT EXISTS(...) likewise reports exactly
	// t1's columns (the same identity FlatMap, negated filter).
	t.Run("p1_select_star_where_not_exists_columns", func(t *testing.T) {
		q := "SELECT * FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) != 3 {
			t.Fatalf("SELECT * WHERE NOT EXISTS reported %d columns %v — expected exactly t1's (ID, COL1, SK)", len(cols), cols)
		}
		got := map[int64]int64{}
		for rows.Next() {
			var id, col1, sk int64
			if err := rows.Scan(&id, &col1, &sk); err != nil {
				t.Fatalf("scan into 3 targets failed: %v", err)
			}
			got[id] = col1
		}
		// ids WITHOUT a t2 = {2,4}.
		want := map[int64]int64{2: 20, 4: 40}
		if len(got) != len(want) {
			t.Fatalf("got %d rows %v, want %v", len(got), got, want)
		}
		for id, c := range want {
			if got[id] != c {
				t.Errorf("id=%d: col1=%d, want %d", id, got[id], c)
			}
		}
	})

	// ════════════════════════════════════════════════════════════════════════
	// P2: comprehensive projected-EXISTS ORDER BY matrix.
	//   {single-table, 2-table INNER JOIN} × {sort key selected, NOT selected}
	//   × {qualified, unqualified} × at least one DESC each.
	// Each asserts REAL ordering with interleaved/distinct values so a no-op or
	// wrong-leg sort visibly fails.
	// ════════════════════════════════════════════════════════════════════════

	requireSortOverFlatMap := func(t *testing.T, q string) {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		if !strings.Contains(plan, "FlatMap") {
			t.Errorf("expected FlatMap (existential probe) in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "FirstOrDefault") {
			t.Errorf("expected FirstOrDefault in plan for %q, got:\n%s", q, plan)
		}
		if !strings.Contains(plan, "Sort") {
			t.Errorf("expected a Sort node above the existential FlatMap for %q, got:\n%s", q, plan)
		}
	}

	// idsInOrder returns the first-column (id) values in row order for a
	// `SELECT id, EXISTS(...) ...` query, plus the boolean column for sanity.
	idsInOrder := func(t *testing.T, q string) ([]int64, []bool) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var ids []int64
		var bs []bool
		for rows.Next() {
			var id int64
			var b bool
			if err := rows.Scan(&id, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			ids = append(ids, id)
			bs = append(bs, b)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return ids, bs
	}

	assertIDOrder := func(t *testing.T, q string, wantIDs []int64) {
		t.Helper()
		gotIDs, _ := idsInOrder(t, q)
		if len(gotIDs) != len(wantIDs) {
			t.Fatalf("%q: got %d rows %v, want %d %v", q, len(gotIDs), gotIDs, len(wantIDs), wantIDs)
		}
		for i := range wantIDs {
			if gotIDs[i] != wantIDs[i] {
				t.Fatalf("%q: row %d id=%d, want %d (full order %v, want %v) — sort no-op or wrong leg?",
					q, i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
			}
		}
	}

	// ── SINGLE-TABLE × selected × unqualified × DESC ─────────────────────────
	t.Run("single_selected_unqualified_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY id DESC"
		requireSortOverFlatMap(t, q)
		assertIDOrder(t, q, []int64{5, 4, 3, 2, 1})
	})

	// ── SINGLE-TABLE × selected × qualified × DESC ───────────────────────────
	t.Run("single_selected_qualified_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.id DESC"
		requireSortOverFlatMap(t, q)
		assertIDOrder(t, q, []int64{5, 4, 3, 2, 1})
	})

	// ── SINGLE-TABLE × NOT selected × unqualified × DESC ─────────────────────
	// ORDER BY col1 DESC — col1 not in SELECT; ascends with id so DESC ⇒ 5..1.
	t.Run("single_notselected_unqualified_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY col1 DESC"
		requireSortOverFlatMap(t, q)
		assertIDOrder(t, q, []int64{5, 4, 3, 2, 1})
	})

	// ── SINGLE-TABLE × NOT selected × qualified × DESC ───────────────────────
	t.Run("single_notselected_qualified_desc", func(t *testing.T) {
		q := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.col1 DESC"
		requireSortOverFlatMap(t, q)
		assertIDOrder(t, q, []int64{5, 4, 3, 2, 1})
	})

	// t1idsInOrder reads a `SELECT t1.id, t2.id, EXISTS(...) ...` row order and
	// returns the t1.id column in row order. The join surfaces t1 rows that have
	// a t2 = {1,3,5}, paired with the matching t2.id ∈ {300,200,100}.
	t1idsInOrder := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var t1id, t2id int64
			var b bool
			if err := rows.Scan(&t1id, &t2id, &b); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			ids = append(ids, t1id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return ids
	}
	assertJoinOrder := func(t *testing.T, q string, wantT1IDs []int64) {
		t.Helper()
		got := t1idsInOrder(t, q)
		if len(got) != len(wantT1IDs) {
			t.Fatalf("%q: got %d rows %v, want %d %v", q, len(got), got, len(wantT1IDs), wantT1IDs)
		}
		for i := range wantT1IDs {
			if got[i] != wantT1IDs[i] {
				t.Fatalf("%q: row %d t1.id=%d, want %d (full %v, want %v) — wrong-leg or no-op sort?",
					q, i, got[i], wantT1IDs[i], got, wantT1IDs)
			}
		}
	}

	// For the JOIN cases: t1 JOIN t2 ON t2.t1_id = t1.id yields these joined rows
	// (the projected EXISTS probes t3, irrelevant to ordering — over surviving
	// t1.id {1,3,5} only id=3 has a t3):
	//
	//   t1.id : 1   3   5     (t1.sk identical: 1, 3, 5)
	//   t2.id : 300 200 100   (DESCENDING as t1.id ascends)
	//   t2.sk : 30  20  10    (DESCENDING as t1.id ascends)
	//
	// Distinct orders make wrong-leg / no-op sorts visible:
	//   ORDER BY t1.id DESC → t1.id 5,3,1
	//   ORDER BY t2.id DESC → t2.id {300,200,100} desc → t1.id 1,3,5  (≠ t1.id desc!)
	//   ORDER BY t1.sk DESC → t1.sk {5,3,1} desc → t1.id 5,3,1
	//   ORDER BY t2.sk DESC → t2.sk {30,20,10} desc → t1.id 1,3,5     (≠ t1.sk desc!)
	// `id`/`sk` collide across legs; the qualifier MUST pick the named leg.

	// ── 2-TABLE JOIN × selected × qualified × DESC (t1.id) ───────────────────
	t.Run("join_selected_qualified_desc_t1id", func(t *testing.T) {
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t1.id DESC"
		requireSortOverFlatMap(t, q)
		assertJoinOrder(t, q, []int64{5, 3, 1})
	})

	// ── 2-TABLE JOIN × selected × qualified × DESC (t2.id) ───────────────────
	// Sort by the RIGHT leg's selected id. t2.id descends as t1.id ascends, so
	// ORDER BY t2.id DESC ⇒ t1.id 1,3,5 — the OPPOSITE of `ORDER BY t1.id DESC`.
	// A wrong-leg sort (bare `ID` last-leg-wins resolving to t1.id) gives 5,3,1
	// and fails. This is the review round-5 probe ("ORDER BY t2.id sorts by t1.id").
	t.Run("join_selected_qualified_desc_t2id", func(t *testing.T) {
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t2.id DESC"
		requireSortOverFlatMap(t, q)
		assertJoinOrder(t, q, []int64{1, 3, 5})
	})

	// ── 2-TABLE JOIN × NOT selected × qualified × DESC (t2.sk) ───────────────
	// THE core regression probe for the non-selected branch. `sk` collides
	// across legs; t2.sk descends as t1.id ascends, so ORDER BY t2.sk DESC ⇒
	// t1.id 1,3,5. A strip-to-bare key (`SK`, last-leg-wins) resolving to t1.sk
	// would give 5,3,1 (DESC of t1.sk) and fail loudly.
	t.Run("join_notselected_qualified_desc_t2sk", func(t *testing.T) {
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t2.sk DESC"
		requireSortOverFlatMap(t, q)
		assertJoinOrder(t, q, []int64{1, 3, 5})
	})

	// ── 2-TABLE JOIN × NOT selected × qualified × ASC (t2.sk) ────────────────
	// Control: ASC reverses the DESC above (5,3,1). Proves the rebase resolves
	// the correct leg in both directions, not just by coincidence.
	t.Run("join_notselected_qualified_asc_t2sk", func(t *testing.T) {
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t2.sk ASC"
		requireSortOverFlatMap(t, q)
		assertJoinOrder(t, q, []int64{5, 3, 1})
	})

	// ── 2-TABLE JOIN × NOT selected × qualified × DESC (t1.sk) ───────────────
	// The LEFT leg's non-selected qualified copy of the colliding `sk`. t1.sk
	// ascends with t1.id, so DESC ⇒ t1.id 5,3,1. A wrong-leg sort resolving the
	// bare `SK` to t2.sk would give 1,3,5 and fail. Mirror of the t2.sk case —
	// proves BOTH legs resolve correctly.
	t.Run("join_notselected_qualified_desc_t1sk", func(t *testing.T) {
		q := "SELECT t1.id, t2.id, EXISTS (SELECT 1 FROM t3 WHERE t3.t1_id = t1.id) AS has_t3 " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t1.sk DESC"
		requireSortOverFlatMap(t, q)
		assertJoinOrder(t, q, []int64{5, 3, 1})
	})
}
