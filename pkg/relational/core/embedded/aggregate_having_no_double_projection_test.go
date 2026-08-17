package embedded

import (
	"strings"
	"testing"
)

const aggHavingSchema = `
CREATE TABLE ORDERS (
  id BIGINT,
  customer_id BIGINT,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX count_by_customer AS SELECT COUNT(*) FROM ORDERS GROUP BY customer_id
CREATE INDEX sum_amount_by_customer AS SELECT SUM(amount) FROM ORDERS GROUP BY customer_id
`

// TestAggregateIndexHavingHasOneProjection pins the shape of an aggregate-index
// GROUP BY ... HAVING plan: ONE projection, above the filter.
//
// The aggregate-index match must publish a row whose exact result type is the one
// the Reference expects, and it used to do that by always wrapping its leaf in an
// ordinal projection. When the leaf already carries those column names the
// projection renames nothing, and the cost of keeping it is not one operator but
// two: the HAVING filter then reads the PROJECTION's row, so the projection is
// materialised BELOW the filter and the identical list is projected again above
// it. On the 1M stress suite that shape ran 1.88x.
//
// A golden also records this plan, but a golden records whatever happens. This
// states what must hold and why, and it fails with the reason rather than a diff.
func TestAggregateIndexHavingHasOneProjection(t *testing.T) {
	t.Parallel()

	plan, err := PlanQueryForTest(
		"SELECT customer_id, SUM(amount) FROM orders GROUP BY customer_id "+
			"HAVING SUM(amount) > 50000 ORDER BY customer_id",
		aggHavingSchema, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Non-vacuity FIRST: a plan that fell back to a scan would trivially have one
	// projection and would say nothing about the elision.
	if !strings.Contains(plan, "AggregateIndex(") {
		t.Fatalf("the query no longer plans against the aggregate indexes, so this "+
			"test can no longer see the projection it is about: %s", plan)
	}
	if !strings.Contains(plan, "PredicatesFilter(") {
		t.Fatalf("HAVING is no longer a PredicatesFilter, so the shape this pins is "+
			"gone: %s", plan)
	}

	if got := strings.Count(plan, "Project("); got != 1 {
		t.Errorf("plan has %d projections, want 1 — an aggregate-index match that "+
			"wraps its leaf in a projection it does not need makes the HAVING filter "+
			"read the projection's row, which materialises that projection below the "+
			"filter and projects the same list again above it:\n  %s", got, plan)
	}
	// And the projection that remains is the one carrying the select list, above
	// the filter — not below it.
	if strings.Contains(plan, "PredicatesFilter(Project(") {
		t.Errorf("the surviving projection sits BELOW the HAVING filter:\n  %s", plan)
	}
}

// TestAggregateIndexArithmeticOverGroupsHasOneProjection is the same elision
// without a HAVING clause: an expression over two aggregates stacks a projection
// directly on the match's own, and only the outer one computes anything.
func TestAggregateIndexArithmeticOverGroupsHasOneProjection(t *testing.T) {
	t.Parallel()

	plan, err := PlanQueryForTest(
		"SELECT customer_id, SUM(amount) / COUNT(amount) FROM orders "+
			"GROUP BY customer_id ORDER BY customer_id",
		`
CREATE TABLE ORDERS (
  id BIGINT,
  customer_id BIGINT,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_sum_amount AS SELECT SUM(amount) FROM ORDERS GROUP BY customer_id
CREATE INDEX idx_count_amount AS SELECT COUNT(amount) FROM ORDERS GROUP BY customer_id
`, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(plan, "AggregateIndex(") {
		t.Fatalf("the query no longer plans against the aggregate indexes: %s", plan)
	}
	if got := strings.Count(plan, "Project("); got != 1 {
		t.Errorf("plan has %d projections, want 1 (the arithmetic one):\n  %s", got, plan)
	}
}
