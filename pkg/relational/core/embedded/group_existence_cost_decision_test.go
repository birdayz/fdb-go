package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// The RFC-209 fixture shape: a grouped SUM with a HAVING and an ORDER BY on the
// grouping key. With the aggregate index declared, companion discovery applies
// and the query plans as the group-existence merge.
const groupExistenceCostQuery = "SELECT customer_id, SUM(amount) FROM orders " +
	"GROUP BY customer_id HAVING SUM(amount) > 50000 ORDER BY customer_id"

const groupExistenceCostSchemaWithAgg = `
CREATE TABLE ORDERS (
  id BIGINT,
  customer_id BIGINT,
  amount BIGINT,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE INDEX sum_amount_by_customer AS SELECT SUM(amount) FROM ORDERS GROUP BY customer_id
`

const groupExistenceCostSchemaNoAgg = `
CREATE TABLE ORDERS (
  id BIGINT,
  customer_id BIGINT,
  amount BIGINT,
  status STRING,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
`

// TestGroupExistenceMerge_RivalIsConstructible establishes the premise the
// cost-model reasoning rests on: the base-table alternative to the merge is a
// plan the planner can and does build. Strip the aggregate index and the SAME
// query plans as streaming aggregation over a sorted full scan.
//
// This matters because "the merge wins on cost" and "the merge is the only
// candidate" are different claims with different fixes, and a plan assertion on
// the merge alone cannot tell them apart. It is pinned separately so that if
// the rival ever stops being constructible, THAT is what reddens — not the
// cost-model test below, which would then be passing vacuously.
func TestGroupExistenceMerge_RivalIsConstructible(t *testing.T) {
	t.Parallel()
	plan, err := PlanQueryForTest(groupExistenceCostQuery, groupExistenceCostSchemaNoAgg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "StreamingAgg(") {
		t.Fatalf("without the aggregate index the rival must be streaming aggregation; got %s", plan)
	}
	if !strings.Contains(plan, "InMemorySort(") || !strings.Contains(plan, "Scan(ORDERS)") {
		t.Fatalf("the rival must be streaming aggregation over a SORTED FULL SCAN — that is what "+
			"makes it more expensive than the merge's two narrow ordered index scans; got %s", plan)
	}
}

// TestGroupExistenceMerge_DecisionIsStructuralNotEconomic pins the mechanism
// that actually decides merge-vs-base-table, established by rung-by-rung
// instrumentation of planningCostModelCompareWith on this exact pair.
//
// The two candidates tie on every rung through the data-access count (the
// whole-plan max-cardinality gate abstains — neither side has a proven bound;
// residuals 0/0; data access 1/1). Three independent rungs then each pick the
// merge unaided: comparePrimaryScanVsIndexScan (covering index over primary
// scan), inMemorySortCount (0 vs 1), and the scalar EstimateCostWith fallback
// that routes through the merge plan's driving-leg HintCost. Disabling any one
// of the three leaves the merge winning; the winner flips only when all three
// are neutralized at once.
//
// The observable consequence, and what this test asserts, is that the choice is
// INSENSITIVE TO STATISTICS. Table cardinality is the only input the magnitude
// cost consumes, so if the economics governed, sweeping it across nine orders
// of magnitude would move the winner somewhere. It does not — because the two
// rungs that decide first are discrete structural counts that never look at
// cardinality at all.
//
// Read this as the guard on the count-as-1 decision in
// findExpressionsByType's concreteCountMultiIntersection arm: counting the
// merge's legs honestly (2, not 1) makes the data-access rung fire BEFORE all
// three of the above and bars the merge at every cardinality. That mutation
// reddens this test at every sweep point.
func TestGroupExistenceMerge_DecisionIsStructuralNotEconomic(t *testing.T) {
	t.Parallel()
	// Nine orders of magnitude, spanning "the table is smaller than one group"
	// to "the table dwarfs any index". If the magnitude cost were marginal, the
	// merge's 2-leg charge would lose somewhere in here.
	for _, card := range []float64{1, 10, 1_000, 100_000, 1e6, 1e9} {
		plan, err := PlanQueryForTest(groupExistenceCostQuery, groupExistenceCostSchemaWithAgg,
			properties.FixedStatistics{Cardinality: card})
		if err != nil {
			t.Fatalf("cardinality %g: %v", card, err)
		}
		if !strings.Contains(plan, "GroupExistenceMerge(") {
			t.Fatalf("cardinality %g: the merge-vs-base-table choice moved with STATISTICS. "+
				"It is supposed to be decided by discrete structural rungs "+
				"(covering-index-over-primary-scan, then in-memory-sort count) that never read "+
				"cardinality; a flip here means the magnitude cost became marginal, and the "+
				"count-as-1 data-access charge for the merge's legs must be re-argued from "+
				"economics rather than from those two rungs. got %s", card, plan)
		}
		if strings.Contains(plan, "Scan(ORDERS)") {
			t.Fatalf("cardinality %g: the winning plan reads the base table; the merge must be "+
				"served entirely from the two aggregate indexes. got %s", card, plan)
		}
	}
}
