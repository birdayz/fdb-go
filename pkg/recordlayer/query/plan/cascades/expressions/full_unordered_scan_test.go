package expressions

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFullUnorderedScan_Construction(t *testing.T) {
	t.Parallel()
	s := NewFullUnorderedScanExpression([]string{"Order", "Customer", "Order"}, values.UnknownType)
	want := []string{"Customer", "Order"}
	if got := s.GetRecordTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recordTypes=%v, want %v (sorted+deduped)", got, want)
	}
	if s.GetFlowedType() != values.UnknownType {
		t.Fatal("flowed type not preserved")
	}
	if s.CanCorrelate() {
		t.Fatal("scan should not anchor a correlation")
	}
	if got := s.GetQuantifiers(); got != nil {
		t.Fatalf("scan has quantifiers: %v", got)
	}
}

func TestFullUnorderedScan_NilFlowedType(t *testing.T) {
	t.Parallel()
	s := NewFullUnorderedScanExpression([]string{"Order"}, nil)
	if s.GetFlowedType() != values.UnknownType {
		t.Fatal("nil flowedType not normalised to UnknownType")
	}
}

func TestFullUnorderedScan_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	s1 := NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	s1Twin := NewFullUnorderedScanExpression([]string{"Customer", "Order"}, values.UnknownType)
	s2 := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	if !s1.EqualsWithoutChildren(s1Twin, EmptyAliasMap()) {
		t.Fatal("permuted-equal scans reported unequal")
	}
	if s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("subset reported equal")
	}
}

func TestFullUnorderedScan_HashCodeStable(t *testing.T) {
	t.Parallel()
	s1 := NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	s1Twin := NewFullUnorderedScanExpression([]string{"Customer", "Order"}, values.UnknownType)
	s2 := NewFullUnorderedScanExpression([]string{"Customer"}, values.UnknownType)
	if s1.HashCodeWithoutChildren() != s1Twin.HashCodeWithoutChildren() {
		t.Fatal("permuted-equal scans produced different hashes")
	}
	if s1.HashCodeWithoutChildren() == s2.HashCodeWithoutChildren() {
		t.Fatal("disjoint scans produced identical hashes (collision unlikely)")
	}
}

// TestRealExpressionTree builds an actual (Scan → Filter → Projection)
// tree using the real FullUnorderedScanExpression as the leaf — proves
// the seed types compose without the test-only leafScan stub.
func TestRealExpressionTree(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := InitialOf(scan)
	scanQ := ForEachQuantifier(scanRef)

	filter := NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	filterRef := InitialOf(filter)
	filterQ := ForEachQuantifier(filterRef)

	proj := NewLogicalProjectionExpression(
		[]values.Value{values.NewBooleanValue(true)},
		filterQ,
	)

	// Walk the tree: Projection → Filter → Scan
	if len(proj.GetQuantifiers()) != 1 {
		t.Fatal("projection should have 1 quantifier")
	}
	pInner := proj.GetQuantifiers()[0].GetRangesOver().Get()
	if _, ok := pInner.(*LogicalFilterExpression); !ok {
		t.Fatalf("projection inner=%T, want *LogicalFilterExpression", pInner)
	}
	fInner := pInner.GetQuantifiers()[0].GetRangesOver().Get()
	if _, ok := fInner.(*FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner=%T, want *FullUnorderedScanExpression", fInner)
	}
	if len(fInner.GetQuantifiers()) != 0 {
		t.Fatalf("scan has %d quantifiers, want 0", len(fInner.GetQuantifiers()))
	}
}

// TestSemanticEquals_FullTree verifies SemanticEquals walks all the
// way to leaves through real RelationalExpressions.
func TestSemanticEquals_FullTree(t *testing.T) {
	t.Parallel()
	build := func() RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := ForEachQuantifier(InitialOf(scan))
		return NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			scanQ,
		)
	}
	a := build()
	b := build()
	if !SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("two identically-built (filter over scan) trees reported semantically unequal")
	}
}

// TestSemanticEquals_DifferentLeaf walks the tree and detects a
// difference at the leaf level even when the operator chain is
// identical.
func TestSemanticEquals_DifferentLeaf(t *testing.T) {
	t.Parallel()
	build := func(recordType string) RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{recordType}, values.UnknownType)
		scanQ := ForEachQuantifier(InitialOf(scan))
		return NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			scanQ,
		)
	}
	a := build("Order")
	b := build("Customer")
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("tree comparison didn't propagate down to leaf — disjoint scans reported semantically equal")
	}
}
