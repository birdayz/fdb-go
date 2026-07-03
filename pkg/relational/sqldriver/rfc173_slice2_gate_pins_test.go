package sqldriver_test

// RFC-173 Slice 2 W3b-2 — the runtime halves of the wedge's coexistence gate
// pins (rfc-173 §4 Slice 2 "the gate carries pins" + §5 item 2), E2E over real
// FDB. The W2 pins cover the TRANSLATION half (cluster-arity walk + wedgeGate
// decisions, rfc173_cluster_gate_test.go); these pin what the user observes:
//
//   - Gate pin (a) runtime half: a 2-way join consumed INSIDE a 3-way inner
//     cluster stays name-model — correct rows AND the pre-flip plan shape
//     (verified byte-identical to master 12516e33f during W3b-2: the nested
//     FlatMap-over-FlatMap anchored merge chain).
//   - Gate pin (b) runtime half: the flattening-evasion shape
//     `FROM (a JOIN b) t1, (c JOIN d) t2 WHERE t1.aid = t2.cid` stays
//     name-model end-to-end. PARTIAL — see TestFDB_RFC173_GatePinB_
//     FlatteningEvasion: the shape is unplannable on master AND branch
//     (pre-existing 0AF00), so the pinned observable is the identical CLEAN
//     rejection (never silent rows, never ordinal artifacts). The
//     correct-rows half is blocked on the pre-existing derived-with-join
//     planner gap.
//   - §5's dedicated GROUP BY/HAVING-over-2-way-join pin: aggregation,
//     HAVING (on the aggregate and on the group key), and ORDER BY/LIMIT
//     sit correctly over a GATED (ordinal) 2-way join, with EXPLAIN
//     fragments proving the plan shape (StreamingAgg over the FlatMap join).
//   - §5's duplicate-name SELECT * pin over a gated join (beyond
//     TestFDB_AmbiguousColumnStar's a/b+name shape): both legs carry
//     bare-colliding ID and V columns; all four come back per-leg-correct
//     in positional scan order.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// rfc173PinExplain returns the EXPLAIN output for q.
func rfc173PinExplain(t *testing.T, db *sql.DB, ctx context.Context, q string) string {
	t.Helper()
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	return plan
}

// rfc173PinRows runs q and renders every row as "v1|v2|...|vN" in scan
// order (callers sort when the query has no ORDER BY). A query error here is
// also the guard against the RFC-173 loud internal errors
// (OrdinalResolutionError / BakedNameContextError / OrdinalBakeError) — those
// surface as query failures, so err==nil proves none fired.
func rfc173PinRows(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", q, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", q, err)
	}
	return out
}

// TestFDB_RFC173_GatePinA_TwoWayUnderThreeWay is gate pin (a)'s runtime half:
// a 2-way join consumed inside a 3-way inner cluster must remain name-model —
// the wedgeGate declines it (pinned at translation in
// TestRFC173S2_WedgeGate_Translation), and at runtime the 3-way cluster must
// return the correct rows through the UNCHANGED pre-flip plan. The plan
// snapshot fragments below were verified byte-identical between this branch
// and master 12516e33f (pre-flip) — the nested FlatMap(outer=FlatMap(...))
// chain IS the name-model anchored 2-way re-enumeration machinery
// (rule_partition_select bipartitions the 3-way select; a gated ordinal seed
// never produces a 3-quantifier select for it to partition).
func TestFDB_RFC173_GatePinA_TwoWayUnderThreeWay(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_gpa")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_gpa")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_gpa_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_gpa/s WITH TEMPLATE rfc173_gpa_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_gpa?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, av) VALUES (1, 100), (2, 200), (3, 300)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, bv) VALUES (10, 1, 111), (11, 1, 222), (12, 2, 333)")
	// c(103) dangles (b_id=99): the inner b↔c 2-way must not leak it.
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, b_id) VALUES (100, 10), (101, 10), (102, 12), (103, 99)")

	// Hand-computed: b10(a1)×{c100,c101}, b11(a1)×∅, b12(a2)×{c102}.
	want := []string{"1|10|100", "1|10|101", "2|12|102"}

	checkRowsAndPlan := func(t *testing.T, q string) string {
		t.Helper()
		got := rfc173PinRows(t, db, ctx, q)
		sort.Strings(got)
		if !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := rfc173PinExplain(t, db, ctx, q)
		// The distinguishing NAME-MODEL fragment: a 3-way inner cluster plans
		// as the anchored 2-way merge CHAIN — FlatMap over FlatMap. A single
		// gated 2-way plans as ONE FlatMap (see the GroupBy pin below); the
		// nested shape only exists where the name-model re-enumeration
		// machinery partitioned a ≥3-way select.
		if !strings.Contains(plan, "FlatMap(outer=FlatMap(") {
			t.Errorf("plan lost the nested name-model FlatMap merge chain:\n%s", plan)
		}
		if !strings.Contains(plan, "Project([A.ID, B.ID, C.ID]") {
			t.Errorf("plan lost the 3-column projection:\n%s", plan)
		}
		return plan
	}

	var commaPlan, joinPlan string
	t.Run("comma_threeway", func(t *testing.T) {
		commaPlan = checkRowsAndPlan(t,
			"SELECT a.id, b.id, c.id FROM a, b, c WHERE a.id = b.a_id AND b.id = c.b_id")
	})
	t.Run("explicit_join_threeway", func(t *testing.T) {
		joinPlan = checkRowsAndPlan(t,
			"SELECT a.id, b.id, c.id FROM a JOIN b ON a.id = b.a_id JOIN c ON b.id = c.b_id")
	})
	t.Run("mixed_join_comma_threeway", func(t *testing.T) {
		// The 2-way `a JOIN b` written explicitly, consumed by a comma join
		// with c — the exact "2-way under a 3-way cluster" surface shape.
		checkRowsAndPlan(t,
			"SELECT a.id, b.id, c.id FROM a JOIN b ON a.id = b.a_id, c WHERE b.id = c.b_id")
	})
	t.Run("syntax_independent_plan", func(t *testing.T) {
		// All three syntaxes reach the same 3-way cluster; comma and explicit
		// JOIN must produce the identical physical plan (they did pre-flip).
		if commaPlan == "" || joinPlan == "" {
			// Only reachable when a prior subtest already failed its EXPLAIN.
			t.Fatal("prior subtest failed to produce a plan snapshot")
		}
		if commaPlan != joinPlan {
			t.Errorf("comma vs explicit-JOIN 3-way plans diverged:\ncomma: %s\njoin:  %s", commaPlan, joinPlan)
		}
	})
	t.Run("derived_variant_status_quo_0AF00", func(t *testing.T) {
		// The `FROM (SELECT ...) s, c` spelling of the same cluster is NOT
		// expressible today: derived tables whose body contains a join do not
		// plan (pre-existing 0AF00 on master 12516e33f AND this branch —
		// buildDerivedTableSource declines join-bodied derived tables and the
		// comma-join cluster over them never yields a physical plan). Pin the
		// CLEAN rejection: the wedge must not turn this into silent rows or
		// ordinal artifacts. When the planner gap closes, this goes red and
		// the subtest must be upgraded to rows+plan assertions like the ones
		// above.
		assertUnsupported(t, db, ctx,
			"SELECT s.aid, s.bid, c.id FROM (SELECT a.id AS aid, b.id AS bid FROM a, b WHERE a.id = b.a_id) s, c WHERE s.bid = c.b_id")
	})
}

// TestFDB_RFC173_GatePinB_FlatteningEvasion is gate pin (b)'s runtime half —
// PARTIAL. The evasion shape `FROM (a JOIN b) t1, (c JOIN d) t2 WHERE
// t1.aid = t2.cid` is 2-way at translation and 4-way post-flattening; the W2
// gate pin proves it does NOT gate ordinal (cluster arity walk sees 4). The
// runtime-green half ("returns correct rows name-model") is BLOCKED on a
// pre-existing planner gap, identical on master 12516e33f and this branch:
//
//   - The comma/CTE spellings translate correctly (the cross-derived
//     predicate survives into the logical plan — asserted below) but the
//     Cascades planner cannot produce a physical plan: clean 0AF00.
//   - The explicit-JOIN spelling (`(...) t1 JOIN (...) t2 ON t1.aid =
//     t2.cid`) is WORSE — pre-existing silent wrong rows, NOT pinned here:
//     buildDerivedTableSource (embedded/logical_predicate.go) declines
//     join-bodied derived tables, scopeOK goes false, and
//     upgradeJoinOnPredicates' early `if !scopeOK { return nil }` silently
//     skips the ON upgrade — bypassing its own fail-closed backstop — so the
//     ON predicate drops and the join degrades to a cross product. That bug
//     predates the wedge (verified on a master worktree) and needs a
//     production fix + its own red→green pin; pinning its current wrong rows
//     green here would be forbidden expectation-adjustment.
//
// What this test pins TODAY is wedge-neutrality: the evasion shape fails
// EXACTLY as it failed pre-flip (clean 0AF00, predicate intact at
// translation), never ordinal artifacts, never silent rows. When the
// derived-with-join gap closes, assertUnsupported goes red and this must be
// upgraded to the full correct-rows assertion.
func TestFDB_RFC173_GatePinB_FlatteningEvasion(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_gpb")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_gpb")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_gpb_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, cv BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, c_id BIGINT, dw BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_gpb/s WITH TEMPLATE rfc173_gpb_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_gpb?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, av) VALUES (1, 100), (2, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id, bv) VALUES (10, 1, 111), (11, 2, 222)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, cv) VALUES (1, 51), (3, 53)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, c_id, dw) VALUES (1000, 1, 41), (1001, 3, 42)")

	const evasion = "SELECT t1.aid, t1.bv, t2.cid, t2.dw " +
		"FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1, " +
		"(SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) t2 " +
		"WHERE t1.aid = t2.cid"

	t.Run("comma_form_fails_cleanly", func(t *testing.T) {
		assertUnsupported(t, db, ctx, evasion)
	})
	t.Run("translation_keeps_cross_derived_predicate", func(t *testing.T) {
		// EXPLAIN falls back to the logical plan when no physical plan exists;
		// the cross-derived predicate must still be there — the clean-decline
		// path must never share the ON-drop bug's silent predicate loss.
		plan := rfc173PinExplain(t, db, ctx, evasion)
		if !strings.Contains(plan, "Filter(t1.aid = t2.cid)") {
			t.Errorf("logical plan lost the cross-derived predicate:\n%s", plan)
		}
	})
	t.Run("cte_form_fails_cleanly", func(t *testing.T) {
		assertUnsupported(t, db, ctx,
			"WITH t1 AS (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id), "+
				"t2 AS (SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) "+
				"SELECT t1.aid, t1.bv, t2.cid, t2.dw FROM t1, t2 WHERE t1.aid = t2.cid")
	})
	t.Run("explicit_join_form_fails_closed", func(t *testing.T) {
		// RED→GREEN for the ON-predicate-drop fix (embedded/
		// logical_predicate.go, upgradeJoinOnPredicates' !scopeOK early
		// return): these spellings previously returned the FULL CROSS
		// PRODUCT — buildDerivedTableSource declines the join-bodied derived
		// table, scope building fails, and the early return silently skipped
		// the ON upgrade, bypassing the function's own fail-closed backstop
		// (the same class as the fixed subquery-in-ON bug). Now a loud clean
		// decline: better no rows than wrong rows.
		assertUnsupported(t, db, ctx,
			"SELECT t1.aid, t1.bv, t2.cid, t2.dw "+
				"FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1 "+
				"JOIN (SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) t2 "+
				"ON t1.aid = t2.cid")
		assertUnsupported(t, db, ctx,
			"SELECT s.aid, c.id FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) s "+
				"JOIN c ON c.id = s.aid")
	})
	t.Run("solo_derived_join_control", func(t *testing.T) {
		// Control: ONE derived-with-join consumed alone hits the same
		// pre-existing gap — the evasion failure is the derived-with-join
		// capability, not the t1×t2 combination. When THIS goes red, the
		// planner learned the shape and the whole test must be upgraded.
		assertUnsupported(t, db, ctx,
			"SELECT t1.aid, t1.bv FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1 WHERE t1.aid = 1")
	})
}

// TestFDB_RFC173_GroupByHavingOverTwoWayJoin is §5's dedicated
// GROUP BY/HAVING-over-a-2-way-join pin, gated at Slice 2 ("where the join's
// merged row becomes authoritative"). The a↔c join is a maximal pure-inner
// 2-way cluster over non-join legs — GATED, so the aggregate consumes the
// ordinal merged row live. EXPLAIN pins the shape: StreamingAgg directly over
// the single FlatMap join (one FlatMap — the gated 2-way; contrast the nested
// chain in the 3-way pin above), HAVING as a PredicatesFilter above the agg.
func TestFDB_RFC173_GroupByHavingOverTwoWayJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_gbhj")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_gbhj")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_gbhj_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, cw BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_gbhj/s WITH TEMPLATE rfc173_gbhj_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_gbhj?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, av) VALUES (1, 100), (2, 200), (3, 300)")
	// Groups: a1 → {c100, c101} (count 2), a2 → {c102}, a3 → {c103}.
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id, cw) VALUES (100, 1, 7), (101, 1, 8), (102, 2, 9), (103, 3, 10)")

	t.Run("having_on_aggregate", func(t *testing.T) {
		q := "SELECT a.id, COUNT(c.id) FROM a JOIN c ON c.a_id = a.id GROUP BY a.id HAVING COUNT(c.id) >= 2 ORDER BY a.id"
		got := rfc173PinRows(t, db, ctx, q)
		if want := []string{"1|2"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := rfc173PinExplain(t, db, ctx, q)
		// The aggregate sits over the ordinal join: StreamingAgg on the
		// group key, directly over the SINGLE FlatMap (the gated 2-way);
		// HAVING is the PredicatesFilter above the agg.
		for _, frag := range []string{"StreamingAgg(keys=[A.ID]", "FlatMap(outer=", "PredicatesFilter("} {
			if !strings.Contains(plan, frag) {
				t.Errorf("plan lost %q:\n%s", frag, plan)
			}
		}
		if strings.Contains(plan, "FlatMap(outer=FlatMap(") {
			t.Errorf("gated 2-way must plan as ONE FlatMap, found a nested chain:\n%s", plan)
		}
		// ORDER BY a.id rides the group-key sort — exactly one sort in the
		// plan (§5's no-spurious-sort axis: a second InMemorySort above the
		// agg means the ordering property stopped propagating over the
		// ordinal join output).
		if n := strings.Count(plan, "InMemorySort("); n != 1 {
			t.Errorf("want exactly 1 InMemorySort (group-key sort, reused by ORDER BY), got %d:\n%s", n, plan)
		}
	})
	t.Run("having_on_group_key", func(t *testing.T) {
		q := "SELECT a.id, COUNT(c.id) FROM a JOIN c ON c.a_id = a.id GROUP BY a.id HAVING a.id >= 2 ORDER BY a.id"
		got := rfc173PinRows(t, db, ctx, q)
		if want := []string{"2|1", "3|1"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := rfc173PinExplain(t, db, ctx, q)
		for _, frag := range []string{"StreamingAgg(keys=[A.ID]", "FlatMap(outer="} {
			if !strings.Contains(plan, frag) {
				t.Errorf("plan lost %q:\n%s", frag, plan)
			}
		}
	})
	t.Run("order_by_aggregate_output", func(t *testing.T) {
		// Sorting on the AGGREGATE output over the gated join (the sort key
		// is the agg value, not a leg column).
		q := "SELECT a.id, COUNT(c.id) FROM a JOIN c ON c.a_id = a.id GROUP BY a.id ORDER BY COUNT(c.id) DESC, a.id"
		got := rfc173PinRows(t, db, ctx, q)
		if want := []string{"1|2", "2|1", "3|1"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := rfc173PinExplain(t, db, ctx, q)
		for _, frag := range []string{"InMemorySort([COUNT(C.ID) DESC", "StreamingAgg(keys=[A.ID]"} {
			if !strings.Contains(plan, frag) {
				t.Errorf("plan lost %q:\n%s", frag, plan)
			}
		}
	})
	t.Run("order_by_limit_over_gated_join", func(t *testing.T) {
		// The sort-key-over-ordinal-join axis that broke during the W3b
		// fallout (datumFromSpans qualified keys): QUALIFIED sort keys from
		// BOTH legs (a.id, c.id) over the gated join's merged row, plus
		// LIMIT. Row ORDER is asserted (not sorted away).
		q := "SELECT a.id, c.id, c.cw FROM a JOIN c ON c.a_id = a.id ORDER BY a.id DESC, c.id DESC LIMIT 3"
		got := rfc173PinRows(t, db, ctx, q)
		if want := []string{"3|103|10", "2|102|9", "1|101|8"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v (in order)", got, want)
		}
		plan := rfc173PinExplain(t, db, ctx, q)
		for _, frag := range []string{"Limit(3", "InMemorySort([A.ID DESC, C.ID DESC]", "FlatMap(outer="} {
			if !strings.Contains(plan, frag) {
				t.Errorf("plan lost %q:\n%s", frag, plan)
			}
		}
	})
}

// TestFDB_RFC173_DupNameStarOverGatedJoin is §5's duplicate-name SELECT * pin
// over a GATED 2-way join, beyond TestFDB_AmbiguousColumnStar's shape: a
// different table pair where BOTH bare names collide (ID and V in both legs),
// with ORDER BY for deterministic rows. All four columns must come back
// per-leg-correct in positional scan order — pdup's values are 1x, qdup's 9x,
// so a leg swap or a last-wins Datum collision (one leg's V shadowing the
// other's) is unmissable. This is the ordinal-model observable the W3b flip
// delivers through the driver-side positional read (§7 partial delivery).
func TestFDB_RFC173_DupNameStarOverGatedJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_dupstar")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_dupstar")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_dupstar_tmpl "+
			"CREATE TABLE pdup (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE qdup (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_dupstar/s WITH TEMPLATE rfc173_dupstar_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_dupstar?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO pdup (id, v) VALUES (1, 11), (2, 12)")
	mwjoMustExec(t, db, ctx, "INSERT INTO qdup (id, v) VALUES (1, 91), (2, 92)")

	check := func(t *testing.T, q string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		// FROM-declaration order: pdup.id, pdup.v, qdup.id, qdup.v — the
		// duplicated bare names must COEXIST in the metadata (no dedup, no
		// _<ordinal> mangling).
		if !eqStrSlices(cols, []string{"ID", "V", "ID", "V"}) {
			t.Fatalf("columns = %v, want [ID V ID V]", cols)
		}
		var got []string
		for rows.Next() {
			var pid, pv, qid, qv int64
			if err := rows.Scan(&pid, &pv, &qid, &qv); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, fmt.Sprintf("%d|%d|%d|%d", pid, pv, qid, qv))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// Per-leg-correct positional values: pdup.v ∈ {11,12}, qdup.v ∈
		// {91,92}. A cross-leg swap would yield 9x in slot 2; a last-wins
		// collision would yield the SAME v in slots 2 and 4.
		if want := []string{"1|11|1|91", "2|12|2|92"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v (in order)", got, want)
		}
	}

	t.Run("comma_form", func(t *testing.T) {
		check(t, "SELECT * FROM pdup, qdup WHERE pdup.id = qdup.id ORDER BY pdup.id")
	})
	t.Run("join_form", func(t *testing.T) {
		check(t, "SELECT * FROM pdup JOIN qdup ON pdup.id = qdup.id ORDER BY pdup.id")
	})
}

// TestFDB_RFC173_CoveringIndexLegOverGatedJoin pins the PR-447 review
// catch's dimension E2E: a GATED join whose probe leg is served by a
// COVERING index scan. Covering rows are INDEX-shaped (value-columns-then-PK
// — [A_ID, ID]) while the seed types the leg in table order ([ID, A_ID]):
// same width, different layout. The leg adapter must reject the misaligned
// passthrough (ordered per-slot name agreement) and synthesize from the
// Datum, or baked leg ordinals silently read c.id where c.a_id was asked —
// unmissable here because the two columns live in DISJOINT value ranges.
// The EXPLAIN assertion proves the plan actually exercises the covering
// dimension (a fetch-backed probe would vacuously pass the row checks).
func TestFDB_RFC173_CoveringIndexLegOverGatedJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_covleg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_covleg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_covleg_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_covleg/s WITH TEMPLATE rfc173_covleg_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_covleg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// DISJOINT ranges: join keys (a.x / c.a_id) in 1..2, c.id in 100.. — a
	// misread of c.a_id as c.id is unmissable. The join is on a.x (NOT a's
	// PK, no index) so the planner cannot probe A — it must drive A as the
	// outer and probe C through c_a_id, which COVERS the query (c.a_id is
	// the only c-column touched).
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 1), (2, 1), (3, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (100, 1), (101, 1), (102, 2)")

	const q = "SELECT a.id, c.a_id FROM a JOIN c ON c.a_id = a.x ORDER BY a.id, c.a_id"

	plan := rfc173PinExplain(t, db, ctx, q)
	// CANARY (gate-pin-(b) precedent): the covering-leg-into-gated-join shape
	// is UNREACHABLE today — fetch elimination lives in
	// MergeProjectionAndFetchRule, which needs a projection DIRECTLY over the
	// fetch, and join legs never have one (identical pre-flip; verified: the
	// probe stays Fetch(IndexScan)). The adapter's alignment guard + unit pin
	// (TestRFC173S2_AdaptLegPositional_IndexShapedFallsBack) carry the
	// protection. If this canary goes RED — a COVERING probe appeared in a
	// gated join's plan — the row assertions below become the live pin for
	// the index-shaped-row dimension.
	if !strings.Contains(plan, "Fetch(IndexScan(C_A_ID") {
		t.Fatalf("expected the fetch-backed index probe (today's reachable shape) — plan: %s", plan)
	}
	if strings.Contains(plan, "COVERING") {
		t.Fatalf("a COVERING probe reached a gated join — the canary fired: promote the row assertions to the live pin (plan: %s)", plan)
	}

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var aid, cAID int64
		if err := rows.Scan(&aid, &cAID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if cAID >= 100 {
			t.Fatalf("c.a_id = %d — the c.id range: the covering row's INDEX-shaped layout leaked into a baked leg ordinal (the adapter passthrough misread)", cAID)
		}
		got = append(got, fmt.Sprintf("%d|%d", aid, cAID))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// a1(x=1)×{c100,c101}, a2(x=1)×{c100,c101}, a3(x=2)×{c102}.
	want := []string{"1|1", "1|1", "2|1", "2|1", "3|2"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFDB_RFC173_PureCrossProduct pins the UNAVOIDABLE cross product through
// the S3 fulcrum's N-way flat seed: a FROM list with NO join predicates
// (every quantifier its own independent component) must still plan — the
// partition rule's disconnected-lower pruning yields to component-aligned
// splits (Java's shouldDeferCrossProducts, on by default), which are the only
// partitions a fully disconnected select has. Regression: the pruning ate
// every bipartition and the 3-ary select had no implementation (0AF00) — the
// same gap took down `WHERE EXISTS (SELECT 1 FROM t2, t3, t4 ...)`, whose
// existential body is exactly this shape plus an outer correlation.
func TestFDB_RFC173_PureCrossProduct(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_cross")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_cross")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_cross_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_cross/s WITH TEMPLATE rfc173_cross_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_cross?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a(9) is the NEGATIVE row for the exists-cross pin below: its probe
	// needs b.id=18, which is absent — an always-true EXISTS would leak it.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2), (9)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id) VALUES (10), (11), (12)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id) VALUES (100)")

	// 3 × 3 × 1 = 9 cross rows; every combination present exactly once.
	got := rfc173PinRows(t, db, ctx, "SELECT a.id, b.id, c.id FROM a, b, c")
	sort.Strings(got)
	want := []string{
		"1|10|100", "1|11|100", "1|12|100",
		"2|10|100", "2|11|100", "2|12|100",
		"9|10|100", "9|11|100", "9|12|100",
	}
	if !eqStrSlices(got, want) {
		t.Errorf("cross rows = %v, want %v", got, want)
	}

	// The EXISTS flavor: a 3-way cross body correlated only through one leg.
	// a(1)→b.id 10 ✓, a(2)→b.id 11 ✓, a(9)→b.id 18 ✗ — the negative row
	// proves the existential actually filters.
	got = rfc173PinRows(t, db, ctx,
		"SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b, c, a a2 WHERE b.id = 10 + a.id - 1)")
	sort.Strings(got)
	if !eqStrSlices(got, []string{"1", "2"}) {
		t.Errorf("exists-cross rows = %v, want [1 2] (a=9 has no matching b.id=18)", got)
	}
}

// TestFDB_RFC173_FullOverGatedBuriedRef is the fulcrum ruling's demanded pin
// (echoed by PR review): `(a JOIN b) FULL JOIN c` where the FULL box's ON and
// the SELECT list both reference the BURIED, non-rightmost leg `a`. The FULL
// select binds ONE quantifier for the whole gated leg, named after its
// RIGHTMOST leaf (`b`, sourceAlias) — no quantifier anywhere is named `a`, so
// `a.id` resolves ONLY through the coexistence machinery: the spliced spans /
// leaf-alias-qualified Datum the executor recovers from the leg's own seed
// RV. All three FULL row classes exercised: matched, left-only, right-only
// (NULL-extended a side).
func TestFDB_RFC173_FullOverGatedBuriedRef(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_fullburied")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_fullburied")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_fullburied_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_fullburied/s WITH TEMPLATE rfc173_fullburied_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_fullburied?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, av) VALUES (1, 100), (2, 200)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (11, 2)")
	// c(100)→a1 matched; c(101)→a_id 99 unmatched: the right-only FULL row
	// NULL-extends the whole (a JOIN b) leg, so the buried a.id reads NULL.
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (100, 1), (101, 99)")

	got := rfc173PinRows(t, db, ctx,
		"SELECT a.id, b.id, c.id FROM a JOIN b ON b.a_id = a.id FULL OUTER JOIN c ON c.a_id = a.id")
	sort.Strings(got)
	want := []string{
		"1|10|100",        // matched through the buried a.id
		"2|11|<nil>",      // left-only: (a2,b11) with no c
		"<nil>|<nil>|101", // right-only: c101, buried leg NULL-extended
	}
	sort.Strings(want)
	if !eqStrSlices(got, want) {
		t.Errorf("FULL-over-gated buried-ref rows = %v, want %v", got, want)
	}
}

// TestFDB_RFC173_SecondaryIndexThroughMerge is the W2 exit criterion's e2e
// half (S3-W3): SECONDARY-index selection must survive the fused/N-way
// substrate — the 3-way chain's winning plan probes c through its index
// inside the merge machinery, and the ORDER BY variant introduces no NEW
// spurious sort (the pre-fulcrum name model also sorted here; the §5
// no-spurious-sort pin guards the single-table frontier). The matching half
// (fused-vs-chained max-match through the ported ExpandFusedFieldValueRule)
// is pinned at the unit level in rfc173_w3_max_match_fused_test.go.
func TestFDB_RFC173_SecondaryIndexThroughMerge(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_rfc173_idxmerge")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_rfc173_idxmerge")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE rfc173_idxmerge_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, y BIGINT, z BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, b_z BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_b_z ON c (b_z)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_rfc173_idxmerge/s WITH TEMPLATE rfc173_idxmerge_tmpl")
	dsn := fmt.Sprintf("fdbsql:///testdb_rfc173_idxmerge?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, x) VALUES (1, 10), (2, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, y, z) VALUES (10, 10, 7), (20, 20, 8)")
	// c(102) dangles (b_z=9): a full-scan-shaped mistake would leak it into
	// a cross product; the index probe never sees it.
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, b_z) VALUES (100, 7), (101, 8), (102, 9)")

	const q = "SELECT a.id, c.id FROM a, b, c WHERE a.x = b.y AND b.z = c.b_z"
	plan := rfc173PinExplain(t, db, ctx, q)
	if !strings.Contains(plan, "Fetch(IndexScan(C_B_Z") {
		t.Fatalf("the c-leg must probe through its secondary index inside the merge machinery — plan: %s", plan)
	}
	got := rfc173PinRows(t, db, ctx, q)
	sort.Strings(got)
	if want := []string{"1|100", "2|101"}; !eqStrSlices(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}

	// ORDER BY variant: same probe, sort present but exactly ONE (no
	// fused-substrate-induced double sort), rows ordered.
	const qo = q + " ORDER BY a.id"
	planO := rfc173PinExplain(t, db, ctx, qo)
	if !strings.Contains(planO, "Fetch(IndexScan(C_B_Z") {
		t.Fatalf("ORDER BY variant lost the index probe — plan: %s", planO)
	}
	if strings.Count(planO, "InMemorySort") > 1 {
		t.Fatalf("more than one sort in the ordered plan (spurious sort) — plan: %s", planO)
	}
	if gotO := rfc173PinRows(t, db, ctx, qo); !eqStrSlices(gotO, []string{"1|100", "2|101"}) {
		t.Errorf("ordered rows = %v, want [1|100 2|101]", gotO)
	}
}
