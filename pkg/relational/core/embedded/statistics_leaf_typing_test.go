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
CREATE INDEX customers_by_region ON customers (region)`

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
	}

	// A provider that answers a DIFFERENT number for the whole store than for
	// any single table, so a leaf taking the fallback is distinguishable from
	// one that is correctly typed — not merely by this test's assertion, but in
	// the cost the planner would actually see.
	stats := properties.MapStatistics{
		PerType:  map[string]float64{"ORDERS": 150, "CUSTOMERS": 40},
		Fallback: properties.LeafScanCardinality,
	}

	var leaves, untyped int
	var offenders []string
	for _, sql := range corpus {
		plan, err := PlanPhysicalForTest(sql, ddl, stats)
		if err != nil {
			// A shape this planner cannot express is not this test's subject;
			// it simply contributes no leaves. The vacuity guard below is what
			// stops the whole corpus quietly becoming unplannable.
			t.Logf("not planned (contributes no leaves): %s: %v", sql, err)
			continue
		}
		walkPlanLeaves(plan, func(p plans.RecordQueryPlan, types []string) {
			leaves++
			if len(types) == 0 {
				untyped++
				offenders = append(offenders, fmt.Sprintf("%T in %q", p, sql))
			}
		})
	}

	// THE VACUITY GUARD. Zero leaves means the corpus stopped planning and the
	// zero below is a statement about an empty set, not about the planner.
	if leaves < len(corpus) {
		t.Fatalf("walked %d scan/index leaves across %d queries — fewer than one per "+
			"query means the corpus is not planning and the count below is vacuous",
			leaves, len(corpus))
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
	}
	for _, c := range p.GetChildren() {
		walkPlanLeaves(c, visit)
	}
}
