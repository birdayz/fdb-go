package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Each operator's GetCorrelatedToWithoutChildren walks the
// node-information's Value / Predicate trees, collecting every
// QuantifiedObjectValue's CorrelationIdentifier. These tests pin that
// the wiring works — a Quantifier alias buried inside a predicate /
// projection / sort key surfaces in the correlation set.

func TestLogicalFilter_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	// Comparison predicate referencing q's flowed object.
	pred := predicates.NewComparisonPredicate(
		q.GetFlowedObjectValue(),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("filter correlation set %v doesn't contain q's alias %v", got, q.GetAlias())
	}
}

func TestLogicalFilter_GetCorrelatedToWithoutChildren_NoCorrelation(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	// Pure constant predicate — no correlations.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if len(got) != 0 {
		t.Fatalf("filter over constant predicate has correlations: %v", got)
	}
}

func TestLogicalProjection_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	p := NewLogicalProjectionExpression([]values.Value{q.GetFlowedObjectValue()}, q)
	got := p.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("projection correlation set %v doesn't contain q's alias", got)
	}
}

func TestLogicalSort_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	s := NewLogicalSortExpression([]SortKey{{Value: q.GetFlowedObjectValue(), Reverse: false}}, q)
	got := s.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("sort correlation set %v doesn't contain q's alias", got)
	}
}

func TestSelect_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	pred := predicates.NewComparisonPredicate(
		q.GetFlowedObjectValue(),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	rv := q.GetFlowedObjectValue()
	s := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{pred})
	got := s.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("select correlation set %v doesn't contain q's alias", got)
	}
}

func TestUpdate_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	upd := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: q.GetFlowedObjectValue()},
	})
	got := upd.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("update correlation set %v doesn't contain q's alias", got)
	}
}

func TestLogicalIntersection_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	keys := []values.Value{q.GetFlowedObjectValue()} // references q's alias
	x := NewLogicalIntersectionExpression(
		[]Quantifier{q},
		keys,
	)
	got := x.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("intersection correlation set %v doesn't contain comparison-key alias %v", got, q.GetAlias())
	}
}

func TestLeafExpressions_NoCorrelations(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	if got := scan.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("scan correlation set non-empty: %v", got)
	}

	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	d := NewLogicalDistinctExpression(q)
	if got := d.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("distinct correlation set non-empty: %v", got)
	}
	u := NewLogicalUnionExpression([]Quantifier{q})
	if got := u.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("union correlation set non-empty: %v", got)
	}
}

func TestCorrelationWalking_PicksUpDeepReference(t *testing.T) {
	t.Parallel()
	// Wrap q's flowed object inside an Arithmetic + Comparison, prove
	// the walker descends into nested Values.
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	deep := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  q.GetFlowedObjectValue(),
		Right: values.NewBooleanValue(true),
	}
	pred := predicates.NewComparisonPredicate(
		deep,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(true)},
	)
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, q)
	got := f.GetCorrelatedToWithoutChildren()
	if _, ok := got[q.GetAlias()]; !ok {
		t.Fatalf("walker didn't descend into nested Arithmetic — got %v", got)
	}
}
