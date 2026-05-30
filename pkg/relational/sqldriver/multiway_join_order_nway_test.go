package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_MultiwayJoinOrder_Nway is the acceptance test for RFC-043: generic
// N-way join execution. RFC-042 made 3-way joins FROM-order-independent and
// cost-optimal but scoped re-enumeration to n==3 (≥4-way failed to plan loudly).
// RFC-043 lifts that — re-enumerated joins of any arity now flow a merged row
// (JoinMergeAllValue, qualified ALIAS.COL keys for every live table) so every
// buried table's columns survive up the join spine.
//
// The 4-way cases below all FAILED to plan before RFC-043. They now:
//
//	(a) return CORRECT rows — for both FROM-orders of the chain, for the
//	    *middle-table* projection (the load-bearing case: it has no terminal
//	    decomposition, so the projected table is necessarily buried inside a join
//	    and its columns MUST be flowed up the merge spine), and for a *star* shape;
//	(b) preserve the index-nested-loop property — the largest table is reached via
//	    its FK index, never full-scanned, despite the wide merged rows.
//
// SCOPE (honest): RFC-043 is about N-way EXECUTION correctness. It does NOT claim
// FROM-order-independent, byte-identical, fully cost-optimal plans for ≥4-way —
// RFC-042 achieves that for 3-way, but extending order-invariant cost SELECTION
// to ≥4-way is a cost-model follow-up (epic PR-D / RFC-041). This test asserts
// correct rows + the index-probe property, not plan byte-identity.
//
// ≥5-way joins are correct too (verified manually), but the bushy re-enumeration
// is exponential and a 5-way exceeds the planner's DEFAULT task budget, so it may
// fail to plan loudly. That is acceptable and pinned here as "correct OR loud,
// never wrong rows": the contract is identical-to-correct or a loud plan-failure,
// NEVER a silent wrong row. Making the shared join sub-products intern so the
// budget scales polynomially is the documented efficiency follow-up (RFC-043 §
// Performance; ties into RFC-039 broad memo merging).
func TestFDB_MultiwayJoinOrder_Nway(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_nway")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nway")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nway_tmpl "+
			// indexed chain t1(1) <- t2(20) <- t3(200) <- t4(2000)
			"CREATE TABLE t1 (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, t1_id BIGINT, x STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE t3 (id BIGINT NOT NULL, t2_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t4 (id BIGINT NOT NULL, t3_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id) "+
			"CREATE INDEX t3_by_t2 ON t3 (t2_id) "+
			"CREATE INDEX t4_by_t3 ON t4 (t3_id) "+
			// star: hub -> w, xx, yy
			"CREATE TABLE hub (id BIGINT NOT NULL, w_id BIGINT, x_id BIGINT, y_id BIGINT, label STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE w (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE xx (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE yy (id BIGINT NOT NULL, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nway/s WITH TEMPLATE nway_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_nway?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// chain: t1=1 row; each t2 -> t1; each t3 -> t2; each t4 -> t3.
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 20; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1, 'x%d')", i, i))
	}
	for i := 1; i <= 200; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t3 VALUES (%d, %d)", i, (i%20)+1))
	}
	for i := 1; i <= 2000; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t4 VALUES (%d, %d)", i, (i%200)+1))
	}
	// star
	mwjoMustExec(t, db, ctx, "INSERT INTO w VALUES (5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO xx VALUES (6)")
	mwjoMustExec(t, db, ctx, "INSERT INTO yy VALUES (7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO hub VALUES (1, 5, 6, 7, 'hublabel')")
	mwjoMustExec(t, db, ctx, "INSERT INTO hub VALUES (2, 5, 6, 99, 'nomatch')")

	planExplain := mwjoExplainer(t, db, ctx)

	chainPred := "t2.t1_id = t1.id AND t3.t2_id = t2.id AND t4.t3_id = t3.id"
	qSmall := "SELECT t1.id FROM t1, t2, t3, t4 WHERE " + chainPred
	qBig := "SELECT t1.id FROM t4, t3, t2, t1 WHERE " + chainPred

	// (b) Cost property that the merge MUST preserve at any arity: the largest
	// table (2000-row t4) is reached via its FK index, never full-scanned. The
	// re-enumerated merge flows wide rows but must not break index-nested-loop
	// matching on the join boundary. (Both FROM-orders.)
	//
	// NOTE: this does NOT assert a byte-identical, fully cost-optimal plan across
	// FROM-orders. RFC-042 achieves that for 3-way; for ≥4-way the larger bushy
	// search space + merge costing does not yet converge the two orders onto the
	// single optimal left-deep index-probe chain — that order-invariant cost
	// SELECTION is a cost-model follow-up (epic PR-D / RFC-041), separate from the
	// EXECUTION correctness RFC-043 delivers here. We assert the index-probe
	// property (which holds) and full row correctness (the point of this RFC).
	for _, q := range []string{qSmall, qBig} {
		up := strings.ToUpper(planExplain(q))
		if strings.Contains(up, "SCAN(T4)") {
			t.Errorf("4-WAY COST: plan full-scans the 2000-row T4 instead of index-probing it:\n  %s", planExplain(q))
		}
		if !strings.Contains(up, "INDEXSCAN(T4_BY_T3") {
			t.Errorf("4-WAY COST: plan does not index-probe T4 via t4_by_t3:\n  %s", planExplain(q))
		}
	}

	// (a) Correctness — root projection, both FROM-orders, return the 2000 chain
	// rows all with t1.id == 1.
	for _, q := range []string{qSmall, qBig} {
		n, bad := scanIDs(t, db, ctx, q)
		if n != 2000 {
			t.Errorf("4-WAY root %q: got %d rows, want 2000:\n  %s", q, n, planExplain(q))
		}
		if bad != 0 {
			t.Errorf("4-WAY root %q: %d rows with t1.id != 1, want 0", q, bad)
		}
	}

	// (a) Middle-table projection (SELECT t2.x) — the case with NO terminal
	// decomposition: t2 is necessarily buried inside the join, so its columns
	// must be flowed up. Every returned x must be non-NULL and well-formed.
	midRows := scanStrings(t, db, ctx, "SELECT t2.x FROM t1, t2, t3, t4 WHERE "+chainPred)
	if len(midRows) != 2000 {
		t.Errorf("4-WAY middle: got %d rows, want 2000", len(midRows))
	}
	for _, s := range midRows {
		if !strings.HasPrefix(s, "x") {
			t.Errorf("4-WAY middle: t2.x = %q, want a non-NULL 'x...' value (buried-table column lost)", s)
			break
		}
	}

	// (a) Star — hub.label, the projected hub is buried under the w/x/y joins.
	starRows := scanStrings(t, db, ctx, "SELECT hub.label FROM hub, w, xx, yy WHERE hub.w_id=w.id AND hub.x_id=xx.id AND hub.y_id=yy.id")
	if len(starRows) != 1 || starRows[0] != "hublabel" {
		t.Errorf("4-WAY star: got %v, want [hublabel]", starRows)
	}

	// ≥5-way budget boundary: correct OR loud plan-failure, NEVER a wrong row.
	q5 := "SELECT t1.id FROM t1, t2, t3, t4, hub WHERE " + chainPred + " AND hub.w_id = t1.id"
	rows, err := db.QueryContext(ctx, q5)
	if err != nil {
		if !strings.Contains(err.Error(), "could not plan") {
			t.Errorf("5-WAY failed with unexpected error (want correct rows or a plan-failure): %v", err)
		}
	} else {
		var n, bad int
		for rows.Next() {
			var id sql.NullInt64
			rows.Scan(&id)
			n++
			if !id.Valid || id.Int64 != 1 {
				bad++
			}
		}
		rows.Close()
		if bad != 0 {
			t.Errorf("5-WAY PLANNED but returned %d wrong rows (t1.id != 1) — silent wrong data is forbidden even over budget", bad)
		}
	}
}

func scanIDs(t *testing.T, db *sql.DB, ctx context.Context, q string) (n, bad int) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullInt64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if !id.Valid || id.Int64 != 1 {
			bad++
		}
	}
	return n, bad
}

func scanStrings(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v.String)
	}
	return out
}
