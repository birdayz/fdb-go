package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_TwoTableOrderInvariantIndexJoin proves order-invariant, cost-optimal
// index-nested-loop join selection: the same 2-table join (t1 1 row, t2 50 rows,
// joined on the indexed FK t2.t1_id) planned under both FROM-orders yields the
// BYTE-IDENTICAL physical plan — and that plan drives from the 1-row table and
// index-probes t2 via t2_by_t1 (the cost-optimal nested-loop order), regardless
// of FROM-clause position. This is the order-invariant cost-based join-ordering
// property (RFC-041/042) with the index-nested-loop join (RFC-042 L3) for the
// 2-table case. The N-way generalization is tracked separately (the partitioned
// sub-product's index-probe).
func TestFDB_TwoTableOrderInvariantIndexJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_2t")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_2t")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE t2t "+
			"CREATE TABLE t1 (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t2_by_t1 ON t2 (t1_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_2t/s WITH TEMPLATE t2t")
	dsn := fmt.Sprintf("fdbsql:///testdb_2t?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 VALUES (1)")
	for i := 1; i <= 50; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t2 VALUES (%d, 1)", i))
	}

	pe := mwjoExplainer(t, db, ctx)
	a := pe("SELECT t1.id FROM t1, t2 WHERE t2.t1_id = t1.id")
	b := pe("SELECT t1.id FROM t2, t1 WHERE t2.t1_id = t1.id")

	// THE ACCESS PATH is what has to be order-invariant, and it is the part
	// BELOW the top projection. The projection itself cannot be: it addresses
	// the merged row by ORDINAL, and the merged row is laid out in FROM order
	// (`SELECT *` column order follows the FROM clause), so t1.id is slot 0 of
	// [T1.ID, T2.ID, T2.T1_ID] and slot 2 of [T2.ID, T2.T1_ID, T1.ID]. Those
	// two projections are the SAME read of the same column through two
	// different row layouts; comparing them byte-for-byte would demand that
	// re-ordering FROM leave the row unchanged, which is a different (and
	// false) claim.
	aSlots, aPath := splitTopProjection(a)
	bSlots, bPath := splitTopProjection(b)
	if aPath != bPath {
		t.Errorf("access path depends on FROM-order (not cost-based reordering):\n t1,t2: %s\n t2,t1: %s", a, b)
	}
	// And the ordinals ARE asserted, because they are what says each
	// projection reads t1.id rather than some other column that happens to
	// share its name.
	if aSlots != "_current.ID#0" {
		t.Errorf("FROM t1, t2 projects %q, want _current.ID#0 — t1.id is slot 0 of "+
			"the merged row [T1.ID, T2.ID, T2.T1_ID]\n  %s", aSlots, a)
	}
	if bSlots != "_current.ID#2" {
		t.Errorf("FROM t2, t1 projects %q, want _current.ID#2 — t1.id is slot 2 of "+
			"the merged row [T2.ID, T2.T1_ID, T1.ID]\n  %s", bSlots, b)
	}
	for _, p := range []string{a, b} {
		up := strings.ToUpper(p)
		if !strings.Contains(up, "INDEXSCAN(T2_BY_T1") {
			t.Errorf("plan does not index-probe t2 via t2_by_t1: %s", p)
		}
		if !strings.Contains(up, "OUTER=SCAN(T1)") {
			t.Errorf("plan does not drive from the 1-row t1: %s", p)
		}
	}
}

// splitTopProjection splits `Project([<slots>], <access path>)` into the slot
// list and the plan below it, so an order-invariance claim can be made about
// the ACCESS PATH without also demanding that the merged row's layout — which
// legitimately follows FROM order — be identical.
//
// A plan with no top-level projection returns ("", plan): there is nothing to
// separate, and the whole text is the access path.
func splitTopProjection(plan string) (slots, accessPath string) {
	const prefix = "Project(["
	const separator = "], "
	if !strings.HasPrefix(plan, prefix) || !strings.HasSuffix(plan, ")") {
		return "", plan
	}
	rest := plan[len(prefix):]
	// The slot list can itself contain brackets (a record constructor, an IN
	// list), so scan for the separator at DEPTH ZERO rather than taking the
	// first one — otherwise a nested `]` ends the list early and the two halves
	// are both wrong.
	depth := 0
	for i := 0; i+len(separator) <= len(rest); i++ {
		switch rest[i] {
		case '[', '(', '{':
			depth++
			continue
		case ']', ')', '}':
			if depth > 0 {
				depth--
				continue
			}
		}
		if depth == 0 && rest[i:i+len(separator)] == separator {
			return rest[:i], strings.TrimSuffix(rest[i+len(separator):], ")")
		}
	}
	return "", plan
}
