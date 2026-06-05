package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
)

// TestAnchoredJoin_NoOpaqueFallback is the RFC-077 7.6 NO-FALLBACK assertion
// (Graefe binding condition (b)). It plans a corpus of real-table multi-way joins
// — chains, a star, and projections that bury a middle table's column — through
// the SAME translator+planner pipeline the SQL engine uses (md threaded), and
// asserts that the opaque JoinMergeAllValue / SeedValue is NEVER constructed: the
// source-anchored RecordConstructorValue replaces it at every real-table join site
// (the translator seed AND PartitionSelectRule's re-enumeration). If ANY real-table
// site falls back to the opaque merge, OpaqueMergeConstructions() climbs and this
// test FAILS — the sentinel that must be green before the opaque types are deleted.
//
// It runs SERIALLY (no t.Parallel) because OpaqueMergeConstructions() is a
// process-global counter; a parallel test constructing opaque merges would make
// the delta unattributable.
func TestAnchoredJoin_NoOpaqueFallback(t *testing.T) {
	md := buildTestMetaData(t)

	corpus := []string{
		// 2-way (the binary seed itself anchors).
		"SELECT o.order_id, c.name FROM Order o, Customer c WHERE o.price = c.price",
		// 3-way chain — re-enumeration collapses ≥2 tables into a merge quantifier.
		"SELECT o.order_id FROM Order o, Customer c, TypedRecord t WHERE o.price = c.price AND c.customer_id = t.id",
		// 3-way projecting the BURIED middle table's column (the load-bearing case:
		// c is necessarily inside a join and its column must flow up the merge spine).
		"SELECT c.name FROM Order o, Customer c, TypedRecord t WHERE o.price = c.price AND c.customer_id = t.id",
		// 4-way chain.
		"SELECT o.order_id FROM Order o, Customer c, TypedRecord t, Order o2 " +
			"WHERE o.price = c.price AND c.customer_id = t.id AND t.val_int64 = o2.order_id",
		// 4-way star (hub = c; o, t, o2 all join c).
		"SELECT c.name FROM Customer c, Order o, TypedRecord t, Order o2 " +
			"WHERE o.price = c.price AND t.price = c.price AND o2.price = c.price",
		// explicit INNER JOIN chain (same shape via JOIN syntax).
		"SELECT o.order_id FROM Order o JOIN Customer c ON o.price = c.price " +
			"JOIN TypedRecord t ON c.customer_id = t.id",
	}

	before := values.OpaqueMergeConstructions()
	planned := 0
	for _, sql := range corpus {
		sq := parseSelect(t, sql)
		op, err := buildLogicalPlanForSelectWithCatalog(sq, md)
		if err != nil || op == nil {
			t.Fatalf("build logical plan failed for %q: op=%v err=%v", sql, op, err)
		}
		ref, _ := query.TranslateToCascadesWithSubqueries(op, md)
		if ref == nil {
			t.Fatalf("translate-to-cascades returned nil for %q", sql)
		}
		rules := append(cascades.DefaultExpressionRules(), cascades.RewritingRules()...)
		p := cascades.NewPlanner(rules, nil).
			WithImplementationRules(cascades.DefaultImplementationRules()).
			WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
			WithMaxTasks(100_000)
		best, _, perr := p.Plan(ref)
		if perr != nil || best == nil {
			t.Fatalf("plan failed for %q: %v", sql, perr)
		}
		planned++
	}

	delta := values.OpaqueMergeConstructions() - before
	if delta != 0 {
		t.Errorf("RFC-077 7.6 NO-FALLBACK assertion FAILED: %d opaque JoinMergeAllValue/Seed "+
			"constructions across %d real-table multi-way joins — a real-table join site took the "+
			"opaque arm instead of the source-anchored RecordConstructorValue. The opaque types "+
			"cannot be retired while any real-table site uses them.", delta, planned)
	}
}
