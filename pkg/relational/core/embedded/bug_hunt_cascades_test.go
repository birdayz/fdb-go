package embedded

// Regression tests for Cascades correctness bugs found in the bug hunt.
// All are plan-only (no FDB) via PlanQueryForTest — each pins a plan-shape
// tell that directly implies the wrong-result behavior.

import (
	"strings"
	"testing"
)

// AGG-RESIDUAL: AggregateDataAccessRule must NOT serve a query from an
// aggregate index when there is a residual predicate it cannot turn into a
// grouping-key scan bound — the precomputed aggregate is over ALL rows, so the
// residual would be silently dropped (wrong SUM / wrong groups). The engine
// must fall back to StreamingAgg over a filtered scan.
func TestBugHunt_AggregateIndexResidualNotDropped(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE ORDERS (id BIGINT NOT NULL, region STRING, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_status ON ORDERS(status)
CREATE INDEX sum_amount_by_region AS SELECT SUM(amount) FROM ORDERS GROUP BY region`

	unfiltered, err := PlanQueryForTest("SELECT region, SUM(amount) FROM orders GROUP BY region", schema, nil)
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if !strings.Contains(unfiltered, "AggregateIndex") {
		t.Fatalf("precondition: unfiltered query should use the aggregate index, got %s", unfiltered)
	}

	cases := []struct {
		name string
		sql  string
	}{
		{"non_group_col", "SELECT region, SUM(amount) FROM orders WHERE status = 'paid' GROUP BY region"},
		{"non_equality_on_group_col", "SELECT region, SUM(amount) FROM orders WHERE region > 'm' GROUP BY region"},
		{"non_group_range", "SELECT region, SUM(amount) FROM orders WHERE amount > 100 GROUP BY region"},
		// RHS is another column, not a constant — `region = status` correlates
		// two columns of the same record; it can never be a scan bound.
		{"non_constant_rhs", "SELECT region, SUM(amount) FROM orders WHERE region = status GROUP BY region"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(c.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("plan: %s", plan)
			if strings.Contains(plan, "AggregateIndex") {
				t.Errorf("residual dropped: aggregate index used despite an uncompensable predicate\n  sql:  %s\n  plan: %s", c.sql, plan)
			}
			if plan == unfiltered {
				t.Errorf("filtered plan is byte-identical to the unfiltered plan (predicate vanished)\n  sql: %s", c.sql)
			}
		})
	}

	// Control: a grouping-key EQUALITY residual IS a valid scan bound — the
	// aggregate index may still be used.
	t.Run("group_col_equality_still_uses_index", func(t *testing.T) {
		plan, err := PlanQueryForTest("SELECT region, SUM(amount) FROM orders WHERE region = 'us' GROUP BY region", schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("grouping-key equality should still use the aggregate index, got %s", plan)
		}
	})
}

// AGG-RESIDUAL multi-key: ToScanPlan consumes only the CONTIGUOUS LEADING prefix
// of grouping-key equality bounds (it breaks at the first gap). An equality on a
// non-leading grouping key, or a gap in the bound prefix, cannot be applied — the
// aggregate index must be declined. A contiguous leading prefix is fine.
func TestBugHunt_AggregateIndexMultiKeyResidual(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a STRING, b STRING, c STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX sum_abc AS SELECT SUM(v) FROM T GROUP BY a, b, c`

	// Must NOT use the aggregate index — the residual can't be faithfully bound.
	// (and_wrapped: `a=x AND b=y` is a single AndPredicate the bound-builder can't
	// decompose, so it conservatively falls back to StreamingAgg — correct rows;
	// binding it via the index is a perf follow-up that needs conjunct flattening.
	// Crucially the PRE-fix code used an *unbounded* aggregate index here and
	// dropped both conjuncts → wrong groups; the guard now declines it.)
	mustDecline := []struct{ name, sql string }{
		{"non_leading_key", "SELECT a, b, c, SUM(v) FROM t WHERE b = 'x' GROUP BY a, b, c"},
		{"gap_in_prefix", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c"},
		{"and_wrapped_multi_equality", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND b = 'y' GROUP BY a, b, c"},
	}
	for _, tc := range mustDecline {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("plan: %s", plan)
			if strings.Contains(plan, "AggregateIndex") {
				t.Errorf("non-faithfully-bound residual dropped: aggregate index used\n  sql: %s\n  plan: %s", tc.sql, plan)
			}
		})
	}

	// A single leading-prefix equality IS a faithful scan bound → index is used.
	t.Run("leading_prefix_one", func(t *testing.T) {
		const sql = "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' GROUP BY a, b, c"
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("leading-prefix equality should use the aggregate index\n  sql: %s\n  plan: %s", sql, plan)
		}
	})
}

// HAVING-PUSHDOWN: a HAVING predicate that references an aggregate must NOT be
// pushed below the GroupBy, regardless of operand order. `g > SUM(v)` must plan
// the same as `SUM(v) < g` (filter above the aggregation).
func TestBugHunt_HavingAggregateNotPushedBelowGroupBy(t *testing.T) {
	t.Parallel()
	const schema = `CREATE TABLE T (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))`

	for _, sql := range []string{
		"SELECT g, SUM(v) FROM t GROUP BY g HAVING g > SUM(v)",
		"SELECT g, SUM(v) FROM t GROUP BY g HAVING SUM(v) < g",
	} {
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		t.Logf("%s\n  => %s", sql, plan)
		// The aggregate predicate must sit ABOVE the StreamingAgg, never on the
		// raw scan below it.
		if strings.Contains(plan, "PredicatesFilter(Scan(T)") {
			t.Errorf("HAVING on aggregate pushed below GroupBy onto raw scan\n  sql:  %s\n  plan: %s", sql, plan)
		}
	}

	// Control: a HAVING/WHERE predicate on a grouping key vs a constant IS
	// safely pushable below the aggregation.
	plan, err := PlanQueryForTest("SELECT g, SUM(v) FROM t GROUP BY g HAVING g > 5", schema, nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	t.Logf("control g>5 => %s", plan)
	if !strings.Contains(plan, "PredicatesFilter(Scan(T)") {
		t.Errorf("key-vs-constant predicate should still push below GroupBy, got %s", plan)
	}
}

// COUNT-COL-COVERING: scalar COUNT(col) must read col (SQL NULL semantics:
// count only non-NULL), so its supporting index scan must NOT be marked
// COVERING with zero columns when the index lacks col.
func TestBugHunt_CountColumnNotForcedCovering(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE ORDERS (id BIGINT NOT NULL, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_amount ON ORDERS(amount)`

	// COUNT(status) over an idx_amount range: status is NOT in idx_amount, so a
	// covering scan would read status as NULL → COUNT=0. Must fetch.
	plan, err := PlanQueryForTest("SELECT COUNT(status) FROM orders WHERE amount > 5", schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("COUNT(status): %s", plan)
	// COUNT(col) must read col → the scan must Fetch (not a zero-column COVERING
	// scan that reads status as NULL and returns 0). Assert the Fetch positively.
	if !strings.Contains(plan, "Fetch") {
		t.Errorf("COUNT(col) must Fetch the counted column, got a non-fetching plan (status would read NULL → COUNT=0): %s", plan)
	}
	if strings.Contains(plan, "COVERING") {
		t.Errorf("COUNT(col) over an index lacking col must not be COVERING: %s", plan)
	}

	// Controls: COUNT(*) and COUNT(<constant>) read no base-record field, so they
	// MAY still use a covering index scan (no Fetch). COUNT(1)/COUNT(TRUE) must
	// not regress to Fetch (the covering decision is about field access, not
	// count-star semantics).
	for _, q := range []string{
		"SELECT COUNT(*) FROM orders WHERE amount > 5",
		"SELECT COUNT(1) FROM orders WHERE amount > 5",
		"SELECT COUNT(TRUE) FROM orders WHERE amount > 5",
	} {
		p, err := PlanQueryForTest(q, schema, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		t.Logf("%s => %s", q, p)
		if strings.Contains(p, "Fetch") {
			t.Errorf("%s reads no field and should stay covering (no Fetch), got %s", q, p)
		}
	}
}

// IN-LIMIT-NIL: an IN-list query with a top-level LIMIT (no ORDER BY) must not
// extract a plan with a nil inner — the limit wrapper must relink its extracted
// child, else InJoin(<nil>)/Fetch(<nil>) survives → 0 rows or execution error.
func TestBugHunt_InListLimitNoNilInner(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE ORDERS (id BIGINT NOT NULL, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_customer ON ORDERS(customer_id)`
	for _, sql := range []string{
		"SELECT id, amount FROM orders WHERE customer_id IN (0,1,2,3,4) LIMIT 5", // non-covering
		"SELECT id FROM orders WHERE customer_id IN (0,1,2,3,4) LIMIT 5",         // covering
	} {
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		t.Logf("%s\n  => %s", sql, plan)
		if strings.Contains(plan, "<nil>") {
			t.Errorf("nil inner survived into the plan (limit wrapper did not relink): %s\n  sql: %s", plan, sql)
		}
	}
}

// DISTINCT-UNIONALL: SELECT DISTINCT over a UNION ALL must keep a dedup
// (Distinct) operator — the no-dedup Union plan must not report itself distinct
// and elide the enclosing DISTINCT.
func TestBugHunt_DistinctOverUnionAllKeepsDedup(t *testing.T) {
	t.Parallel()
	const schema = `CREATE TABLE T (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))`
	const sql = "SELECT DISTINCT * FROM (SELECT * FROM t WHERE id > 0 UNION ALL SELECT * FROM t WHERE id > 0) AS u"
	plan, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Distinct") {
		t.Errorf("SELECT DISTINCT over UNION ALL dropped the dedup operator: %s", plan)
	}
}
