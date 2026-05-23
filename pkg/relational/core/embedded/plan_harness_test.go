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
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_AMOUNT,")
	assertPlanContains(t, plan, "COVERING")
	assertPlanNotContains(t, plan, "Fetch")
}

func TestPlanHarness_IndexRangeCoveringIDAndAmount(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE amount > 9000",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_AMOUNT,")
	assertPlanContains(t, plan, "COVERING")
}

func TestPlanHarness_IndexRangeNonCovering(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, status FROM orders WHERE amount > 9000",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_AMOUNT,")
	assertPlanNotContains(t, plan, "COVERING")
}

func TestPlanHarness_IndexEqualityCovering(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE customer_id = 42",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_CUSTOMER, [=]")
	assertPlanContains(t, plan, "COVERING")
}

func TestPlanHarness_IndexEqualityNonCovering(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE customer_id = 42",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_CUSTOMER, [=]")
	assertPlanNotContains(t, plan, "COVERING")
}

func TestPlanHarness_IndexRangeSelectStar(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT * FROM orders WHERE amount > 9000",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan(IDX_AMOUNT,")
	assertPlanNotContains(t, plan, "COVERING")
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
	assertPlanContains(t, plan, "COVERING")
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
	assertPlanContains(t, plan, "IDX_STATUS")
	assertPlanContains(t, plan, "COVERING")
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
}

func TestPlanHarness_GroupBySumCompositeIndex(t *testing.T) {
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
	assertPlanContains(t, plan, "IDX_STATUS")
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
	assertPlanContains(t, plan, "IndexScan")
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
	assertPlanContains(t, plan, "IndexScan")
}

// --- Multi-table schemas ---

const multiTableSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE TABLE CUSTOMERS (
  id BIGINT NOT NULL,
  name STRING,
  region STRING,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE INDEX idx_status ON ORDERS(status)
CREATE INDEX idx_amount ON ORDERS(amount)
CREATE INDEX idx_region ON CUSTOMERS(region)
`

// --- EXISTS / NOT EXISTS ---

func TestPlanHarness_ExistsSubquery(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE EXISTS (SELECT 1 FROM customers WHERE customers.id = orders.customer_id)",
		multiTableSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

// --- DISTINCT ---

func TestPlanHarness_SelectDistinct(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT DISTINCT status FROM orders",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Distinct")
}

// --- Multi-column PK ---

func TestPlanHarness_CompositePK(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ITEMS (
  order_id BIGINT NOT NULL,
  item_num BIGINT NOT NULL,
  name STRING,
  PRIMARY KEY (order_id, item_num)
)
`
	plan, err := PlanQueryForTest(
		"SELECT name FROM items WHERE order_id = 1 AND item_num = 2",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ITEMS, [=, =])")
}

func TestPlanHarness_CompositePKPrefixScan(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ITEMS (
  order_id BIGINT NOT NULL,
  item_num BIGINT NOT NULL,
  name STRING,
  PRIMARY KEY (order_id, item_num)
)
`
	plan, err := PlanQueryForTest(
		"SELECT name FROM items WHERE order_id = 1",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ITEMS, [=])")
}

// --- Stats-driven plan changes ---

func TestPlanHarness_StatsAffectCost(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE EVENTS (
  id BIGINT NOT NULL,
  category STRING,
  PRIMARY KEY (id)
)
CREATE INDEX idx_category ON EVENTS(category)
`
	smallStats := properties.MapStatistics{PerType: map[string]float64{"EVENTS": 10}}
	largeStats := properties.MapStatistics{PerType: map[string]float64{"EVENTS": 10_000_000}}

	planSmall, err := PlanQueryForTest(
		"SELECT id FROM events ORDER BY category",
		schema, smallStats)
	if err != nil {
		t.Fatal(err)
	}
	planLarge, err := PlanQueryForTest(
		"SELECT id FROM events ORDER BY category",
		schema, largeStats)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("small table plan: %s", planSmall)
	t.Logf("large table plan: %s", planLarge)
	assertPlanContains(t, planSmall, "IDX_CATEGORY")
	assertPlanContains(t, planLarge, "IDX_CATEGORY")
}

func TestPlanHarness_GroupByHaving(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT customer_id, COUNT(*) FROM orders GROUP BY customer_id HAVING COUNT(*) >= 2 ORDER BY customer_id",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
}

func TestPlanHarness_FullScanSparseFilter(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE tier = 'platinum'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_TIER")
}

// --- UNION ---

func TestPlanHarness_UnionAll(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE status = 'a' UNION ALL SELECT id FROM orders WHERE status = 'b'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Union")
}

// --- Recursive CTE ---

func TestPlanHarness_RecursiveCTE(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE NODES (
  id BIGINT NOT NULL,
  parent_id BIGINT,
  name STRING,
  PRIMARY KEY (id)
)
`
	plan, err := PlanQueryForTest(
		"WITH RECURSIVE tree AS (SELECT id, name FROM nodes WHERE id = 1 UNION ALL SELECT n.id, n.name FROM nodes n, tree t WHERE n.parent_id = t.id) SELECT * FROM tree",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "RecursiveDfsJoin")
}

// --- LIKE prefix pushdown ---

func TestPlanHarness_LikePrefix(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE status LIKE 'pend%'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ORDERS)")
	// LIKE prefix pushdown to index is a future optimization.
	// Currently falls back to full scan + filter.
}

// --- Multiple WHERE predicates ---

func TestPlanHarness_MultiplePredicates(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE status = 'active' AND amount > 100",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IndexScan")
}

// --- ORDER BY with LIMIT ---

func TestPlanHarness_OrderByWithLimit(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders ORDER BY id LIMIT 10",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanNotContains(t, plan, "InMemorySort")
}

// --- Subquery in WHERE ---

func TestPlanHarness_FilterOnNonIndexColumn(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE tier = 'gold'",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_TIER")
}

// --- CROSS JOIN ---

func TestPlanHarness_CrossJoin(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE A (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE TABLE B (id BIGINT NOT NULL, PRIMARY KEY (id))
`
	plan, err := PlanQueryForTest(
		"SELECT a.id, b.id FROM a, b",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "NestedLoopJoin")
}

// --- COUNT(*) without WHERE ---

func TestPlanHarness_CountStarFullTable(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT COUNT(*) FROM orders",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
}

// --- BETWEEN ---

func TestPlanHarness_Between(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE amount BETWEEN 100 AND 200",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_AMOUNT")
}

// --- LEFT JOIN ---

func TestPlanHarness_LeftJoin(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT o.id, c.name FROM orders o LEFT JOIN customers c ON o.customer_id = c.id",
		multiTableSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

// --- NOT EXISTS ---

func TestPlanHarness_NotExists(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE NOT EXISTS (SELECT 1 FROM customers WHERE customers.id = orders.customer_id)",
		multiTableSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

// --- IS NULL ---

func TestPlanHarness_IsNull(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id FROM orders WHERE customer_id IS NULL",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ORDERS)")
}

// --- Multiple aggregates ---

func TestPlanHarness_MultipleAggregates(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT MIN(amount), MAX(amount), COUNT(*) FROM orders",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "StreamingAgg")
}

// --- Self-join ---

func TestPlanHarness_SelfJoin(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE EMPLOYEES (id BIGINT NOT NULL, manager_id BIGINT, name STRING, PRIMARY KEY (id))
`
	plan, err := PlanQueryForTest(
		"SELECT e.name, m.name FROM employees e, employees m WHERE e.manager_id = m.id",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

// --- CASE WHEN ---

func TestPlanHarness_CaseWhen(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, CASE WHEN amount > 1000 THEN 'high' ELSE 'low' END FROM orders",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ORDERS)")
}

// --- COALESCE ---

func TestPlanHarness_Coalesce(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, COALESCE(customer_id, 0) FROM orders",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "Scan(ORDERS)")
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
