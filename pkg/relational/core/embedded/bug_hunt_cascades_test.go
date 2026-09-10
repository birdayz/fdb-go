package embedded

// Regression tests for Cascades correctness bugs found in the bug hunt.
// All are plan-only (no FDB) via PlanQueryForTest — each pins a plan-shape
// tell that directly implies the wrong-result behavior.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/relational/core/query"
)

// AGG-RESIDUAL: AggregateDataAccessRule must NOT serve a query from an
// aggregate index when there is a residual predicate it cannot turn into a
// grouping-key scan bound — the precomputed aggregate is over ALL rows, so the
// residual would be silently dropped (wrong SUM / wrong groups). The engine
// must fall back to StreamingAgg over a filtered scan.
func TestBugHunt_AggregateIndexResidualNotDropped(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE ORDERS (id BIGINT, region STRING, status STRING, amount BIGINT, PRIMARY KEY (id))
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

	// Control: a grouping-key INEQUALITY on the leading column is a RANGE scan
	// bound of the group key (RFC-248; Java's computeBoundParameterPrefixMap
	// binds one trailing inequality the same way) — the aggregate index is
	// used, and rows are pinned by TestFDB_AggregateIndexResidual's range arms.
	// This arm used to assert the decline, when the rule could bind equalities
	// only.
	t.Run("group_col_inequality_binds_a_range", func(t *testing.T) {
		plan, err := PlanQueryForTest("SELECT region, SUM(amount) FROM orders WHERE region > 'm' GROUP BY region", schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("a leading grouping-key inequality should bind the aggregate index as a range, got %s", plan)
		}
	})

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

// AGG-RESIDUAL multi-key. ToScanPlan consumes only the CONTIGUOUS LEADING run
// of grouping-key bounds; a grouping-key predicate outside that run used to
// decline the aggregate index outright (the rule had no residual, and a
// dropped predicate meant wrong groups). Since RFC-248 it is a residual filter
// above the aggregate scan — sound, because a predicate over grouping columns
// filters whole groups — so every shape here binds; the typed-tree pins with
// the scan arity and residual count are in aggregate_index_residual_test.go.
// What still declines is a predicate on the aggregation INPUT.
func TestBugHunt_AggregateIndexMultiKeyResidual(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT, a STRING, b STRING, c STRING, v BIGINT, PRIMARY KEY (id))
CREATE INDEX sum_abc AS SELECT SUM(v) FROM T GROUP BY a, b, c`

	for _, tc := range []struct{ name, sql string }{
		{"non_leading_key", "SELECT a, b, c, SUM(v) FROM t WHERE b = 'x' GROUP BY a, b, c"},
		{"gap_in_prefix", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c"},
		{"leading_prefix_one", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' GROUP BY a, b, c"},
		{"and_wrapped_leading_prefix_two", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND b = 'y' GROUP BY a, b, c"},
		{"and_wrapped_every_key_bound", "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND b = 'y' AND c = 'z' GROUP BY a, b, c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, "AggregateIndex") {
				t.Errorf("a predicate over grouping columns must be served by the aggregate index (bound or residual)\n  sql: %s\n  plan: %s", tc.sql, plan)
			}
		})
	}

	// A predicate on the aggregation input cannot be answered above a
	// pre-aggregated stream: the index must still decline.
	t.Run("input_predicate_declines", func(t *testing.T) {
		const sql = "SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND v > 0 GROUP BY a, b, c"
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if strings.Contains(plan, "AggregateIndex") {
			t.Errorf("aggregate index used with a predicate on the aggregation input\n  sql: %s\n  plan: %s", sql, plan)
		}
	})
}

// HAVING-PUSHDOWN: a HAVING predicate that references an aggregate must NOT be
// pushed below the GroupBy, regardless of operand order. `g > SUM(v)` must plan
// the same as `SUM(v) < g` (filter above the aggregation).
func TestBugHunt_HavingAggregateNotPushedBelowGroupBy(t *testing.T) {
	t.Parallel()
	const schema = `CREATE TABLE T (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))`

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
CREATE TABLE ORDERS (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_amount ON ORDERS(amount)`

	// COUNT(status) over an idx_amount range: status is NOT in idx_amount, so a
	// covering scan would read status as NULL → COUNT=0. Must fetch.
	plan, err := PlanQueryForTest("SELECT COUNT(status) FROM orders WHERE amount > 5", schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("COUNT(status): %s", plan)
	// COUNT(col) must read col, so the IDX_AMOUNT scan must NOT answer from the
	// index entry: idx_amount does not carry status, so a covering scan would
	// read NULL for every row and COUNT would come back 0.
	//
	// BOTH of the substring forms this used to be written in are retired, for the
	// same underlying reason — a rendered substring is not the property.
	//
	//   - `!strings.Contains(plan, "COVERING")` is satisfied by a FULL TABLE SCAN,
	//     which carries no COVERING marker anywhere. The test exists to reject
	//     exactly that class of plan and would have passed on it. The scoped form
	//     below fails when the scan is absent, which is a distinct answer from
	//     "the scan is not covering" — see scanCoverage.
	//   - `strings.Contains(plan, "Fetch")` as a proxy for "the base record is
	//     read" is dead by RFC-220: a bare `IndexScan(…)` IS a fetching scan
	//     (executeIndexScan resolves every entry by primary key), so no `Fetch`
	//     node renders in a correct plan and its absence proves nothing.
	assertScanReadsBaseRecords(t, plan, "IndexScan(IDX_AMOUNT")

	// Controls: COUNT(*) and COUNT(<constant>) read no base-record field, so the
	// same scan MAY answer from the index entry — and must, or the count is doing
	// a primary-key lookup per row for values it never looks at. Asserted as the
	// positive property (this scan is COVERING) rather than as "no Fetch node
	// renders", which is true of the fetching plan as well.
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
		assertScanAnswersFromIndexEntry(t, p, "IndexScan(IDX_AMOUNT")
	}
}

// IN-LIMIT-NIL: an IN-list query with a top-level LIMIT (no ORDER BY) must not
// extract a plan with a nil inner — the limit wrapper must relink its extracted
// child, else InJoin(<nil>)/Fetch(<nil>) survives → 0 rows or execution error.
func TestBugHunt_InListLimitNoNilInner(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE ORDERS (id BIGINT, customer_id BIGINT, amount BIGINT, PRIMARY KEY (id))
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
	const schema = `CREATE TABLE T (id BIGINT, v BIGINT, PRIMARY KEY (id))`
	const sql = "SELECT DISTINCT * FROM (SELECT * FROM t WHERE id > 0 UNION ALL SELECT * FROM t WHERE id > 0) AS u"
	plan, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("plan: %s", plan)
	// The dedup may be carried by (a) the explicit Distinct operator, (b) a
	// merge-sort union planned WITH the distinct flag (MergeSortUnion(...,
	// DISTINCT) — comparison-key dedup on the pk is full-row dedup via pk
	// uniqueness), or (c) NO union at all: the two UNION ALL branches are
	// IDENTICAL, so under the enclosing DISTINCT the rewriting phase may
	// collapse to a single branch whose pk-scan is provably distinct and
	// elide the dedup entirely (the RFC-186 designated comparator picks this
	// deterministically; the old member-summing tier-4 picked the two-legged
	// form by accident of inflated counts). What must never happen is the
	// historical bug: a MULTI-LEG union — which produces duplicates — with
	// no dedup anywhere above it.
	hasDedup := strings.Contains(plan, "Distinct") || strings.Contains(plan, "DISTINCT)")
	hasUnion := strings.Contains(plan, "Union")
	if hasUnion && !hasDedup {
		t.Errorf("SELECT DISTINCT over a multi-leg union dropped the dedup operator (wrong rows — duplicates survive): %s", plan)
	}
	if !hasUnion && strings.Contains(plan, "<nil>") {
		t.Errorf("collapsed plan carries a nil child: %s", plan)
	}
}

// These SQL forms start with their cross-leg predicate inside Select, not in
// Filter(Select). Keep the negative result separate from the rule-level nested
// dependency regression: correct rows here alone did not catch that defect.
func TestBugHunt_NestedJoinPredicateStartsInsideSelect(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT)
CREATE TABLE a (id BIGINT, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT, n nst, PRIMARY KEY (id))`
	for _, sql := range []string{
		"SELECT a.id, b.id FROM a JOIN b ON TRUE WHERE a.id = b.n.co ORDER BY a.id",
		"SELECT a.id, b.id FROM a JOIN b ON TRUE WHERE b.n.co = a.id ORDER BY a.id",
		"SELECT a.id, b.id FROM a JOIN b ON a.id = b.n.co ORDER BY a.id",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(ddl)
			if err != nil {
				t.Fatal(err)
			}
			md := tmpl.Underlying()
			op := buildViaPlanVisitor(t, md, sql)
			ref, _, err := query.TranslateToCascadesWithError(op, md)
			if err != nil || ref == nil {
				t.Fatalf("translation: ref=%v err=%v", ref, err)
			}
			crossLegPredicates := 0
			var walk func(expressions.RelationalExpression)
			walk = func(expr expressions.RelationalExpression) {
				if filter, ok := expr.(*expressions.LogicalFilterExpression); ok {
					if sel, isJoin := filter.GetInner().GetRangesOver().Get().(*expressions.SelectExpression); isJoin && len(sel.GetQuantifiers()) == 2 {
						t.Error("translator introduced Filter around this inner join; Filter(Select) re-arms the specialized pushdown path that previously lost nested sibling dependencies")
					}
				}
				if sel, ok := expr.(*expressions.SelectExpression); ok && len(sel.GetQuantifiers()) == 2 {
					qs := sel.GetQuantifiers()
					for _, pred := range sel.GetPredicates() {
						corr := pred.GetCorrelatedTo()
						_, left := corr[qs[0].GetAlias()]
						_, right := corr[qs[1].GetAlias()]
						if left && right {
							crossLegPredicates++
						}
					}
				}
				for _, q := range expr.GetQuantifiers() {
					for _, child := range q.GetRangesOver().AllMembers() {
						walk(child)
					}
				}
			}
			walk(ref.Get())
			if crossLegPredicates != 1 {
				t.Fatalf("translated Select carries %d cross-leg predicates, want one; losing this placement re-arms nested-dependency pushdown", crossLegPredicates)
			}
		})
	}
}
