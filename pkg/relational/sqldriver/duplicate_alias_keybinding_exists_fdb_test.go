package sqldriver_test

// Sort/group keys over duplicate FROM aliases must bind by the minted
// binding correlation (not the shared display name), and a correlated EXISTS
// over an un-collapsed cross join must read the merged outer row for
// references buried in the existential subplan. Asserts Java-expected
// behavior (live-verified 4.12.11.0) so a failure prints the actual result.
// Richer q seed ({5,7,9}) so the ORDER BY discriminates.

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestFDB_KeyBindingAndBuriedExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/dupaliaskeybind"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE dupaliaskeybind_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE dupaliaskeybind_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, "INSERT INTO p VALUES (1, 10), (2, 20)"); err != nil {
		t.Fatalf("seed p: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO q VALUES (5), (7), (9)"); err != nil {
		t.Fatalf("seed q: %v", err)
	}

	// ---- P1a: ORDER BY over a duplicate-alias leg's unique column ----
	// a.qid resolves per-attribute to the q leg (binding-keyed row). Cross
	// product p(2) × q(3) = 6 rows, qid ∈ {5,7,9} each twice. DESC LIMIT 1 → 9.
	// If the sort key keeps the DISPLAY alias while the row is binding-keyed,
	// it sorts on a missing key and returns a scan-order row.
	t.Run("P1a_order_by", func(t *testing.T) {
		var got int64
		err := db.QueryRowContext(ctx,
			"SELECT a.qid FROM p AS a, q AS a ORDER BY a.qid DESC LIMIT 1").Scan(&got)
		if err != nil {
			t.Fatalf("ORDER BY over dup-alias leg errored: %v", err)
		}
		t.Logf("ACTUAL ORDER BY a.qid DESC LIMIT 1 = %d (want 9)", got)
		if got != 9 {
			t.Errorf("ORDER BY a.qid DESC LIMIT 1 = %d, want 9 (sort key not rebound to binding)", got)
		}
	})

	// ---- P1b: GROUP BY over a duplicate-alias leg's unique column ----
	// Each q row × 2 p rows → count 2 per qid.
	t.Run("P1b_group_by", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.qid, COUNT(*) FROM p AS a, q AS a GROUP BY a.qid")
		if err != nil {
			t.Fatalf("GROUP BY over dup-alias leg errored: %v", err)
		}
		defer rows.Close()
		got := map[int64]int64{}
		for rows.Next() {
			var k sql.NullInt64
			var c int64
			if err := rows.Scan(&k, &c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !k.Valid {
				t.Errorf("GROUP BY key is NULL — group key not rebound to binding")
				continue
			}
			got[k.Int64] = c
		}
		t.Logf("ACTUAL GROUP BY a.qid = %v (want {5:2 7:2 9:2})", got)
		want := map[int64]int64{5: 2, 7: 2, 9: 2}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GROUP BY a.qid = %v, want %v", got, want)
		}
	})

	// ---- P2: correlated EXISTS referencing a duplicate outer alias ----
	// a.id is unique to the p leg. EXISTS is true for outer rows with a.id=1.
	// Java binds the unique p leg and answers; an alias-keyed outer-scope
	// map loses a leg and falsely reports 42703.
	t.Run("P2_correlated_exists", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM q WHERE a.id = 1)")
		if err != nil {
			t.Fatalf("correlated EXISTS over dup outer alias errored: %v", err)
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
		t.Logf("ACTUAL correlated-EXISTS ids = %v (want [1 1 1] — id=1 × q(3))", got)
		if !reflect.DeepEqual(got, []int64{1, 1, 1}) {
			t.Errorf("correlated EXISTS ids = %v, want [1 1 1]", got)
		}
	})

	// ---- P2 control: DISTINCT outer aliases must answer identically ----
	t.Run("P2_control_distinct", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id FROM p AS a, q AS b WHERE EXISTS (SELECT 1 FROM q WHERE a.id = 1)")
		if err != nil {
			t.Fatalf("distinct-alias control errored: %v", err)
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
		t.Logf("ACTUAL distinct-alias control ids = %v (want [1 1 1])", got)
		if !reflect.DeepEqual(got, []int64{1, 1, 1}) {
			t.Errorf("distinct-alias control = %v, want [1 1 1]", got)
		}
	})

	// ---- P2: the SECOND-leg twins (Java live-verified: [7 7]) ----
	// The correlated reference binds the SECOND outer leg (q's qid; two p
	// rows survive as the EXISTS is true only where qid matches). The dup
	// variant additionally exercises the resolved flatten's binding-correlation
	// quantifier naming on top of
	// the buried-reference rebase.
	secondLeg := func(t *testing.T, q string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("second-leg correlated EXISTS errored: %v\n  sql: %s", err, q)
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
		t.Logf("ACTUAL second-leg ids = %v (want [7 7])\n  sql: %s", got, q)
		if !reflect.DeepEqual(got, []int64{7, 7}) {
			t.Errorf("second-leg correlated EXISTS = %v, want [7 7]\n  sql: %s", got, q)
		}
	}
	t.Run("P2_second_leg_distinct", func(t *testing.T) {
		secondLeg(t, "SELECT b.qid FROM p AS a, q AS b WHERE EXISTS (SELECT 1 FROM p WHERE b.qid = 7)")
	})
	t.Run("P2_second_leg_dup", func(t *testing.T) {
		secondLeg(t, "SELECT a.qid FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM p WHERE a.qid = 7)")
	})

	// ---- P1 fold twin: projected-EXISTS + dup-alias ORDER BY ----
	// The FOLD sort path (classifySortSource / sortKeySourceValue) is a
	// separate seam from qualifyShadowedSortKeys: its legAliases carry the
	// DISPLAY aliases. The dup-alias sort key must still order by the bound
	// leg's values through the fold.
	t.Run("P1_fold_order_by_dup", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.qid, EXISTS (SELECT 1 FROM p WHERE id = 1) AS e FROM p AS a, q AS a ORDER BY a.qid DESC")
		if err != nil {
			t.Fatalf("projected-EXISTS + dup ORDER BY errored: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var qid sql.NullInt64
			var e bool
			if err := rows.Scan(&qid, &e); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !qid.Valid {
				t.Errorf("fold sort key served NULL — the dup-leg key silently missed")
				continue
			}
			if !e {
				t.Errorf("projected EXISTS = false, want true (p has id=1)")
			}
			got = append(got, qid.Int64)
		}
		t.Logf("ACTUAL fold ORDER BY qid DESC = %v (want [9 9 7 7 5 5])", got)
		if !reflect.DeepEqual(got, []int64{9, 9, 7, 7, 5, 5}) {
			t.Errorf("fold ORDER BY a.qid DESC = %v, want [9 9 7 7 5 5]", got)
		}
	})

	// ---- P4: minted-binding queries that reach a DISPLAY-keyed (name-model)
	// construction must decline LOUDLY — never silent NULLs. The name-keyed
	// merged row cannot represent a later duplicate leg, so until a given path
	// is made binding-aware, correct-or-loud means a typed decline. Each shape
	// below served silent NULL rows before its path was made binding-aware.
	loudDecline := func(t *testing.T, q, wantSub string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			// Drain: if it answers, every value must be non-NULL (never
			// silent wrong rows) — and the shape should not answer at all
			// until the path speaks bindings.
			defer rows.Close()
			n, nulls := 0, 0
			for rows.Next() {
				var v sql.NullInt64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
				if !v.Valid {
					nulls++
				}
			}
			if rerr := rows.Err(); rerr != nil {
				t.Fatalf("rows: %v", rerr)
			}
			t.Errorf("must decline loudly, got %d rows (%d NULL — silent wrong rows)\n  sql: %s", n, nulls, q)
			return
		}
		if wantSub != "" && !strings.Contains(err.Error(), wantSub) {
			t.Errorf("decline error = %v, want it to contain %q\n  sql: %s", err, wantSub, q)
		}
	}
	// (a) A shadowing existential over a dup outer. The
	// exists subquery's own `p AS a` SHADOWS the outer dup alias, so `a.id = 1`
	// is INNER-scoped (the subquery's p) — the EXISTS is leg-independent and
	// always true (p has id=1). Same class as P4e: the minted-dup outer
	// flows its positional row through the identity-FlatMap pass-through and
	// a.qid binds the q leg. 6 rows, qid ∈ {5,7,9} each twice. (This valid query
	// used to be declined — a Java-parity reach gap, not a real collision;
	// the outer `a.qid` sees only the two outer legs, the subquery's `a` is a
	// separate scope.)
	t.Run("P4a_shadowing_exists_dup", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.qid FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM p AS a WHERE a.id = 1)")
		if err != nil {
			t.Fatalf("shadowing leg-independent EXISTS over a dup outer errored: %v", err)
		}
		defer rows.Close()
		counts := map[int64]int{}
		total := 0
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				t.Fatal("a.qid served NULL — the minted-dup upper must resolve positionally")
			}
			counts[v.Int64]++
			total++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if total != 6 || counts[5] != 2 || counts[7] != 2 || counts[9] != 2 {
			t.Fatalf("P4a = %d rows %v, want 6 rows {5:2,7:2,9:2} (a.qid binds q, inner-scoped EXISTS always true)", total, counts)
		}
	})
	// (b) Arity-3 FROM with a dup pair + uncorrelated EXISTS — the arity
	// narrowing routes OFF the gate.
	t.Run("P4b_arity3_dup_exists", func(t *testing.T) {
		loudDecline(t, "SELECT a.qid FROM p AS a, q AS a, q AS b WHERE EXISTS (SELECT 1 FROM p)",
			"duplicate FROM alias")
	})
	// (c) Correlated SCALAR subquery over a dup outer — the scalar lowering
	// keys legs by display alias (not yet binding-aware).
	t.Run("P4c_scalar_dup_outer", func(t *testing.T) {
		loudDecline(t, "SELECT (SELECT a.id FROM q WHERE a.id = 1) FROM p AS a, q AS a",
			"duplicate outer FROM alias")
	})
	t.Run("P4d_scalar_dup_outer_agg", func(t *testing.T) {
		loudDecline(t, "SELECT (SELECT MAX(qid) FROM q WHERE a.id = 1) FROM p AS a, q AS a",
			"duplicate outer FROM alias")
	})
	// (e) The GATED flatten's leg-independent EXISTS.
	// The minted-dup outer (p AS a, q AS a) resolves ordinally and a.qid binds the q
	// leg; the leg-independent EXISTS (SELECT 1 FROM p, always true) leaves the
	// exists inner's baked-ref probe negative, so the executor's identity-FlatMap
	// pass-through keys on the OUTER's own ordinal seed (downstreamLegWindows) to
	// flow the positional row through — the minted-dup upper QOV(Q$DUP) then
	// resolves positionally instead of declining loudly. Cross
	// product p(2) × q(3) = 6 rows, qid ∈ {5,7,9} each twice. The correlated
	// control is P2_second_leg_dup above.
	t.Run("P4e_leg_independent_exists_dup", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.qid FROM p AS a, q AS a WHERE EXISTS (SELECT 1 FROM p)")
		if err != nil {
			t.Fatalf("leg-independent EXISTS over a dup outer errored (must flow the positional row through): %v", err)
		}
		defer rows.Close()
		counts := map[int64]int{}
		total := 0
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				t.Fatal("a.qid served NULL — the minted-dup upper must resolve positionally, never off the name Datum")
			}
			counts[v.Int64]++
			total++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if total != 6 || counts[5] != 2 || counts[7] != 2 || counts[9] != 2 {
			t.Fatalf("P4e = %d rows %v, want 6 rows {5:2,7:2,9:2} (a.qid binds the q leg, EXISTS always true)", total, counts)
		}
	})
	// (f) The UNION face: a dup-alias branch under UNION ALL keeps its
	// per-attribute reference display-keyed and dies LOUD at the executor's
	// ordinal-resolution guard (zero rows served; the distinct-alias control
	// answers). Correct-or-loud holds; the typed-decline-at-translation
	// upgrade rides the flip rider.
	t.Run("P4f_union_dup_branch", func(t *testing.T) {
		loudDecline(t, "SELECT a.qid FROM p AS a, q AS a UNION ALL SELECT id FROM p",
			"not resolvable in the runtime row")
	})

	// ---- P5: the LEFT-box dup FLIP ----
	// Duplicate aliases across a LEFT box now resolve correctly: the
	// binding-keyed seed distinguishes the legs and the per-attribute
	// reference binds through the pad.
	t.Run("P5_left_box_dup_first_leg", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.v FROM p AS a LEFT JOIN q AS a ON a.qid = a.id + 4")
		if err != nil {
			t.Fatalf("LEFT-box dup first-leg errored (the flipped class): %v", err)
		}
		defer rows.Close()
		got := map[int64]bool{}
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !v.Valid {
				t.Fatal("first-leg value NULL — the pad must never reach the preserved leg")
			}
			got[v.Int64] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(got) != 2 || !got[10] || !got[20] {
			t.Errorf("first-leg values = %v, want {10, 20}", got)
		}
	})
	t.Run("P5_left_box_dup_second_leg_pad", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.qid FROM p AS a LEFT JOIN q AS a ON a.qid = a.id + 4")
		if err != nil {
			t.Fatalf("LEFT-box dup second-leg errored: %v", err)
		}
		defer rows.Close()
		n, pads := 0, 0
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
			if !v.Valid {
				pads++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n != 2 || pads != 1 {
			t.Errorf("second-leg = %d rows (%d pads), want 2 rows with exactly 1 NULL pad (q(5) matches p.id=1 only)", n, pads)
		}
	})
}
