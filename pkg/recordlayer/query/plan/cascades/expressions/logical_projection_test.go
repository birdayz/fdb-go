package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalProjection_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v1 := values.NewBooleanValue(true)
	v2 := values.NewNullValue(values.UnknownType)
	p := NewLogicalProjectionExpression([]values.Value{v1, v2}, q)
	if got := p.GetProjectedValues(); len(got) != 2 {
		t.Fatalf("projected size=%d, want 2", len(got))
	}
	if p.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("inner alias mismatch")
	}
	if p.CanCorrelate() {
		t.Fatal("projection should not anchor a correlation")
	}
	// GetResultValue passes through to inner's flowed object — must
	// carry the inner's alias.
	resultCorr := p.GetResultValue().(*values.QuantifiedObjectValue).GetCorrelatedTo()
	if _, ok := resultCorr[q.GetAlias()]; !ok {
		t.Fatal("GetResultValue does not carry the inner Quantifier's alias")
	}
}

func TestLogicalProjection_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	src := []values.Value{values.NewBooleanValue(true)}
	p := NewLogicalProjectionExpression(src, q)
	src[0] = values.NewBooleanValue(false)
	if got, err := p.GetProjectedValues()[0].(*values.BooleanValue).Evaluate(nil); err != nil || got != true {
		t.Fatal("constructor failed to defensively copy projection list")
	}
}

func TestLogicalProjection_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v1 := values.NewBooleanValue(true)
	v2 := values.NewBooleanValue(false)
	p1 := NewLogicalProjectionExpression([]values.Value{v1}, q)
	p1Twin := NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q)
	p2 := NewLogicalProjectionExpression([]values.Value{v2}, q)
	pBoth := NewLogicalProjectionExpression([]values.Value{v1, v2}, q)
	if !p1.EqualsWithoutChildren(p1Twin, EmptyAliasMap()) {
		t.Fatal("structurally identical projections reported unequal")
	}
	if p1.EqualsWithoutChildren(p2, EmptyAliasMap()) {
		t.Fatal("projections with different values reported equal")
	}
	if p1.EqualsWithoutChildren(pBoth, EmptyAliasMap()) {
		t.Fatal("projections with different lengths reported equal")
	}
	if p1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("projection reported equal to non-projection expression")
	}
}

func TestLogicalProjection_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v := values.NewBooleanValue(true)
	p1 := NewLogicalProjectionExpression([]values.Value{v}, q)
	p2 := NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q)
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal projections produced different hash codes")
	}
}
