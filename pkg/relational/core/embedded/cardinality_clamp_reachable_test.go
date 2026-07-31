package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// RFC-195 reachability: the clamp must fire on plans a USER can produce, not
// only on hand-built shapes in a unit test.
//
// This test exists because of a measurement that would otherwise read as good
// news. The corpus-wide plan diff over 2643 entries came back CLEAN — zero
// plan-shape changes — which is either "the cost fix is low-risk" or "the clamp
// never fires in practice", and those two have opposite consequences. Nothing
// in the invariant suite distinguishes them: it builds its shapes directly with
// plans.NewXxx constructors and never asks whether SQL can reach them.
//
// So the fact is pinned here instead: real SQL, through the real planner, with
// the estimate observed on the winning plan.

const clampReachableSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
`

// TestRFC195_UngroupedAggregateCapIsReachableFromSQL pins the RFC's headline
// case end-to-end: `SELECT COUNT(*) FROM orders` provably emits ONE row, and
// its cost estimate must say so.
//
// Before the clamp this plan was priced at ~700,000 rows — the ungrouped
// aggregation charges in*DistinctSelectivity over a 1e6-row default scan with
// no cap of its own. That is the five-orders-of-magnitude error every join
// ordering ABOVE such an aggregate was computed against.
func TestRFC195_UngroupedAggregateCapIsReachableFromSQL(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"SELECT COUNT(*) FROM orders",
		"SELECT SUM(o.amount) FROM orders o",
		"SELECT COUNT(*) FROM orders WHERE status = 'shipped'",
	} {
		sql := sql
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPhysicalForTest(sql, clampReachableSchema, nil)
			if err != nil {
				t.Fatalf("planning %q failed: %v", sql, err)
			}
			cost := properties.EstimateCost(plan)
			if cost.Cardinality > 1 {
				t.Fatalf("cost estimate for %q is %v rows, but an UNGROUPED aggregate provably "+
					"emits at most one.\n"+
					"RFC-195's cap is not reaching plans real SQL produces -- which would mean the "+
					"clean corpus plan-diff reflects a clamp that never fires, not a low-risk change.\n"+
					"plan:\n%s",
					sql, cost.Cardinality, plan.Explain())
			}
			// Guard the other direction: a cap that drove the estimate to zero
			// would satisfy the assertion above and be just as wrong, since a
			// zero propagates multiplicatively through every join above it.
			if cost.Cardinality <= 0 {
				t.Fatalf("cost estimate for %q is %v -- an ungrouped aggregate emits a row, and a "+
					"zero here would cost an entire enclosing join subtree at zero",
					sql, cost.Cardinality)
			}
		})
	}
}

// TestRFC195_GroupedAggregateIsNotCapped is the negative control. A GROUPED
// aggregation's row count is data, not structure, so the property abstains and
// the clamp must leave the honest estimate alone.
//
// Without this, a clamp that capped every aggregation at one row would pass the
// test above while destroying the cost model's ability to tell a two-group
// aggregate from a million-group one.
func TestRFC195_GroupedAggregateIsNotCapped(t *testing.T) {
	t.Parallel()

	plan, err := PlanPhysicalForTest(
		"SELECT status, COUNT(*) FROM orders GROUP BY status", clampReachableSchema, nil)
	if err != nil {
		t.Fatalf("planning failed: %v", err)
	}
	cost := properties.EstimateCost(plan)
	if cost.Cardinality <= 1 {
		t.Fatalf("GROUPED aggregate costed at %v rows -- the property proves NO bound for a "+
			"grouped aggregation (the group count is data), so the clamp must be a no-op here. "+
			"Capping it would erase the distinction between a two-group and a million-group "+
			"aggregate.\nplan:\n%s", cost.Cardinality, plan.Explain())
	}
}
