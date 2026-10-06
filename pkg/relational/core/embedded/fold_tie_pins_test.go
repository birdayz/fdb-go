package embedded_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// survivingFilterPredicates renders the predicates of every PredicatesFilter in
// the plan, top-down, so a pin can name the member a prune kept (EXPLAIN shows
// only `[n preds]`).
func survivingFilterPredicates(p plans.RecordQueryPlan) string {
	var out []string
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if f, ok := p.(*plans.RecordQueryPredicatesFilterPlan); ok {
			for _, q := range f.GetPredicates() {
				out = append(out, q.Explain())
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(p)
	return strings.Join(out, " AND ")
}

// TestFoldTiePins_DecidingRung pins WS-E design 5.4(g)/(j): each statement whose
// simplification leaves a member pair the cost models cannot separate on
// structure is planned cold 20 times over the WS-E oracle's two-table (v5) and
// seven-table (v4) schemas, and the plan and the surviving member's predicates
// are asserted, with the rung that decides between the members.
//
// Rungs (read off the members and confirmed by mutation, 2026-10):
//   - rewriting-hash: REWRITING's `deepHash` tiebreak. Inverting its compare
//     flips every such row here to the other member (`NULL = 1` for the
//     div0 rows, `NOT (FALSE)` for the NOT row) and no other row.
//   - planning-hash: PLANNING's `costExprHash`. Inverting it swaps only the
//     join-nesting row's NLJ sides.
//   - rewriting-conjuncts: the residual-conjunct rung; the tautology a fold
//     leaves counts 0 (NormalizedResidualPredicateProperty).
//   - walk-time: no pair reaches the memo, because Go folds at walk time.
//
// Where Go and the target differ: the target's original member keeps
// `(1 / 0) + NULL` and its hash decides per schema (FILTER null over v5, the
// division over v4: trace_null_strict_div0_cast_null_eq_one,
// trace_v4_schema_div0_cast_null_eq_one). Go's walker collapses arithmetic over a
// typed NULL (expr.ResolveArithmetic), so its original member is `NULL = 1` and
// both members answer no rows: the hash picks the plan, never the answer. In
// the same way IS NULL over a NOT NULL constant folds at walk time
// (expr.ResolveIsNull), so `COALESCE(1 / 0, 5) IS NULL` is `FALSE` without a
// tie, as the target's prune keeps it over v5. The oracle's wseGoDivergences
// declares the two v4 rows.
func TestFoldTiePins_DecidingRung(t *testing.T) {
	t.Parallel()
	const v5 = "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
	const v4 = "CREATE TABLE T (id BIGINT, s STRING, n BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE L (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE I (id BIGINT, i INTEGER, PRIMARY KEY (id)) " +
		"CREATE TABLE F (id BIGINT, f FLOAT, PRIMARY KEY (id)) " +
		"CREATE TABLE D (id BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
		"CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, m BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE E (id BIGINT, x BIGINT, PRIMARY KEY (id))"
	const filterT = "Map(PredicatesFilter(Scan(T), [1 preds]), {ID: _current.ID#0})"
	rows := []struct {
		oracle, sql, plan, kept, rung string
	}{
		{
			"null_strict_div0_cast_null_eq_one_where, v4_schema_div0_cast_null_eq_one_where, null_strict_cast_null_beside_div0_where",
			"SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1",
			filterT, "UNKNOWN", "rewriting-hash",
		},
		{
			"v5_schema_div0_cast_null_eq_one_where_first",
			"SELECT id FROM T WHERE (5 / 0) + CAST(NULL AS INTEGER) = 5",
			filterT, "UNKNOWN", "rewriting-hash",
		},
		{
			"first_planning_a_exec",
			"SELECT id FROM T WHERE (3 / 0) + CAST(NULL AS INTEGER) = 3 AND id > 0 ORDER BY id",
			"Map(PredicatesFilter(Scan(T, [<>]), [1 preds]), {ID: _current.ID#0})", "UNKNOWN", "rewriting-hash",
		},
		{
			"first_planning_b_exec",
			"SELECT id FROM T WHERE (4 / 0) + CAST(NULL AS INTEGER) = 4 AND id >= 0 ORDER BY id",
			"Map(PredicatesFilter(Scan(T, [<>]), [1 preds]), {ID: _current.ID#0})", "UNKNOWN", "rewriting-hash",
		},
		{
			"null_strict_cast_null_beside_div0_where_empty_table",
			"SELECT id FROM E WHERE (1 / 0) + CAST(NULL AS INTEGER) = 1",
			"Map(PredicatesFilter(Scan(E), [1 preds]), {ID: _current.ID#0})", "UNKNOWN", "rewriting-hash",
		},
		{
			"not_coalesce_false_div0_where (5.4(h))",
			"SELECT id FROM T WHERE NOT COALESCE(FALSE, 1 / 0 = 1) ORDER BY id",
			filterT, "NOT (COALESCE(FALSE, predicate) = TRUE)", "rewriting-hash",
		},
		{
			"in_cast_null_join_inner_empty",
			"SELECT T.id FROM T, E WHERE E.x IN (CAST(NULL AS BIGINT))",
			"NestedLoopJoin(INNER, Scan(T), PredicatesFilter(Scan(E), [1 preds]))", "_current.X#1 IN array", "planning-hash",
		},
		{
			"null_strict_div0_cast_null_is_null_where",
			"SELECT id FROM T WHERE (1 / 0) + CAST(NULL AS INTEGER) IS NULL ORDER BY id",
			"Map(Scan(T), {ID: _current.ID#0})", "", "rewriting-conjuncts",
		},
		{
			"coalesce_true_div0_and_column_where (5.4(h))",
			"SELECT id FROM T WHERE COALESCE(TRUE, 1 / 0 = 1) AND n > 0 ORDER BY id",
			filterT, "_current.N#2 > 0", "rewriting-conjuncts",
		},
		{
			"coalesce_div0_five_is_null_where",
			"SELECT id FROM T WHERE COALESCE(1 / 0, 5) IS NULL ORDER BY id",
			filterT, "FALSE", "walk-time",
		},
	}
	for _, schema := range []struct{ name, ddl string }{{"v5", v5}, {"v4", v4}} {
		for _, r := range rows {
			for i := 0; i < 20; i++ {
				plan, err := embedded.PlanPhysicalForTest(r.sql, schema.ddl, nil)
				if err != nil {
					t.Fatalf("%s [%s] plan %d: %v", r.oracle, schema.name, i, err)
				}
				if got := plan.Explain(); got != r.plan {
					t.Fatalf("%s [%s] plan %d (%s rung):\n got %s\nwant %s", r.oracle, schema.name, i, r.rung, got, r.plan)
				}
				if got := survivingFilterPredicates(plan); got != r.kept {
					t.Fatalf("%s [%s] plan %d (%s rung) kept %q, want %q", r.oracle, schema.name, i, r.rung, got, r.kept)
				}
			}
		}
	}
}
