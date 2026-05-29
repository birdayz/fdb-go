package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestMemoEqual_InternsAliasVariants is the RFC-039/PR-A core property: two
// expressions identical except for their quantifier alias (referenced in
// node-info) are MemoEqual — because memoEqual builds the node's own quantifier
// alias map and feeds it to the (foundation's) alias-aware EqualsWithoutChildren.
// SemanticEquals (which passes only the empty incoming map at the top) does NOT
// see them equal — that's the gap memoEqual closes.
func TestMemoEqual_InternsAliasVariants(t *testing.T) {
	t.Parallel()
	scanRef := InitialOf(NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	filter := func(k int64) RelationalExpression {
		q := ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(values.NewQuantifiedObjectValue(q.GetAlias()),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: k}})
		return NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	}
	a := filter(1) // fresh alias q$N
	b := filter(1) // fresh alias q$M, same shape

	if !MemoEqual(a, b) {
		t.Fatal("alias-variant filters must be MemoEqual (the activation property)")
	}
	// Contrast: plain SemanticEquals (empty map at top) misses them — the gap.
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("precondition: SemanticEquals should NOT see alias-variants equal (top-level empty map) — test vacuous otherwise")
	}
	// Negative: different constant ⇒ not MemoEqual.
	if MemoEqual(a, filter(2)) {
		t.Fatal("filters with different constants must not be MemoEqual")
	}
}
