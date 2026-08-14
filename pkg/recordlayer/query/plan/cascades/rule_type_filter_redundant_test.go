package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

func TestTypeFilterRedundantOverScanRule_ExactMatch(t *testing.T) {
	t.Parallel()
	scan := typeRewriteScan("Order")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter([]string{"Order"}, q)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded %T, want *FullUnorderedScanExpression", yielded[0])
	}
}

func TestTypeFilterRedundantOverScanRule_FilterIsSuperset(t *testing.T) {
	t.Parallel()
	// Scan has only Order; filter allows Order + Customer + Product.
	// Scan ⊆ filter → filter is redundant.
	scan := typeRewriteScan("Order")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter([]string{"Order", "Customer", "Product"}, q)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_FilterIsStrictSubset_NoFire(t *testing.T) {
	t.Parallel()
	// Scan has Order + Customer; filter allows only Order. The filter
	// IS doing work — rejects Customer rows. Don't eliminate.
	scan := typeRewriteScan("Order", "Customer")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter([]string{"Order"}, q)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (filter narrows the scan)", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_DisjointTypes_NoFire(t *testing.T) {
	t.Parallel()
	// Scan has Order; filter allows only Customer. Result is empty,
	// but the rule should still decline (the scan-bare result would
	// silently include rejected types). No-fire is correct.
	scan := typeRewriteScan("Order")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter([]string{"Customer"}, q)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (disjoint types)", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_BothNilTypes_Fires(t *testing.T) {
	t.Parallel()
	// Both filter and scan carry nil record-type sets. nil does NOT mean
	// "empty / no rows" — it means "ALL record types" (an unrestricted full
	// scan / a filter that rejects nothing). So the filter's type set ⊇ the
	// scan's type set (all ⊇ all) and the filter is redundant over the scan:
	// eliminating it doesn't change output. The rule fires. (Worth pinning so a
	// refactor doesn't accidentally exempt the nil/nil case — and so nobody
	// resurrects the "nil = empty source" misconception that broke LIMIT 0.)
	scan := typeRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := typeRewriteFilter(nil, q)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d on (empty, empty), want 1", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_DeclinesOnNonScanInner(t *testing.T) {
	t.Parallel()
	// Inner is a Filter, not a Scan — the rule is specific to Scans.
	scan := typeRewriteScan("Order")
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filter := mustTypeRewriteConstruct(expressions.NewLogicalFilterExpression(nil, innerQ))
	tfQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	tf := typeRewriteFilter([]string{"Order"}, tfQ)
	ref := expressions.InitialOf(tf)
	yielded := fireTypeRewriteRule(t, NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (inner is Filter, not Scan)", len(yielded))
	}
}
