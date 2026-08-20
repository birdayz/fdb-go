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
//
// WHERE JAVA STANDS ON THIS, measured rather than assumed:
// conformance/case_parenthesized_condition_java_probe_test.go runs both engines
// on these shapes and finds that Java REJECTS every parenthesized condition with
// SQLSTATE 42804 — the simple `(a = 1)` as much as the compound one, because to
// Java both are records and visitCaseFunctionCall asserts the condition is
// BOOLEAN. Go accepts them all and, after this repair, answers them all
// correctly.
//
// That is the DivergenceJavaErrorsGoCorrect direction, and it is booked in
// TODO.md as an open owner decision: keep Go permissive-and-correct, or narrow
// it to Java's rejection for strict parity. It is not a widening of Go's
// accepted surface — Go accepted these before the repair too; what changed is
// that the answers stopped being wrong.

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

// TestFDB_CaseWithNonBooleanCondition pins the shapes around the
// predicate-first repair, which is how that repair's scope is stated as a
// measurement rather than as a claim. Of eleven condition shapes probed across
// it, exactly ONE moved: `(f)`, a parenthesized boolean column, which was
// broken and is now correct.
//
// THE OPEN QUESTION THIS FILE RECORDED HAS BEEN ANSWERED, and by the mechanism
// the pin existed for. The earlier version asserted that a non-boolean
// condition — an integer, a string, a bare non-boolean column — silently takes
// the ELSE branch, and said in as many words that whether that is the right
// treatment was a separate question, that standard SQL would make it a type
// error, and that pinning today's answer was what would make a future change
// deliberate rather than accidental.
//
// It did exactly that. Java was then measured
// (conformance/case_condition_typing_java_probe_test.go): it rejects all six
// shapes with "argument of case when must be of boolean type", and Go answered
// the ELSE branch for every row of all six. Go HAD the check —
// WalkPredicate's bare-value lift raises DATATYPE_MISMATCH for a
// definitively-typed non-boolean — and walkCaseCondition's fallback swallowed
// it, turning a rejected query into a silently wrong one. The fallback now
// distinguishes "no predicate reading of this shape exists" from "this IS a
// condition and its type is wrong", and only the first falls through.
//
// So these arms flipped from ELSE-for-every-row to 42804, and this test failing
// is what made that flip visible rather than silent.
//
// NULL is NOT among them and stays permissive: the lift folds a NULL to an
// unknown ConstantPredicate before the type switch, so `WHEN NULL` still takes
// the ELSE branch, which is what SQL says a non-TRUE condition does. Java
// rejects that one too — the same open owner decision as the parenthesized
// condition, still open.
func TestFDB_CaseWithNonBooleanCondition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_case_nonbool", "casenb",
		"CREATE TABLE t (id BIGINT, a BIGINT, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) ")
	w.Exec("INSERT INTO t (id, a, f) VALUES (1, 1, true), (2, 0, false), (3, NULL, NULL)")

	// A definitively-typed non-boolean condition is a type error, on BOTH
	// schemas and with the same sqlstate — an index may not change whether a
	// query is accepted any more than it may change its rows.
	for _, cond := range []string{"1", "0", "a", "'x'", "(1)", "(a)", "a + 1"} {
		w.WantRejected("non-boolean condition "+cond,
			fmt.Sprintf("SELECT id, CASE WHEN %s THEN 1 ELSE 0 END FROM t ORDER BY id", cond),
			"42804")
	}

	// NULL is the deliberate exception, and it is asserted as ROWS rather than
	// as a rejection so that narrowing it later cannot pass unnoticed.
	w.Want("NULL condition still resolves",
		"SELECT id, CASE WHEN NULL THEN 1 ELSE 0 END FROM t ORDER BY id",
		[]string{"1|0", "2|0", "3|0"})

	// Boolean conditions, parenthesized and not, including the bare column that
	// the repair also fixed.
	for _, c := range []struct {
		cond string
		want []string
	}{
		{"f", []string{"1|1", "2|0", "3|0"}},
		{"(f)", []string{"1|1", "2|0", "3|0"}},
		{"NOT f", []string{"1|0", "2|1", "3|0"}},
		{"(NOT f)", []string{"1|0", "2|1", "3|0"}},
		{"f AND a = 1", []string{"1|1", "2|0", "3|0"}},
		{"(f AND a = 1)", []string{"1|1", "2|0", "3|0"}},
		{"CASE WHEN f THEN true ELSE false END", []string{"1|1", "2|0", "3|0"}},
	} {
		w.Want("boolean condition "+c.cond,
			fmt.Sprintf("SELECT id, CASE WHEN %s THEN 1 ELSE 0 END FROM t ORDER BY id", c.cond),
			c.want)
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
