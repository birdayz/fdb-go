package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalSort_Unsorted(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	s := UnsortedLogicalSortExpression(q)
	if !s.IsUnsorted() {
		t.Fatal("UnsortedLogicalSortExpression reports IsUnsorted=false")
	}
	if got := s.GetSortKeys(); len(got) != 0 {
		t.Fatalf("unsorted has %d keys, want 0", len(got))
	}
	if _, ok := s.GetResultValue().(*values.QuantifiedObjectValue).GetCorrelatedTo()[q.GetAlias()]; !ok {
		t.Fatal("ResultValue does not carry the inner Quantifier's alias")
	}
}

func TestLogicalSort_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys := []SortKey{
		{Value: values.NewBooleanValue(true), Reverse: false},
		{Value: values.NewBooleanValue(false), Reverse: true},
	}
	s := NewLogicalSortExpression(keys, q)
	if s.IsUnsorted() {
		t.Fatal("non-empty sort keys but IsUnsorted=true")
	}
	if got := s.GetSortKeys(); len(got) != 2 {
		t.Fatalf("size=%d, want 2", len(got))
	}
	if s.GetSortKeys()[0].Reverse {
		t.Fatal("first key Reverse should be false (ASC)")
	}
	if !s.GetSortKeys()[1].Reverse {
		t.Fatal("second key Reverse should be true (DESC)")
	}
	if s.CanCorrelate() {
		t.Fatal("sort should not anchor a correlation")
	}
}

func TestLogicalSort_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	src := []SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}
	s := NewLogicalSortExpression(src, q)
	src[0].Reverse = true
	if s.GetSortKeys()[0].Reverse {
		t.Fatal("constructor failed to defensively copy sort keys")
	}
}

func TestLogicalSort_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys1 := []SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}
	keys2 := []SortKey{{Value: values.NewBooleanValue(true), Reverse: true}}   // different direction
	keys3 := []SortKey{{Value: values.NewBooleanValue(false), Reverse: false}} // different value
	s1 := NewLogicalSortExpression(keys1, q)
	s1Twin := NewLogicalSortExpression([]SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}, q)
	s2 := NewLogicalSortExpression(keys2, q)
	s3 := NewLogicalSortExpression(keys3, q)
	if !s1.EqualsWithoutChildren(s1Twin, EmptyAliasMap()) {
		t.Fatal("structurally identical sorts reported unequal")
	}
	if s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("sorts with different reverse flags reported equal")
	}
	if s1.EqualsWithoutChildren(s3, EmptyAliasMap()) {
		t.Fatal("sorts with different values reported equal")
	}
	if s1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("sort reported equal to non-sort expression")
	}
}

func TestLogicalSort_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys1 := []SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}
	keys2 := []SortKey{{Value: values.NewBooleanValue(true), Reverse: true}}
	s1 := NewLogicalSortExpression(keys1, q)
	s1Twin := NewLogicalSortExpression([]SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}, q)
	s2 := NewLogicalSortExpression(keys2, q)
	if s1.HashCodeWithoutChildren() != s1Twin.HashCodeWithoutChildren() {
		t.Fatal("structurally equal sorts produced different hash codes")
	}
	if s1.HashCodeWithoutChildren() == s2.HashCodeWithoutChildren() {
		t.Fatal("sorts with different reverse flags produced identical hashes (collision unlikely)")
	}
}
