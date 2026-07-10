package sqldriver_test

// RFC-173 P2 existential-flatten slice, commit A — the N-way WHERE-EXISTS
// gated seed at TRANSLATION (rfcs/173-ordinal-column-resolution.md, "P2
// EXISTENTIAL-FLATTEN TRANSLATION SEED"). translateJoinWithExists retired its
// arity-exactly-2 narrowing and delegates gated flattens to the ONE gated
// seed construction (translateGatheredInnerCluster) with the existential
// quantifiers riding the flat select — so a comma/N-way FROM with WHERE
// EXISTS is born `[ForEach×N, Existential]` ordinal instead of the anchored
// 2+1 the rewriter had to flatten and the executor had to re-derive.
//
// Every row set below is LIVE-JAVA-GROUNDED (4.12.11.0 conformance server,
// the P2 probe basket recorded in the RFC design entry): Go and Java agreed
// byte-identically on all supported shapes BEFORE the flip, so these pins
// hold behavior invariant while the seed model underneath changes. Fixture
// pks are DISJOINT across record types (records share the store extent keyed
// by pk; a collision silently overwrites).

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_RFC173P2_NWayWhereExistsFlatten(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173p2nwf"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173p2nwf_tmpl"+
		" CREATE TABLE t_pa (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE t_qb (qid BIGINT, pref BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE t_rc (rid BIGINT, rref BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE t_ed (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"+
		" CREATE TABLE t_sd (sid BIGINT, sref BIGINT, PRIMARY KEY (sid))"+
		" CREATE TABLE t_ka (id BIGINT, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE t_kb (id2 BIGINT, k BIGINT, kref BIGINT, PRIMARY KEY (id2))"+
		" CREATE TABLE t_kc (id3 BIGINT, k BIGINT, kref2 BIGINT, PRIMARY KEY (id3))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173p2nwf_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// pa.id=2 joins through qb/rc/sd but has no t_ed match — the existential
	// is DISCRIMINATING on every correlated shape (pk ranges disjoint:
	// 1-2 / 101-102 / 201-202 / 301 / 401-402).
	for _, stmt := range []string{
		"INSERT INTO t_pa VALUES (1, 10), (2, 20)",
		"INSERT INTO t_qb VALUES (101, 1), (102, 2)",
		"INSERT INTO t_rc VALUES (201, 1), (202, 2)",
		"INSERT INTO t_ed VALUES (301, 1)",
		"INSERT INTO t_sd VALUES (401, 1), (402, 2)",
		"INSERT INTO t_ka VALUES (501, 1)",
		"INSERT INTO t_kb VALUES (601, 7, 501)",
		"INSERT INTO t_kc VALUES (701, 9, 501)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	queryInts := func(t *testing.T, sqlText string) []int64 {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, sqlText)
		if qErr != nil {
			t.Fatalf("query %q: %v", sqlText, qErr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if sErr := rows.Scan(&v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, v)
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows: %v", rErr)
		}
		return got
	}

	t.Run("comma_3way_where_exists", func(t *testing.T) {
		t.Parallel()
		// Java: [[10]].
		got := queryInts(t, "SELECT pa.v FROM t_pa AS pa, t_qb AS qb, t_rc AS rc"+
			" WHERE qb.pref = pa.id AND rc.rref = pa.id"+
			" AND EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)")
		if len(got) != 1 || got[0] != 10 {
			t.Fatalf("rows = %v, want [10]", got)
		}
	})

	t.Run("comma_3way_not_exists", func(t *testing.T) {
		t.Parallel()
		// Java: [[20]] — the anti-join twin through the same N-way seed.
		got := queryInts(t, "SELECT pa.v FROM t_pa AS pa, t_qb AS qb, t_rc AS rc"+
			" WHERE qb.pref = pa.id AND rc.rref = pa.id"+
			" AND NOT EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)")
		if len(got) != 1 || got[0] != 20 {
			t.Fatalf("rows = %v, want [20]", got)
		}
	})

	t.Run("comma_4way_where_exists", func(t *testing.T) {
		t.Parallel()
		// Java: [[10]] — depth-3 left-deep chain in the executor fold.
		got := queryInts(t, "SELECT pa.v FROM t_pa AS pa, t_qb AS qb, t_rc AS rc, t_sd AS sd"+
			" WHERE qb.pref = pa.id AND rc.rref = pa.id AND sd.sref = pa.id"+
			" AND EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)")
		if len(got) != 1 || got[0] != 10 {
			t.Fatalf("rows = %v, want [10]", got)
		}
	})

	t.Run("comma_3way_projected_exists", func(t *testing.T) {
		t.Parallel()
		// Java: [[10 true] [20 false]] — the projected consumer over the same
		// born-flat select.
		rows, qErr := db.QueryContext(ctx,
			"SELECT pa.v, EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)"+
				" FROM t_pa AS pa, t_qb AS qb, t_rc AS rc"+
				" WHERE qb.pref = pa.id AND rc.rref = pa.id")
		if qErr != nil {
			t.Fatalf("query: %v", qErr)
		}
		defer rows.Close()
		var got [][2]any
		for rows.Next() {
			var v int64
			var ex sql.NullBool
			if sErr := rows.Scan(&v, &ex); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, [2]any{v, ex.Valid && ex.Bool})
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows: %v", rErr)
		}
		want := [][2]any{{int64(10), true}, {int64(20), false}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})

	t.Run("comma_3way_dupcol_exists", func(t *testing.T) {
		t.Parallel()
		// Java: [[1]]. `k` lives on ALL THREE legs and the EXISTS correlates
		// on ka.k specifically (kb.k=7 / kc.k=9 have no t_ed match) — a leg
		// mis-bind in the gathered seed's windows flips the answer to empty
		// (the NewAnchoredJoinRecord last-leg-wins hazard, comma form).
		got := queryInts(t, "SELECT ka.k FROM t_ka AS ka, t_kb AS kb, t_kc AS kc"+
			" WHERE kb.kref = ka.id AND kc.kref2 = ka.id"+
			" AND EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = ka.k)")
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("rows = %v, want [1] (a mis-bound k window flips this empty)", got)
		}
	})

	t.Run("limit_distinct_inner_stays_name_model", func(t *testing.T) {
		t.Parallel()
		// An existential inner with LIMIT or DISTINCT is not a
		// simple scan-family subquery (LIMIT 0 / OFFSET can empty a non-empty
		// inner; both are declined by the executor's existInnerIsScanSafe), so
		// the existential-inner narrowing keeps the shape on the name-model
		// route. Retiring the arity-2 narrowing without the guard regressed
		// both to 0AF00 (base planned them). The inners are always-true (t_ed
		// non-empty) and UNCORRELATED, so every joined outer row passes: both
		// pa.id=1 (qb.pref=1, rc.rref=1) and pa.id=2 (qb.pref=2, rc.rref=2)
		// join → {10, 20}. Java parity: an uncorrelated always-true EXISTS
		// keeps all rows regardless of the inner's cardinality shaping.
		for _, sub := range []struct{ name, inner string }{
			{"limit", "SELECT 1 FROM t_ed LIMIT 1"},
			{"distinct", "SELECT DISTINCT eref FROM t_ed"},
		} {
			got := queryInts(t, "SELECT pa.v FROM t_pa AS pa, t_qb AS qb, t_rc AS rc"+
				" WHERE qb.pref = pa.id AND rc.rref = pa.id AND EXISTS ("+sub.inner+")")
			counts := map[int64]int{}
			for _, v := range got {
				counts[v]++
			}
			if len(counts) != 2 || counts[10] != 1 || counts[20] != 1 {
				t.Fatalf("%s-inner N-way EXISTS rows = %v, want {10,20} (name-model route; 0AF00 is the codex P1#3 regression)", sub.name, got)
			}
		}
	})

	t.Run("derived_table_leg_stays_name_model", func(t *testing.T) {
		t.Parallel()
		// A DERIVED-TABLE leg (an opaque LogicalCTE box) in the comma cluster
		// is NOT fold-plannable, so the existential flatten narrows OFF the gate
		// and the query plans via the name-model route — the passthrough-derived
		// twin of comma_3way_where_exists, which returns [[10]]. Retiring the
		// arity-2 narrowing without the scan-family guard REGRESSED this shape to
		// 0AF00 (the fold declined the box leg); the guard restores the
		// pre-P2 name-model fallback. Java-grounded [[10]] (the derived-leg
		// N-way EXISTS structure verified Go==Java live).
		got := queryInts(t, "SELECT pa.v FROM t_pa AS pa, (SELECT qid, pref FROM t_qb) AS dq, t_rc AS rc"+
			" WHERE dq.pref = pa.id AND rc.rref = pa.id"+
			" AND EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)")
		if len(got) != 1 || got[0] != 10 {
			t.Fatalf("derived-table-leg N-way EXISTS rows = %v, want [10] (name-model route; a 0AF00 is the pre-guard regression)", got)
		}
	})

	// The executor fold admits exactly ONE trailing existential
	// — `[ForEach×N, Existential×M≥2]` has no arm, and PartitionSelectRule
	// declines any existential quantifier. This shape declined at planning
	// BEFORE the translation-seed flip (the post-merge name-model select hit
	// the same wall) and must KEEP declining fail-closed — never wrong rows.
	// FLIPS when the M≥2 fold arm lands (the multi-EXISTS-in-ON reach gap's
	// executor half; Java answers the ON twin `[[10],[10]]` — RFC P2 design
	// entry, live-grounded).
	t.Run("multi_exists_fail_closed", func(t *testing.T) {
		t.Parallel()
		_, qErr := db.QueryContext(ctx, "SELECT pa.v FROM t_pa AS pa, t_qb AS qb, t_rc AS rc"+
			" WHERE qb.pref = pa.id AND rc.rref = pa.id"+
			" AND EXISTS (SELECT 1 FROM t_ed AS ed WHERE ed.eref = pa.id)"+
			" AND EXISTS (SELECT 1 FROM t_sd AS sd WHERE sd.sref = pa.id)")
		if qErr == nil {
			t.Fatal("M=2 existentials over an N-way flatten planned — the M≥2 fold arm landed; " +
				"row-verify against the grounded Java twin and flip this pin to rows")
		}
		var apiErr *api.Error
		if !errors.As(qErr, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("multi-EXISTS decline = %v, want a loud %s (fail-closed, never wrong rows)",
				qErr, api.ErrCodeUnsupportedQuery)
		}
	})
}

// TestFDB_RFC173P2_UncorrExistsPeel pins the existInnerIsScanSafe peel
// widening (Projection/TypeFilter) the P2 flip forced: an UNCORRELATED
// existential inner plans as Projection(1, TypeFilter(Scan)) — row-count-
// preserving wrappers outside the guard's hazard set — and the born-flat
// N-way seed must serve it (pre-P2 the shape planned through the name-model
// 2+1 route; without the peel the flip REGRESSED it to 0AF00, caught by the
// P4b suite red). Fixture and rows are the live-Java peel probe VERBATIM
// (4.12.11.0): nonempty inner → the full 18-row cross; a correlated
// empty-match inner → 0 rows (the peel must not manufacture existence for a
// no-match correlation). Order unspecified — count-map.
//
// The emit-on-empty SAFETY POLE (FirstOrDefault/DefaultOnEmpty stay
// UNPEELED) is pinned two ways, NOT by the empty-inner row count above (that
// inner plans without a FOD node, so it would not redden if a FOD arm were
// added): STRUCTURALLY by TestExistInnerIsScanSafe's compositional cases
// (projection(firstOrDefault(scan))→false, typeFilter(nlj)→false — a FOD/nlj
// behind a peel still declines), and — because a non-simple existential
// inner is narrowed off the born-flat gate at TRANSLATION and never reaches
// the fold — by nway_nonsimple_inner_stays_name_model below (a LEFT-join
// exist-inner plans via the name-model route, proving the narrowing keeps a
// non-emptiness-preserving inner away from the fold's FOD signal).
func TestFDB_RFC173P2_UncorrExistsPeel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/rfc173p2peel"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE rfc173p2peel_tmpl"+
		" CREATE TABLE p (id BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE t_ed (eid BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE rfc173p2peel_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"INSERT INTO p VALUES (1, 10), (2, 20)",
		"INSERT INTO q VALUES (5), (7), (9)",
		"INSERT INTO t_ed VALUES (301)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	t.Run("pure_cross_uncorr_exists_nonempty", func(t *testing.T) {
		t.Parallel()
		// Java: 18 rows, qid ∈ {5,7,9} each ×6.
		rows, qErr := db.QueryContext(ctx,
			"SELECT a.qid FROM p AS x, q AS a, q AS b WHERE EXISTS (SELECT 1 FROM t_ed)")
		if qErr != nil {
			t.Fatalf("pure-cross + uncorrelated EXISTS errored (the peel regression): %v", qErr)
		}
		defer rows.Close()
		counts := map[int64]int{}
		total := 0
		for rows.Next() {
			var v int64
			if sErr := rows.Scan(&v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			counts[v]++
			total++
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows: %v", rErr)
		}
		if total != 18 || counts[5] != 6 || counts[7] != 6 || counts[9] != 6 {
			t.Fatalf("rows = %d %v, want 18 {5:6,7:6,9:6} (Java-grounded)", total, counts)
		}
	})

	t.Run("pure_cross_uncorr_exists_empty_inner", func(t *testing.T) {
		t.Parallel()
		// Java: 0 rows — a correlated no-match inner (eid=999 selects nothing)
		// must stay EXISTS-false through the peeled Projection/TypeFilter/Filter
		// wrappers. NOTE: this inner plans WITHOUT a FirstOrDefault node, so it
		// does NOT exercise the emit-on-empty pole — that is the unit + the
		// N-way non-scan-safe decline below (see the doc-comment).
		rows, qErr := db.QueryContext(ctx,
			"SELECT a.qid FROM p AS x, q AS a, q AS b WHERE EXISTS (SELECT 1 FROM t_ed WHERE t_ed.eid = 999)")
		if qErr != nil {
			t.Fatalf("empty-inner EXISTS errored: %v", qErr)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows: %v", rErr)
		}
		if n != 0 {
			t.Fatalf("empty-inner EXISTS returned %d rows, want 0 (the peel must never manufacture existence)", n)
		}
	})

	t.Run("nway_nonsimple_inner_stays_name_model", func(t *testing.T) {
		t.Parallel()
		// A NON-SIMPLE existential inner (here a LEFT JOIN — also LIMIT,
		// DISTINCT, aggregate) is narrowed OFF the born-flat gate at
		// translation and plans via the name-model route, exactly as the
		// pre-P2 arity-2 narrowing did: the born-flat fold's FirstOrDefault
		// emptiness signal is only faithful for an emptiness-preserving scan
		// inner, so a non-simple inner must NOT reach it. This is why the
		// emit-on-empty safety pole is unreachable from the N-way arm via SQL
		// (the pole is pinned structurally by TestExistInnerIsScanSafe's
		// compositional cases). The LEFT-join inner is always-true (t_ed
		// non-empty), so the query returns the full p × q1 × e2 cross: 2 rows
		// {10, 20}. Retiring the arity-2 narrowing without this guard
		// REGRESSED the LIMIT/DISTINCT twins to 0AF00 (the fold declined the
		// non-scan inner with no name-model fallback).
		rows, qErr := db.QueryContext(ctx,
			"SELECT p.v FROM p, q AS q1, t_ed AS e2 WHERE q1.qid = 5 AND e2.eid = 301"+
				" AND EXISTS (SELECT 1 FROM t_ed LEFT JOIN q AS qn ON qn.qid = t_ed.eid)")
		if qErr != nil {
			t.Fatalf("N-way LEFT-join existential inner errored (the narrowing must route it to name-model): %v", qErr)
		}
		defer rows.Close()
		counts := map[int64]int{}
		for rows.Next() {
			var v int64
			if sErr := rows.Scan(&v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			counts[v]++
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("rows: %v", rErr)
		}
		if len(counts) != 2 || counts[10] != 1 || counts[20] != 1 {
			t.Fatalf("rows = %v, want {10:1,20:1} (name-model route, always-true LEFT-join inner)", counts)
		}
	})
}
