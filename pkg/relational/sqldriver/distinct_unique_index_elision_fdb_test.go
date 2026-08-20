package sqldriver_test

// SELECT DISTINCT over a column carrying a secondary UNIQUE index.
//
// A UNIQUE index licenses the planner to ELIDE the distinct operator outright:
// if no two rows can share the value, deduplicating it is a no-op. That license
// rests on mutable store state rather than on a storage invariant, and — more
// delicately — uniqueness in this engine EXEMPTS the values that are not
// comparable to themselves. A NULL does not violate a unique index, so a unique
// column may hold many NULLs, and eliding the dedup entirely would then collapse
// nothing and return every one of them.
//
// That is what the "narrowed" dedup exists for: retain only the exempt values
// (NULL, NaN) in the seen-set and pass everything else through, on the argument
// that uniqueness already guarantees at most one row per non-exempt value. The
// argument is sound only if the exempt set is exactly right, which makes the
// interesting fixtures the ones with SEVERAL NULLs, several NaNs, and duplicate
// non-exempt values that the index should have made impossible.
//
// The oracle is the unindexed twin, where DISTINCT is always a full dedup.
//
// READ VERSIONS DECIDE WHICH PATH IS UNDER TEST, and it is not a detail. The
// proof is licensed only where the whole result comes from ONE read version, so
// in AUTO-COMMIT — each page taking a fresh read version, a value able to move
// between pages and be emitted twice — it is deliberately withheld and the plan
// keeps a full Distinct. Everything below therefore runs twice:
//
//	auto-commit          the full-dedup path over a unique column
//	explicit transaction the R2 elision / R3 narrowing
//
// Reading these plans in auto-commit and expecting an elision would be asserting
// the wrong thing about a correct engine; asserting only in a transaction would
// leave the auto-commit answers unchecked. Both answers must be the same rows.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DistinctOverUniqueIndexWithNulls(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_distinct_uniq", "duq",
		"CREATE TABLE t (id BIGINT, u BIGINT, d DOUBLE, g BIGINT, PRIMARY KEY (id)) ",
		"CREATE UNIQUE INDEX t_u ON t (u) CREATE UNIQUE INDEX t_d ON t (d) ")

	// u is unique where it is non-NULL, and NULL four times over — the shape a
	// full elision would get wrong by returning four NULLs where DISTINCT must
	// return one.
	w.Exec("INSERT INTO t (id, u, d, g) VALUES " +
		"(1, 10, 1.5, 1), " +
		"(2, 20, 2.5, 1), " +
		"(3, NULL, NULL, 1), " +
		"(4, NULL, NULL, 2), " +
		"(5, 30, 3.5, 2), " +
		"(6, NULL, NULL, 2), " +
		"(7, NULL, NULL, 3)")

	w.Want("DISTINCT over a unique column with repeated NULLs",
		"SELECT DISTINCT u FROM t ORDER BY u",
		[]string{"NULL", "10", "20", "30"})
	w.Want("DISTINCT over a unique DOUBLE with repeated NULLs",
		"SELECT DISTINCT d FROM t ORDER BY d",
		[]string{"NULL", "1.5", "2.5", "3.5"})
	w.Want("COUNT DISTINCT-shaped: the NULLs collapse to one row",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t) AS s",
		[]string{"4"})

	// DISTINCT on the unique column PAIRED with a non-unique one. The pair is
	// unique because its first component is, so the elision is still licensed —
	// but only for the non-exempt rows, and the NULL rows now differ in g.
	w.Want("DISTINCT over a unique column and a non-unique one",
		"SELECT DISTINCT u, g FROM t ORDER BY u, g",
		[]string{"NULL|1", "NULL|2", "NULL|3", "10|1", "20|1", "30|2"})

	// DISTINCT on the NON-unique column alone: no license at all, so this is the
	// control that the suite is not simply eliding everything.
	w.Want("DISTINCT over the non-unique column",
		"SELECT DISTINCT g FROM t ORDER BY g",
		[]string{"1", "2", "3"})

	// A predicate that excludes every NULL leaves a set the index proves
	// distinct outright, which is the arm where a FULL elision is correct.
	w.Want("DISTINCT with the exempt rows filtered away",
		"SELECT DISTINCT u FROM t WHERE u IS NOT NULL ORDER BY u",
		[]string{"10", "20", "30"})
	w.Want("DISTINCT with only the exempt rows left",
		"SELECT DISTINCT u FROM t WHERE u IS NULL ORDER BY u",
		[]string{"NULL"})

	// ---- the same rows, now with the proof licensed --------------------
	//
	// Inside a transaction the planner may discharge the DISTINCT against the
	// unique index. The rows must not move, and the plan must show WHICH route
	// discharged it — otherwise a green here is compatible with the optimization
	// having silently stopped firing, which is the state these assertions exist
	// to distinguish from a correct answer reached the slow way.
	elided := "SELECT DISTINCT u FROM t WHERE u IS NOT NULL ORDER BY u"
	// No ORDER BY, deliberately. An ORDER BY on the dedup key makes the inner
	// ordered by that key, which selects the STREAMING distinct executor — and
	// the narrowing is REFUSED to a streaming distinct by design, because
	// streaming retains only the previous row's key and so has no seen-set for a
	// narrowing to shrink. `SELECT DISTINCT u FROM t ORDER BY u` therefore plans
	// as a bare Distinct with no narrowed-by stamp, and asserting one there
	// would be demanding an optimization that is intentionally withheld.
	narrowed := "SELECT DISTINCT u FROM t"

	if plan := w.ExplainInTx(elided); strings.Contains(plan, "Distinct(") {
		t.Errorf("a NULL-rejecting conjunct covers the whole unique key, so the distinct should be "+
			"ELIDED outright inside a transaction (R2). It is still there:\n  plan: %s", plan)
	} else if !strings.Contains(plan, "distinct-by:") {
		t.Errorf("the distinct was elided with no proof stamp naming the index it rests on. The "+
			"license is mutable store state and must be revalidated per transaction, which needs "+
			"the dependency recorded.\n  plan: %s", plan)
	}
	w.WantInTx("R2-elided rows are unchanged", elided, []string{"10", "20", "30"})

	// Without the conjunct the exempt rows are still in play, so the operator
	// must SURVIVE — narrowed to retain only them.
	if plan := w.ExplainInTx(narrowed); !strings.Contains(plan, "Distinct(") {
		t.Errorf("the distinct was elided even though NULL rows are present. Four exempt rows would "+
			"then be returned where DISTINCT must return one.\n  plan: %s", plan)
	} else if !strings.Contains(plan, "narrowed-by:") {
		t.Errorf("the distinct is neither elided nor narrowed, so the unique index bought nothing "+
			"here (R3 is the floor, not an optional extra).\n  plan: %s", plan)
	}
	// Row CONTENT for the narrowed shape is checked through the ordered
	// spelling, which is deterministic; the unordered one is checked by count
	// just below. Both must see the exempt rows collapsed.
	w.WantInTx("rows under the ordered (streaming) spelling",
		"SELECT DISTINCT u FROM t ORDER BY u", []string{"NULL", "10", "20", "30"})

	// The narrowing's whole argument is that a non-exempt row cannot duplicate
	// anything, so only exempt rows need retaining. If that were wrong in the
	// direction of retaining too little, the repeated NULLs would come back —
	// which is exactly what this counts.
	w.WantInTx("the exempt rows still collapse under the narrowing",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t) AS s", []string{"4"})
}

// TestFDB_DistinctOverUniqueIndexUnderMutation drives the same shapes while the
// unique column's values move. The elision is licensed by store state, so what
// has to hold is that the license tracks the state: a value that becomes NULL
// joins the exempt set, and one that stops being NULL leaves it.
func TestFDB_DistinctOverUniqueIndexUnderMutation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_distinct_uniq_mut", "duqm",
		"CREATE TABLE t (id BIGINT, u BIGINT, g BIGINT, PRIMARY KEY (id)) ",
		"CREATE UNIQUE INDEX t_u ON t (u) ")

	distinctQ := "SELECT DISTINCT u FROM t ORDER BY u"

	w.Exec("INSERT INTO t (id, u, g) VALUES (1, 10, 1), (2, 20, 1), (3, NULL, 1)")
	w.Want("start", distinctQ, []string{"NULL", "10", "20"})

	// A second NULL arrives: two exempt rows must still collapse to one.
	w.Exec("INSERT INTO t (id, u, g) VALUES (4, NULL, 2)")
	w.Want("a second NULL collapses", distinctQ, []string{"NULL", "10", "20"})

	// A value becomes NULL, joining the exempt set and vacating its own value.
	w.Exec("UPDATE t SET u = NULL WHERE id = 10")
	w.Exec("UPDATE t SET u = NULL WHERE id = 1")
	w.Want("a value moves into the exempt set", distinctQ, []string{"NULL", "20"})

	// And back out: the row takes a fresh value the index must accept.
	w.Exec("UPDATE t SET u = 40 WHERE id = 1")
	w.Want("and back out again", distinctQ, []string{"NULL", "20", "40"})

	// Every NULL removed: the exempt set empties and the elision becomes total.
	w.Exec("DELETE FROM t WHERE u IS NULL")
	w.Want("with no exempt rows left", distinctQ, []string{"20", "40"})

	// Every row removed.
	w.Exec("DELETE FROM t")
	w.Want("empty table", distinctQ, []string{})
}

// TestFDB_UniqueIndexRejectsDuplicates is the precondition the elision rests
// on. If the index stopped enforcing uniqueness, eliding a dedup on its
// strength would silently return duplicates — so the enforcement is pinned
// here, next to the tests that depend on it, rather than assumed from the DDL.
func TestFDB_UniqueIndexRejectsDuplicates(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_uniq_enforce", "uqe",
		"CREATE TABLE t (id BIGINT, u BIGINT, PRIMARY KEY (id)) ",
		"CREATE UNIQUE INDEX t_u ON t (u) ")
	w.Exec("INSERT INTO t (id, u) VALUES (1, 10), (2, NULL), (3, NULL)")

	// A duplicate non-exempt value must be refused on the INDEXED schema. The
	// unindexed twin has no index and therefore accepts it, which is exactly why
	// this case uses the raw connections instead of the twin's Exec.
	if _, err := w.idx.ExecContext(ctx, "INSERT INTO t (id, u) VALUES (4, 10)"); err == nil {
		t.Errorf("the unique index accepted a duplicate value. Every distinct elision licensed by " +
			"this index is now unsound: the planner drops the dedup operator on the strength of a " +
			"uniqueness that no longer holds, and DISTINCT returns duplicates.")
	}
	// Repeated NULLs must still be accepted — that is what makes them exempt,
	// and what forces the narrowed dedup to exist at all.
	if _, err := w.idx.ExecContext(ctx, "INSERT INTO t (id, u) VALUES (5, NULL)"); err != nil {
		t.Errorf("a repeated NULL was refused by the unique index: %v.\nIf NULLs are now unique, the "+
			"exempt set is empty and the narrowed dedup has nothing to retain — the distinct could "+
			"be elided outright, and the tests that expect NULLs to collapse describe a state that "+
			"no longer exists.", err)
	}
	// An UPDATE into an occupied value is the other route to a duplicate.
	if _, err := w.idx.ExecContext(ctx, "UPDATE t SET u = 10 WHERE id = 2"); err == nil {
		t.Errorf("an UPDATE created a duplicate in a unique index; see above for why that unsounds " +
			"every elision licensed by it")
	}
}

// TestFDB_DistinctOverUniqueIndexAtScale runs the same invariant over enough
// rows that the plan is chosen on cost rather than on the table being tiny, and
// with enough exempt rows that a narrowed dedup retaining the wrong set shows as
// a row count rather than as a single stray row.
func TestFDB_DistinctOverUniqueIndexAtScale(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_distinct_uniq_scale", "duqs",
		"CREATE TABLE t (id BIGINT, u BIGINT, g BIGINT, PRIMARY KEY (id)) ",
		"CREATE UNIQUE INDEX t_u ON t (u) ")

	// 600 unique values and 300 NULLs interleaved.
	var rows []string
	id := 1
	for i := 0; i < 600; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d, %d)", id, i, i%7))
		id++
		if i%2 == 0 {
			rows = append(rows, fmt.Sprintf("(%d, NULL, %d)", id, i%7))
			id++
		}
	}
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		w.Exec("INSERT INTO t (id, u, g) VALUES " + strings.Join(rows[start:end], ", "))
	}

	w.Want("distinct count over 600 values and 300 NULLs",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t) AS s",
		[]string{"601"})
	w.Want("the exempt rows collapse to exactly one",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t WHERE u IS NULL) AS s",
		[]string{"1"})

	// The same counts with the proof licensed, which is where the NARROWED
	// dedup runs and where a wrong exempt set shows at full size: retaining too
	// little returns 300 NULL rows instead of one, and the auto-commit arm above
	// cannot see that because it never narrows. 300 exempt rows is also enough
	// that an off-by-one in the retention would not hide in the count.
	w.WantInTx("distinct count under the narrowing",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t) AS s",
		[]string{"601"})
	w.WantInTx("300 exempt rows collapse to one under the narrowing",
		"SELECT COUNT(*) FROM (SELECT DISTINCT u FROM t WHERE u IS NULL) AS s",
		[]string{"1"})
	if plan := w.ExplainInTx("SELECT DISTINCT u FROM t"); !strings.Contains(plan, "narrowed-by:") &&
		!strings.Contains(plan, "distinct-by:") {
		t.Errorf("at 900 rows the distinct is neither narrowed nor elided, so the two counts above "+
			"were taken on the full-dedup path and say nothing about the narrowing\n  plan: %s", plan)
	}
	w.Want("a bounded page of the distinct values",
		"SELECT DISTINCT u FROM t WHERE u IS NOT NULL ORDER BY u LIMIT 5",
		[]string{"0", "1", "2", "3", "4"})
}
