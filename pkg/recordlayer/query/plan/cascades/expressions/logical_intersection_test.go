package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalIntersection_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	qs := []Quantifier{
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
	}
	keys := []values.Value{values.NewBooleanValue(true)}
	x := NewLogicalIntersectionExpression(qs, keys)
	if got := x.GetQuantifiers(); len(got) != 2 {
		t.Fatalf("size=%d, want 2", len(got))
	}
	if got := x.GetComparisonKeyValues(); len(got) != 1 {
		t.Fatalf("comparison keys size=%d, want 1", len(got))
	}
	if x.CanCorrelate() {
		t.Fatal("intersection should not anchor a correlation")
	}
}

func TestLogicalIntersection_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	srcQs := []Quantifier{q}
	srcKs := []values.Value{values.NewBooleanValue(true)}
	x := NewLogicalIntersectionExpression(srcQs, srcKs)
	srcQs[0] = ForEachQuantifier(InitialOf(leaf))
	srcKs[0] = values.NewBooleanValue(false)
	if x.GetQuantifiers()[0].GetAlias() != q.GetAlias() {
		t.Fatal("quantifier list not defensively copied")
	}
	if v := x.GetComparisonKeyValues()[0].(*values.BooleanValue); mustEvaluate(v, nil) != true {
		t.Fatal("comparison key list not defensively copied")
	}
}

func TestLogicalIntersection_EqualsWithoutChildren_SameKeys(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keysA := []values.Value{values.NewBooleanValue(true)}
	keysB := []values.Value{values.NewBooleanValue(true)} // same Explain text
	x1 := NewLogicalIntersectionExpression([]Quantifier{q}, keysA)
	x2 := NewLogicalIntersectionExpression([]Quantifier{q}, keysB)
	if !x1.EqualsWithoutChildren(x2, EmptyAliasMap()) {
		t.Fatal("intersections with structurally identical comparison keys reported unequal")
	}
}

func TestLogicalIntersection_EqualsWithoutChildren_DifferentKeys(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keysT := []values.Value{values.NewBooleanValue(true)}
	keysF := []values.Value{values.NewBooleanValue(false)}
	x1 := NewLogicalIntersectionExpression([]Quantifier{q}, keysT)
	x2 := NewLogicalIntersectionExpression([]Quantifier{q}, keysF)
	if x1.EqualsWithoutChildren(x2, EmptyAliasMap()) {
		t.Fatal("intersections with different comparison keys reported equal")
	}
}

func TestLogicalIntersection_NotEqualToUnion(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	x := NewLogicalIntersectionExpression([]Quantifier{q}, nil)
	u := NewLogicalUnionExpression([]Quantifier{q})
	if x.EqualsWithoutChildren(u, EmptyAliasMap()) {
		t.Fatal("intersection reported equal to union")
	}
}

func TestLogicalIntersection_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys := []values.Value{values.NewBooleanValue(true)}
	x1 := NewLogicalIntersectionExpression([]Quantifier{q}, keys)
	x2 := NewLogicalIntersectionExpression([]Quantifier{q}, []values.Value{values.NewBooleanValue(true)})
	if x1.HashCodeWithoutChildren() != x2.HashCodeWithoutChildren() {
		t.Fatal("intersections with structurally identical comparison keys produced different hashes")
	}
}
