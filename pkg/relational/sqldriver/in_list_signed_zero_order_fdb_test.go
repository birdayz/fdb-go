package sqldriver_test

// `WHERE <float column> IN (…, 0, …)` inside a derived table, with an outer
// residual predicate and `ORDER BY <primary key>`.
//
// An IN list over an indexed column plans as a per-binding leg executed once per
// list element. When the planner believes each leg delivers primary-key order it
// merges the legs (RecordQueryInUnionPlan) instead of sorting, reading one row of
// lookahead per leg.
//
// A ZERO element breaks that belief, and only on a FLOAT/DOUBLE column. -0.0 and
// +0.0 are IEEE-equal, so one binding admits both, but they pack to two distinct
// adjacent tuple keys — so the executor widens that probe across both blocks and
// the leg emits every -0.0 row before its first +0.0 row. The leg is therefore
// NOT in primary-key order, and a merge that trusts it appends the +0.0 rows
// after the entire result.
//
// The operand's declared type cannot detect this. An IN-list binding reaches the
// ordering derivation as an UNKNOWN-typed correlation, which the operand-only
// predicate reads as "not a float" — correct and load-bearing on an INT column,
// unsound here. The COLUMN's type is the discriminator, which is why TN below
// runs the identical shape over BIGINT.
//
// THE SHAPE IS THE TEST. The plain `SELECT … FROM t WHERE e IN (…) ORDER BY id`
// does NOT reach the defect: it already plans a sort over an InJoin, and a test
// written that way passes with the bug fully present (measured — it was, and it
// did). The merge is only chosen when the IN scan sits inside a derived table
// whose OUTER predicate keeps it from collapsing, which is exactly the shape the
// generative harness found (rowdiff seed 3889302). The derived table and the
// `g >= 0` residual are load-bearing, not incidental.
//
// The unindexed baseline is the oracle: with no index on the IN column there is
// no leg to merge and the plan must sort, so it computes the order SQL requires.
// Asserting against it makes this a statement about ROWS, not about plan shape —
// the defect returned the right rows in the wrong order, which no set comparison
// can see.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_InListSignedZeroKeepsPrimaryKeyOrder(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const dbPath = "/testdb_inlist_signed_zero"
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE inlistzero "+
			// idx_e is what makes the per-binding leg — and so the merge —
			// available at all.
			"CREATE TABLE ti (id BIGINT, e DOUBLE, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_e ON ti (e) "+
			// Same rows, same query, NO index: forced to sort, so it is the oracle.
			"CREATE TABLE tb (id BIGINT, e DOUBLE, g BIGINT, PRIMARY KEY (id)) "+
			// An INT column with the identical shape. It has no signed zero, so its
			// leg genuinely delivers primary-key order and must KEEP the merge —
			// the direction a blanket "never trust a leg" fix would break.
			"CREATE TABLE tn (id BIGINT, e BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_n ON tn (e)")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE inlistzero")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The interleaving is what makes the defect visible. Rows 2 and 6 hold +0.0
	// and rows 4 and 8 hold -0.0, so the zero binding's two blocks are BOTH
	// non-empty and neither is a suffix of the id order. Rows 1/3/5/7 carry the
	// nonzero list elements so their legs interleave with the zero leg. With
	// every zero row at the end of the id range the wrong answer and the right
	// one would coincide.
	const rows = "(1, 5.0, 1), (2, 0.0, 1), (3, 7.0, 1), (4, -0.0, 1), " +
		"(5, 5.0, 1), (6, 0.0, 1), (7, 7.0, 1), (8, -0.0, 1)"
	mwjoMustExec(t, db, ctx, "INSERT INTO ti (id, e, g) VALUES "+rows)
	mwjoMustExec(t, db, ctx, "INSERT INTO tb (id, e, g) VALUES "+rows)
	mwjoMustExec(t, db, ctx,
		"INSERT INTO tn (id, e, g) VALUES (1, 5, 1), (2, 0, 1), (3, 7, 1), (4, 0, 1), "+
			"(5, 5, 1), (6, 0, 1), (7, 7, 1), (8, 0, 1)")

	ids := func(q string) []int64 {
		t.Helper()
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer r.Close()
		var out []int64
		for r.Next() {
			var id int64
			if err := r.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		if err := r.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return out
	}
	eq := func(a, b []int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	// The derived table plus the outer residual is the shape that selects the
	// merge; see the file header for why a plain IN query cannot reach it.
	derived := func(tbl string) string {
		return "WITH d AS (SELECT * FROM " + tbl + " WHERE e IN (5, 7, 0)) " +
			"SELECT id FROM d WHERE g >= 0 ORDER BY id"
	}

	want := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	oracle := ids(derived("tb"))
	// Guard the oracle itself: a baseline that selected nothing, or that lost the
	// zero rows, would make the comparison below vacuously true.
	if !eq(oracle, want) {
		t.Fatalf("unindexed baseline returned %v, want %v.\n\n"+
			"The oracle is wrong, so nothing below is a statement about the "+
			"indexed plan. Either the IN list stopped admitting both signed "+
			"zeros (-0.0 and +0.0 are IEEE-equal, so both must match 0), or the "+
			"baseline grew an index and stopped being forced to sort.",
			oracle, want)
	}

	if got := ids(derived("ti")); !eq(got, oracle) {
		t.Errorf("indexed IN-list over a DOUBLE column returned %v, want %v.\n\n"+
			"ORDER BY id is a TOTAL order, so this is a row-order defect, not a "+
			"permissible tie. The zero binding widens across the -0.0 and +0.0 "+
			"key blocks, so its leg is not in primary-key order; a merge that "+
			"trusts the leg emits the +0.0 rows after the whole result.",
			got, oracle)
	}

	// The INT column must NOT be made conservative by the fix. Its leg has no
	// signed zero to widen across, so it genuinely delivers primary-key order and
	// the merge stays sound.
	if gotInt := ids(derived("tn")); !eq(gotInt, want) {
		t.Errorf("indexed IN-list over a BIGINT column returned %v, want %v.\n\n"+
			"An int coordinate has no signed zero, so nothing about it should "+
			"have changed; if this failed, the signed-zero guard is being applied "+
			"by OPERAND type instead of by COLUMN type and is now costing every "+
			"untyped IN binding its ordering claim.",
			gotInt, want)
	}

	// The row assertions above CANNOT see over-conservatism: a guard that
	// stopped trusting every leg, int columns included, still returns the right
	// rows — it just sorts them again. Only the plan shows it, so the two
	// directions are asserted on the two different things they are visible in.
	//
	// This is the same reason the DOUBLE case gets a plan assertion too: its row
	// assertion proves the ANSWER is right, not that it is right for the stated
	// reason, and a fix that reached the right rows by abandoning the index
	// everywhere would satisfy it.
	explain := func(q string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		return plan
	}
	if p := explain(derived("tn")); !strings.Contains(p, "InUnion") {
		t.Errorf("BIGINT IN-list plan lost its ordered merge: %s\n\n"+
			"An int coordinate has no signed zero, so its per-binding leg still "+
			"delivers primary-key order and the merge is still sound. Losing it "+
			"means the guard is keyed on something other than the COLUMN's type "+
			"— which costs every untyped IN binding over an int column an "+
			"O(N log N) sort it does not need.", p)
	}
	if p := explain(derived("ti")); strings.Contains(p, "InUnion") {
		t.Errorf("DOUBLE IN-list plan still merges its legs: %s\n\n"+
			"The zero binding widens across both signed-zero blocks, so its leg "+
			"is not in primary-key order and no merge over it can be trusted. "+
			"The rows may still come back ordered for this data by luck of the "+
			"leg interleaving; the plan is where the unsoundness is visible.", p)
	}
}
