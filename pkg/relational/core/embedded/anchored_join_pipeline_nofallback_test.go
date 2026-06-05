package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestAnchoredJoin_NoOpaqueFallback_FullPipeline is the RFC-077 7.6 NO-FALLBACK
// assertion over the FULL SQL→plan pipeline (PlanQueryForTest: VisitQuery → real
// logical plan → TranslateToCascadesWithSubqueries → Cascades). The sibling
// TestAnchoredJoin_NoOpaqueFallback drives the translator directly via the
// catalog-aware logical builder, which cannot construct derived-table / CTE /
// subquery-in-FROM legs (they translate to nil there) — exactly the shapes that
// most recently fell back to the opaque merge. This test plans them through the
// SAME pipeline the SQL engine uses and asserts the opaque JoinMergeAllValue /
// SeedValue is NEVER constructed: every reachable join-leg shape resolves to the
// source-anchored RecordConstructorValue.
//
//   - real-table 2/3/4-way joins (chain + star, projecting a buried column);
//   - correlated scalar subqueries — aggregate (COUNT/MAX), GROUP BY, and the
//     NON-aggregate form whose scalarCol keeps its source qualifier ("C.NAME"),
//     which NewScalarSubqueryAnchoredRecord re-qualifies under the inner alias;
//   - derived tables / subqueries-in-FROM and aggregate subqueries as join legs
//     (LogicalCTE-wrapped — their columns derive from the CTE body);
//   - CTE references as join legs; nested + 3-way derived tables.
//
// Runs SERIALLY (no t.Parallel) because OpaqueMergeConstructions() is a
// process-global counter; a parallel test constructing opaque merges would make
// the delta unattributable.
func TestAnchoredJoin_NoOpaqueFallback_FullPipeline(t *testing.T) {
	const schema = `
CREATE TABLE ORDERS (id BIGINT NOT NULL, customer_id BIGINT, price BIGINT, PRIMARY KEY (id))
CREATE TABLE CUSTOMERS (id BIGINT NOT NULL, name STRING, price BIGINT, PRIMARY KEY (id))
CREATE TABLE LINEITEM (id BIGINT NOT NULL, oid BIGINT, qty BIGINT, PRIMARY KEY (id))
`
	corpus := []string{
		// real-table joins
		"SELECT o.id, c.name FROM orders o, customers c WHERE o.price = c.price",
		"SELECT o.id FROM orders o, customers c, lineitem l WHERE o.price = c.price AND c.id = l.oid",
		"SELECT c.name FROM orders o, customers c, lineitem l WHERE o.price = c.price AND c.id = l.oid",
		"SELECT o.id FROM orders o, customers c, lineitem l, orders o2 WHERE o.price = c.price AND c.id = l.oid AND l.qty = o2.price",
		"SELECT c.name FROM customers c, orders o, lineitem l, orders o2 WHERE o.price = c.price AND l.qty = c.price AND o2.price = c.price",
		"SELECT o.id FROM orders o JOIN customers c ON o.price = c.price JOIN lineitem l ON c.id = l.oid",
		// correlated scalar subqueries
		"SELECT o.id, (SELECT c.name FROM customers c WHERE c.id = o.customer_id) FROM orders o",
		"SELECT o.id, (SELECT COUNT(*) FROM customers c WHERE c.price = o.price) FROM orders o",
		"SELECT o.id, (SELECT c.name FROM customers c WHERE c.price = o.price GROUP BY c.name) FROM orders o",
		"SELECT id FROM orders o WHERE price > (SELECT MAX(price) FROM customers c WHERE c.id = o.customer_id)",
		// derived tables / aggregate subqueries / CTE references as join legs
		"SELECT o.id FROM orders o, (SELECT id, name FROM customers) c WHERE o.id = c.id",
		"SELECT x.c FROM orders o, (SELECT price, COUNT(*) AS c FROM customers GROUP BY price) x WHERE o.price = x.price",
		"WITH cc AS (SELECT id, name FROM customers) SELECT o.id FROM orders o, cc WHERE o.id = cc.id",
		"SELECT o.id FROM orders o, (SELECT id, name FROM customers) c, lineitem l WHERE o.id = c.id AND l.oid = o.id",
		"SELECT t.id FROM orders o, (SELECT id FROM (SELECT id, name FROM customers) z) t WHERE o.id = t.id",
		"SELECT o.id FROM orders o, (SELECT COUNT(*) AS n FROM customers) c WHERE o.price = c.n",
	}

	before := values.OpaqueMergeConstructions()
	for _, sql := range corpus {
		if _, err := PlanQueryForTest(sql, schema, nil); err != nil {
			t.Fatalf("plan failed for %q: %v", sql, err)
		}
	}
	if delta := values.OpaqueMergeConstructions() - before; delta != 0 {
		t.Errorf("RFC-077 7.6 NO-FALLBACK (full pipeline) FAILED: %d opaque JoinMergeAllValue/Seed "+
			"constructions across %d planning queries — a join-leg shape took the opaque arm instead "+
			"of the source-anchored RecordConstructorValue. The opaque types cannot be retired while "+
			"any reachable shape uses them.", delta, len(corpus))
	}
}
