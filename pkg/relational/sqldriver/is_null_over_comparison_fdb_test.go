package sqldriver_test

// Regression for BOOLEAN-VALUED EXPRESSIONS AS OPERANDS — every position that
// takes one, pinned together.
//
// The entry point was `(<comparison>) IS NULL`: the only way SQL can ask
// whether a predicate evaluated to UNKNOWN, and therefore the third branch of
// every ternary-logic partition. Go rejected it with 0AF00 while accepting
// every neighbouring shape (`(a > 3 AND b > 1) IS NULL`, `(NOT (a > 3)) IS
// NULL`, `(a IS NULL) IS NULL`). Java accepts all of them: a comparison there
// is a Value (RelOpValue), the one-field record flatten removes the
// parentheses, and one shared operand visit feeds the IS / IN / BETWEEN /
// binary-comparison arms alike.
//
// The first fix gave the IS operand its own walk, which closed IS NULL and
// left five siblings rejecting — `(a > 3) = (b > 1)`, `= TRUE`, `IS DISTINCT
// FROM`, `IN`, and a projected `(a > 3)`. That was the defect repeated, not
// cured: the real cause was the paren-unwrap discarding the caller's POSITION,
// so `(x)` resolved differently from `x` everywhere. Propagating the position
// gave Go one operand walk, as Java has, and closed all six at once.
//
// Two further divergences fell out of measuring rather than assuming, and both
// are pinned below: a comparison is a legal CASE consequent (Java accepts it;
// Go rejected it on the strength of an unsourced comment), and BOOLEAN has no
// order (Java rejects `f > FALSE`; Go accepted it and returned rows no Java
// client could see).
//
// Planning is not the assertion. A predicate that plans and returns the wrong
// rows is worse than one that refuses, so this pins ROWS throughout, plus the
// partition property that makes the IS NULL form worth having.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_IsNullOverComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_isnullcmp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_isnullcmp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE isnullcmp "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT NOT NULL, s STRING, f BOOLEAN, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_isnullcmp/s WITH TEMPLATE isnullcmp")
	dsn := fmt.Sprintf("fdbsql:///testdb_isnullcmp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: 1, 5, NULL, 9, NULL   → `a > 3` is FALSE, TRUE, UNKNOWN, TRUE, UNKNOWN
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, s, f) VALUES "+
		"(1, 1, 10, 'x', TRUE), (2, 5, 20, 'y', FALSE), (3, NULL, 30, NULL, NULL), (4, 9, 40, 'z', TRUE), (5, NULL, 50, 'w', FALSE)")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}

	eq := func(name string, got, want []int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}

	// The three-valued split of `a > 3` over the seeded rows.
	eq("p", ids("SELECT id FROM t WHERE (a > 3)"), []int64{2, 4})
	eq("not-p", ids("SELECT id FROM t WHERE NOT (a > 3)"), []int64{1})
	eq("p-is-null", ids("SELECT id FROM t WHERE (a > 3) IS NULL"), []int64{3, 5})
	eq("p-is-not-null", ids("SELECT id FROM t WHERE (a > 3) IS NOT NULL"), []int64{1, 2, 4})

	// The partition property the predicate exists to make expressible: the
	// three branches reconstruct the unfiltered table exactly, with no row in
	// two branches and none missing. Asserting the branches individually would
	// pass a NULL-semantics bug that shifted a row from one branch to another
	// in a way the hand-written expectations happened to encode.
	var union []int64
	union = append(union, ids("SELECT id FROM t WHERE (a > 3)")...)
	union = append(union, ids("SELECT id FROM t WHERE NOT (a > 3)")...)
	union = append(union, ids("SELECT id FROM t WHERE (a > 3) IS NULL")...)
	sort.Slice(union, func(i, j int) bool { return union[i] < union[j] })
	eq("tlp partition", union, ids("SELECT id FROM t"))

	// Operand shapes that already worked must keep working — the fix routes
	// the IS operand through a resolver of its own, and a regression there
	// would silently change what these mean rather than failing to plan.
	eq("and-tree", ids("SELECT id FROM t WHERE (a > 3 AND b > 1) IS NULL"), []int64{3, 5})
	eq("not-tree", ids("SELECT id FROM t WHERE (NOT (a > 3)) IS NULL"), []int64{3, 5})
	eq("nested-isnull", ids("SELECT id FROM t WHERE (a IS NULL) IS NULL"), nil)
	eq("bare-column", ids("SELECT id FROM t WHERE a IS NULL"), []int64{3, 5})

	// A NOT NULL column can never make the comparison UNKNOWN.
	eq("not-null-col", ids("SELECT id FROM t WHERE (b > 3) IS NULL"), nil)
	// A string comparison takes the same path.
	eq("string-cmp", ids("SELECT id FROM t WHERE (s = 'x') IS NULL"), []int64{3})

	// Every OTHER position that takes a boolean-valued operand. Java resolves
	// all of them with one shared visit of the operand atom, so a comparison is
	// legal in each; Go rejected five of the six while accepting IS NULL,
	// because each position decided for itself what its operand meant. They are
	// pinned together because that is the shape of the defect: fixing one and
	// leaving the siblings is what produced the split in the first place.
	eq("eq-comparison", ids("SELECT id FROM t WHERE (a > 3) = (b > 1)"), []int64{2, 4})
	eq("eq-literal", ids("SELECT id FROM t WHERE (a > 3) = TRUE"), []int64{2, 4})
	eq("is-distinct-from", ids("SELECT id FROM t WHERE (a > 3) IS DISTINCT FROM (b > 1)"), []int64{1, 3, 5})
	eq("in-list", ids("SELECT id FROM t WHERE (a > 3) IN (TRUE, FALSE)"), []int64{1, 2, 4})
	eq("unparenthesised", ids("SELECT id FROM t WHERE a > 3 IS NULL"), []int64{3, 5})

	// The four remaining IS forms. resolveIsBoolean's desugar had no test at
	// all, so these were fixed-but-unpinned: the code worked and nothing said
	// so, which is the state a refactor silently breaks.
	eq("is-true", ids("SELECT id FROM t WHERE (a > 3) IS TRUE"), []int64{2, 4})
	eq("is-false", ids("SELECT id FROM t WHERE (a > 3) IS FALSE"), []int64{1})
	eq("is-not-true", ids("SELECT id FROM t WHERE (a > 3) IS NOT TRUE"), []int64{1, 3, 5})
	eq("is-not-false", ids("SELECT id FROM t WHERE (a > 3) IS NOT FALSE"), []int64{2, 3, 4, 5})

	// A comparison is a legal CASE consequent — measured on the live Java
	// server, which ACCEPTS it. Go rejected it with 0AF00 on the strength of a
	// comment claiming the opposite; conformance's boolean-operand probe is the
	// measurement that replaced the claim.
	var caseCount int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM t WHERE CASE WHEN id = 1 THEN a > 3 ELSE b > 30 END").Scan(&caseCount); err != nil {
		t.Errorf("a comparison as a CASE consequent was rejected (%v); Java accepts it", err)
	} else if caseCount != 2 {
		// id=1 takes the THEN arm (1 > 3 false); ids 2..5 take the ELSE arm
		// (b > 30 → 20,30,40,50 → ids 4 and 5).
		t.Errorf("CASE-consequent comparison matched %d rows, want 2", caseCount)
	}

	// BOOLEAN HAS NO ORDER. Both engines reject ordering it; Go used to accept,
	// silently returning rows no Java client could ever see. BETWEEN is the
	// same defect wearing a desugaring, so it is pinned beside the bare form.
	for _, q := range []struct{ name, sql string }{
		{"bool-col-gt", "SELECT id FROM t WHERE f > FALSE"},
		{"bool-col-ge", "SELECT id FROM t WHERE f >= FALSE"},
		{"bool-col-between", "SELECT id FROM t WHERE f BETWEEN FALSE AND TRUE"},
		{"bool-cmp-ge", "SELECT id FROM t WHERE (a > 3) >= FALSE"},
		{"bool-cmp-between", "SELECT id FROM t WHERE (a > 3) BETWEEN FALSE AND TRUE"},
	} {
		rows, err := db.QueryContext(ctx, q.sql)
		if err == nil {
			rows.Close()
			t.Errorf("%s: ordering a BOOLEAN was accepted; Java rejects it, so this returns rows no Java "+
				"client would see: %s", q.name, q.sql)
			continue
		}
		if !strings.Contains(err.Error(), "42804") {
			t.Errorf("%s: rejected with %v, want 42804 (datatype mismatch) to match Java's type error", q.name, err)
		}
	}
	// Equality over BOOLEAN stays legal — the gate must reject ORDERING, not
	// booleans, or it would break every `flag = TRUE` in the corpus.
	eq("bool-col-eq", ids("SELECT id FROM t WHERE f = TRUE"), []int64{1, 4})

	// NEGATIVE RESULT, pinned deliberately. An EXISTS as the operand of IS
	// [NOT] NULL is still rejected, and it was rejected before the operand walk
	// was unified — measured both ways. That matters because routing operand
	// positions through a comparison-allowing walk could plausibly have moved
	// EXISTS onto the projection folding path, where an ExistsValue sits above
	// the FlatMap with the existential binding dead and evaluates to a constant.
	// It did not: the position carries comparison folding WITHOUT the EXISTS
	// projection rules, so EXISTS stays exactly where it was.
	//
	// If these start succeeding, that guarantee has been relaxed and the rows
	// need checking against a live Java run before anyone celebrates.
	for _, q := range []struct{ name, sql string }{
		{"exists-is-null", "SELECT id FROM t WHERE (EXISTS (SELECT id FROM t x WHERE x.id = t.id)) IS NULL"},
		{"exists-is-not-null", "SELECT id FROM t WHERE (EXISTS (SELECT id FROM t x WHERE x.id = t.id)) IS NOT NULL"},
		{"not-exists-is-null", "SELECT id FROM t WHERE (NOT EXISTS (SELECT id FROM t x WHERE x.id = t.id)) IS NULL"},
	} {
		rows, err := db.QueryContext(ctx, q.sql)
		if err == nil {
			rows.Close()
			t.Errorf("%s: an EXISTS operand of IS NULL was accepted. That is not obviously wrong — EXISTS is "+
				"never NULL, so the answer would be 'no rows' — but it means EXISTS changed position, and the "+
				"binding-dead failure mode is silent. Verify against a live Java run before pinning rows: %s",
				q.name, q.sql)
		}
	}
}
