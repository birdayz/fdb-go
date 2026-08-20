package sqldriver_test

// A parenthesized condition in a searched CASE.
//
// `CASE WHEN (a = 1 AND b = 1) THEN 1 ELSE 0 END` must mean what
// `CASE WHEN a = 1 AND b = 1 THEN 1 ELSE 0 END` means: parentheses group, they
// do not change a condition into something else. The same predicate in a WHERE
// clause is correct either way, so a reader has every reason to expect the two
// spellings to agree.
//
// They did not. This grammar parses `( expr )` as a one-field RECORD
// constructor — the same production that builds `(x, y)` — and the searched
// CASE walked its condition as a VALUE first, falling back to a predicate only
// if the value walk FAILED. For a parenthesized COMPOUND boolean the value walk
// succeeds, yielding `{_0: predicate}`, and the CASE then tests that record for
// equality with TRUE. A record is never TRUE, so every row took the ELSE branch:
//
//	CASE WHEN  a = 1 AND b = 1  THEN 1 ELSE 0 END -> WHEN(predicate, TRUE)
//	CASE WHEN (a = 1 AND b = 1) THEN 1 ELSE 0 END -> WHEN({_0: predicate}, TRUE)
//
// A parenthesized SIMPLE comparison was unaffected, which is what made this hard
// to notice: `(a = 1)` works, `(a = 1 AND b = 1)` does not, and the difference is
// invisible in the SQL.
//
// The failure is silent and the wrong answer is plausible — a COUNT that is too
// low, a SUM that is zero — so these cases assert the CASE's per-row output and
// the aggregate over it, and cross-check both against the same predicate in a
// WHERE clause, which is the spelling that was always right.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CaseWithParenthesizedCondition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_case_paren", "casepar",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) ")
	//  id=1: a=1 b=1  -> both conjuncts true
	//  id=2: a=1 b=2  -> first only
	//  id=3: a=2 b=1  -> second only
	//  id=4: a=NULL   -> neither, and UNKNOWN rather than false
	w.Exec("INSERT INTO t (id, a, b, f) VALUES (1, 1, 1, true), (2, 1, 2, false), (3, 2, 1, true), (4, NULL, 9, NULL)")

	// Each case states the condition and the per-row CASE output it must
	// produce. The unparenthesized spelling of the same condition is asserted
	// beside it, so a divergence between them is attributed to the parentheses
	// and nothing else.
	cases := []struct {
		cond string
		want []string // CASE ... THEN 1 ELSE 0 END, ordered by id
	}{
		{"a = 1 AND b = 1", []string{"1", "0", "0", "0"}},
		{"(a = 1 AND b = 1)", []string{"1", "0", "0", "0"}},
		{"((a = 1 AND b = 1))", []string{"1", "0", "0", "0"}},
		{"a = 1 OR b = 1", []string{"1", "1", "1", "0"}},
		{"(a = 1 OR b = 1)", []string{"1", "1", "1", "0"}},
		{"NOT (a = 1)", []string{"0", "0", "1", "0"}},
		{"(NOT (a = 1))", []string{"0", "0", "1", "0"}},
		{"(a = 1)", []string{"1", "1", "0", "0"}},
		{"a = 1", []string{"1", "1", "0", "0"}},
		{"(a = 1) AND (b = 1)", []string{"1", "0", "0", "0"}},
		{"(a = 1 AND b = 1) OR id = 3", []string{"1", "0", "1", "0"}},
		{"(a IS NULL)", []string{"0", "0", "0", "1"}},
		{"(a IS NULL OR b = 9)", []string{"0", "0", "0", "1"}},
		{"(a = 1 AND b = 1 AND id = 1)", []string{"1", "0", "0", "0"}},
		// A bare boolean column, which is a VALUE rather than a comparison —
		// the shape a predicate-first walk must still handle.
		{"f", []string{"1", "0", "1", "0"}},
		{"(f)", []string{"1", "0", "1", "0"}},
		{"(f AND a = 1)", []string{"1", "0", "0", "0"}},
	}

	for _, c := range cases {
		rowwise := fmt.Sprintf("SELECT CASE WHEN %s THEN 1 ELSE 0 END FROM t ORDER BY id", c.cond)
		w.Want("CASE WHEN "+c.cond, rowwise, c.want)

		// The aggregate over the same CASE, where a wrong condition is a wrong
		// number rather than a visibly wrong column.
		var sum int
		for _, v := range c.want {
			if v == "1" {
				sum++
			}
		}
		w.Want("SUM over CASE WHEN "+c.cond,
			fmt.Sprintf("SELECT SUM(CASE WHEN %s THEN 1 ELSE 0 END) FROM t", c.cond),
			[]string{fmt.Sprintf("%d", sum)})

		// And the same condition as a WHERE clause, which is the spelling that
		// was always correct: the CASE must agree with it row for row.
		var ids []string
		for i, v := range c.want {
			if v == "1" {
				ids = append(ids, fmt.Sprintf("%d", i+1))
			}
		}
		if ids == nil {
			ids = []string{}
		}
		w.Want("WHERE "+c.cond,
			fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", c.cond), ids)
	}
}

// TestFDB_CaseParenthesizedConditionPlanShape pins the compiled form, because
// the row assertions above pass for any condition that happens to evaluate
// correctly and this is the fact that says WHY: a searched CASE's condition must
// compile to a predicate, never to a record wrapping one.
func TestFDB_CaseParenthesizedConditionPlanShape(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_case_paren_plan", "caseparp",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) ")
	w.Exec("INSERT INTO t (id, a, b) VALUES (1, 1, 1)")

	for _, cond := range []string{
		"(a = 1 AND b = 1)",
		"(a = 1 OR b = 1)",
		"(NOT (a = 1))",
		"((a = 1 AND b = 1))",
	} {
		q := fmt.Sprintf("SELECT CASE WHEN %s THEN 1 ELSE 0 END FROM t", cond)
		plan := w.Explain(q)
		if strings.Contains(plan, "{_0:") {
			t.Errorf("the condition of a searched CASE compiled to a one-field RECORD wrapping the "+
				"predicate instead of the predicate itself, so it is tested for equality with TRUE "+
				"and can never match:\n  cond: %s\n  plan: %s", cond, plan)
		}
	}
}
