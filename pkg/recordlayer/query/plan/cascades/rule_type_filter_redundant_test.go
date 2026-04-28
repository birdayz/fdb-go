package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestTypeFilterRedundantOverScanRule_ExactMatch(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, q)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order", "Customer", "Product"}, q)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_FilterIsStrictSubset_NoFire(t *testing.T) {
	t.Parallel()
	// Scan has Order + Customer; filter allows only Order. The filter
	// IS doing work — rejects Customer rows. Don't eliminate.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, q)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (filter narrows the scan)", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_DisjointTypes_NoFire(t *testing.T) {
	t.Parallel()
	// Scan has Order; filter allows only Customer. Result is empty,
	// but the rule should still decline (the scan-bare result would
	// silently include rejected types). No-fire is correct.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Customer"}, q)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (disjoint types)", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_BothEmpty_Fires(t *testing.T) {
	t.Parallel()
	// Both filter and scan are empty record-type sets — the empty
	// scan ⊆ empty filter trivially. The rule fires (even though
	// both are degenerate). The semantics are "scan produces no
	// rows, filter rejects nothing" — eliminating the filter doesn't
	// change output. This is a corner case but worth pinning so an
	// over-cautious refactor doesn't accidentally exempt it.
	scan := expressions.NewFullUnorderedScanExpression(nil, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	tf := expressions.NewLogicalTypeFilterExpression(nil, q)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d on (empty, empty), want 1", len(yielded))
	}
}

func TestTypeFilterRedundantOverScanRule_DeclinesOnNonScanInner(t *testing.T) {
	t.Parallel()
	// Inner is a Filter, not a Scan — the rule is specific to Scans.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filter := expressions.NewLogicalFilterExpression(nil, innerQ)
	tfQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	tf := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, tfQ)
	ref := expressions.InitialOf(tf)
	yielded := FireExpressionRule(NewTypeFilterRedundantOverScanRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (inner is Filter, not Scan)", len(yielded))
	}
}
