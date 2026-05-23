package embedded

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
)

const ordersSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  tier STRING,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE INDEX idx_status ON ORDERS(status)
CREATE INDEX idx_amount ON ORDERS(amount)
CREATE INDEX idx_tier ON ORDERS(tier)
`

func TestPlanHarness_PKPointLookup(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE id = 1",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "Scan(ORDERS, [=])")
}

func TestPlanHarness_IndexEquality(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE customer_id = 42",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "IndexScan(IDX_CUSTOMER, [=])")
}

func TestPlanHarness_IndexRange(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE amount > 9000",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "IndexScan(IDX_AMOUNT,")
}

func TestPlanHarness_OrderByPK(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders ORDER BY id",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "Scan(ORDERS")
	assertPlanNotContains(t, plan, "InMemorySort")
}

func TestPlanHarness_OrderByIndex(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders ORDER BY status",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "IndexScan(IDX_STATUS,")
}

func TestPlanHarness_OrderByIndexDesc(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders ORDER BY status DESC",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "IndexScan(IDX_STATUS,")
	assertPlanContains(t, plan, "REVERSE")
}

func TestPlanHarness_GroupByCountCovering(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
	assertPlanContains(t, plan, "IDX_STATUS")
	assertPlanNotContains(t, plan, "InMemorySort")
}

func TestPlanHarness_GroupByCountOrderBy(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status ORDER BY status",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
}

func TestPlanHarness_GroupByCountOrderByDesc(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status ORDER BY status DESC",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
	assertPlanContains(t, plan, "IDX_STATUS")
	assertPlanContains(t, plan, "REVERSE")
	assertPlanNotContains(t, plan, "InMemorySort")
}

func TestPlanHarness_GroupBySumNonCovering(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT status, SUM(amount) FROM orders GROUP BY status",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanNotContains(t, plan, "COVERING")
	assertPlanContains(t, plan, "StreamingAgg")
	assertPlanContains(t, plan, "IDX_STATUS")
}

func TestPlanHarness_GroupBySumCoveringComposite(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (id BIGINT NOT NULL, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_status_amount ON ORDERS(status, amount)
`
	plan, err := PlanQueryForTest(
		"SELECT status, SUM(amount) FROM orders GROUP BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
	assertPlanContains(t, plan, "IDX_STATUS_AMOUNT")
	assertPlanContains(t, plan, "COVERING")
}

func TestPlanHarness_PKLookupAndFilter(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE id = 500 AND status = 'pending'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanContains(t, plan, "Scan(ORDERS, [=])")
}

func TestPlanHarness_JoinOnIndex(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (id BIGINT NOT NULL, customer_id BIGINT, PRIMARY KEY (id))
CREATE TABLE CUSTOMERS (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))
CREATE INDEX idx_customer ON ORDERS(customer_id)
`
	plan, err := PlanQueryForTest(
		"SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

func TestPlanHarness_InList(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "InJoin")
}

func TestPlanHarness_CountStarNoGroupBy(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT COUNT(*) FROM orders WHERE status = 'pending'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
	assertPlanContains(t, plan, "IDX_STATUS")
	assertPlanContains(t, plan, "COVERING")
}

func TestPlanHarness_OrderByNonIndexColumn(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders ORDER BY amount",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_AMOUNT")
	assertPlanNotContains(t, plan, "InMemorySort")
}

func TestPlanHarness_FilterAndOrderDifferentIndexes(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE status = 'active' ORDER BY id",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
}

func TestPlanHarness_WithStats_SmallTable(t *testing.T) {
	t.Parallel()
	stats := properties.MapStatistics{
		PerType: map[string]float64{"orders": 100},
	}
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE amount > 50",
		ordersSchema, stats)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan (100 rows): %s", plan)
}

func TestPlanHarness_WithStats_LargeTable(t *testing.T) {
	t.Parallel()
	stats := properties.MapStatistics{
		PerType: map[string]float64{"orders": 1_000_000},
	}
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE amount > 50",
		ordersSchema, stats)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan (1M rows): %s", plan)
}

func assertPlanContains(t *testing.T, plan, substr string) {
	t.Helper()
	if !strings.Contains(plan, substr) {
		t.Errorf("plan does not contain %q:\n  %s", substr, plan)
	}
}

func assertPlanNotContains(t *testing.T, plan, substr string) {
	t.Helper()
	if strings.Contains(plan, substr) {
		t.Errorf("plan should not contain %q:\n  %s", substr, plan)
	}
}
