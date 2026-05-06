package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestWalk_Nil(t *testing.T) {
	t.Parallel()
	visited := 0
	Walk(nil, func(_ RelationalExpression) bool {
		visited++
		return true
	})
	if visited != 0 {
		t.Fatalf("nil walk visited %d nodes, want 0", visited)
	}
}

func TestWalk_Leaf(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	visited := 0
	Walk(scan, func(_ RelationalExpression) bool {
		visited++
		return true
	})
	if visited != 1 {
		t.Fatalf("leaf walk visited %d nodes, want 1", visited)
	}
}

func TestWalk_Tree(t *testing.T) {
	t.Parallel()
	// Filter over Filter over Scan = 3 nodes
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	innerF := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerQ := ForEachQuantifier(InitialOf(innerF))
	outerF := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerQ)

	if got := Size(outerF); got != 3 {
		t.Fatalf("Size(outerF)=%d, want 3", got)
	}
}

func TestWalk_ShortCircuit(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)

	// Visit returns false from the root — short-circuits the walk.
	visited := 0
	Walk(f, func(_ RelationalExpression) bool {
		visited++
		return false // don't descend
	})
	if visited != 1 {
		t.Fatalf("short-circuit walk visited %d nodes, want 1 (just the root)", visited)
	}
}

func TestWalk_FindsAggregateLeaf(t *testing.T) {
	t.Parallel()
	// Demonstrate a real use case: search for a specific operator type
	// in a tree.
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := ForEachQuantifier(InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	d := NewLogicalDistinctExpression(ForEachQuantifier(InitialOf(f)))

	foundDistinct := false
	Walk(d, func(e RelationalExpression) bool {
		if _, ok := e.(*LogicalDistinctExpression); ok {
			foundDistinct = true
			return false // stop descending
		}
		return true
	})
	if !foundDistinct {
		t.Fatal("walk didn't find LogicalDistinctExpression")
	}
}
