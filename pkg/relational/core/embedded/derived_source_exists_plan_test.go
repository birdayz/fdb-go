package embedded

import (
	"strings"
	"testing"
)

const derivedExistsSchema = `CREATE TABLE t1 (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
CREATE TABLE t3 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))`

// EXISTS correlated to a DERIVED SOURCE — a WITH-CTE leg or a derived table —
// plans, in the SELECT list and in the WHERE alike, joined and unjoined.
//
// The matrix is the point, because the three defects it covers presented as ONE
// symptom (a projected EXISTS over a derived source fails) and were three
// unrelated causes; the arms that separated them are the ones that look
// redundant:
//
//   - the CTE arms failed 42703 "no FROM source aliased as C" because the
//     subquery's OUTER SCOPE registered every real leg and no WITH leg. The
//     where_exists arm is what showed this had nothing to do with projection.
//   - the derived-table PROJECTED arms planned and then died in the executor,
//     because the join's step-1 ordinal seed refused a PROJECTION leg. The
//     base-table twin is what showed it was the leg SHAPE.
//   - the derived-table WHERE arms lost their plan entirely once a projection
//     began stating its row: the orientation gate compared the seed's unstated
//     window types against the leg's newly stated ones and declined both
//     orientations. Nothing else in this tree covered that shape.
//
// Planning only — the rows are asserted end-to-end against real FDB by
// TestFDB_ProjectionResultTypeProbe. This test is the fast sentinel: it runs in
// milliseconds and fails on the exact decision, where the FDB test proves the
// answer.
func TestDerivedSourceCorrelatedExistsPlans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		// wantNLJ requires the plan to materialize the two-leg join — the shape
		// whose merged row the correlated read addresses. Without it an arm could
		// "pass" through some degenerate plan that never exercises the seed.
		wantNLJ bool
	}{
		{"cte_projected_exists", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, t1_id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h
			FROM c, t3 WHERE t3.t1_id = c.id ORDER BY c.id`, true},
		{"cte_where_exists", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, t1_id FROM c, t3
			WHERE t3.t1_id = c.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) ORDER BY c.id`, true},
		{"cte_projected_exists_unjoined", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) AS h FROM c ORDER BY c.id`, false},
		{"cte_where_exists_unjoined", `WITH c AS (SELECT id, v FROM t1)
			SELECT c.id FROM c WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) ORDER BY c.id`, false},

		{"derived_projected_exists", `SELECT d.id, t1_id,
			EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) AS h
			FROM (SELECT id, v FROM t1) AS d, t3 WHERE t3.t1_id = d.id ORDER BY d.id`, true},
		{"derived_where_exists", `SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
			WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id) ORDER BY d.id`, true},
		{"derived_where_exists_unordered", `SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
			WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id)`, true},

		// The base-table twins. They passed throughout, and they are what
		// localized each defect to the derived leg rather than to EXISTS, to the
		// join, or to the ORDER BY.
		{"table_projected_exists", `SELECT t1.id, t3.t1_id,
			EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h
			FROM t1, t3 WHERE t3.t1_id = t1.id ORDER BY t1.id`, true},
		{"table_where_exists", `SELECT t1.id, t3.t1_id FROM t1, t3
			WHERE t3.t1_id = t1.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) ORDER BY t1.id`, true},
	}

	// A CTE that SHADOWS a same-named catalog table, correlated from inside an
	// EXISTS. The subquery's outer scope consults the CTE registry BEFORE the
	// catalog, in lockstep with the scope the query itself is resolved in — the
	// other order analyzes the TABLE's schema for reads that execute against the
	// CTE.
	//
	// The correlated column is chosen so the two orders give DIFFERENT answers:
	// the CTE named `t1` emits T1_ID (it selects from t3), and the base table t1
	// has no such column. A catalog-first outer scope therefore cannot resolve
	// `t1.t1_id` at all and the query dies 42703, so this arm fails loudly rather
	// than passing on a column both schemas happen to share.
	cases = append(cases, struct {
		name    string
		sql     string
		wantNLJ bool
	}{"cte_shadowing_a_real_table", `WITH t1 AS (SELECT id, t1_id FROM t3)
		SELECT t1.id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.t1_id) AS h
		FROM t1 ORDER BY t1.id`, false})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(c.sql, derivedExistsSchema, nil)
			if err != nil {
				t.Fatalf("plan failed: %T: %v\n  sql: %s", err, err, c.sql)
			}
			explain := plan.Explain()
			// A correlated EXISTS lowers to a FlatMap over the existential leg.
			// Without this the arm would stay green on a plan that answered the
			// query some other way and exercised none of the three decisions.
			if !strings.Contains(explain, "FlatMap") {
				t.Fatalf("no FlatMap in the plan — the correlated EXISTS did not lower to "+
					"the existential fold, so this arm witnesses nothing:\n  %s", explain)
			}
			if c.wantNLJ && !strings.Contains(explain, "NestedLoopJoin") {
				t.Fatalf("no NestedLoopJoin in the plan — the two-leg join whose merged row "+
					"the correlated read addresses is what these arms exist to cover:\n  %s", explain)
			}
		})
	}
}
