package sqldriver_test

// RFC-173 Outcome-B B1 (U-1): a multi-way (>=3) INNER join under a correlated
// WHERE EXISTS now ORDINALIZES while preserving the index SARG. The mechanism
// (translateExistsOverGatheredCluster): the join + the WHERE's non-EXISTS
// conjuncts translate as their OWN gathered ordinal cluster (separately
// enumerated — index probes live), the existential wraps it as an outer
// semi-join, and every leg reference (the folded projection + each EXISTS
// correlation) is rebased at translation onto the box's positional output
// (ofOrdinal over the box QOV via the seed's leg-window layout,
// values.OrdinalSeedLegWindows). A flat [ForEach×N, Existential] select would
// instead be implementable only MATERIALIZED (PartitionSelectRule is
// ForEach-only), which is why the wrap + the SelectMergeRule
// existential-wrap guard exist.
//
// SCOPE: queries with an intervening ORDER BY/LIMIT keep the name-model plan
// (the fold's chain re-application emits leg-qualified reads above the wrap —
// a booked extension), so these certs compare row SETS, no ORDER BY.
//
// This test pins: (a) ROW falsification — an EXISTS correlated to the 3rd/4th
// leg resolves to THAT leg's ordinal window (a wrong-window bake would cross the
// answers; the data makes every leg's answer distinct); (b) PLAN SHAPE — the
// SARG'd index probe survives and the plan is NOT a materialized cross-product.
// The ORDINAL-fired proof is the producer census (dualwindow
// TestFDB_RFC173_ProducerCensus: 0 name-model firings on this shape) — asserted
// there because the census observer is a process-global unsafe to install in
// this parallel suite; SARG-here + ordinal-fired-there together satisfy the
// "don't pass on the name-model plan" cert requirement.

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RFC173S4_B1_NwayExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_b1_nway_exists"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	// r.rid / s.sid are the PKs the joins SARG (r.rid=p.id, s.sid=p.id) — the
	// index probe the plan-shape assertion checks.
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE b1nwe_tmpl"+
		" CREATE TABLE p (id BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE q (qid BIGINT, PRIMARY KEY (qid))"+
		" CREATE TABLE r (rid BIGINT, rc BIGINT, PRIMARY KEY (rid))"+
		" CREATE TABLE s (sid BIGINT, sc BIGINT, PRIMARY KEY (sid))"+
		" CREATE TABLE e (eid BIGINT, eref BIGINT, PRIMARY KEY (eid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE b1nwe_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"INSERT INTO p VALUES (1), (2), (3)",
		"INSERT INTO q VALUES (1), (2), (3)",                   // q.qid = p.id: all three join
		"INSERT INTO r VALUES (1, 100), (2, 200), (3, 300)",    // r.rid = p.id; rc distinct
		"INSERT INTO s VALUES (1, 10), (2, 20), (3, 30)",       // 4th leg
		"INSERT INTO e VALUES (501, 1), (502, 200), (503, 30)", // eref matches p.id=1, r.rc=200, s.sc=30
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	// idSet runs q and returns the sorted result ids (row-SET comparison — B1
	// scopes out ORDER BY; see the header).
	idSet := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query %q: %v", q, qErr)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if sErr := rows.Scan(&id); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got = append(got, id)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		return got
	}
	eq := func(t *testing.T, name string, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: rows=%v want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: rows=%v want %v", name, got, want)
			}
		}
	}

	// Correlate to the 1st leg (p): e.eref=1 matches p.id=1 → {1}.
	t.Run("correlate_first_leg", func(t *testing.T) {
		eq(t, "correlate_first_leg", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id)"),
			[]int64{1})
	})

	// FALSIFICATION: correlate to the 3rd leg (r.rc). e.eref=200 matches r.rc=200
	// → r.rid=2 → p.id=2 → {2}. A wrong-window bake would NOT yield {2}.
	t.Run("correlate_third_leg", func(t *testing.T) {
		eq(t, "correlate_third_leg", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = r.rc)"),
			[]int64{2})
	})

	// 4-way, correlate to the 4th leg (s.sc) → {3}. Holds past 3 legs.
	t.Run("correlate_fourth_leg_4way", func(t *testing.T) {
		eq(t, "correlate_fourth_leg_4way", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id JOIN s ON s.sid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = s.sc)"),
			[]int64{3})
	})

	// A non-EXISTS WHERE conjunct rides INSIDE the box (baked/pushed with the
	// join): p.id=1 has the e-match but r.rc=100 fails >150; p.id=2,3 pass the
	// conjunct but have no e-match → {}.
	t.Run("conjunct_and_exists", func(t *testing.T) {
		eq(t, "conjunct_and_exists", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE r.rc > 150 AND EXISTS (SELECT 1 FROM e WHERE e.eref = p.id)"),
			nil)
	})

	// NOT EXISTS over the N-way cluster (negated existential): the complement.
	t.Run("not_exists_third_leg", func(t *testing.T) {
		eq(t, "not_exists_third_leg", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE NOT EXISTS (SELECT 1 FROM e WHERE e.eref = r.rc)"),
			[]int64{1, 3})
	})

	// Comma-join form (the WHERE carries the join conjuncts — they must land
	// INSIDE the box to SARG): same answer as the explicit-ON form.
	t.Run("comma_join_third_leg", func(t *testing.T) {
		eq(t, "comma_join_third_leg", idSet(t,
			"SELECT p.id FROM p, q, r WHERE q.qid = p.id AND r.rid = p.id AND EXISTS (SELECT 1 FROM e WHERE e.eref = r.rc)"),
			[]int64{2})
	})

	// ORDER BY (the scoped-out chain shape) keeps the NAME-MODEL plan and must
	// still answer correctly — the decline is fail-open, never wrong rows.
	t.Run("order_by_falls_back_correct", func(t *testing.T) {
		eq(t, "order_by_falls_back_correct", idSet(t,
			"SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = r.rc) ORDER BY p.id"),
			[]int64{2})
	})

	// MIXED projection (the impl review's wrong-rows catch): a bare column AND a
	// computed expression in one SELECT. Pre-fix, the computed field's baked
	// ordinals flipped the wrap cursor to birth-ordinal evaluation while the bare
	// column stayed a lazy frontier read → (NULL, 101). The TOTAL rebase bakes
	// both, and wrapRVFullyBaked declines any RV that is not uniformly baked.
	// p.id=1 (the e-match row): p.id + r.rc = 1 + 100 = 101.
	t.Run("mixed_bare_and_computed_projection", func(t *testing.T) {
		rows, qErr := db.QueryContext(ctx,
			"SELECT p.id, p.id + r.rc FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id)")
		if qErr != nil {
			t.Fatalf("query: %v", qErr)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no rows, want (1, 101)")
		}
		var id, sum int64
		if sErr := rows.Scan(&id, &sum); sErr != nil {
			t.Fatalf("scan (a NULL here is the mixed-channel drift): %v", sErr)
		}
		if id != 1 || sum != 101 {
			t.Fatalf("mixed projection = (%d, %d), want (1, 101)", id, sum)
		}
		if rows.Next() {
			t.Fatalf("extra rows, want exactly one")
		}
	})

	// PURE COMPUTED projection — the RV-rebase channel that IS runtime-observable
	// (a slot skew flips the arithmetic's operands): p.id + r.rc = 101.
	t.Run("computed_projection", func(t *testing.T) {
		eq(t, "computed_projection", idSet(t,
			"SELECT p.id + r.rc FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id)"),
			[]int64{101})
	})

	// SLOT-ADJACENT bare projection: project the 3rd leg's PK (r.rid, an interior
	// slot whose neighbor r.rc holds distinct 100/200/300 — p.id == q.qid ==
	// r.rid by the join, so those columns cannot discriminate slots). Under the
	// TOTAL rebase the bare column is baked too, so a slot skew flips this
	// subtest (r.rc=100 instead of r.rid=1) — genuinely discriminating now.
	t.Run("slot_discriminator_projects_rid", func(t *testing.T) {
		eq(t, "slot_discriminator_projects_rid", idSet(t,
			"SELECT r.rid FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = p.id)"),
			[]int64{1})
	})

	// PLAN SHAPE: the join keeps its SARG'd PK probes ([=]) and is NOT a
	// materialized cross-product. Proves the wrap kept the join separately
	// enumerated (FlatMap(join, FirstOrDefault(e)) — the P2/P7 shape) instead of
	// collapsing into the N-way existential arm's cross-product.
	t.Run("plan_keeps_sarg_not_cross_product", func(t *testing.T) {
		var explain string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT p.id FROM p JOIN q ON q.qid = p.id JOIN r ON r.rid = p.id WHERE EXISTS (SELECT 1 FROM e WHERE e.eref = r.rc)").Scan(&explain); err != nil {
			t.Fatalf("explain: %v", err)
		}
		if !strings.Contains(explain, "[=]") {
			t.Fatalf("B1 lost the SARG'd index probe (collapsed to a cross product?):\n%s", explain)
		}
		if strings.Contains(explain, "NestedLoopJoin(INNER, NestedLoopJoin(INNER, Scan(P), Scan(Q)), Scan(R))") {
			t.Fatalf("B1 regressed to the materialized P×Q×R cross product:\n%s", explain)
		}
	})
}
