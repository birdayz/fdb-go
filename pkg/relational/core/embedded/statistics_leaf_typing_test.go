package embedded

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// EVERY PHYSICAL LEAF A SQL PLAN PRODUCES MUST NAME ITS RECORD TYPES.
//
// A scan or index leaf prices itself from stats.RecordTypeCardinality(name),
// summed over the record types it declares. With NO declared types it falls
// back to RecordTypeCardinality(""), which asks for the WHOLE STORE.
//
// Under DefaultStatistics that fallback was invisible: every name, including
// "", answered LeafScanCardinality, so a typed leaf and an untyped one costed
// identically. With collected statistics they no longer do — a typed leaf
// reports its own table while an untyped one reports the SUM of every table in
// the schema. Two leaves over the same table in one plan would then disagree
// about how big it is, and the cost model would compare them as if they did
// not.
//
// So this is a census over SQL-produced plans, not an argument: it walks every
// physical plan the corpus below produces and requires each scan/index leaf to
// carry a non-empty record-type list. If it ever fails, the fallback has become
// reachable from SQL and the disagreement above is live — the fix is at the
// leaf's construction, not here.
func TestPhysicalLeavesAlwaysNameTheirRecordTypes(t *testing.T) {
	t.Parallel()

	const ddl = `CREATE TABLE orders (id BIGINT, cust BIGINT, total BIGINT, PRIMARY KEY (id))
CREATE TABLE customers (id BIGINT, region BIGINT, PRIMARY KEY (id))
CREATE INDEX orders_by_cust ON orders (cust)
CREATE INDEX orders_by_total ON orders (total)
CREATE INDEX customers_by_region ON customers (region)
CREATE INDEX orders_total_by_cust AS SELECT SUM(total) FROM orders GROUP BY cust`

	// Shapes chosen to reach different leaf constructors: bare scan, primary-key
	// probe, index equality, index range, covering index, join (two leaves at
	// once), intersection (two index leaves under one parent), aggregate, sort,
	// and IN (an InJoin inner).
	corpus := []string{
		"SELECT id FROM orders",
		"SELECT id FROM orders WHERE id = 7",
		"SELECT id FROM orders WHERE cust = 3",
		"SELECT id FROM orders WHERE total > 100",
		"SELECT cust FROM orders WHERE cust = 3",
		"SELECT o.id, c.region FROM orders o, customers c WHERE o.cust = c.id",
		"SELECT id FROM orders WHERE cust = 3 AND total = 9",
		"SELECT COUNT(*) FROM orders",
		"SELECT id FROM orders ORDER BY total",
		"SELECT id FROM orders WHERE id IN (1, 2, 3)",
		"SELECT id FROM customers WHERE region = 2",
		"SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM customers c WHERE c.id = o.cust)",
		// Aggregate-index shapes. Without one of these the aggregate class never
		// appears and the census cannot fail on it however wrong it is -- which
		// is how the class stayed uncovered while the walker was blind to it.
		"SELECT cust, SUM(total) FROM orders GROUP BY cust",
		"SELECT SUM(total) FROM orders GROUP BY cust",
	}

	// A provider that answers a DIFFERENT number for the whole store than for
	// any single table, so a leaf taking the fallback is distinguishable from
	// one that is correctly typed — not merely by this test's assertion, but in
	// the cost the planner would actually see.
	stats := properties.MapStatistics{
		PerType:  map[string]float64{"ORDERS": 150, "CUSTOMERS": 40},
		Fallback: properties.LeafScanCardinality,
	}

	var leaves, untyped, planned, aggregates int
	var offenders, unplanned []string
	for _, sql := range corpus {
		plan, err := PlanPhysicalForTest(sql, ddl, stats)
		if err != nil {
			unplanned = append(unplanned, fmt.Sprintf("%s: %v", sql, err))
			continue
		}
		planned++
		walkPlanLeaves(plan, func(p plans.RecordQueryPlan, types []string) {
			leaves++
			if _, isAgg := p.(*plans.RecordQueryAggregateIndexPlan); isAgg {
				aggregates++
			}
			if len(types) == 0 {
				untyped++
				offenders = append(offenders, fmt.Sprintf("%T in %q", p, sql))
			}
		})
	}

	// THE VACUITY GUARDS. A shape that does not plan contributes no leaves, so
	// it is silently exempt from the census -- and the earlier form of this guard
	// (leaves < len(corpus)) tolerated a QUARTER of the corpus going unplannable
	// while still reporting a clean bill. The population is asserted directly
	// instead: every curated shape must plan, and the classes the census exists
	// to cover must actually appear in it.
	if planned != len(corpus) {
		t.Fatalf("%d of %d corpus shapes did not plan, so they are exempt from this "+
			"census without saying so:\n  %s",
			len(corpus)-planned, len(corpus), strings.Join(unplanned, "\n  "))
	}
	if leaves < planned {
		t.Fatalf("walked %d scan/index leaves across %d planned queries — fewer than "+
			"one per query means the walk is not reaching leaves and the count below "+
			"is vacuous", leaves, planned)
	}
	// The aggregate class is the one this walk was blind to: GetChildren() returns
	// nil and the index scan is a field. If the corpus stops producing one, the
	// class silently leaves the census exactly as it was absent before.
	if aggregates == 0 {
		t.Fatalf("no RecordQueryAggregateIndexPlan appeared, so the class whose costing " +
			"this census was extended to cover is not being exercised at all")
	}
	if untyped != 0 {
		t.Fatalf("%d of %d physical leaves carry NO record types, so they price "+
			"themselves from the whole-store total while their typed siblings price "+
			"one table — the two disagree about the same data inside one plan:\n  %s",
			untyped, leaves, strings.Join(offenders, "\n  "))
	}
	t.Logf("walked %d scan/index leaves across %d queries; all name their record types",
		leaves, len(corpus))
}

// walkPlanLeaves visits every scan-like leaf in the tree, handing the visitor
// the plan and the record-type list it prices itself from.
func walkPlanLeaves(p plans.RecordQueryPlan, visit func(plans.RecordQueryPlan, []string)) {
	if p == nil {
		return
	}
	switch leaf := p.(type) {
	case *plans.RecordQueryScanPlan:
		visit(p, leaf.GetRecordTypes())
	case *plans.RecordQueryIndexPlan:
		visit(p, leaf.GetRecordTypes())
	case *plans.RecordQueryCoveringIndexPlan:
		// A covering plan holds its index scan as a FIELD, not a child, so
		// GetChildren() does not reach it. Descending only through children
		// silently skipped every covering leaf -- which since RFC-220 is every
		// index-backed access, so the census counted 9 leaves for 12 queries
		// and the vacuity guard is what surfaced it.
		if inner, ok := plans.IndexPlanOf(leaf); ok {
			walkPlanLeaves(inner, visit)
		}
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		// Same shape: the fetched plan is a field.
		walkPlanLeaves(leaf.GetInner(), visit)
	case *plans.RecordQueryAggregateIndexPlan:
		// THE SAME FIELD-NOT-CHILD SHAPE A THIRD TIME. GetChildren() returns
		// nil and the index scan is a field, so this class was invisible to
		// this walk -- the identical blind spot the covering case had already
		// exposed, left uncorrected one type over.
		//
		// It is visited in its OWN right as well as descended into, because it
		// prices itself: RecordQueryAggregateIndexPlan.HintCost derives a
		// cardinality rather than inheriting its child's.
		if ip := leaf.GetIndexPlan(); ip != nil {
			visit(p, ip.GetRecordTypes())
			walkPlanLeaves(ip, visit)
		} else {
			visit(p, nil)
		}
	}
	for _, c := range p.GetChildren() {
		walkPlanLeaves(c, visit)
	}
}
