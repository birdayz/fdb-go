package properties

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

type stubScanProvider struct {
	comparisons []*predicates.ComparisonRange
	quants      []expressions.Quantifier
}

func (s *stubScanProvider) GetScanComparisons() []*predicates.ComparisonRange { return s.comparisons }
func (s *stubScanProvider) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}
func (s *stubScanProvider) GetQuantifiers() []expressions.Quantifier { return s.quants }
func (s *stubScanProvider) CanCorrelate() bool                       { return false }
func (s *stubScanProvider) ChildrenAsSet() bool                      { return false }
func (s *stubScanProvider) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubScanProvider) EqualsWithoutChildren(expressions.RelationalExpression, *expressions.AliasMap) bool {
	return false
}
func (s *stubScanProvider) HashCodeWithoutChildren() uint64 { return 0 }
func (s *stubScanProvider) WithQuantifiers([]expressions.Quantifier) expressions.RelationalExpression {
	return s
}

type stubIntersection struct {
	quants []expressions.Quantifier
}

func (s *stubIntersection) IsIntersection() {}
func (s *stubIntersection) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}
func (s *stubIntersection) GetQuantifiers() []expressions.Quantifier { return s.quants }
func (s *stubIntersection) CanCorrelate() bool                       { return false }
func (s *stubIntersection) ChildrenAsSet() bool                      { return true }
func (s *stubIntersection) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubIntersection) EqualsWithoutChildren(expressions.RelationalExpression, *expressions.AliasMap) bool {
	return false
}
func (s *stubIntersection) HashCodeWithoutChildren() uint64 { return 0 }
func (s *stubIntersection) WithQuantifiers([]expressions.Quantifier) expressions.RelationalExpression {
	return s
}

func eqRange(v any) *predicates.ComparisonRange {
	c := predicates.NewLiteralComparison(predicates.ComparisonEquals, v)
	r := predicates.EmptyComparisonRange()
	result := r.Merge(&c)
	if !result.Ok {
		panic("unexpected merge failure")
	}
	return result.Range
}

func TestEvaluateComparisons_Nil(t *testing.T) {
	t.Parallel()
	if got := EvaluateComparisons(nil); got != nil {
		t.Fatalf("EvaluateComparisons(nil) = %v, want nil", got)
	}
}

func TestEvaluateComparisons_SingleScan(t *testing.T) {
	t.Parallel()
	scan := &stubScanProvider{
		comparisons: []*predicates.ComparisonRange{eqRange(42)},
	}
	got := EvaluateComparisons(scan)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Type != predicates.ComparisonEquals {
		t.Fatalf("type = %v, want EQUALS", got[0].Type)
	}
}

func TestEvaluateComparisons_UnionDefault(t *testing.T) {
	t.Parallel()
	scan1 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(1)}}
	scan2 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(2)}}
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)
	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	parent := &stubScanProvider{quants: []expressions.Quantifier{q1, q2}}
	got := EvaluateComparisons(parent)
	if len(got) != 2 {
		t.Fatalf("union: len = %d, want 2", len(got))
	}
}

func TestEvaluateComparisons_IntersectionKeepsCommon(t *testing.T) {
	t.Parallel()
	scan1 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(1), eqRange(2)}}
	scan2 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(2), eqRange(3)}}
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)
	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	inter := &stubIntersection{quants: []expressions.Quantifier{q1, q2}}
	got := EvaluateComparisons(inter)
	if len(got) != 1 {
		t.Fatalf("intersection: len = %d, want 1 (only eqRange(2) common)", len(got))
	}
	if got[0].Type != predicates.ComparisonEquals {
		t.Fatalf("type = %v, want EQUALS", got[0].Type)
	}
}

func TestEvaluateComparisons_IntersectionDisjoint(t *testing.T) {
	t.Parallel()
	scan1 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(1)}}
	scan2 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(2)}}
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)
	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	inter := &stubIntersection{quants: []expressions.Quantifier{q1, q2}}
	got := EvaluateComparisons(inter)
	if len(got) != 0 {
		t.Fatalf("disjoint intersection: len = %d, want 0", len(got))
	}
}

func TestEvaluateComparisons_IntersectionAllCommon(t *testing.T) {
	t.Parallel()
	scan1 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(5)}}
	scan2 := &stubScanProvider{comparisons: []*predicates.ComparisonRange{eqRange(5)}}
	ref1 := expressions.InitialOf(scan1)
	ref2 := expressions.InitialOf(scan2)
	q1 := expressions.ForEachQuantifier(ref1)
	q2 := expressions.ForEachQuantifier(ref2)
	inter := &stubIntersection{quants: []expressions.Quantifier{q1, q2}}
	got := EvaluateComparisons(inter)
	if len(got) != 1 {
		t.Fatalf("all-common intersection: len = %d, want 1", len(got))
	}
}
