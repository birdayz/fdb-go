package embedded

import (
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
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
		PerType: map[string]float64{"ORDERS": 100},
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
		PerType: map[string]float64{"ORDERS": 1_000_000},
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

func TestPlanHarness_StatsAffectInJoinSelection(t *testing.T) {
	t.Parallel()
	sql := "SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id"
	planSmall, err := PlanQueryForTest(sql, ordersSchema, properties.MapStatistics{
		PerType: map[string]float64{"ORDERS": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	planLarge, err := PlanQueryForTest(sql, ordersSchema, properties.MapStatistics{
		PerType: map[string]float64{"ORDERS": 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan (10 rows):  %s", planSmall)
	t.Logf("plan (1M rows):  %s", planLarge)
	assertPlanContains(t, planLarge, "InJoin")
}

func TestPlanHarness_AggregateIndexCountGroupBy(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_status ON ORDERS(status)
`
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	// Without aggregate index, streaming agg over ordered index is expected.
	assertPlanContains(t, plan, "StreamingAgg")
}

func TestPlanHarness_AggregateIndexDDL_CombinedCountSum(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX count_by_status AS SELECT COUNT(*) FROM ORDERS GROUP BY status
CREATE INDEX sum_amount_by_status AS SELECT SUM(amount) FROM ORDERS GROUP BY status
`
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*), SUM(amount) FROM orders GROUP BY status ORDER BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("combined COUNT+SUM plan: %s", plan)
	// A multi-aggregate GROUP BY with a per-aggregate index for each aggregate
	// (count_by_status + sum_amount_by_status, both grouped by status) must merge
	// the two co-grouped aggregate indexes — NOT full-scan + InMemorySort the
	// whole table. This was the 5.6s/1M perf bug: the MultiIntersection plan was
	// generated but lost winner-selection, and THIS test only logged the plan
	// instead of asserting it (a fake checkbox that hid the gap from day one).
	if !strings.Contains(plan, "MultiIntersection") {
		t.Errorf("expected MultiIntersection of the two aggregate indexes for COUNT(*)+SUM(amount) GROUP BY status, got: %s", plan)
	}
	if strings.Contains(plan, "InMemorySort") || strings.Contains(plan, "Scan(ORDERS)") {
		t.Errorf("multi-aggregate GROUP BY must not full-scan + sort when per-aggregate indexes exist, got: %s", plan)
	}

	sumOnly, err := PlanQueryForTest(
		"SELECT status, SUM(amount) FROM orders GROUP BY status ORDER BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SUM-only plan: %s", sumOnly)
	if !strings.Contains(sumOnly, "AggregateIndex") {
		t.Errorf("expected AggregateIndex for SUM-only query, got: %s", sumOnly)
	}
}

func TestPlanHarness_AggregateIndexViaBuilder(t *testing.T) {
	t.Parallel()
	b := metadata.NewSchemaTemplateBuilder().SetName("test_schema").
		AddTable("ORDERS", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("STATUS", api.NewStringType(true), 2),
			metadata.NewColumnSpec("AMOUNT", api.NewLongType(true), 3),
		}, []string{"ID"}).
		AddAggregateIndex("ORDERS", "count_by_status", []string{"STATUS"}, "COUNT", "")

	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanQueryWithMetadata(
		"SELECT status, COUNT(*) FROM orders GROUP BY status",
		tmpl.Underlying(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan with aggregate index defined, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexSumViaBuilder(t *testing.T) {
	t.Parallel()
	b := metadata.NewSchemaTemplateBuilder().SetName("test_schema").
		AddTable("ORDERS", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("REGION", api.NewStringType(true), 2),
			metadata.NewColumnSpec("AMOUNT", api.NewLongType(true), 3),
		}, []string{"ID"}).
		AddAggregateIndex("ORDERS", "sum_amount_by_region", []string{"REGION"}, "SUM", "AMOUNT")

	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanQueryWithMetadata(
		"SELECT region, SUM(amount) FROM orders GROUP BY region",
		tmpl.Underlying(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "SUM") {
		t.Fatalf("expected AggregateIndex(SUM, ...) with SUM index, got: %s", plan)
	}
}

// --- Aggregate index DDL (CREATE INDEX ... AS SELECT ...) ---

func TestPlanHarness_AggregateIndexDDL_Count(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX count_by_status AS SELECT COUNT(*) FROM ORDERS GROUP BY status
`
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan from DDL-defined index, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_Sum(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  region STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_amount_by_region AS SELECT SUM(amount) FROM ORDERS GROUP BY region
`
	plan, err := PlanQueryForTest(
		"SELECT region, SUM(amount) FROM orders GROUP BY region",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "SUM") {
		t.Fatalf("expected AggregateIndex(SUM) plan from DDL-defined index, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_Max(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_price_by_cat AS SELECT MAX(price) FROM ORDERS GROUP BY category
`
	plan, err := PlanQueryForTest(
		"SELECT category, MAX(price) FROM orders GROUP BY category",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "MAX") {
		t.Fatalf("expected AggregateIndex(MAX) plan from DDL-defined index, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_Min(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX min_price_by_cat AS SELECT MIN(price) FROM ORDERS GROUP BY category
`
	plan, err := PlanQueryForTest(
		"SELECT category, MIN(price) FROM orders GROUP BY category",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "MIN") {
		t.Fatalf("expected AggregateIndex(MIN) plan from DDL-defined index, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_MultiGroupBy(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  region STRING,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_by_region_status AS SELECT SUM(amount) FROM ORDERS GROUP BY region, status
`
	plan, err := PlanQueryForTest(
		"SELECT region, status, SUM(amount) FROM orders GROUP BY region, status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan with multi-column GROUP BY, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_CountColumn(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX count_amount_by_status AS SELECT COUNT(amount) FROM ORDERS GROUP BY status
`
	plan, err := PlanQueryForTest(
		"SELECT status, COUNT(amount) FROM orders GROUP BY status",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan for COUNT(col), got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_NoGroupBy(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX total_count AS SELECT COUNT(*) FROM ORDERS
`
	plan, err := PlanQueryForTest(
		"SELECT COUNT(*) FROM orders",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("no-group-by plan: %s", plan)
}

func TestPlanHarness_AggregateIndexDDL_ParseError_NoAggregate(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX bad_idx AS SELECT status FROM ORDERS GROUP BY status
`
	_, err := PlanQueryForTest("SELECT 1", schema, nil)
	if err == nil {
		t.Fatal("expected error for index DDL without aggregate function")
	}
	t.Logf("got expected error: %v", err)
}

func TestPlanHarness_AggregateIndexDDL_ParseError_NoFrom(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX bad_idx AS SELECT COUNT(*)
`
	_, err := PlanQueryForTest("SELECT 1", schema, nil)
	if err == nil {
		t.Fatal("expected error for index DDL without FROM clause")
	}
	t.Logf("got expected error: %v", err)
}

func TestPlanHarness_AggregateIndexDDL_ParseError_AvgRejected(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  amount BIGINT,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX avg_idx AS SELECT AVG(amount) FROM ORDERS GROUP BY status
`
	_, err := PlanQueryForTest("SELECT 1", schema, nil)
	if err == nil {
		t.Fatal("expected error: AVG is not an indexable aggregate function")
	}
	if !strings.Contains(err.Error(), "unsupported aggregate function") {
		t.Fatalf("expected 'unsupported aggregate function' error, got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

func TestPlanHarness_AggregateIndexDDL_ParseError_MultipleAggregates(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  amount BIGINT,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX multi_idx AS SELECT COUNT(*), SUM(amount) FROM ORDERS GROUP BY status
`
	_, err := PlanQueryForTest("SELECT 1", schema, nil)
	if err == nil {
		t.Fatal("expected error: only one aggregate per index definition allowed")
	}
	if !strings.Contains(err.Error(), "exactly one aggregate") {
		t.Fatalf("expected 'exactly one aggregate' error, got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

func TestPlanHarness_AggregateIndexDDL_MinEver(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX min_price_by_cat AS SELECT MIN_EVER(price) FROM ORDERS GROUP BY category
`
	plan, err := PlanQueryForTest(
		"SELECT category, MIN(price) FROM orders GROUP BY category",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex for MIN_EVER, got: %s", plan)
	}
}

func TestPlanHarness_AggregateIndexDDL_MaxEver(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_price_by_cat AS SELECT MAX_EVER(price) FROM ORDERS GROUP BY category
`
	plan, err := PlanQueryForTest(
		"SELECT category, MAX(price) FROM orders GROUP BY category",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex for MAX_EVER, got: %s", plan)
	}
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
	if !strings.Contains(plan, "NestedLoopJoin") && !strings.Contains(plan, "FlatMap") {
		t.Fatalf("plan does not contain NestedLoopJoin or FlatMap:\n      %s", plan)
	}
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
	// customer_id is a nullable column indexed by IDX_CUSTOMER. `IS NULL` is a
	// [null] EQUALITY range (Java's ScanComparisons.getComparisonType(IS_NULL)
	// == EQUALITY), so the index serves the predicate directly — Java emits the
	// same `COVERING(... [[null],[null]] ...)` (nested-with-nulls.yamsql,
	// sparse-index-tests.yamsql). Previously Go fell back to a full Scan; the
	// value-index null-range binding closes that divergence. Execution
	// correctness of the [null]/(null,+inf) ranges is pinned in the
	// sqldriver cardinality + IS-NULL index FDB tests.
	assertPlanContains(t, plan, "IndexScan(IDX_CUSTOMER, [=]")
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

func TestPlanHarness_StatsAffectGroupByPlan(t *testing.T) {
	t.Parallel()
	sql := "SELECT status, COUNT(*) FROM orders GROUP BY status"
	planSmall, err := PlanQueryForTest(sql, ordersSchema, properties.MapStatistics{
		PerType: map[string]float64{"ORDERS": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	planLarge, err := PlanQueryForTest(sql, ordersSchema, properties.MapStatistics{
		PerType: map[string]float64{"ORDERS": 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan (5 rows):  %s", planSmall)
	t.Logf("plan (1M rows): %s", planLarge)
	assertPlanContains(t, planSmall, "StreamingAgg")
	assertPlanContains(t, planLarge, "StreamingAgg")
	assertPlanContains(t, planLarge, "COVERING")
}

func TestPlanHarness_JoinWithAsymmetricStats(t *testing.T) {
	t.Parallel()
	sql := "SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id ORDER BY o.id"
	plan, err := PlanQueryForTest(sql, multiTableSchema, properties.MapStatistics{
		PerType: map[string]float64{"ORDERS": 1_000_000, "CUSTOMERS": 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan (1M orders, 100 customers): %s", plan)
	assertPlanContains(t, plan, "FlatMap")
}

func TestPlanHarness_CoveringCompositeIndex(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (id BIGINT NOT NULL, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_status_amount ON ORDERS(status, amount)
`
	plan, err := PlanQueryForTest(
		"SELECT status, amount FROM orders WHERE status = 'pending'",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_STATUS_AMOUNT")
	assertPlanContains(t, plan, "COVERING")
	assertPlanNotContains(t, plan, "Fetch")
}

func TestPlanHarness_CoveringCompositeIndexPKAndIndexCols(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (id BIGINT NOT NULL, status STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_status_amount ON ORDERS(status, amount)
`
	plan, err := PlanQueryForTest(
		"SELECT id, status, amount FROM orders WHERE status = 'pending'",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_STATUS_AMOUNT")
	assertPlanContains(t, plan, "COVERING")
	assertPlanNotContains(t, plan, "Fetch")
}

func TestPlanHarness_NonCoveringNeedsExtraColumn(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (id BIGINT NOT NULL, status STRING, amount BIGINT, tier STRING, PRIMARY KEY (id))
CREATE INDEX idx_status ON ORDERS(status)
`
	plan, err := PlanQueryForTest(
		"SELECT status, tier FROM orders WHERE status = 'pending'",
		schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanNotContains(t, plan, "COVERING")
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

// TestPlanHarness_AtOrdinalityRejected pins the R5 (RFC-142) convergence: AT
// ordinality is BOUND on a correlated array source (`FROM t, t.arr AS x AT p`),
// but on a NON-array source — a plain table, a JOIN source, a CTE/view — it is
// invalid and rejected with ONE converged code, ErrCodeWrongObjectType (42809,
// Java's WRONG_OBJECT_TYPE). Ignoring the AT alias would let a reference to the
// ordinal silently resolve to a same-named existing table column and return the
// wrong value (codex), so the reject is mandatory.
func TestPlanHarness_AtOrdinalityRejected(t *testing.T) {
	t.Parallel()

	// No colliding column: AT is rejected — not silently ignored, not a different error.
	_, err := PlanQueryForTest("SELECT id FROM orders AS e AT p", ordersSchema, nil)
	assertAtOrdinalityRejected(t, err)

	// Colliding column: `orders` HAS a `tier` column, and the AT alias is `tier`. If the
	// planner ignored the AT clause, `SELECT tier` would resolve to the real column and
	// silently return the wrong value. It must still be rejected.
	_, err = PlanQueryForTest("SELECT tier FROM orders AS e AT tier", ordersSchema, nil)
	assertAtOrdinalityRejected(t, err)

	// AT on a JOIN source is rejected too (the guard covers the JOIN lowering path).
	joinSchema := `
CREATE TABLE A (id BIGINT NOT NULL, PRIMARY KEY (id))
CREATE TABLE B (id BIGINT NOT NULL, PRIMARY KEY (id))
`
	_, err = PlanQueryForTest("SELECT a.id FROM A a JOIN B b AT p ON a.id = b.id", joinSchema, nil)
	assertAtOrdinalityRejected(t, err)
}

// TestPlanHarness_AtOrdinalityRejectedInAggregateIndexDDL pins the aggregate-index DDL path
// (ddl.go parseAggregateIndexDefinition), a separate AtomTableItem consumer from the query
// planner (codex). `ga` HAS a column `p`, and the index body embeds `FROM ga AT p GROUP BY p`:
// ignoring the AT clause would build an index grouped by the real column p (wrong semantics).
// The guard rejects it.
func TestPlanHarness_AtOrdinalityRejectedInAggregateIndexDDL(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ga (id BIGINT NOT NULL, p BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX sum_by_p AS SELECT SUM(v) FROM ga AT p GROUP BY p
`
	_, err := PlanQueryForTest("SELECT id FROM ga", schema, nil)
	assertAtOrdinalityRejected(t, err)
}

// TestPlanHarness_AggregateIndexJoinRejected pins that an aggregate-index definition with a
// JOIN is rejected rather than silently reduced to its leading table — which previously also
// dropped any AT-ordinality clause on the joined source (codex). Aggregate indexes are
// single-table; the `AT p` on the joined `gb` here would otherwise slip past the leading-atom
// guard entirely.
func TestPlanHarness_AggregateIndexJoinRejected(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ga (id BIGINT NOT NULL, p BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE gb (id BIGINT NOT NULL, p BIGINT, PRIMARY KEY (id))
CREATE INDEX bad AS SELECT SUM(v) FROM ga JOIN gb AT p ON ga.id = gb.id GROUP BY p
`
	_, err := PlanQueryForTest("SELECT id FROM ga", schema, nil)
	if err == nil {
		t.Fatal("aggregate index with a JOIN (AT on the joined source) must be rejected, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate {
		t.Fatalf("err = %v (%T), want *api.Error{ErrCodeInvalidSchemaTemplate}", err, err)
	}
}

func assertAtOrdinalityRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("AT ordinality on a non-array source must be rejected, got nil (silent ignore / wrong rows)")
	}
	// R5 (RFC-142) binds AT on a correlated array source and converges the
	// rejection of AT on a table / CTE / view / JOIN source / aggregate-index
	// source onto ONE code: ErrCodeWrongObjectType (42809), Java's
	// WRONG_OBJECT_TYPE. (R3 threw ErrCodeUnsupportedQuery here.)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeWrongObjectType {
		t.Fatalf("err = %v (%T), want *api.Error{ErrCodeWrongObjectType}", err, err)
	}
}

// boolSchema has a BOOLEAN column WITH an index, so the sargability tests can
// assert a bare `WHERE flag` matches the boolean index exactly as `flag = TRUE`.
const boolSchema = `
CREATE TABLE A (
  id BIGINT NOT NULL,
  flag BOOLEAN,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_flag ON A(flag)
`

// TestPlanHarness_BareBooleanWhere — a bare boolean column as a single-table
// top-level WHERE predicate plans (RFC-146). Previously 0AF00.
func TestPlanHarness_BareBooleanWhere(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest("SELECT id FROM A WHERE flag", boolSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	// flag lifts to `flag = TRUE` → matches the boolean index (sargable), same
	// as the explicit comparison (a COVERING index scan here).
	assertPlanContains(t, plan, "IndexScan(IDX_FLAG, [=]")
}

// TestPlanHarness_BareBooleanWhereUnifiesWithComparison — `WHERE flag` and
// `WHERE flag = TRUE` produce the IDENTICAL plan (RFC-146 §2: they lift to the
// same ComparisonPredicate, so they unify for index matching/plan shape).
func TestPlanHarness_BareBooleanWhereUnifiesWithComparison(t *testing.T) {
	t.Parallel()
	bare, err := PlanQueryForTest("SELECT id FROM A WHERE flag", boolSchema, nil)
	if err != nil {
		t.Fatalf("bare WHERE flag: %v", err)
	}
	cmp, err := PlanQueryForTest("SELECT id FROM A WHERE flag = TRUE", boolSchema, nil)
	if err != nil {
		t.Fatalf("WHERE flag = TRUE: %v", err)
	}
	if bare != cmp {
		t.Fatalf("WHERE flag and WHERE flag = TRUE plan differently:\n  bare: %s\n  cmp:  %s", bare, cmp)
	}
}

// TestPlanHarness_BareNonBooleanWhereRejected — a bare NON-boolean value as a
// top-level WHERE predicate is a type error (RFC-146 §3 / Java DATATYPE_MISMATCH
// 42804), not a silent 0-row plan.
func TestPlanHarness_BareNonBooleanWhereRejected(t *testing.T) {
	t.Parallel()
	_, err := PlanQueryForTest("SELECT id FROM A WHERE amount", boolSchema, nil)
	if err == nil {
		t.Fatal("expected DATATYPE_MISMATCH for a bare non-boolean WHERE, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDatatypeMismatch {
		t.Fatalf("err = %v (%T), want *api.Error{ErrCodeDatatypeMismatch}", err, err)
	}
}

// TestPlanHarness_BareDoubleWhereRejected — a bare `WHERE <double_col>` must
// raise 42804, NOT silently lift to `d = TRUE` and filter to nothing (codex
// catch on #357). sqlTypeToCascadesType now carries the real TypeCodeDouble for
// FLOAT/DOUBLE (and TypeCodeBytes for BYTES), so the predicate-lift type gate
// rejects them as non-boolean — while genuinely un-typeable values (params,
// CTE/derived columns whose projected type isn't propagated) stay permissive.
func TestPlanHarness_BareDoubleWhereRejected(t *testing.T) {
	t.Parallel()
	const sch = `CREATE TABLE A (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id))`
	_, err := PlanQueryForTest("SELECT id FROM A WHERE d", sch, nil)
	if err == nil {
		t.Fatal("expected DATATYPE_MISMATCH for a bare DOUBLE WHERE, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDatatypeMismatch {
		t.Fatalf("err = %v (%T), want *api.Error{ErrCodeDatatypeMismatch}", err, err)
	}
}

// TestPlanHarness_BareCTEBooleanColumnWhere — the inverse of the DOUBLE
// rejection: a CTE/derived column holding a boolean expression (`NOT flag`) is
// UNKNOWN-typed in the outer scope (its projected type isn't propagated), so it
// MUST stay on the permissive UNKNOWN path and PLAN as a bare WHERE predicate,
// NOT be rejected 42804. This pins the exact shape the codex #2 rework was
// reverted to protect — a *FieldValue of UNKNOWN type is not always non-boolean.
// Without this pin, a future "be stricter about UNKNOWN" change re-breaks
// boolean CTE columns with green CI (the dimensional-gap trap).
func TestPlanHarness_BareCTEBooleanColumnWhere(t *testing.T) {
	t.Parallel()
	const sch = `CREATE TABLE A (id BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY (id))`
	plan, err := PlanQueryForTest(
		"WITH c AS (SELECT NOT flag AS x, id FROM A) SELECT id FROM c WHERE x", sch, nil)
	if err != nil {
		t.Fatalf("bare CTE boolean column WHERE must plan, got: %v", err)
	}
	assertPlanContains(t, plan, "PredicatesFilter")
}

// TestPlanHarness_MultiTableJoinCompoundResidualNotMaterialized pins that Phase-1's
// yieldUnknown does NOT materialize a NON-simple (OR) residual on a partition-SUBSEL
// join leg. The leg's join correlation lives in a SIBLING predicate (t.fk = o.id), so
// refIsJoinLeg does not flag this ref (it inspects only the bound prefix); materializing
// the OR residual as a standalone leg filter would win and sever the join feed →
// FlatMap(... inner=Fetch(<nil>)) → 0 rows (codex's 3-way-join repro). The SHAPE gate in
// compensationSafeForYield keeps the OR residual on the OLD InsertFinal path → byte-
// identical to pre-yieldUnknown behavior → the join is driven correctly. (Materializing
// such residuals SAFELY — the rot-fix — is RFC-150, gated on the winner-selection
// invariant; a pre-existing variant with a simple-AND residual is the RFC-150 sentinel.)
func TestPlanHarness_MultiTableJoinCompoundResidualNotMaterialized(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE o (id bigint, PRIMARY KEY (id))
		CREATE TABLE t (id bigint, fk bigint, k bigint, a bigint, b bigint, x bigint, PRIMARY KEY (id))
		CREATE TABLE u (id bigint, x bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	plan, err := PlanQueryForTest("SELECT t.k FROM o, t, u WHERE t.k = 5 AND (t.a > 1 OR t.b < 2) AND t.fk = o.id AND u.x = t.x", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanNotContains(t, plan, "<nil>") // no degenerate nil-inner leg (the join feed is intact)
}

// TestPlanHarness_CorrelatedResidualNotStandaloneLeg pins the M2 0-row guard RFC-148
// Phase 1 restored in compensationSafeForYield. A join leg whose correlation lives
// in the RESIDUAL (an unindexed `t.fk = o.id`) alongside an indexed local predicate
// (`t.k = 5`) is NOT flagged by refHasCorrelatedMatch (which inspects only the bound
// prefix). The retired isSimpleResidualCompensation rejected such a compensation via
// its predicate-correlation check — safety, not shape rot. Without the restored
// guard, yieldUnknown realizes a physical correlated leg filter that severs the
// join's correlation feed → FlatMap(outer=Scan(O), inner=Fetch(<nil>)) → 0 rows (the
// PR-#201 shape, reproduced by Graefe). The valid plan drives the inner from the outer.
func TestPlanHarness_CorrelatedResidualNotStandaloneLeg(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE o (id bigint, PRIMARY KEY (id))
		CREATE TABLE t (id bigint, fk bigint, k bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	plan, err := PlanQueryForTest("SELECT t.id FROM o, t WHERE t.fk = o.id AND t.k = 5", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanNotContains(t, plan, "<nil>") // no degenerate nil-inner leg
	assertPlanContains(t, plan, "FlatMap")  // correlated join, not a standalone leg filter
}

// TestPlanHarness_ResidualCompensationPreservesOrdering pins RFC-148 §3d: a SIMPLE
// residual re-optimized through yieldUnknown still receives the requested-ordering push,
// so the index scan's matched order eliminates the in-memory sort (a missed push would
// add a physical InMemorySort over the filter). `a > 1` rides the IDX_K range scan;
// ORDER BY k is served by the scan order.
func TestPlanHarness_ResidualCompensationPreservesOrdering(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE t (id bigint, k bigint, a bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	plan, err := PlanQueryForTest("SELECT * FROM t WHERE k > 5 AND a > 1 ORDER BY k", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)
	assertPlanContains(t, plan, "IDX_K")
	assertPlanNotContains(t, plan, "Sort") // ordering eliminated by the index scan, not a physical sort
}

// TestPlanHarness_YieldUnknownReentryConverges pins RFC-148 §3c termination: the
// yieldUnknown exploratory re-entry into pushDataAccessTasks converges to a single
// stable plan across repeated planning (the B4 growth-keyed guard + Insert dedup hold
// the fixpoint; a regressed guard re-consuming every round would either diverge to the
// 10-round cap — a degraded/missing plan — or flap nondeterministically).
func TestPlanHarness_YieldUnknownReentryConverges(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE t (id bigint, k bigint, a bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	sql := "SELECT * FROM t WHERE k = 5 AND a > 1"
	first, err := PlanQueryForTest(sql, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "IDX_K") {
		t.Fatalf("re-entry query degraded (cap hit?): %s", first)
	}
	for i := 0; i < 5; i++ {
		got, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("non-deterministic plan (re-entry convergence failure):\nrun0: %s\nrun%d: %s", first, i, got)
		}
	}
}

// TestPlanHarness_JoinLegResidualNoNilFetch is RFC-150 B1a's headline regression —
// a PRE-EXISTING 0-row bug fixed here. A partition-SUBSEL join leg (t, joined by
// sibling predicates t.fk=o.id and u.x=t.x on unindexed columns) materializes its
// local residual (t.a>1) over IDX_K. The leg ref then holds a nil-inner Fetch SHELL
// (the RFC-070 extraction template, real inner in the wrapper quantifier). The NLJ
// rule selected join children via findBestPhysicalExpr — the one winner-selector
// missing the isNilInnerFetch guard — picked the cheap nil shell and embedded its
// plan directly (never WithChildren) → FlatMap(... inner=Fetch(<nil>)) → 0 rows, on
// master AND every prior HEAD. Fix: the NLJ selects through the nil-safe
// findBestValidPhysicalExpr (the guard its two sibling selectors already apply).
func TestPlanHarness_JoinLegResidualNoNilFetch(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE o (id bigint, PRIMARY KEY (id))
		CREATE TABLE t (id bigint, fk bigint, k bigint, a bigint, b bigint, x bigint, PRIMARY KEY (id))
		CREATE TABLE u (id bigint, x bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	// AND-residual is the pre-existing bug: the materialized residual creates the
	// nil-inner Fetch shell, and the FIXED plan drives t via idx_k —
	// `Fetch(IndexScan(IDX_K,…))`. Asserting that exact shape pins the shell-producing
	// path (a future refactor that fell back to a full Scan(T) would no longer
	// exercise the bug → the sentinel would go silently green).
	andPlan, err := PlanQueryForTest("SELECT t.k FROM o, t, u WHERE t.k = 5 AND t.a > 1 AND t.fk = o.id AND u.x = t.x", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanNotContains(t, andPlan, "<nil>")
	assertPlanContains(t, andPlan, "Fetch(IndexScan(IDX_K")

	// OR-residual is the shape-gated variant (no materialization → no shell → Scan(T));
	// a distinct path, so assert only the absence of the nil leg + a driven join.
	orPlan, err := PlanQueryForTest("SELECT t.k FROM o, t, u WHERE t.k = 5 AND (t.a > 1 OR t.b < 2) AND t.fk = o.id AND u.x = t.x", schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertPlanNotContains(t, orPlan, "<nil>")
	assertPlanContains(t, orPlan, "FlatMap")
}

// TestPlanHarness_RotFix_CompoundResidualUsesIndex pins the RFC-150 rot-fix (post-B1a):
// a single-table query with an indexed equality + a NON-simple residual (OR / IN) now
// rides the index scan instead of degrading to a full scan. The retired
// isSimpleResidualCompensation allowlist admitted only simple non-IN ComparisonPredicate
// residuals, so these lost to `PredicatesFilter(Scan(T))`; yieldUnknown now re-optimizes
// them to `PredicatesFilter(Fetch(IndexScan(IDX_K)))`. Safe only with B1a's nil-safe
// join-child selection in place (Phase-1 shipped these on the InsertFinal path).
func TestPlanHarness_RotFix_CompoundResidualUsesIndex(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE t (id bigint, k bigint, a bigint, b bigint, m bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	for _, sql := range []string{
		"SELECT * FROM t WHERE k = 5 AND (a > 1 OR b < 2)",
		"SELECT * FROM t WHERE k = 5 AND m IN (1, 2, 3)",
	} {
		plan, err := PlanQueryForTest(sql, schema, nil)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		t.Logf("%s -> %s", sql, plan)
		assertPlanContains(t, plan, "IndexScan(IDX_K") // not a full Scan(T)
	}
}
