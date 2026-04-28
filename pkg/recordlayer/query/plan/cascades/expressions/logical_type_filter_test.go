package expressions

import (
	"reflect"
	"testing"
)

func TestLogicalTypeFilter_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	tf := NewLogicalTypeFilterExpression([]string{"Order", "Customer"}, q)
	// Result is canonical (sorted, deduped).
	want := []string{"Customer", "Order"}
	if got := tf.GetRecordTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("types=%v, want %v", got, want)
	}
	if tf.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("inner alias mismatch")
	}
	if tf.CanCorrelate() {
		t.Fatal("type filter should not anchor a correlation")
	}
}

func TestLogicalTypeFilter_Construction_Dedupes(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	tf := NewLogicalTypeFilterExpression([]string{"Order", "Order", "Customer", "Customer"}, q)
	want := []string{"Customer", "Order"}
	if got := tf.GetRecordTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("types=%v, want %v", got, want)
	}
}

func TestLogicalTypeFilter_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	tf1 := NewLogicalTypeFilterExpression([]string{"Order", "Customer"}, q)
	tf1Twin := NewLogicalTypeFilterExpression([]string{"Customer", "Order"}, q) // different order
	tf2 := NewLogicalTypeFilterExpression([]string{"Order"}, q)                 // subset
	tf3 := NewLogicalTypeFilterExpression([]string{"Order", "Sale"}, q)         // different name
	if !tf1.EqualsWithoutChildren(tf1Twin, EmptyAliasMap()) {
		t.Fatal("type filters with permuted but identical sets reported unequal")
	}
	if tf1.EqualsWithoutChildren(tf2, EmptyAliasMap()) {
		t.Fatal("subset reported equal")
	}
	if tf1.EqualsWithoutChildren(tf3, EmptyAliasMap()) {
		t.Fatal("filters with different names reported equal")
	}
	if tf1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("type filter reported equal to non-filter expression")
	}
}

func TestLogicalTypeFilter_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	tf1 := NewLogicalTypeFilterExpression([]string{"Order", "Customer"}, q)
	tf1Twin := NewLogicalTypeFilterExpression([]string{"Customer", "Order"}, q)
	tf2 := NewLogicalTypeFilterExpression([]string{"Order"}, q)
	if tf1.HashCodeWithoutChildren() != tf1Twin.HashCodeWithoutChildren() {
		t.Fatal("permuted-but-equal type filters produced different hashes")
	}
	if tf1.HashCodeWithoutChildren() == tf2.HashCodeWithoutChildren() {
		t.Fatal("disjoint type filters produced identical hashes (collision unlikely)")
	}
}

func TestLogicalTypeFilter_Empty(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	tf := NewLogicalTypeFilterExpression(nil, q)
	if got := tf.GetRecordTypes(); len(got) != 0 {
		t.Fatalf("empty filter returned %v, want []", got)
	}
}
