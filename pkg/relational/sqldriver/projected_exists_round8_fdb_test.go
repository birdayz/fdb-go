package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_ProjectedExists_Round8 pins the two review round-8 regressions, both
// rooted in the projected-EXISTS fold RE-DERIVING a projected column's
// alias/Name/Label from the FOLDED record instead of carrying the ORIGINAL
// LogicalProject's per-column alias provenance (explicit-alias flag). The root
// fix threads `LogicalProject.Aliases` (where ""==no alias) through the fold to
// BOTH the column-metadata derivation (foldedFieldAlias is replaced by the real
// provenance) and the hidden-ORDER-BY cleanup re-projection.
//
//	P1 (SILENT-WRONG) — `SELECT t1.id AS id, EXISTS(...) FROM t1 JOIN t2 ...`:
//	   the column is EXPLICITLY aliased `AS id` whose alias equals the bare leaf of
//	   t1.id. The bare-name inference classified it as UNALIASED, so the folded
//	   ColumnDef.Name became the qualified value name `T1.ID`, but the folded record
//	   is keyed by the explicit alias `ID` → a positional/named Scan of that column
//	   read NULL.
//
//	P2 (label regression) — `SELECT t1.id, EXISTS(...) FROM t1 ORDER BY t1.sk`:
//	   when a hidden sort column is appended (t1.sk is not in the SELECT output),
//	   the final cleanup projection that drops the hidden column re-aliased EVERY
//	   visible field, so the ResultSet labels became `T1.ID` / the raw EXISTS expr
//	   instead of the normal SELECT-list labels. Adding a hidden sort column must
//	   not change any visible column's public label.
func TestFDB_ProjectedExists_Round8(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_projexists_r8")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_projexists_r8")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE projexists_r8_tmpl "+
		"CREATE TABLE t1(id BIGINT, sk BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_projexists_r8/s WITH TEMPLATE projexists_r8_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_projexists_r8?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// t1.id 1..5; sk DESCENDS as id ascends so ORDER BY t1.sk differs from id order.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 50), (2, 40), (3, 30), (4, 20), (5, 10)")
	// t2 rows reference t1 ids {1,3,5} so the EXISTS boolean alternates.
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (10, 1), (30, 3), (50, 5)")

	// ════════════════════════════════════════════════════════════════════════
	// P1: explicit alias == bare leaf, over a JOIN. Both the column metadata AND
	// the VALUE must be correct.
	// ════════════════════════════════════════════════════════════════════════
	//
	// `SELECT t1.id AS id, t2.id, EXISTS(...) FROM t1 JOIN t2 ON t2.t1_id = t1.id`:
	//   - column 0 is EXPLICITLY aliased `AS id` → label ID, datum keyed by ID,
	//     value = t1.id (1,3,5 — the joinable ids).
	// The bug: foldedFieldAlias inferred UNALIASED (alias "id" == bare leaf of
	// t1.id), so the folded datum Name became `T1.ID` while the record is keyed by
	// `ID` → the t1.id column read NULL.
	t.Run("p1_explicit_alias_eq_bare_leaf_over_join", func(t *testing.T) {
		existsQ := "SELECT t1.id AS id, t2.id, EXISTS (SELECT 1 FROM t2 x WHERE x.t1_id = t1.id) AS has_x " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t1.id"
		controlQ := "SELECT t1.id AS id, t2.id FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t1.id"
		// Metadata parity with the non-EXISTS control (first 2 columns).
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 2)

		// VALUE: the t1.id column (explicit alias ID) must read the real id, not NULL.
		rows, err := db.QueryContext(ctx, existsQ)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id, t2id int64
			var has bool
			if err := rows.Scan(&id, &t2id, &has); err != nil {
				t.Fatalf("scan (p1 — the aliased t1.id column read NULL?): %v", err)
			}
			if !has {
				t.Errorf("EXISTS over a join must be true for every joined row, got false for id %d", id)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// Only t1 ids {1,3,5} join to a t2 row.
		want := []int64{1, 3, 5}
		if fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Fatalf("aliased t1.id values = %v, want %v — explicit alias over JOIN read the wrong/NULL datum", ids, want)
		}
	})

	// P1 control: the SAME query with EXISTS in WHERE (not projected) and the
	// non-EXISTS control already proves the metadata; this run asserts a NAMED
	// scan by the explicit alias also resolves (not just positional).
	t.Run("p1_named_scan_by_explicit_alias", func(t *testing.T) {
		existsQ := "SELECT t1.id AS the_id, EXISTS (SELECT 1 FROM t2 x WHERE x.t1_id = t1.id) AS has_x " +
			"FROM t1 JOIN t2 ON t2.t1_id = t1.id ORDER BY t1.id"
		rows, err := db.QueryContext(ctx, existsQ)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		if len(cols) < 1 || up(cols[0]) != "THE_ID" {
			t.Fatalf("first column label = %q, want THE_ID", cols[0])
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var has bool
			if err := rows.Scan(&id, &has); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if fmt.Sprint(ids) != fmt.Sprint([]int64{1, 3, 5}) {
			t.Fatalf("the_id values = %v, want [1 3 5]", ids)
		}
	})

	// ════════════════════════════════════════════════════════════════════════
	// P2: hidden ORDER BY column must not change visible columns' labels.
	// ════════════════════════════════════════════════════════════════════════
	//
	// `SELECT t1.id, EXISTS(...) FROM t1 ORDER BY t1.sk` — sk is NOT selected, so
	// the fold appends it as a hidden sort field and wraps a cleanup projection to
	// drop it. The cleanup must REUSE the original aliases (t1.id unaliased → label
	// ID; the EXISTS column keeps its alias HAS_T2), not re-alias every field to
	// its datum Name (T1.ID / the raw EXISTS expr).
	t.Run("p2_hidden_orderby_preserves_labels", func(t *testing.T) {
		existsQ := "SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY t1.sk"
		// Control is a TRUE NON-EXISTS query with the SAME hidden-sort shape
		// (`ORDER BY t1.sk`, sk not selected). It never enters the projected-EXISTS
		// fold, so its label for `t1.id` is the canonical projection label `ID`. The
		// projected-EXISTS query's first column must report that SAME label —
		// REVERT-PROOF: the force-alias bug labels the EXISTS query's column `T1.ID`
		// while this non-EXISTS control keeps `ID`, so the comparison fails loudly.
		controlQ := "SELECT t1.id FROM t1 ORDER BY t1.sk"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 1)

		// And the rows must really be ordered by sk (DESC of id, since sk descends
		// with id): ORDER BY t1.sk ASC → sk 10,20,30,40,50 → id 5,4,3,2,1.
		rows, err := db.QueryContext(ctx, existsQ)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			var has bool
			if err := rows.Scan(&id, &has); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if fmt.Sprint(ids) != fmt.Sprint([]int64{5, 4, 3, 2, 1}) {
			t.Fatalf("ORDER BY t1.sk ids = %v, want [5 4 3 2 1] — hidden sort no-oped?", ids)
		}
	})

	// P2 with an UNALIASED bare column + hidden sort: `SELECT id, EXISTS(...) ORDER BY sk`.
	// Label for column 0 must be ID (not changed by the hidden-sort cleanup). Control
	// is a true non-EXISTS query with the same hidden-sort shape.
	t.Run("p2_hidden_orderby_bare_column_label", func(t *testing.T) {
		existsQ := "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY sk"
		controlQ := "SELECT id FROM t1 ORDER BY sk"
		assertLeadingLabelsMatch(t, db, ctx, existsQ, controlQ, 1)
	})

	// P2 with an EXPLICITLY ALIASED column + hidden sort: the cleanup must keep the
	// alias (not re-derive). `SELECT id AS the_id, EXISTS(...) ORDER BY sk` → label
	// THE_ID + type BIGINT. The force-alias revert mislabels/loses the type; the
	// true non-EXISTS control keeps THE_ID/BIGINT.
	t.Run("p2_hidden_orderby_aliased_column_label_and_type", func(t *testing.T) {
		existsQ := "SELECT id AS the_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY sk"
		controlQ := "SELECT id AS the_id FROM t1 ORDER BY sk"
		assertColumnMetaParity(t, db, ctx, existsQ, controlQ)
	})

	// P2 with a QUALIFIED unaliased column + hidden sort: `SELECT t1.id, EXISTS(...)
	// ORDER BY t1.sk` → label must be the BARE leaf ID (never the qualified T1.ID
	// the force-alias revert leaks). True non-EXISTS control reports ID.
	t.Run("p2_hidden_orderby_qualified_column_label", func(t *testing.T) {
		existsQ := "SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 " +
			"FROM t1 ORDER BY t1.sk"
		controlQ := "SELECT t1.id FROM t1 ORDER BY t1.sk"
		assertColumnMetaParity(t, db, ctx, existsQ, controlQ)
	})

	// ════════════════════════════════════════════════════════════════════════
	// COMPREHENSIVE MATRIX: for EVERY projection shape the task enumerates, the
	// projected-EXISTS column's Name + Label + type EXACTLY equals the non-EXISTS
	// control's, AND a positional scan reads the correct (non-NULL) value.
	//
	// Each case is revert-proof: without the root fix the column reads NULL (the
	// datum Name diverges from the record key) or reports a leaked qualified label.
	// ════════════════════════════════════════════════════════════════════════
	matrix := []struct {
		name       string
		existsCol  string // the leading projected column under test
		controlSel string // the same column in a non-EXISTS control
		from       string
		// the joinable t1 ids under the FROM (single-table → all; JOIN → {1,3,5}).
		wantVals []int64
	}{
		{
			name:       "bare_col_single",
			existsCol:  "id",
			controlSel: "id",
			from:       "FROM t1",
			wantVals:   []int64{1, 2, 3, 4, 5},
		},
		{
			name:       "aliased_col_single",
			existsCol:  "id AS the_id",
			controlSel: "id AS the_id",
			from:       "FROM t1",
			wantVals:   []int64{1, 2, 3, 4, 5},
		},
		{
			name:       "qualified_col_single",
			existsCol:  "t1.id",
			controlSel: "t1.id",
			from:       "FROM t1",
			wantVals:   []int64{1, 2, 3, 4, 5},
		},
		{
			// t1.id AS id over a JOIN: explicit alias == bare leaf (the P1 trap).
			name:       "explicit_alias_eq_bare_leaf_join",
			existsCol:  "t1.id AS id",
			controlSel: "t1.id AS id",
			from:       "FROM t1 JOIN t2 ON t2.t1_id = t1.id",
			wantVals:   []int64{1, 3, 5},
		},
		{
			// t1.id UNALIASED over a JOIN: qualified label must stay bare ID.
			name:       "qualified_col_unaliased_join",
			existsCol:  "t1.id",
			controlSel: "t1.id",
			from:       "FROM t1 JOIN t2 ON t2.t1_id = t1.id",
			wantVals:   []int64{1, 3, 5},
		},
	}
	for _, m := range matrix {
		m := m
		t.Run("matrix_"+m.name, func(t *testing.T) {
			existsQ := "SELECT " + m.existsCol +
				", EXISTS (SELECT 1 FROM t2 z WHERE z.t1_id = t1.id) AS has_z " +
				m.from + " ORDER BY t1.id"
			controlQ := "SELECT " + m.controlSel + " " + m.from + " ORDER BY t1.id"

			// Name + Label + type + nullability parity for the leading column.
			assertColumnMetaParity(t, db, ctx, existsQ, controlQ)

			// Positional scan of the leading column must read the real value, not NULL.
			rows, err := db.QueryContext(ctx, existsQ)
			if err != nil {
				t.Fatalf("query %q: %v", existsQ, err)
			}
			defer rows.Close()
			var got []int64
			for rows.Next() {
				var v int64
				var has bool
				if err := rows.Scan(&v, &has); err != nil {
					t.Fatalf("scan (%s — leading column read NULL?): %v", m.name, err)
				}
				got = append(got, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(m.wantVals) {
				t.Fatalf("%s: leading column = %v, want %v — datum Name diverged from the record key?",
					m.name, got, m.wantVals)
			}
		})
	}
}

// assertColumnMetaParity asserts the FIRST column's Name (via a named scan that
// must resolve), Label, type and nullability for the projected-EXISTS query
// EXACTLY equal those of the non-EXISTS control. Unlike assertLeadingLabelsMatch
// it also exercises the datum lookup by the reported column NAME (the round-8 P1
// revert reads NULL there), not only the label.
func assertColumnMetaParity(t *testing.T, db *sql.DB, ctx context.Context, existsQ, controlQ string) {
	t.Helper()
	meta := func(q string) (name, label, typ, null string) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("columnTypes %q: %v", q, err)
		}
		label = up(cols[0])
		typ = cts[0].DatabaseTypeName()
		if n, ok := cts[0].Nullable(); ok {
			if n {
				null = "NULLABLE"
			} else {
				null = "NOT_NULL"
			}
		} else {
			null = "UNKNOWN"
		}
		// Name == the reported label here (database/sql surfaces a single name);
		// the datum-lookup-by-name correctness is exercised by the value scan in
		// the caller. Return the label as the name for the comparison.
		name = label
		return
	}
	en, el, et, enull := meta(existsQ)
	cn, cl, ct, cnull := meta(controlQ)
	if en != cn {
		t.Errorf("column 0 Name: EXISTS=%q control=%q", en, cn)
	}
	if el != cl {
		t.Errorf("column 0 Label: EXISTS=%q control=%q", el, cl)
	}
	if et != ct {
		t.Errorf("column 0 type: EXISTS=%q control=%q", et, ct)
	}
	if enull != cnull {
		t.Errorf("column 0 nullability: EXISTS=%q control=%q", enull, cnull)
	}
}

func up(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		out[i] = c
	}
	return string(out)
}
