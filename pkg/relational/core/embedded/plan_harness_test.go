package embedded

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"

	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

const ordersSchema = `
CREATE TABLE ORDERS (
  id BIGINT,
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
CREATE TABLE ORDERS (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))
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
CREATE TABLE ORDERS (id BIGINT, customer_id BIGINT, PRIMARY KEY (id))
CREATE TABLE CUSTOMERS (id BIGINT, name STRING, PRIMARY KEY (id))
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

// TestPlanHarness_InList pins that an ORDERED IN-list query is answered WITHOUT
// an in-memory sort.
//
// It asserted `InJoin` as a substring. That assertion could not express the
// property that matters, and it actively misled: an InJoin over IDX_CUSTOMER
// (entries (customer_id, id), PK id) concatenates per-binding scans and yields
// rows ordered by (customer_id, id), which does NOT satisfy ORDER BY id. Reading
// the substring, two people in one session concluded the plan was
// wrong-ordered. It was not — the full plan at merge-base 789b29ea8 was
//
//	Project([ID#0, AMOUNT#3], InMemorySort([ID ASC], Fetch(InJoin(IndexScan(IDX_CUSTOMER, [=]), binding ASC))))
//
// with the ordering supplied by a sort the substring never mentioned. A
// plan-shape SUBSTRING cannot distinguish a correctly-ordered plan from an
// incorrectly-ordered one, which is why this now asserts the PROPERTY.
//
// The plan is now InUnion, which merges the per-binding streams on id and
// satisfies ORDER BY id directly — same rows, same order, one operator fewer.
// The improvement IS the sort's elimination, so that is what is asserted.
//
// ATTRIBUTED, not assumed. The change comes from RFC-220's access path emitting
// Fetch(Covering(IndexScan)) on every value-index match. MEASURED by reverting
// that one construction on the finished branch and replanning: the plan returns
// to the merge-base InMemorySort(Fetch(InJoin(...))) shape, and restoring it
// gives InUnion again. No other change on the branch moves it — enumeration in
// particular is excluded, measured byte-identical with it on and off.
//
// So this test pins a property that RFC-220 SUPPLIES: the covering alternative
// existing in the memo is what lets the IN-union form. It is not orthogonal to
// RFC-220 and it would regress if that access-path construction became
// conditional again. Note the final plan's leg is a bare (fetching) IndexScan —
// MergeFetchIntoCoveringIndexRule collapses it after the union is formed — so
// the covering scan's role here is to EXIST as an alternative, not to survive
// into the chosen plan. That is easy to misread from the plan string alone.
func TestPlanHarness_InList(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(
		"SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id",
		ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %s", plan)

	// The IN-list must still be answered by bounded per-value index reads, not
	// by a scan of the table.
	if !strings.Contains(plan, "InJoin(Map(IndexScan(IDX_CUSTOMER, [=])") {
		t.Fatalf("expected an IN-join over IDX_CUSTOMER probes, got: %s", plan)
	}
	// An IN-union merging the probes on id is NOT built: idx_customer's entries
	// are (customer_id, record type, id), and Java builds no in-union ordered by
	// an id it reaches only past that record-type coordinate (RFC-257 WS-F 4.3
	// item 2; Java scans the table here). So the probes' rows are sorted.
	if strings.Contains(plan, "InUnion") || !strings.HasPrefix(plan, "InMemorySort([_current.ID#0 ASC], InJoin(") {
		t.Fatalf("ORDER BY id over IN probes of a one-column index must be a sorted InJoin: %s", plan)
	}
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

// TestInListAccessPathIsInvariantUnderCardinality pins that the IN-list access
// path does NOT depend on table statistics — and it is named for what holds,
// because its predecessor was named for the opposite.
//
// It was TestPlanHarness_StatsAffectInJoinSelection. That test planned the query
// at 10 rows and at 1,000,000 rows, logged both, and then asserted a SUBSTRING
// on the large plan only. It never compared the two. MEASURED at merge-base
// 789b29ea8, the two plans it computed were byte-identical:
//
//	plan (10 rows):  Project([ID#0, AMOUNT#3], InMemorySort([ID ASC], Fetch(InJoin(IndexScan(IDX_CUSTOMER, [=]), binding ASC))))
//	plan (1M rows):  Project([ID#0, AMOUNT#3], InMemorySort([ID ASC], Fetch(InJoin(IndexScan(IDX_CUSTOMER, [=]), binding ASC))))
//
// So it was a test named for a decision being responsive, which never checked
// responsiveness, over a decision that is not responsive. It passed on a
// substring.
//
// JAVA IS THE SPEC AND AGREES THE CHOICE IS NOT STATISTICAL.
// ImplementInJoinRule and ImplementInUnionRule are both registered
// (PlanningRuleSet.java:151-162) and both gated only on
// RequestedOrderingConstraint.REQUESTED_ORDERING (ImplementInUnionRule.java:98,
// ImplementInJoinRule.java:98), returning early when no ordering is requested.
// Neither rule mentions cardinality or statistics anywhere — grep for
// Cardinalit|Statistic|RecordCount over both files returns ZERO. The only size
// input is attemptFailedInJoinAsUnionMaxSize (ImplementInUnionRule.java:170), a
// STATIC planner-configuration integer, and it is a runtime leg-count guard
// (RecordQueryInUnionPlan.java:152), not a planning-time statistic.
//
// So the choice is driven by the REQUESTED ORDERING and by which alternative can
// satisfy it — not by how big the table is. This test asserts that, and asserts
// it by COMPARING the two plans, which is the thing its predecessor computed and
// then threw away.
//
// If this ever goes red, a statistics input has entered the IN-list access-path
// choice. That may well be an improvement, but it is a DIVERGENCE from Java and
// must be argued as one — do not simply re-bless it.
//
// The shared plan is InUnion rather than the merge-base's
// InMemorySort(Fetch(InJoin(...))) because RFC-220's access path emits
// Fetch(Covering(IndexScan)), which is what lets the IN-union form (attributed
// by reverting that one construction and replanning — see TestPlanHarness_InList).
// The invariance asserted here holds on both shapes; only the shape moved.
func TestInListAccessPathIsInvariantUnderCardinality(t *testing.T) {
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

	if planSmall != planLarge {
		t.Fatalf("the IN-list access path became cardinality-SENSITIVE.\n"+
			"  10 rows: %s\n  1M rows: %s\n"+
			"Java's ImplementInJoinRule/ImplementInUnionRule consult no statistics "+
			"at all (zero Cardinalit|Statistic|RecordCount references in either), so "+
			"a stats-driven choice here is a divergence that must be argued, not "+
			"re-blessed.", planSmall, planLarge)
	}

	// Non-vacuity: the shared plan must actually be the IN-list access path. If
	// planning ever collapsed to something else entirely, the equality above
	// would still hold and assert nothing about IN-lists.
	if !strings.Contains(planLarge, "InUnion") && !strings.Contains(planLarge, "InJoin") {
		t.Fatalf("neither plan uses an IN-list access path, so the invariance "+
			"above is vacuous: %s", planLarge)
	}
}

func TestPlanHarness_AggregateIndexCountGroupBy(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
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
  id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX count_by_status AS SELECT COUNT(*) FROM ORDERS GROUP BY status
CREATE INDEX sum_amount_by_status AS SELECT SUM(amount) FROM ORDERS GROUP BY status
CREATE INDEX sum_amount_by_status_nn AS SELECT COUNT(amount) FROM ORDERS GROUP BY status
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
	if !strings.Contains(plan, "MultiIntersection(") {
		t.Errorf("expected the merge of the two aggregate indexes for COUNT(*)+SUM(amount) GROUP BY status, got: %s", plan)
	}
	if strings.Count(plan, "COUNT_BY_STATUS") > 1 {
		t.Errorf("the group-existence companion duplicated an aggregate leg — one index "+
			"scanned twice in a single merge, and group existence decided twice over. The "+
			"query's own COUNT(*) leg IS the existence stream and must be designated, not "+
			"copied. got: %s", plan)
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
		AddAggregateIndex("ORDERS", "sum_amount_by_region", []string{"REGION"}, "SUM", "AMOUNT").
		AddAggregateIndex("ORDERS", "cnt_amount_by_region", []string{"REGION"}, "COUNT_NOT_NULL", "AMOUNT")

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
  id BIGINT,
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
  id BIGINT,
  region STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_amount_by_region AS SELECT SUM(amount) FROM ORDERS GROUP BY region
CREATE INDEX sum_amount_by_region_nn AS SELECT COUNT(amount) FROM ORDERS GROUP BY region
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
  id BIGINT,
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

// A nonzero permutation moves grouping columns behind the extremum. The
// candidate must retain that physical-prefix boundary for safe scan bindings.
func TestAggregateIndexCandidate_NonzeroPermutedSize(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_price_by_cat AS SELECT MAX(price) FROM ORDERS GROUP BY category
`
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	var idx *recordlayer.Index
	for name, i := range md.GetAllIndexes() {
		if strings.EqualFold(name, "max_price_by_cat") {
			idx = i
			break
		}
	}
	if idx == nil {
		t.Fatalf("permuted index max_price_by_cat not found in built metadata (have %v)",
			func() []string {
				var names []string
				for n := range md.GetAllIndexes() {
					names = append(names, n)
				}
				return names
			}())
	}
	if idx.Type != recordlayer.IndexTypePermutedMax {
		t.Fatalf("index type = %q, want %q", idx.Type, recordlayer.IndexTypePermutedMax)
	}

	// Control — DDL-built permutedSize=0 must produce an aggregate candidate.
	if got := tryAggregateIndexCandidate(idx, md); got == nil {
		t.Fatal("permutedSize=0 (DDL-built) must produce an aggregate candidate — the guard over-rejects")
	}

	idx.Options[recordlayer.IndexOptionPermutedSize] = "1"
	if got := tryAggregateIndexCandidate(idx, md); got == nil || got.GetPhysicalGroupingPrefixCount() != 0 {
		t.Fatalf("permutedSize=1 must have a zero-length grouping prefix, got %v", got)
	}

	// The size is read with Integer.parseInt, as Java's
	// AggregateIndexMatchCandidate reads it: an Arabic-Indic zero is size 0, so
	// the index is a candidate (strconv.Atoi refused it and declined).
	idx.Options[recordlayer.IndexOptionPermutedSize] = "\u0660"
	if got := tryAggregateIndexCandidate(idx, md); got == nil {
		t.Fatal("permutedSize \"\\u0660\" is 0 to Integer.parseInt and must produce a candidate")
	}

	// A malformed permutedSize (unparseable) must also decline, not default open.
	idx.Options[recordlayer.IndexOptionPermutedSize] = "not-a-number"
	if got := tryAggregateIndexCandidate(idx, md); got != nil {
		t.Fatal("malformed permutedSize must DECLINE candidacy (fail-safe), got a candidate")
	}
}

func TestPlanHarness_AggregateIndexDDL_Min(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
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
  id BIGINT,
  region STRING,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_by_region_status AS SELECT SUM(amount) FROM ORDERS GROUP BY region, status
CREATE INDEX sum_by_region_status_nn AS SELECT COUNT(amount) FROM ORDERS GROUP BY region, status
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
  id BIGINT,
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
  id BIGINT,
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

func TestPlanHarness_GroupingOnlyIndexDDL(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX bad_idx AS SELECT status FROM ORDERS GROUP BY status
`
	plan, err := PlanQueryForTest("SELECT 1", schema, nil)
	if err != nil || !strings.Contains(plan, "Explode") {
		t.Fatalf("singleton over grouping-only index schema: plan=%s error=%v", plan, err)
	}
	// With no aggregate call Java emits a VALUE index over the grouping key.
	// Exercise that index, not merely the unrelated SELECT's admission.
	indexed, err := PlanQueryForTest("SELECT status FROM orders WHERE status = 'pending'", schema, nil)
	if err != nil || !strings.Contains(indexed, "IndexScan(BAD_IDX") {
		t.Fatalf("grouping-only index is not usable: plan=%s error=%v", indexed, err)
	}
}

func TestPlanHarness_AggregateIndexDDL_ParseError_NoFrom(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
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
  id BIGINT,
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
	// The Java 4.14.2.0 target: ProjectionResolver skips only an INDEXABLE
	// aggregate when it aligns the projection with the grouping, so a lone AVG
	// is compared with the grouping column and refused there, before
	// checkValidity's non-indexable message (measured: the WS-J oracle shape
	// avg_grouped).
	if !strings.Contains(err.Error(), "Aggregate result value does not align with grouping value") {
		t.Fatalf("expected the target's alignment refusal, got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

func TestPlanHarness_AggregateIndexDDL_ParseError_MultipleAggregates(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
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
	// Java: checkValidity (MaterializedViewIndexGenerator.java:619-623),
	// pinned by IndexTest.java:773-779.
	if !strings.Contains(err.Error(), "found group by expression with more than one aggregation") {
		t.Fatalf("expected 'more than one aggregation' error, got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

// TestPlanHarness_AggregateIndexDDL_MinMaxPermutedType pins the wire/metadata
// identity of plain SQL MAX(col)/MIN(col) aggregate indexes: they materialize
// as PERMUTED_MAX / PERMUTED_MIN with permuted size 0, matching Java's
// NumericAggregationValue.Max/Min.getIndexTypeName() and
// MaterializedViewIndexGenerator (permutedSize = aggregateOrderIndex < 0 ? 0).
// A monotone MAX_EVER_LONG / MIN_EVER_LONG index would go stale under deletes;
// the permuted index tracks the true current extremum, and — critically for a
// cluster shared with Java — is the identical index type Java writes for the
// same DDL text.
func TestPlanHarness_AggregateIndexDDL_MinMaxPermutedType(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_price_by_cat AS SELECT MAX(price) FROM ORDERS GROUP BY category
CREATE INDEX min_price_by_cat AS SELECT MIN(price) FROM ORDERS GROUP BY category
`
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		idxName  string
		wantType string
	}{
		{"MAX_PRICE_BY_CAT", recordlayer.IndexTypePermutedMax},
		{"MIN_PRICE_BY_CAT", recordlayer.IndexTypePermutedMin},
	} {
		idx := tmpl.Underlying().GetIndex(tc.idxName)
		if idx == nil {
			t.Fatalf("index %s not found in metadata", tc.idxName)
		}
		if idx.Type != tc.wantType {
			t.Errorf("index %s type = %q, want %q (NOT a monotone _EVER type)", tc.idxName, idx.Type, tc.wantType)
		}
		if got := idx.Options[recordlayer.IndexOptionPermutedSize]; got != "0" {
			t.Errorf("index %s permuted size = %q, want %q", tc.idxName, got, "0")
		}
	}
}

// TestPlanHarness_AggregateIndexDDL_MinEver verifies the SEPARATE min_ever()
// SQL function in a CREATE INDEX materializes a monotone MIN_EVER index — the
// wire/metadata identity for the _EVER function, kept distinct from plain
// MIN() (which maps to permuted_min). Go's read side does not expose min_ever()
// as a query aggregate, so there is no served-query shape to assert — only the
// DDL index type. A plain MIN() query is intentionally NOT served by this
// index (that would return stale extrema); it falls back to an aggregation over
// a scan.
func TestPlanHarness_AggregateIndexDDL_MinEver(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX min_price_by_cat AS SELECT MIN_EVER(price) FROM ORDERS GROUP BY category
`
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	idx := tmpl.Underlying().GetIndex("MIN_PRICE_BY_CAT")
	if idx == nil {
		t.Fatal("index MIN_PRICE_BY_CAT not found in metadata")
	}
	if idx.Type != recordlayer.IndexTypeMinEverTuple {
		t.Fatalf("MIN_EVER index type = %q, want %q", idx.Type, recordlayer.IndexTypeMinEverTuple)
	}
}

// TestPlanHarness_AggregateIndexDDL_MaxEver mirrors _MinEver for max_ever().
func TestPlanHarness_AggregateIndexDDL_MaxEver(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ORDERS (
  id BIGINT,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_price_by_cat AS SELECT MAX_EVER(price) FROM ORDERS GROUP BY category
`
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	idx := tmpl.Underlying().GetIndex("MAX_PRICE_BY_CAT")
	if idx == nil {
		t.Fatal("index MAX_PRICE_BY_CAT not found in metadata")
	}
	if idx.Type != recordlayer.IndexTypeMaxEverTuple {
		t.Fatalf("MAX_EVER index type = %q, want %q", idx.Type, recordlayer.IndexTypeMaxEverTuple)
	}
}

// everOnlySchema has ONLY monotone MAX_EVER / MIN_EVER indexes on price — no
// permuted / plain aggregate index. A plain SQL MAX()/MIN() query therefore has
// no legitimate aggregate index to match.
const everOnlySchema = `
CREATE TABLE ORDERS (
  id BIGINT,
  category STRING,
  price BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX max_ever_price AS SELECT MAX_EVER(price) FROM ORDERS GROUP BY category
CREATE INDEX min_ever_price AS SELECT MIN_EVER(price) FROM ORDERS GROUP BY category
`

// TestPlanHarness_PlainMaxMinOverEverIndex_FallsBackToBaseAgg pins that a plain
// SQL MAX()/MIN() query is NOT served by scanning an explicit monotone MAX_EVER /
// MIN_EVER index. Those indexes store running extrema maintained by atomic
// mutations, not per-record values; scanning one as an ordinary VALUE index
// (StreamingAgg over IndexScan) reads stale data that deletes never lower. With
// no permuted/plain aggregate index present, the query must fall back to an
// aggregation over the base-record scan — mirroring Java, whose
// AtomicMutationIndexMaintainerFactory never produces a value-scan candidate.
//
// Revert-proof: removing the IsAtomicMutationIndex candidacy filter in
// cascades_generator makes the _EVER index a value candidate again, and the plan
// regresses to StreamingAgg(IndexScan(<_EVER index>, COVERING)) — the assertion
// below then fails.
func TestPlanHarness_PlainMaxMinOverEverIndex_FallsBackToBaseAgg(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		agg   string
		query string
	}{
		{"MAX", "SELECT category, MAX(price) FROM orders GROUP BY category"},
		{"MIN", "SELECT category, MIN(price) FROM orders GROUP BY category"},
	} {
		tc := tc
		t.Run(tc.agg, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.query, everOnlySchema, nil)
			if err != nil {
				t.Fatalf("plan %s: %v", tc.agg, err)
			}
			t.Logf("%s plan: %s", tc.agg, plan)
			// The only indexes are the monotone _EVER indexes; any IndexScan means
			// the planner scanned one as a value source (the F35 bug).
			assertPlanNotContains(t, plan, "IndexScan")
			// Correct shape: aggregate over a base-record scan.
			assertPlanContains(t, plan, "StreamingAgg")
			assertPlanContains(t, plan, "Scan(ORDERS")
		})
	}
}

// --- Multi-table schemas ---

const multiTableSchema = `
CREATE TABLE ORDERS (
  id BIGINT,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE TABLE CUSTOMERS (
  id BIGINT,
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

func TestPlanHarness_CorrelatedExistsAggregatePagination(t *testing.T) {
	t.Parallel()

	t.Run("known_false_positive", func(t *testing.T) {
		plan, err := PlanQueryForTest(
			"SELECT id FROM orders WHERE EXISTS ("+
				"SELECT COUNT(*) FROM customers WHERE customers.id = orders.customer_id LIMIT 1 OFFSET 1)",
			multiTableSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertPlanNotContains(t, plan, "FlatMap")
	})

	t.Run("known_false_negated", func(t *testing.T) {
		plan, err := PlanQueryForTest(
			"SELECT id FROM orders WHERE NOT EXISTS ("+
				"SELECT COUNT(*) FROM customers WHERE customers.id = orders.customer_id LIMIT 1 OFFSET 1)",
			multiTableSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertPlanNotContains(t, plan, "FlatMap")
	})

	// translateProject has a widened projection fold for an arity>=3 gathered
	// cluster. That early-return path bypasses translateFilter, so both known
	// cardinalities must already have removed the existential before it can build
	// the gathered EXISTS wrap. FlatMap is the load-bearing negative sentinel:
	// before this regression, both queries raw-semi-joined the inner rows here.
	for _, test := range []struct {
		name       string
		pagination string
	}{
		{name: "arity_three_known_false", pagination: " LIMIT 1 OFFSET 1"},
		{name: "arity_three_known_true"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(
				"SELECT orders.id FROM orders, customers AS c1, customers AS c2 WHERE EXISTS ("+
					"SELECT COUNT(*) FROM customers AS inner_c "+
					"WHERE inner_c.id = orders.customer_id"+test.pagination+
					") ORDER BY orders.id",
				multiTableSchema, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertPlanNotContains(t, plan, "FlatMap")
		})
	}

	t.Run("group_key_only_is_not_global_aggregate", func(t *testing.T) {
		plan, err := PlanQueryForTest(
			"SELECT id FROM orders WHERE EXISTS ("+
				"SELECT region FROM customers WHERE customers.id = orders.customer_id GROUP BY region LIMIT 1)",
			multiTableSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertPlanContains(t, plan, "FlatMap")
	})

	for _, test := range []struct {
		name string
		sql  string
	}{
		{
			name: "grouped_offset",
			sql: "SELECT id FROM orders WHERE EXISTS (" +
				"SELECT COUNT(*) FROM customers WHERE customers.id = orders.customer_id " +
				"GROUP BY region LIMIT 1 OFFSET 1)",
		},
		{
			name: "planning_time_unresolved_limit",
			sql: "SELECT id FROM orders WHERE EXISTS (" +
				"SELECT COUNT(*) FROM customers WHERE customers.id = orders.customer_id LIMIT ?)",
		},
		{
			name: "projected_known_truth",
			sql: "SELECT id, EXISTS (" +
				"SELECT COUNT(*) FROM customers WHERE customers.id = orders.customer_id LIMIT 1 OFFSET 1) " +
				"FROM orders",
		},
	} {
		test := test
		t.Run(test.name+"_typed_decline", func(t *testing.T) {
			_, err := PlanQueryForTest(test.sql, multiTableSchema, nil)
			if err == nil {
				t.Fatal("expected typed unsupported rejection")
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
				t.Fatalf("error = %v, want SQLSTATE %s", err, api.ErrCodeUnsupportedQuery)
			}
		})
	}

	// An inner join's ON-clause EXISTS is a WHERE-EXISTS (the builder folds it
	// into the WHERE), so a cardinality-known one is substituted exactly as
	// the WHERE spelling is: the two spellings plan to the same tree, and
	// neither builds an existential probe (the FlatMap present is the join's
	// own nested loop, not a semi-join).
	t.Run("join_on_known_false_substituted", func(t *testing.T) {
		const existsKnownFalse = "EXISTS (SELECT COUNT(*) FROM customers AS c2 WHERE c2.id = orders.customer_id LIMIT 1 OFFSET 1)"
		onPlan, err := PlanQueryForTest(
			"SELECT orders.id FROM orders JOIN customers ON customers.id = orders.customer_id AND "+existsKnownFalse,
			multiTableSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		wherePlan, err := PlanQueryForTest(
			"SELECT orders.id FROM orders JOIN customers ON customers.id = orders.customer_id WHERE "+existsKnownFalse,
			multiTableSchema, nil)
		if err != nil {
			t.Fatal(err)
		}
		if onPlan != wherePlan {
			t.Fatalf("ON-EXISTS and WHERE-EXISTS must plan identically:\n  on:    %s\n  where: %s", onPlan, wherePlan)
		}
		assertPlanNotContains(t, onPlan, "FirstOrDefault")
	})
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
  order_id BIGINT,
  item_num BIGINT,
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
  order_id BIGINT,
  item_num BIGINT,
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
  id BIGINT,
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
  id BIGINT,
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
CREATE TABLE A (id BIGINT, PRIMARY KEY (id))
CREATE TABLE B (id BIGINT, PRIMARY KEY (id))
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
CREATE TABLE EMPLOYEES (id BIGINT, manager_id BIGINT, name STRING, PRIMARY KEY (id))
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
CREATE TABLE ORDERS (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))
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
CREATE TABLE ORDERS (id BIGINT, status STRING, amount BIGINT, PRIMARY KEY (id))
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
CREATE TABLE ORDERS (id BIGINT, status STRING, amount BIGINT, tier STRING, PRIMARY KEY (id))
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

// TestPlanHarness_MergedJoinRangeKeepsSelectiveProbe pins the partition split
// of a range merged over one value: the selective part binds at its own
// quantifier while the join part probes the other. Java partitions the merged
// range whole and plans the first two as full scans of the driving table.
func TestPlanHarness_MergedJoinRangeKeepsSelectiveProbe(t *testing.T) {
	t.Parallel()
	const schema = `CREATE TABLE orders (id BIGINT, cust_id BIGINT, PRIMARY KEY (id))
CREATE TABLE customers (id BIGINT, name STRING, PRIMARY KEY (id))
CREATE TABLE a (id BIGINT, k BIGINT, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT, k BIGINT, PRIMARY KEY (id))
CREATE TABLE c (id BIGINT, k BIGINT, PRIMARY KEY (id))
CREATE INDEX o_cust ON orders (cust_id)`
	for _, tc := range []struct{ sql, want string }{
		{
			"SELECT o.id FROM orders o JOIN customers c ON o.cust_id = c.id WHERE o.cust_id = 42",
			"FlatMap(outer=IndexScan(O_CUST, [=]), inner=Scan(CUSTOMERS, [=]))",
		},
		{
			"SELECT x.id FROM a AS x INNER JOIN a AS y ON x.id = y.id WHERE x.id = 1",
			"FlatMap(outer=Scan(A, [=]), inner=Scan(A, [=]))",
		},
		{
			// A leg only the join predicates read publishes Java's
			// PartitionSelectRule literal 1.
			"SELECT a.id FROM a JOIN b USING (id, k) JOIN c USING (id, k) ORDER BY a.id",
			"FlatMap(outer=Scan(A), inner=FlatMap(outer=Map(PredicatesFilter(Scan(B, [=]), [1 preds]), {_0: 1}), inner=Map(PredicatesFilter(Scan(C, [=]), [1 preds]), {_0: 1})))",
		},
	} {
		plan, err := PlanQueryForTest(tc.sql, schema, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		assertPlanContains(t, plan, tc.want)
	}
}

func assertPlanContains(t *testing.T, plan, substr string) {
	t.Helper()
	if !strings.Contains(plan, substr) {
		t.Errorf("plan does not contain %q:\n  %s", substr, plan)
	}
}

// indexScanOf returns the index scan a plan node represents, seeing THROUGH a
// covering wrapper. It is a thin alias for plans.IndexPlanOf, kept only so the
// existing call sites in this package read unchanged.
//
// It USED to be a hand-written copy of that function, and so did
// indexScanOfNode in package sqldriver_test — the same six lines written twice
// because an unexported test helper cannot cross a package boundary and there
// was no exported symbol to share. The exported one now exists, and the copies
// are gone: two copies of a structural guard agree exactly until one of them is
// edited, and the edit that matters here is a THIRD plan type learning to carry
// an index scan.
//
// RFC-220 made coveringness a plan TYPE holding the index scan as a FIELD, and
// criterion C1 makes that field invisible to child traversal — deliberately, so
// the memo cannot yield a bare-scan group member whose rows differ from the
// covering plan's. The consequence for test-side walkers is that `plans.Walk`
// no longer reaches the inner scan, and a walker type-switching on
// *RecordQueryIndexPlan silently observes nothing.
//
// Walkers asking about the SCAN — its index name, direction, uniqueness, sort
// guarantee — want the inner, because a covering wrapper reads the same physical
// range and only reshapes the row. Walkers asking whether a plan answers from
// the entry want the covering plan itself, and must NOT use this.
func indexScanOf(node plans.RecordQueryPlan) (*plans.RecordQueryIndexPlan, bool) {
	return plans.IndexPlanOf(node)
}

// assertScanReadsBaseRecords asserts that the index scan introduced by
// scanPrefix reads BASE RECORDS rather than answering from the index entry.
//
// The old proxy for this was the literal `Fetch(IndexScan(IDX_X…))`, and RFC-220
// killed it: a bare `IndexScan(…)` IS a fetching scan now — Java's semantics,
// where executeIndexScan resolves every entry to its record by primary key — and
// a separate `Fetch(` node renders only when one survives above a COVERING scan.
// So `Fetch(` disappearing does not mean the base record stopped being read; it
// usually means MergeFetchIntoCoveringIndexRule collapsed two nodes into one.
//
// The real property is "this scan does not answer from the entry alone", i.e. it
// carries no COVERING marker. That is what is checked, on the scan's own label
// rather than on the whole plan, so a covering scan elsewhere in the tree cannot
// mask a regression here.
func assertScanReadsBaseRecords(t *testing.T, plan, scanPrefix string) {
	t.Helper()
	label, covering, found := scanCoverage(plan, scanPrefix)
	if !found {
		t.Errorf("plan does not contain the scan %q, so the question of whether that scan "+
			"answers from the index entry is not being asked of anything:\n  %s", scanPrefix, plan)
		return
	}
	if covering {
		t.Errorf("the scan %q answers from the INDEX ENTRY (covering), so the base "+
			"record is never read — any column outside the entry reads NULL:\n  %s\n  scan: %s",
			scanPrefix, plan, label)
	}
}

// assertScanAnswersFromIndexEntry is the mirror: the named scan must be COVERING.
//
// It exists because the natural way to write this — "the plan renders no
// `Fetch(`" — is dead. A bare `IndexScan(…)` IS a fetching scan since RFC-220,
// so `Fetch(` is absent from correct fetching plans too and its absence proves
// nothing. The property is a positive one, checked on the scan's own label.
func assertScanAnswersFromIndexEntry(t *testing.T, plan, scanPrefix string) {
	t.Helper()
	label, covering, found := scanCoverage(plan, scanPrefix)
	if !found {
		t.Errorf("plan does not contain the scan %q, so the question of whether that scan "+
			"answers from the index entry is not being asked of anything:\n  %s", scanPrefix, plan)
		return
	}
	if !covering {
		t.Errorf("the scan %q reads base records, but every value the query needs is in the "+
			"index entry so it should answer from the entry alone:\n  %s\n  scan: %s",
			scanPrefix, plan, label)
	}
}

// scanCoverage isolates the label of the scan introduced by scanPrefix and
// reports whether that scan answers from the index entry.
//
// SCOPED TO ONE SCAN'S LABEL, not to the whole plan, and that is the entire
// reason it exists. `!strings.Contains(plan, "COVERING")` is the same claim
// written over the whole string, and it is satisfied by a plan that does not
// scan the index AT ALL — a full table scan carries no COVERING marker either,
// so the assertion passes on precisely the plan it was written to reject. The
// `found` result is what closes that hole: absence of the scan is a distinct
// answer from "the scan is not covering", and the caller must fail on it rather
// than read it as success.
func scanCoverage(plan, scanPrefix string) (label string, covering, found bool) {
	i := strings.Index(plan, scanPrefix)
	if i < 0 {
		return "", false, false
	}
	label = plan[i:]
	if j := strings.Index(label, ")"); j >= 0 {
		label = label[:j+1]
	}
	return label, strings.Contains(label, "COVERING"), true
}

// TestPlanHarness_CoverageAssertionsRejectAPlanWithoutTheScan drives every arm of
// scanCoverage from synthetic plan strings, including the arm the corpus does not
// currently produce.
//
// The arm that matters is `found == false`. The assertion these helpers replaced
// was `!strings.Contains(plan, "COVERING")`, and a full table scan satisfies it:
// the test meant "this index scan must read base records" and would have passed
// on a plan that reached no index at all. That arm is not reachable from today's
// planner for the query in question, so only a unit pin can drive it — and an
// untested arm in an instrument reads its first real firing as a finding.
func TestPlanHarness_CoverageAssertionsRejectAPlanWithoutTheScan(t *testing.T) {
	t.Parallel()
	const prefix = "IndexScan(IDX_AMOUNT"
	for _, tc := range []struct {
		name               string
		plan               string
		wantFound          bool
		wantCovering       bool
		oldAssertionPasses bool // what `!Contains(plan, "COVERING")` would have said
	}{
		{
			name:               "fetching_index_scan",
			plan:               "Project([COUNT(STATUS)#0], StreamingAgg(keys=[], IndexScan(IDX_AMOUNT, [<>])))",
			wantFound:          true,
			wantCovering:       false,
			oldAssertionPasses: true,
		},
		{
			name:               "covering_index_scan",
			plan:               "Project([COUNT(*)#0], StreamingAgg(keys=[], IndexScan(IDX_AMOUNT, [<>] COVERING)))",
			wantFound:          true,
			wantCovering:       true,
			oldAssertionPasses: false,
		},
		{
			// THE HOLE. No index scan anywhere, so nothing answers from an index
			// entry — and the whole-plan COVERING check calls that a pass.
			name:               "full_table_scan_reaches_no_index",
			plan:               "Project([COUNT(STATUS)#0], StreamingAgg(keys=[], PredicatesFilter(Scan(ORDERS), [1 preds])))",
			wantFound:          false,
			wantCovering:       false,
			oldAssertionPasses: true,
		},
		{
			// A covering scan on a DIFFERENT index must not be read as this one's
			// answer — the label is isolated, the whole plan is not consulted.
			name:               "covering_scan_on_another_index_is_not_this_scan",
			plan:               "Union(IndexScan(IDX_STATUS, [=] COVERING), IndexScan(IDX_AMOUNT, [<>]))",
			wantFound:          true,
			wantCovering:       false,
			oldAssertionPasses: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			label, covering, found := scanCoverage(tc.plan, prefix)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v for %q in\n  %s", found, tc.wantFound, prefix, tc.plan)
			}
			if covering != tc.wantCovering {
				t.Fatalf("covering = %v, want %v (scan label %q)\n  %s",
					covering, tc.wantCovering, label, tc.plan)
			}
			// The premise of the whole refactor, asserted so it cannot rot into a
			// claim nobody checks: on at least one of these plans the retired
			// whole-plan check disagrees with the scoped one.
			if got := !strings.Contains(tc.plan, "COVERING"); got != tc.oldAssertionPasses {
				t.Fatalf("the retired whole-plan check `!Contains(plan, \"COVERING\")` reports "+
					"%v here, expected %v — the comparison this table records is stale", got, tc.oldAssertionPasses)
			}
			if !tc.wantFound && tc.oldAssertionPasses {
				// Restating the defect as an assertion rather than as prose: this is
				// the exact combination that made the old form worthless.
				if covering {
					t.Fatalf("internal: a plan with no index scan cannot be covering")
				}
			}
		})
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
// wrong value, so the reject is mandatory.
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
CREATE TABLE A (id BIGINT, PRIMARY KEY (id))
CREATE TABLE B (id BIGINT, PRIMARY KEY (id))
`
	_, err = PlanQueryForTest("SELECT a.id FROM A a JOIN B b AT p ON a.id = b.id", joinSchema, nil)
	assertAtOrdinalityRejected(t, err)
}

// TestPlanHarness_AtOrdinalityRejectedInAggregateIndexDDL pins the AS-SELECT index DDL path
// (ddl.go parseAsSelectIndexDefinition → runFromResolutionPostPasses' AT-ordinality
// rejection — parseAggregateIndexDefinition is deleted, RFC-202 D1; the generator plans
// the index's SELECT through the ordinary front end), a separate AtomTableItem consumer
// from the query planner. `ga` HAS a column `p`, and the index body embeds
// `FROM ga AT p GROUP BY p`: ignoring the AT clause would build an index grouped by the
// real column p (wrong semantics). The guard rejects it.
func TestPlanHarness_AtOrdinalityRejectedInAggregateIndexDDL(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ga (id BIGINT, p BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE INDEX sum_by_p AS SELECT SUM(v) FROM ga AT p GROUP BY p
`
	_, err := PlanQueryForTest("SELECT id FROM ga", schema, nil)
	assertAtOrdinalityRejected(t, err)
}

// TestPlanHarness_AggregateIndexJoinRejected pins that an aggregate-index definition with a
// JOIN is rejected rather than silently reduced to its leading table — which previously also
// dropped any AT-ordinality clause on the joined source. Aggregate indexes are
// single-table; the `AT p` on the joined `gb` here would otherwise slip past the leading-atom
// guard entirely.
func TestPlanHarness_AggregateIndexJoinRejected(t *testing.T) {
	t.Parallel()
	schema := `
CREATE TABLE ga (id BIGINT, p BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE gb (id BIGINT, p BIGINT, PRIMARY KEY (id))
CREATE INDEX bad AS SELECT SUM(v) FROM ga JOIN gb AT p ON ga.id = gb.id GROUP BY p
`
	_, err := PlanQueryForTest("SELECT id FROM ga", schema, nil)
	if err == nil {
		t.Fatal("aggregate index with a JOIN (AT on the joined source) must be rejected, got nil")
	}
	// The index SELECT plans through the ordinary front end and its
	// post-passes (RFC-202 D4), so AT on a non-array source lands on the
	// converged 42809 WRONG_OBJECT_TYPE rejection — the same code the query
	// path uses (assertAtOrdinalityRejected).
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeWrongObjectType {
		t.Fatalf("err = %v (%T), want *api.Error{ErrCodeWrongObjectType}", err, err)
	}

	// A plain JOIN (no AT) fails record-type resolution in the generator with
	// Java's message (MaterializedViewIndexGenerator.java:791-801;
	// IndexTest.java:511-520).
	schemaPlainJoin := `
CREATE TABLE ga (id BIGINT, p BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE gb (id BIGINT, p BIGINT, PRIMARY KEY (id))
CREATE INDEX bad AS SELECT SUM(v) FROM ga JOIN gb ON ga.id = gb.id GROUP BY ga.p
`
	_, err = PlanQueryForTest("SELECT id FROM ga", schemaPlainJoin, nil)
	if err == nil {
		t.Fatal("aggregate index over a JOIN must be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "expected to find exactly one type filter operator") {
		t.Fatalf("plain-JOIN rejection must carry Java's record-type-resolution message, got: %v", err)
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
  id BIGINT,
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
// raise 42804, NOT silently lift to `d = TRUE` and filter to
// nothing. sqlTypeToCascadesType now carries the real TypeCodeDouble for
// FLOAT/DOUBLE (and TypeCodeBytes for BYTES), so the predicate-lift type gate
// rejects them as non-boolean — while genuinely un-typeable values (params,
// CTE/derived columns whose projected type isn't propagated) stay permissive.
func TestPlanHarness_BareDoubleWhereRejected(t *testing.T) {
	t.Parallel()
	const sch = `CREATE TABLE A (id BIGINT, d DOUBLE, PRIMARY KEY (id))`
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
// rejection: a CTE/derived column holding a boolean expression (`NOT flag`)
// must carry its exact BOOLEAN type across the CTE boundary and PLAN as a bare
// WHERE predicate, not be rejected 42804. RFC-232 permits no UNKNOWN-typed QOV
// escape hatch here: the CTE output schema itself is the type authority.
func TestPlanHarness_BareCTEBooleanColumnWhere(t *testing.T) {
	t.Parallel()
	const sch = `CREATE TABLE A (id BIGINT, flag BOOLEAN, PRIMARY KEY (id))`
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
// the bound-prefix correlation signal (matchBoundPrefixIsCorrelated) does not flag this
// ref (it inspects only the bound prefix); materializing
// the OR residual as a standalone leg filter would win and sever the join feed →
// FlatMap(... inner=Fetch(<nil>)) → 0 rows (the 3-way-join repro). The SHAPE gate in
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
// (`t.k = 5`) is NOT visible to the bound-prefix correlation signal
// (matchBoundPrefixIsCorrelated, which inspects only the bound prefix). The retired
// isSimpleResidualCompensation rejected such a compensation via its predicate-correlation
// check — safety, not shape rot. Without the restored guard (now compensationSafeForYield's
// outer-correlation check, backed structurally by B1's task-graph invariant — RFC-150
// Phase 2b), yieldUnknown realizes a physical correlated leg filter that severs the
// join's correlation feed → FlatMap(outer=Scan(O), inner=Fetch(<nil>)) → 0 rows (the
// PR-#201 shape, reproduced in review). The valid plan drives the inner from the outer.
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
// local residual (t.a>1) over IDX_K. The leg ref then held a nil-inner Fetch SHELL
// (the RFC-070 extraction template, real inner in the wrapper quantifier), and the
// NLJ rule picked that cheap shell and embedded its plan directly (never
// WithChildren) → FlatMap(... inner=Fetch(<nil>)) → 0 rows, on master AND every
// prior HEAD. RFC-183 removed the shell state itself (rules build the parent with
// its concrete child), so the leg ref now holds a fully-linked plan whichever
// selector reaches it; this test keeps pinning the rows.
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
	assertPlanContains(t, andPlan, "IndexScan(IDX_K")
	assertScanReadsBaseRecords(t, andPlan, "IndexScan(IDX_K")

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
		// Assert the residual SURVIVES over the index scan — `PredicatesFilter(Fetch(
		// IndexScan(IDX_K…)))`, not a bare `Fetch(IndexScan(IDX_K))`. A plan that used
		// the index but DROPPED the OR/IN residual would return wrong rows yet still
		// contain "IndexScan(IDX_K"; this is the dimension that actually matters
		// (the hazard is "residual survives", not "index used").
		assertPlanContains(t, plan, "PredicatesFilter(IndexScan(IDX_K")
		assertScanReadsBaseRecords(t, plan, "IndexScan(IDX_K")
	}

	// Join-leg IN: a 3-way join where the indexed leg t carries an IN residual and is
	// consumed correlated. Structurally distinct from the single-table IN (the
	// explode-or-filter path on a partition-SUBSEL leg) and from the OR/AND join-leg
	// shapes already pinned — and join legs are where this series repeatedly
	// bit. Must plan valid (no nil inner) with the IN residual realized.
	joinSchema := `CREATE TABLE o (id bigint, PRIMARY KEY (id))
		CREATE TABLE t (id bigint, fk bigint, k bigint, m bigint, x bigint, PRIMARY KEY (id))
		CREATE TABLE u (id bigint, x bigint, PRIMARY KEY (id))
		CREATE INDEX idx_k ON t(k)`
	joinPlan, err := PlanQueryForTest("SELECT t.k FROM o, t, u WHERE t.k = 5 AND t.m IN (1, 2, 3) AND t.fk = o.id AND u.x = t.x", joinSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("join-leg IN -> %s", joinPlan)
	assertPlanNotContains(t, joinPlan, "<nil>")
	assertPlanContains(t, joinPlan, "IndexScan(IDX_K") // t drives via idx_k, residual applied
}

// These are the SQL shapes whose symmetric join orientation changed when
// FirstOrDefault stopped publishing a NOT NULL child type for its NULL default.
// Keep the type correction and the equal-cost premise separate from the exact
// orientation sentinel in explaindiff's plan_shape.golden. Neither assertion is
// a latency claim or permission to erase result types from semantic hashes.
// The derived and CTE shapes now plan as correlated FlatMap chains (Java's
// shape), so no materialized join and no orientation tie remains for them.
func TestPlanHarness_ExistsDefaultTypesAndSymmetricJoinCosts(t *testing.T) {
	t.Parallel()
	const derivedSchema = `
CREATE TABLE t1 (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))
CREATE TABLE t3 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))`
	const antiSchema = `
CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE b (id BIGINT, v BIGINT, PRIMARY KEY (id))`
	for _, tc := range []struct {
		name        string
		schema      string
		sql         string
		defaults    int
		joins       int
		cardinality float64
	}{
		{
			name: "cte_exists", schema: derivedSchema,
			sql: `WITH c AS (SELECT id, v FROM t1)
SELECT c.id, t1_id FROM c, t3 WHERE t3.t1_id = c.id
AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = c.id) ORDER BY c.id`,
			defaults: 1,
		},
		{
			name: "derived_exists", schema: derivedSchema,
			sql: `SELECT d.id, t1_id FROM (SELECT id, v FROM t1) AS d, t3
WHERE t3.t1_id = d.id AND EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = d.id)
ORDER BY d.id`,
			defaults: 1,
		},
		{
			name: "correlated_and_uncorrelated_not_exists", schema: antiSchema,
			sql: `SELECT id FROM a WHERE a.v IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.id = 101 AND sub.v = a.v)
AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.id = 101 AND sub.v IS NULL)
ORDER BY id`,
			defaults: 2, joins: 1, cardinality: 62500,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(tc.sql, tc.schema, nil)
			if err != nil {
				t.Fatal(err)
			}
			var joins []*plans.RecordQueryNestedLoopJoinPlan
			defaults := 0
			var walk func(plans.RecordQueryPlan)
			walk = func(p plans.RecordQueryPlan) {
				switch p := p.(type) {
				case *plans.RecordQueryFirstOrDefaultPlan:
					defaults++
					if p.GetInner().GetResultType().IsNullable() {
						t.Error("premise changed: FirstOrDefault no longer has a NOT NULL child")
					}
					if !p.GetDefaultValue().Type().IsNullable() || !p.GetResultType().IsNullable() {
						t.Errorf("nullable default must widen the exact output: default=%s output=%s",
							p.GetDefaultValue().Type(), p.GetResultType())
					}
				case *plans.RecordQueryNestedLoopJoinPlan:
					joins = append(joins, p)
				}
				for _, child := range p.GetChildren() {
					walk(child)
				}
			}
			walk(plan)
			if defaults != tc.defaults || len(joins) != tc.joins {
				t.Fatalf("type/cost probe lost its population: defaults=%d want=%d joins=%d want=%d; %s",
					defaults, tc.defaults, len(joins), tc.joins, plan.Explain())
			}
			if tc.joins == 0 {
				return
			}
			join := joins[0]
			if join.GetJoinType() != plans.JoinInner {
				t.Fatal("symmetric cost comparison requires an INNER join")
			}
			qs := join.GetQuantifiers()
			reversed, err := plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
				qs[1], qs[0], join.GetPredicates(), join.GetJoinType(),
				join.GetInnerAlias(), join.GetOuterAlias(), join.GetResultValue())
			if err != nil {
				t.Fatal(err)
			}
			cost := properties.EstimateCostWith(join, nil)
			reversedCost := properties.EstimateCostWith(reversed, nil)
			if cost.Cardinality != tc.cardinality || cost.CPU <= 0 || cost != reversedCost {
				t.Fatalf("equal-cost premise changed: selected=%+v reversed=%+v want cardinality=%g and equal positive CPU",
					cost, reversedCost, tc.cardinality)
			}
		})
	}
}

// TestPlanHarness_ConstantExpressionComparandIsSargable pins that a comparand
// written as a constant expression (`id = 1 + 2`) binds a scan range like the
// literal it denotes. The target keeps such a comparand UNFOLDED and still
// sargable (EXPLAIN `SCAN([IS T, EQUALS promote(@c7 + @c9 AS LONG)])`, RFC-257
// WS-E oracle rows constant_expression_comparand_explain and
// constant_expression_range_explain), so deleting the translator's eager
// predicate folds (ws-e-design.md section 5.4a) must not cost the index. Go's
// sargability does not depend on the fold: with predicates.SimplifyPredicateValues
// made the identity, every row below planned exactly as it does here.
func TestPlanHarness_ConstantExpressionComparandIsSargable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ sql, want string }{
		{"SELECT id, amount FROM orders WHERE id = 1 + 2", "Scan(ORDERS, [=])"},
		{"SELECT id, amount FROM orders WHERE customer_id = 40 + 2", "IndexScan(IDX_CUSTOMER, [=])"},
		{"SELECT id, amount FROM orders WHERE amount > 3 - 2", "IndexScan(IDX_AMOUNT, [<>] COVERING)"},
	} {
		plan, err := PlanQueryForTest(tc.sql, ordersSchema, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		assertPlanContains(t, plan, tc.want)
	}
}

// A struct column IN a list of records over an index on its leaves: Java
// explodes the distinct list and probes the index with both leaves
// (in-predicate.yamsql, `EXPLODE arrayDistinct(...) | FLATMAP q0 -> {
// ISCAN(F1 [EQUALS q0._0, EQUALS q0._1]) }`).
func TestPlanHarness_RecordIn(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest("SELECT id FROM t WHERE f IN ((90L, 9L), (81L, 18L))", "CREATE TYPE AS STRUCT pair(x bigint, y bigint) CREATE TABLE t(id bigint, a bigint, b bigint, f pair, PRIMARY KEY(id)) CREATE INDEX f1 AS SELECT f.x, f.y FROM t ORDER BY f.x, f.y", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	assertPlanContains(t, plan, "FlatMap(outer=Explode(array_distinct), inner=IndexScan(F1, [=, =]))")
	assertPlanNotContains(t, plan, "Filter")
}

func TestPlanHarness_PermutedAggregateDerivedOrder(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest("select t.* from (select col3, max(col2) as m from t2 where col1 = 1 group by col1, col3) as t where m < 2 order by m desc", "create table t2(id bigint, col1 bigint, col2 bigint, col3 bigint, primary key(id)) create index mv9 as select col1, max(col2), col3 from t2 group by col1, col3 order by col1, max(col2), col3", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	assertPlanContains(t, plan, "AggregateIndex")
	assertPlanNotContains(t, plan, "InMemorySort")
}

func TestPlanHarness_PermutedFanoutAggregate(t *testing.T) {
	t.Parallel()
	plan, err := PlanPhysicalForTest("select a, ek.k, b, max(d) from t6, (select k from t6.c where k = 'q') as ek group by a, ek.k, b having a = 1 and max(d) > 100", "create type as struct item(k string) create table t6(id bigint, a bigint, b bigint, c item array, d bigint, primary key(id)) create index mv20 as select a, ek.k, b, max(d) from t6, (select k from t6.c) as ek group by a, ek.k, b order by a, ek.k, max(d), b", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan.Explain())
	found := false
	var visit func(plans.RecordQueryPlan)
	visit = func(p plans.RecordQueryPlan) {
		if agg, ok := p.(*plans.RecordQueryStreamingAggregationPlan); ok {
			found = true
			keys := agg.GetGroupingKeys()
			want := []int{1, 5, 2}
			if len(keys) != len(want) {
				t.Fatalf("group key count = %d, want 3", len(keys))
			}
			for i, key := range keys {
				fv, ok := values.AsFieldValue(key)
				if !ok || len(fv.Path().Ordinals()) != 1 || fv.Path().Ordinals()[0] != want[i] {
					t.Fatalf("group key %d = %v; want combined-row ordinal %d", i, key, want[i])
				}
			}
		}
		for _, child := range p.GetChildren() {
			visit(child)
		}
	}
	visit(plan)
	if !found {
		t.Fatal("missing streaming aggregate")
	}
}

func TestPlanHarness_BitmapAggregateIndex(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE t1(id bigint, category string, PRIMARY KEY(id)) CREATE INDEX bitmapIndex AS SELECT bitmap_construct_agg(bitmap_bit_position(id)), bitmap_bucket_offset(id) FROM t1 GROUP BY bitmap_bucket_offset(id)`
	p, err := PlanPhysicalForTest(`SELECT bitmap_construct_agg(bitmap_bit_position(id)) as bitmap, bitmap_bucket_offset(id) as offset FROM t1 GROUP BY bitmap_bucket_offset(id)`, ddl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Explain(), "AggregateIndex") || strings.Contains(p.Explain(), "StreamingAgg") {
		t.Fatalf("bitmap must read precomputed index: %s", p.Explain())
	}
}

func TestPlanHarness_BitmapIndexRequiresMatchingArithmetic(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TABLE t1(id bigint, other bigint, PRIMARY KEY(id)) CREATE INDEX bm AS SELECT bitmap_construct_agg(bitmap_bit_position(id)), bitmap_bucket_offset(id) FROM t1 GROUP BY bitmap_bucket_offset(id)`
	for _, tc := range []struct {
		name, argument, group string
		rejected              bool
	}{
		{"position size", "bitmap_bit_position(id, 100)", "bitmap_bucket_offset(id)", true},
		{"bucket size", "bitmap_bit_position(id)", "bitmap_bucket_offset(id, 100)", true},
		{"position field", "bitmap_bit_position(other)", "bitmap_bucket_offset(id)", false},
		{"bucket field", "bitmap_bit_position(id)", "bitmap_bucket_offset(other)", false},
		{"raw operand", "id", "bitmap_bucket_offset(id)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := "SELECT bitmap_construct_agg(" + tc.argument + "), " + tc.group + " FROM t1 GROUP BY " + tc.group
			p, err := PlanPhysicalForTest(q, ddl, nil)
			if tc.rejected {
				if err == nil {
					t.Fatal("SQL bitmap functions must reject an explicit size argument")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(p.Explain(), "AggregateIndex") || !strings.Contains(p.Explain(), "StreamingAgg") {
				t.Fatalf("mismatched bitmap arithmetic used index: %s", p.Explain())
			}
		})
	}
}

func TestPlanHarness_GroupSortRetainsSelectiveInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ query, bound string }{
		{"SELECT val, COUNT(*) FROM t WHERE id > 100 GROUP BY val", "Scan(T, [<>])"},
		{"SELECT val, COUNT(*) FROM t WHERE id = 999 GROUP BY val", "Scan(T, [=])"},
	} {
		t.Run(tc.bound, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.query, "CREATE TABLE t (id BIGINT, val BIGINT, PRIMARY KEY (id))", nil)
			if err != nil {
				t.Fatal(err)
			}
			assertPlanContains(t, plan, tc.bound)
			assertPlanContains(t, plan, "StreamingAgg")
		})
	}
}

func TestAggregateIndexCandidatePreservesKeyExpressionIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		root recordlayer.KeyExpression
	}{
		{"computed_operand", recordlayer.GroupBy(recordlayer.FunctionExpr("add", recordlayer.Concat(recordlayer.Field("V"), recordlayer.Literal(int64(1)))), recordlayer.Field("G"))},
		{"computed_group", recordlayer.GroupBy(recordlayer.Field("V"), recordlayer.FunctionExpr("add", recordlayer.Concat(recordlayer.Field("G"), recordlayer.Literal(int64(1)))))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := metadata.NewSchemaTemplateBuilder().SetName("expr_identity")
			b.AddTable("T", []metadata.ColumnSpec{
				metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
				metadata.NewColumnSpec("G", api.NewLongType(false), 2),
				metadata.NewColumnSpec("V", api.NewLongType(false), 3),
			}, []string{"ID"})
			b.AddGeneratedIndex("T", "MX", tc.root, recordlayer.IndexTypePermutedMax, false, map[string]string{recordlayer.IndexOptionPermutedSize: "0"}, nil)
			tmpl, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := PlanRecordQueryWithMetadata("SELECT g, MAX(v) FROM t GROUP BY g", tmpl.Underlying(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan.Explain(), "AggregateIndex") {
				t.Fatalf("computed index must not answer a different grouping or operand: %s", plan.Explain())
			}
		})
	}
}

// An indexed OR beside EXISTS still reaches predicate-union exploration.
func TestPlanHarness_UnionWithExistentialPredicate(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest("SELECT id FROM orders o WHERE (status = 'pending' OR amount = 42) AND EXISTS (SELECT id FROM orders i WHERE i.id = 1)", ordersSchema, properties.MapStatistics{PerType: map[string]float64{"ORDERS": 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	assertPlanContains(t, plan, "Union")
}

func TestPlanHarness_DisjunctiveExistsAdmission(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest("SELECT id FROM orders o WHERE status = 'pending' OR EXISTS (SELECT id FROM orders i WHERE i.id = o.customer_id)", ordersSchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	assertPlanContains(t, plan, "PredicatesFilter(FirstOrDefault(")
}

func TestPlanHarness_UnionWithFixedUnindexedFactor(t *testing.T) {
	t.Parallel()
	schema := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY(id)) CREATE INDEX ix_a ON t(a) CREATE INDEX ix_b ON t(b)"
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse("SELECT id FROM t WHERE (a=1 OR b=2) AND (c=10 OR c=20)")
	if err != nil {
		t.Fatal(err)
	}
	logical, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(parsed.Statements().AllStatement()[0].SelectStatement().Query())
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveQualifiedTableNames(logical, defaultEmbeddedTemplate); err != nil {
		t.Fatal(err)
	}
	if err := validateTablesAndColumns(logical, tmpl.Underlying()); err != nil {
		t.Fatal(err)
	}
	ref, _, err := query.TranslateToCascadesWithError(logical, tmpl.Underlying())
	if err != nil {
		t.Fatal(err)
	}
	planner := newCascadesPlanner(tmpl.Underlying(), plannerOptionsFrom(nil), cascades.BatchAExpressionRules(), properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}})
	implemented := make(map[expressions.RelationalExpression]bool)
	consumedAfterImplementation := 0
	planner.WithTaskObserver(func(task cascades.Task) {
		switch task := task.(type) {
		case *cascades.TransformExprTask:
			if _, ok := task.Rule.(*cascades.ImplementFilterRule); ok {
				implemented[task.Expr] = true
			}
		case *cascades.TransformImplTask:
			if _, ok := task.Rule.(*cascades.ImplementSimpleSelectRule); ok {
				implemented[task.Expr] = true
			}
		case *cascades.ConsumeMatchPartitionTask:
			for _, raw := range task.Ref.GetAllPartialMatches() {
				match := raw.(cascades.PartialMatch)
				switch expr := match.GetQueryExpression(); expr.(type) {
				case *expressions.SelectExpression, *expressions.LogicalFilterExpression:
					if !implemented[expr] {
						t.Fatalf("access consumption preceded %T implementation", expr)
					}
					consumedAfterImplementation++
				}
			}
		}
	})
	best, _, err := planner.PlanWithContext(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if consumedAfterImplementation == 0 {
		t.Fatal("no matched filter/select reached access consumption")
	}
	plan := best.(interface{ GetRecordQueryPlan() plans.RecordQueryPlan }).GetRecordQueryPlan().Explain()
	hints, unions := 0, 0
	memoTypes := make(map[string]int)
	staleHints := 0
	for reference := range planner.Memo().References() {
		for _, raw := range reference.GetAllPartialMatches() {
			match := raw.(cascades.PartialMatch)
			if carrier, ok := match.GetQueryExpression().(expressions.RelationalExpressionWithPredicates); ok {
				for _, predicate := range carrier.GetPredicates() {
					t.Logf("match %T %s bindings=%d predicate=%s", match.GetQueryExpression(), match.GetMatchCandidate().CandidateName(), len(match.GetMatchInfo().GetRegularMatchInfo().GetParameterBindingMap()), predicate.Explain())
				}
			}
			for _, entry := range match.GetMatchInfo().GetRegularMatchInfo().GetPredicateMap().Entries() {
				if entry.Mapping.GetMappingKind() == cascades.MappingOrTermImpliesCandidate {
					hints++
					if !reference.ContainsExactly(match.GetQueryExpression()) {
						staleHints++
					}
				}
			}
		}
		for _, expression := range reference.AllMembers() {
			memoTypes[fmt.Sprintf("%T", expression)]++
			if holder, ok := expression.(interface{ GetRecordQueryPlan() plans.RecordQueryPlan }); ok {
				explain := holder.GetRecordQueryPlan().Explain()
				if strings.Contains(explain, "Union") && strings.Count(explain, "IndexScan(") == 2 {
					unions++
				}
			}
		}
	}
	if hints == 0 || unions == 0 {
		t.Fatalf("match-partition scheduling produced hints=%d stale=%d indexed unions=%d types=%v", hints, staleHints, unions, memoTypes)
	}
	// Java breaks the two-residual tie by preferring one data access over two.
	assertPlanContains(t, plan, "PredicatesFilter(Scan(T)")
	assertPlanNotContains(t, plan, "Union")
}

func TestPlanHarness_FixedFactorUnionConsumesCompositeBounds(t *testing.T) {
	t.Parallel()
	schema := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, d BIGINT, PRIMARY KEY(id)) CREATE INDEX ix_a ON t(a,d) CREATE INDEX ix_b ON t(b,d)"
	plan, err := PlanQueryForTest("SELECT id FROM t WHERE (a=1 OR b=2) AND (c=10 OR c=20) AND d=9", schema, properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan)
	assertPlanContains(t, plan, "Union")
	if strings.Count(plan, "IndexScan(") != 2 {
		t.Fatalf("want two index scans: %s", plan)
	}
}

func TestPlanHarness_FixedFactorUnionExistsConverges(t *testing.T) {
	t.Parallel()
	schema := "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY(id)) CREATE INDEX idx_d ON T_RD(d) CREATE INDEX idx_a ON T_RD(a) CREATE INDEX idx_b ON T_RD(b) CREATE INDEX idx_ab ON T_RD(a,b)"
	sql := "SELECT * FROM t_rd WHERE (((a=9) AND (d=0.1)) OR ((b=5) AND (s='beta') AND (CAST(b AS STRING)='1'))) AND EXISTS (SELECT 1 FROM t_rd AS r WHERE r.b=t_rd.b AND r.a>=8) ORDER BY id DESC"
	assertFixedFactorUnionConverges(t, schema, sql)
}

func TestPlanHarness_FixedFactorUnionExistsRangeConverges(t *testing.T) {
	t.Parallel()
	schema := "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY(id)) CREATE INDEX idx_a ON T_RD(a) CREATE INDEX idx_e ON T_RD(e) CREATE INDEX idx_s ON T_RD(s)"
	sql := "SELECT * FROM t_rd WHERE (((s IS NOT NULL) AND (a=8) AND (e BETWEEN 3.0 AND 4.0)) OR ((d IN (2,5)) AND (COALESCE(a,8)<5))) AND EXISTS (SELECT 1 FROM t_rd AS r WHERE r.c<t_rd.c) ORDER BY c DESC NULLS FIRST,id"
	assertFixedFactorUnionConverges(t, schema, sql)
}

func TestPlanHarness_IndexAccessRealizedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, indexes, predicate string
		accesses, arity          int
	}{
		{"redundant", "CREATE INDEX ia ON t(a) CREATE INDEX iab ON t(a,b)", "a=1 AND c=2", 2, 0},
		{"pair", "CREATE INDEX ia ON t(a) CREATE INDEX ib ON t(b)", "a=1 AND b=2 AND d=3", 2, 2},
		{"triple", "CREATE INDEX ia ON t(a) CREATE INDEX ib ON t(b) CREATE INDEX ic ON t(c)", "a=1 AND b=2 AND c=3 AND d=4", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertIndexAccessRealizedOnce(t, tc.indexes, tc.predicate, tc.accesses, tc.arity)
		})
	}
}

func assertIndexAccessRealizedOnce(t *testing.T, indexes, predicate string, accesses, arity int) {
	t.Helper()
	schema := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, d BIGINT, PRIMARY KEY(id)) " + indexes
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse("SELECT * FROM t WHERE " + predicate)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(parsed.Statements().AllStatement()[0].SelectStatement().Query())
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveQualifiedTableNames(logical, defaultEmbeddedTemplate); err != nil {
		t.Fatal(err)
	}
	if err := validateTablesAndColumns(logical, tmpl.Underlying()); err != nil {
		t.Fatal(err)
	}
	ref, _, err := query.TranslateToCascadesWithError(logical, tmpl.Underlying())
	if err != nil {
		t.Fatal(err)
	}
	planner := newCascadesPlanner(tmpl.Underlying(), plannerOptionsFrom(nil), cascades.BatchAExpressionRules(), nil)
	best, _, err := planner.PlanWithContext(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if best == nil {
		t.Fatal("empty plan")
	}
	coverings := make(map[*plans.RecordQueryCoveringIndexPlan]bool)
	fetches := make(map[*plans.RecordQueryFetchFromPartialRecordPlan]bool)
	largestIntersection := 0
	for group := range planner.Memo().References() {
		for _, expr := range group.AllMembers() {
			switch plan := expr.(type) {
			case *plans.RecordQueryCoveringIndexPlan:
				coverings[plan] = true
			case *plans.RecordQueryFetchFromPartialRecordPlan:
				fetches[plan] = true
			case *plans.RecordQueryIntersectionPlan:
				largestIntersection = max(largestIntersection, len(plan.GetQuantifiers()))
			}
		}
	}
	accessFetches := 0
	for fetch := range fetches {
		t.Logf("fetch alternative: %s", fetch.Explain())
		if _, isAccess := fetch.GetInner().(*plans.RecordQueryCoveringIndexPlan); isAccess {
			accessFetches++
		} else if intersection, ok := fetch.GetInner().(*plans.RecordQueryIntersectionPlan); !ok || len(intersection.GetQuantifiers()) != arity {
			t.Fatalf("unexpected non-access fetch: %s", fetch.Explain())
		}
	}
	if len(coverings) != accesses || accessFetches != accesses {
		t.Fatalf("%d index matches must be realized once each, including intersection bookkeeping: covering=%d access fetch=%d", accesses, len(coverings), accessFetches)
	}
	if largestIntersection != arity {
		t.Fatalf("largest intersection arity=%d, want %d", largestIntersection, arity)
	}
	for plan := range coverings {
		t.Logf("retained access: %s", plan.Explain())
	}
}

func TestPlanHarness_FixedFactorUnionAccessConverges(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	const sql = "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4)))"
	for _, tc := range []struct{ name, suffix string }{
		{"unordered", ""},
		{"ordered", " ORDER BY b, id"},
		{"exists", " AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9)"},
		{"scalar", " AND c <= (SELECT MIN(a) FROM t_rd)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFixedFactorUnionConverges(t, schema, sql+tc.suffix)
		})
	}
}

func TestPlanHarness_FixedFactorUnionScalarSubqueryConverges(t *testing.T) {
	t.Parallel()
	schema := "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	sql := "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= (SELECT MIN(a) FROM t_rd) ORDER BY b, id"
	planner := assertFixedFactorUnionConverges(t, schema, sql)
	unions := make(map[*plans.RecordQueryUnorderedUnionPlan]bool)
	legs := 0
	for ref := range planner.Memo().References() {
		for _, member := range ref.AllMembers() {
			if union, ok := member.(*plans.RecordQueryUnorderedUnionPlan); ok && !unions[union] {
				unions[union] = true
				legs += len(union.GetQuantifiers())
			}
		}
	}
	if len(unions) != 511 || legs != 2898 {
		t.Fatalf("convergence must retain every physical union alternative: unions=%d legs=%d, want 511/2898", len(unions), legs)
	}
	// Over all 513 logical unions this query explores (2,910 union inputs): a
	// leg is its term plus the fixed factors the term does not imply, so each
	// distinct leg is one group, planned once.
	legGroups := make(map[*expressions.Reference]bool)
	for ref := range planner.Memo().References() {
		for _, member := range ref.AllMembers() {
			if union, ok := member.(*expressions.LogicalUnionExpression); ok {
				for _, quantifier := range union.GetQuantifiers() {
					legGroups[quantifier.GetRangesOver().Canonical()] = true
				}
			}
		}
	}
	if len(legGroups) != 78 {
		t.Fatalf("logical union legs span %d groups, want 78 distinct legs", len(legGroups))
	}
	// The original select's split normalizes into the CNF select's split;
	// merging the two groups enumerates the 511 subsets once.
	enumerations := 0
	for ref := range planner.Memo().References() {
		uniques := 0
		for _, member := range ref.Canonical().Members() {
			if _, ok := member.(*expressions.LogicalUniqueExpression); ok {
				uniques++
			}
		}
		if ref.Canonical() == ref && uniques >= 511 {
			enumerations++
		}
	}
	if planner.Memo().MergeCount() == 0 || enumerations != 1 {
		t.Fatalf("merges=%d, groups holding the 511 union alternatives=%d, want one", planner.Memo().MergeCount(), enumerations)
	}
}

// Java needs the scalar aggregate lifted into FROM and ABS(c)=4 expressed
// as c=4 OR c=-4; the conformance probe runs this same SQL and schema.
func TestPlanHarness_FixedFactorUnionJavaComparable(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	const sql = "SELECT t_rd.* FROM t_rd, (SELECT MIN(a) AS min_a FROM t_rd) AS m WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (c = 4 OR c = -4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= m.min_a"
	for _, order := range []string{" ORDER BY b, id", ""} {
		t.Run(order, func(t *testing.T) {
			assertFixedFactorUnionConverges(t, schema, sql+order)
		})
	}
}

func TestPlanHarness_FixedFactorUnionCompleteSearch(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	const sql = "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9)"
	// Diagnostic ceiling only; the production-budget regressions above remain separate.
	assertFixedFactorUnionConvergesWithBudget(t, schema, sql, 500_000)
}

func TestPlanHarness_FixedFactorUnionScalarSubqueryCompleteSearch(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	const sql = "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= (SELECT MIN(a) FROM t_rd) ORDER BY b, id"
	// Keep the exact seed's complete census separate from its production-budget regression.
	assertFixedFactorUnionConvergesWithBudget(t, schema, sql, 500_000)
}

func assertFixedFactorUnionConverges(t *testing.T, schema, sql string) *cascades.Planner {
	t.Helper()
	return assertFixedFactorUnionConvergesWithBudget(t, schema, sql, 0)
}

func assertFixedFactorUnionConvergesWithBudget(t *testing.T, schema, sql string, taskBudget int) *cascades.Planner {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(parsed.Statements().AllStatement()[0].SelectStatement().Query())
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveQualifiedTableNames(logical, defaultEmbeddedTemplate); err != nil {
		t.Fatal(err)
	}
	if err := validateTablesAndColumns(logical, tmpl.Underlying()); err != nil {
		t.Fatal(err)
	}
	ref, _, err := query.TranslateToCascadesWithError(logical, tmpl.Underlying())
	if err != nil {
		t.Fatal(err)
	}
	planner := newCascadesPlanner(tmpl.Underlying(), plannerOptionsFrom(nil), cascades.BatchAExpressionRules(), nil)
	if taskBudget > 0 {
		planner.WithMaxTasks(taskBudget)
	}
	taskCounts := make(map[string]int)
	groupTasks := make(map[*expressions.Reference]int)
	groupWork := make(map[string]map[*expressions.Reference]int)
	groupStates := make(map[string]int)
	consumeCounts := make(map[*expressions.Reference]int)
	accessOrderings := make(map[string]int)
	accessesWithoutOrdering := make(map[string]int)
	preparedAccesses := 0
	exploredObjects := make(map[expressions.RelationalExpression]int)
	explorationTypes := make(map[string]int)
	type transformKey struct {
		rule string
		ref  *expressions.Reference
		expr expressions.RelationalExpression
	}
	transformCounts := make(map[transformKey]int)
	inputCounts := make(map[transformKey]int)
	inputShapes := make(map[string]int)
	optimizerShapes := make(map[string]int)
	planner.WithTaskObserver(func(task cascades.Task) {
		key := fmt.Sprintf("%T", task)
		var group *expressions.Reference
		switch task := task.(type) {
		case *cascades.ExploreGroupTask:
			group = task.Ref
		case *cascades.OptimizeGroupTask:
			group = task.Ref
			optimizerShapes[fmt.Sprintf("members=%d/finals=%d/explored=%t/exploring=%t", len(group.Members()), len(group.FinalMembers()),
				group.ConstraintsMap().IsExplored(), group.ConstraintsMap().IsExploring())]++
		case *cascades.OptimizeInputsTask:
			group = task.Ref
			inputCounts[transformKey{key, group.Canonical(), task.Expr}]++
			inputShapes[fmt.Sprintf("%T/inputs=%d/live=%t", task.Expr, len(task.Expr.GetQuantifiers()), group.ContainsExactly(task.Expr))]++
		}
		if group != nil {
			if groupWork[key] == nil {
				groupWork[key] = make(map[*expressions.Reference]int)
			}
			groupWork[key][group.Canonical()]++
			groupStates[fmt.Sprintf("%s/stage=%d/explore=%t/winner=%t", key, group.Stage(), group.NeedsExploration(), group.HasWinner())]++
		}
		switch transform := task.(type) {
		case *cascades.ExploreExprTask:
			exploredObjects[transform.Expr]++
			explorationTypes[fmt.Sprintf("%v/%T", transform.Phase, transform.Expr)]++
		case *cascades.ConsumeMatchPartitionTask:
			consumeCounts[transform.Ref]++
			var orderings []*properties.RequestedOrdering
			if constraint, ok := transform.Ref.ConstraintsMap().GetConstraint(cascades.RequestedOrderingConstraintKey); ok {
				orderings = constraint.([]*properties.RequestedOrdering)
			}
			for _, ordering := range orderings {
				accessOrderings[fmt.Sprint(ordering)]++
			}
			for _, candidate := range cascades.GetPartialMatchCandidatesTyped(transform.Ref) {
				if _, aggregate := candidate.(*cascades.AggregateIndexMatchCandidate); aggregate {
					continue
				}
				traversal := candidate.GetTraversal()
				if traversal == nil {
					continue
				}
				var complete []cascades.PartialMatch
				for _, match := range cascades.GetPartialMatchesForCandidate(transform.Ref, candidate) {
					if match.GetCandidateRef() == traversal.GetRootReference() {
						complete = append(complete, match)
					}
				}
				for _, access := range cascades.PrepareMatchesAndCompensations(complete, orderings, nil) {
					preparedAccesses++
					if len(access.GetSatisfyingRequestedOrderings()) == 0 {
						accessesWithoutOrdering[candidate.CandidateName()]++
					}
				}
			}
		case *cascades.TransformExprTask:
			key += fmt.Sprintf("/%T", transform.Rule)
			groupTasks[transform.Ref]++
			transformCounts[transformKey{key, transform.Ref, transform.Expr}]++
		case *cascades.TransformImplTask:
			key += fmt.Sprintf("/%T", transform.Rule)
			groupTasks[transform.Ref]++
			transformCounts[transformKey{key, transform.Ref, transform.Expr}]++
		}
		taskCounts[key]++
	})
	started := time.Now()
	best, tasks, err := planner.PlanWithContext(context.Background(), ref)
	t.Logf("planning elapsed=%s tasks=%d error=%v", time.Since(started), tasks, err)
	t.Logf("task counts: %v", taskCounts)
	for kind, groups := range groupWork {
		repeated, maximum := 0, 0
		for _, count := range groups {
			repeated += count - 1
			maximum = max(maximum, count)
		}
		t.Logf("%s: groups=%d repeats=%d maximum=%d", kind, len(groups), repeated, maximum)
	}
	t.Logf("group task states: %v", groupStates)
	repeatedInputs := make(map[string]int)
	for key, count := range inputCounts {
		if count > 1 {
			repeatedInputs[fmt.Sprintf("%T", key.expr)] += count - 1
		}
	}
	t.Logf("repeated (group,expression) input tasks=%v; optimizer shapes=%v", repeatedInputs, optimizerShapes)
	t.Logf("input optimization shapes=%v", inputShapes)
	repeatedExplorations := make(map[string]int)
	for expr, count := range exploredObjects {
		if count > 1 {
			repeatedExplorations[fmt.Sprintf("%T", expr)] += count - 1
		}
	}
	t.Logf("expression exploration tasks by phase/type=%v; repeated object explorations=%v", explorationTypes, repeatedExplorations)
	t.Logf("consumption ordering requests=%v; prepared accesses=%d; accesses without satisfied ordering by candidate=%v", accessOrderings, preparedAccesses, accessesWithoutOrdering)
	if err != nil {
		reachable := make(map[*expressions.Reference]bool)
		var visit func(*expressions.Reference)
		visit = func(group *expressions.Reference) {
			group = group.Canonical()
			if reachable[group] {
				return
			}
			reachable[group] = true
			for _, member := range group.AllMembers() {
				for _, q := range member.GetQuantifiers() {
					visit(q.GetRangesOver())
				}
			}
		}
		visit(ref)
		orphanTasks := 0
		for group, count := range groupTasks {
			if !reachable[group.Canonical()] {
				orphanTasks += count
			}
		}
		t.Logf("reachable groups=%d; transform tasks in now-unreachable groups=%d", len(reachable), orphanTasks)
		repeatedTransforms := make(map[string]int)
		maxRepeats := 0
		for key, count := range transformCounts {
			if count > 1 {
				repeatedTransforms[key.rule] += count - 1
			}
			if count > maxRepeats {
				maxRepeats = count
			}
		}
		t.Logf("repeated (rule,ref,expression) tasks=%v; maximum repetition=%d", repeatedTransforms, maxRepeats)
		repeatedConsumptions, maxConsumptions := 0, 0
		for _, count := range consumeCounts {
			if count > 1 {
				repeatedConsumptions += count - 1
			}
			maxConsumptions = max(maxConsumptions, count)
		}
		t.Logf("data access: groups=%d repeated consumption tasks=%d maximum per group=%d", len(consumeCounts), repeatedConsumptions, maxConsumptions)
		type selectKey struct {
			hash uint64
		}
		type selectEntry struct {
			ref  *expressions.Reference
			expr *expressions.SelectExpression
		}
		seen := make(map[selectKey][]selectEntry)
		selectsByChild := make(map[*expressions.Reference][]selectEntry)
		duplicates := 0
		hashComparisons := 0
		for group := range planner.Memo().References() {
			for _, member := range group.AllMembers() {
				sel, ok := member.(*expressions.SelectExpression)
				if !ok {
					continue
				}
				key := selectKey{sel.HashCodeWithoutChildren()}
				if qs := sel.GetQuantifiers(); len(qs) == 1 {
					child := qs[0].GetRangesOver().Canonical()
					for _, previous := range selectsByChild[child] {
						aliases := expressions.AliasMapOf(previous.expr.GetQuantifiers()[0].GetAlias(), qs[0].GetAlias())
						hashComparisons++
						if previous.expr.EqualsWithoutChildren(sel, aliases) && previous.expr.HashCodeWithoutChildren() != key.hash {
							t.Fatalf("memo-equal Select payloads have different hashes: groups %d and %d", previous.ref.ID(), group.ID())
						}
					}
					selectsByChild[child] = append(selectsByChild[child], selectEntry{group, sel})
				}
				for _, previous := range seen[key] {
					if previous.ref != group && previous.ref.Stage() == group.Stage() && expressions.MemoEqual(previous.expr, sel) {
						duplicates++
						if duplicates <= 5 {
							t.Logf("duplicate Select across lanes/children: groups %d and %d", previous.ref.ID(), group.ID())
						}
					}
				}
				seen[key] = append(seen[key], selectEntry{group, sel})
			}
		}
		t.Logf("same-stage Select duplicate pairs across all lanes/children=%d over %d hash buckets; independent payload hash comparisons=%d", duplicates, len(seen), hashComparisons)
		finalPairs := make(map[string]int)
		for group := range planner.Memo().References() {
			finals := group.FinalMembers()
			for i, a := range finals {
				for _, b := range finals[i+1:] {
					if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() && expressions.MemoEqual(a, b) && expressions.MemoEqual(b, a) {
						finalPairs[fmt.Sprintf("%T", a)]++
					}
				}
			}
		}
		t.Logf("memo-equivalent final pairs within a group=%v", finalPairs)
	}
	observed := 0
	for _, count := range taskCounts {
		observed += count
	}
	if observed != tasks {
		t.Fatalf("observed %d tasks, planner reported %d", observed, tasks)
	}
	types := make(map[string]int)
	uniqueTypes := make(map[string]int)
	seenExpressions := make(map[expressions.RelationalExpression]bool)
	atomicCount := 0
	largestGroup := 0
	var largestMembers []expressions.RelationalExpression
	for reference := range planner.Memo().References() {
		if seeds := reference.Members(); len(seeds) != 0 {
			required := expressions.GetCorrelatedToOfExpression(seeds[0])
			for _, alternative := range reference.AllMembers() {
				for alias := range expressions.GetCorrelatedToOfExpression(alternative) {
					if _, expected := required[alias]; !expected {
						t.Fatalf("group %d seed %T needs %v; alternative %T adds alias %#v (own=%v)", reference.ID(), seeds[0], required, alternative, alias, alternative.GetCorrelatedToWithoutChildren())
					}
				}
			}
		}
		if members := reference.AllMembers(); len(members) > largestGroup {
			largestGroup = len(members)
			largestMembers = members
		}
		for _, expression := range reference.AllMembers() {
			types[fmt.Sprintf("%T", expression)]++
			if !seenExpressions[expression] {
				seenExpressions[expression] = true
				uniqueTypes[fmt.Sprintf("%T", expression)]++
			}
			if carrier, ok := expression.(expressions.RelationalExpressionWithPredicates); ok {
				for _, predicate := range carrier.GetPredicates() {
					if predicates.IsAtomic(predicate) {
						atomicCount++
					}
				}
			}
		}
	}
	t.Logf("tasks=%d groups=%d atomic=%d types=%v", tasks, len(planner.Memo().References()), atomicCount, types)
	t.Logf("unique expression objects by type=%v", uniqueTypes)
	largestTypes := make(map[string]int)
	for _, expression := range largestMembers {
		largestTypes[fmt.Sprintf("%T", expression)]++
		if carrier, ok := expression.(expressions.RelationalExpressionWithPredicates); ok && largestTypes[fmt.Sprintf("%T", expression)] <= 2 {
			for _, predicate := range carrier.GetPredicates() {
				t.Logf("largest group %T predicate: %s", expression, predicate.Explain())
			}
		}
	}
	t.Logf("largest group: %d members, types=%v", largestGroup, largestTypes)
	if types["*expressions.LogicalUnionExpression"] == 0 {
		t.Fatal("convergence test did not explore a union")
	}
	if err != nil {
		t.Fatal(err)
	}
	if best == nil {
		t.Fatal("empty plan")
	}
	return planner
}
