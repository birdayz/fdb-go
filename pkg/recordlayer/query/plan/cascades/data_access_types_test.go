package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// stubRelExpr is a minimal RelationalExpression for testing.
type stubRelExpr struct{ name string }

func (s *stubRelExpr) GetResultValue() values.Value             { return values.NewNullValue(values.UnknownType) }
func (s *stubRelExpr) GetQuantifiers() []expressions.Quantifier { return nil }
func (s *stubRelExpr) CanCorrelate() bool                       { return false }
func (s *stubRelExpr) ChildrenAsSet() bool                      { return false }
func (s *stubRelExpr) HashCodeWithoutChildren() uint64          { return 0 }
func (s *stubRelExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *stubRelExpr) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	o, ok := other.(*stubRelExpr)
	return ok && o.name == s.name
}

func (s *stubRelExpr) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return s
}

// ---------------------------------------------------------------------------
// IntersectionResult tests
// ---------------------------------------------------------------------------

func TestIntersectionResult_NewAndGetters(t *testing.T) {
	t.Parallel()

	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{},
		nil,
		false,
	)
	comp := NoCompensation
	expr := &stubRelExpr{name: "scan1"}

	result := NewIntersectionResult(ordering, comp, []expressions.RelationalExpression{expr})

	if !result.IsViable() {
		t.Fatal("expected viable intersection")
	}
	if result.GetCommonOrdering() != ordering {
		t.Fatal("ordering mismatch")
	}
	if result.GetCompensation() != comp {
		t.Fatal("compensation mismatch")
	}
	exprs := result.GetExpressions()
	if len(exprs) != 1 || exprs[0] != expr {
		t.Fatal("expressions mismatch")
	}
}

func TestIntersectionResult_NoViableIntersection(t *testing.T) {
	t.Parallel()

	result := NoViableIntersection()
	if result.IsViable() {
		t.Fatal("expected not viable")
	}
	if result.GetCompensation() != NoCompensation {
		t.Fatal("expected NoCompensation")
	}
	if len(result.GetExpressions()) != 0 {
		t.Fatal("expected empty expressions")
	}
}

func TestIntersectionResult_GetCommonOrdering_PanicsWhenNotViable(t *testing.T) {
	t.Parallel()

	result := NoViableIntersection()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
	}()
	result.GetCommonOrdering()
}

func TestIntersectionResult_NilOrderingWithExprs_Panics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when nil ordering with non-empty expressions")
		}
	}()
	NewIntersectionResult(nil, NoCompensation, []expressions.RelationalExpression{&stubRelExpr{name: "x"}})
}

func TestIntersectionResult_DefensiveCopy(t *testing.T) {
	t.Parallel()

	ordering := EmptyOrdering()
	exprs := []expressions.RelationalExpression{&stubRelExpr{name: "a"}, &stubRelExpr{name: "b"}}
	result := NewIntersectionResult(ordering, NoCompensation, exprs)

	// Mutate the original slice — should not affect the result.
	exprs[0] = &stubRelExpr{name: "mutated"}
	if result.GetExpressions()[0].(*stubRelExpr).name == "mutated" {
		t.Fatal("IntersectionResult did not defensively copy expressions")
	}
}

func TestIntersectionResult_String(t *testing.T) {
	t.Parallel()

	noViable := NoViableIntersection()
	s := noViable.String()
	if s == "" {
		t.Fatal("expected non-empty string")
	}
	if !strings.Contains(s, "no common ordering") {
		t.Fatalf("expected 'no common ordering' in %q", s)
	}

	viable := NewIntersectionResult(EmptyOrdering(), NoCompensation, nil)
	s2 := viable.String()
	if s2 == "" {
		t.Fatal("expected non-empty string for viable result")
	}
}

// ---------------------------------------------------------------------------
// IntersectionInfo tests
// ---------------------------------------------------------------------------

func TestIntersectionInfo_NewAndGetters(t *testing.T) {
	t.Parallel()

	ordering := EmptyOrdering()
	comp := ImpossibleCompensation
	expr := &stubRelExpr{name: "idx1"}
	info := NewIntersectionInfo(ordering, comp, []expressions.RelationalExpression{expr}, 42)

	if info.GetOrdering() != ordering {
		t.Fatal("ordering mismatch")
	}
	if info.GetCompensation() != comp {
		t.Fatal("compensation mismatch")
	}
	if len(info.GetExpressions()) != 1 || info.GetExpressions()[0] != expr {
		t.Fatal("expressions mismatch")
	}
	if info.GetMaxCardinality() != 42 {
		t.Fatalf("expected max cardinality 42, got %d", info.GetMaxCardinality())
	}
}

func TestIntersectionInfo_UnknownCardinality(t *testing.T) {
	t.Parallel()

	info := NewIntersectionInfo(EmptyOrdering(), NoCompensation, nil, CardinalityUnknown)
	if info.GetMaxCardinality() != CardinalityUnknown {
		t.Fatalf("expected CardinalityUnknown (-1), got %d", info.GetMaxCardinality())
	}
}

func TestIntersectionInfo_OfSingleAccess(t *testing.T) {
	t.Parallel()

	ordering := EmptyOrdering()
	expr := &stubRelExpr{name: "single"}
	info := IntersectionInfoOfSingleAccess(ordering, NoCompensation, expr, 100)

	if len(info.GetExpressions()) != 1 {
		t.Fatalf("expected 1 expression, got %d", len(info.GetExpressions()))
	}
	if info.GetMaxCardinality() != 100 {
		t.Fatalf("expected max cardinality 100, got %d", info.GetMaxCardinality())
	}
}

func TestIntersectionInfo_OfImpossibleAccess(t *testing.T) {
	t.Parallel()

	info := IntersectionInfoOfImpossibleAccess(EmptyOrdering(), ImpossibleCompensation)
	if len(info.GetExpressions()) != 0 {
		t.Fatal("expected empty expressions for impossible access")
	}
	if info.GetMaxCardinality() != CardinalityUnknown {
		t.Fatalf("expected unknown cardinality, got %d", info.GetMaxCardinality())
	}
}

func TestIntersectionInfo_OfIntersection(t *testing.T) {
	t.Parallel()

	exprs := []expressions.RelationalExpression{
		&stubRelExpr{name: "a"},
		&stubRelExpr{name: "b"},
	}
	info := IntersectionInfoOfIntersection(EmptyOrdering(), NoCompensation, exprs)

	if len(info.GetExpressions()) != 2 {
		t.Fatalf("expected 2 expressions, got %d", len(info.GetExpressions()))
	}
	if info.GetMaxCardinality() != CardinalityUnknown {
		t.Fatalf("expected unknown cardinality, got %d", info.GetMaxCardinality())
	}
}

func TestIntersectionInfo_EvictExpressions(t *testing.T) {
	t.Parallel()

	info := IntersectionInfoOfSingleAccess(EmptyOrdering(), NoCompensation, &stubRelExpr{name: "x"}, 10)
	if len(info.GetExpressions()) != 1 {
		t.Fatal("expected 1 expression before eviction")
	}
	info.EvictExpressions()
	if len(info.GetExpressions()) != 0 {
		t.Fatal("expected 0 expressions after eviction")
	}
}

func TestIntersectionInfo_DefensiveCopy(t *testing.T) {
	t.Parallel()

	exprs := []expressions.RelationalExpression{&stubRelExpr{name: "orig"}}
	info := NewIntersectionInfo(EmptyOrdering(), NoCompensation, exprs, 5)

	exprs[0] = &stubRelExpr{name: "mutated"}
	if info.GetExpressions()[0].(*stubRelExpr).name == "mutated" {
		t.Fatal("IntersectionInfo did not defensively copy expressions")
	}
}

// ---------------------------------------------------------------------------
// Vectored tests
// ---------------------------------------------------------------------------

func TestVectored_Construction(t *testing.T) {
	t.Parallel()

	v := NewVectored("hello", 3)
	if v.Value != "hello" {
		t.Fatalf("expected value 'hello', got %q", v.Value)
	}
	if v.Position != 3 {
		t.Fatalf("expected position 3, got %d", v.Position)
	}
}

func TestVectored_String(t *testing.T) {
	t.Parallel()

	v := NewVectored(42, 7)
	s := v.String()
	if s != "[42:7]" {
		t.Fatalf("expected '[42:7]', got %q", s)
	}
}

func TestVectored_ZeroPosition(t *testing.T) {
	t.Parallel()

	v := NewVectored("first", 0)
	if v.Position != 0 {
		t.Fatal("position should be 0")
	}
}

// ---------------------------------------------------------------------------
// BitSet tests
// ---------------------------------------------------------------------------

func TestBitSet_SetAndGet(t *testing.T) {
	t.Parallel()

	bs := NewBitSet()
	if bs.Get(0) {
		t.Fatal("expected false for unset bit")
	}
	bs.Set(0)
	if !bs.Get(0) {
		t.Fatal("expected true after Set(0)")
	}
	bs.Set(5)
	bs.Set(100)
	if !bs.Get(5) || !bs.Get(100) {
		t.Fatal("expected true for set bits")
	}
	if bs.Get(1) || bs.Get(99) {
		t.Fatal("expected false for unset bits")
	}
}

func TestBitSet_Or(t *testing.T) {
	t.Parallel()

	a := NewBitSet()
	a.Set(1)
	a.Set(3)

	b := NewBitSet()
	b.Set(2)
	b.Set(3)

	result := a.Or(b)

	if result.Cardinality() != 3 {
		t.Fatalf("expected cardinality 3, got %d", result.Cardinality())
	}
	if !result.Get(1) || !result.Get(2) || !result.Get(3) {
		t.Fatal("union should contain bits 1, 2, 3")
	}
	// Original sets unchanged.
	if a.Cardinality() != 2 {
		t.Fatal("Or should not mutate receiver")
	}
	if b.Cardinality() != 2 {
		t.Fatal("Or should not mutate argument")
	}
}

func TestBitSet_And(t *testing.T) {
	t.Parallel()

	a := NewBitSet()
	a.Set(1)
	a.Set(3)
	a.Set(5)

	b := NewBitSet()
	b.Set(3)
	b.Set(5)
	b.Set(7)

	result := a.And(b)

	if result.Cardinality() != 2 {
		t.Fatalf("expected cardinality 2, got %d", result.Cardinality())
	}
	if !result.Get(3) || !result.Get(5) {
		t.Fatal("intersection should contain bits 3 and 5")
	}
	if result.Get(1) || result.Get(7) {
		t.Fatal("intersection should not contain bits only in one set")
	}
}

func TestBitSet_And_EmptyResult(t *testing.T) {
	t.Parallel()

	a := NewBitSet()
	a.Set(1)

	b := NewBitSet()
	b.Set(2)

	result := a.And(b)
	if result.Cardinality() != 0 {
		t.Fatal("expected empty intersection")
	}
}

func TestBitSet_IsSubsetOf(t *testing.T) {
	t.Parallel()

	a := NewBitSet()
	a.Set(1)
	a.Set(3)

	b := NewBitSet()
	b.Set(1)
	b.Set(2)
	b.Set(3)

	if !a.IsSubsetOf(b) {
		t.Fatal("{1,3} should be a subset of {1,2,3}")
	}
	if b.IsSubsetOf(a) {
		t.Fatal("{1,2,3} should not be a subset of {1,3}")
	}

	// Empty set is a subset of everything.
	empty := NewBitSet()
	if !empty.IsSubsetOf(a) {
		t.Fatal("empty set should be a subset of any set")
	}
	if !empty.IsSubsetOf(empty) {
		t.Fatal("empty set should be a subset of itself")
	}
}

func TestBitSet_Cardinality(t *testing.T) {
	t.Parallel()

	bs := NewBitSet()
	if bs.Cardinality() != 0 {
		t.Fatal("empty bitset should have cardinality 0")
	}
	bs.Set(10)
	bs.Set(20)
	bs.Set(30)
	if bs.Cardinality() != 3 {
		t.Fatalf("expected cardinality 3, got %d", bs.Cardinality())
	}
	// Setting the same bit again doesn't change cardinality.
	bs.Set(10)
	if bs.Cardinality() != 3 {
		t.Fatalf("duplicate Set should not change cardinality, got %d", bs.Cardinality())
	}
}

func TestBitSet_Equal(t *testing.T) {
	t.Parallel()

	a := NewBitSet()
	a.Set(1)
	a.Set(5)

	b := NewBitSet()
	b.Set(1)
	b.Set(5)

	if !a.Equal(b) {
		t.Fatal("identical bitsets should be equal")
	}

	b.Set(9)
	if a.Equal(b) {
		t.Fatal("different cardinality bitsets should not be equal")
	}

	c := NewBitSet()
	c.Set(1)
	c.Set(6) // same cardinality as a, different bits

	if a.Equal(c) {
		t.Fatal("same cardinality but different bits should not be equal")
	}

	// Empty sets are equal.
	if !NewBitSet().Equal(NewBitSet()) {
		t.Fatal("two empty bitsets should be equal")
	}
}

func TestBitSet_String(t *testing.T) {
	t.Parallel()

	empty := NewBitSet()
	if empty.String() != "{}" {
		t.Fatalf("expected '{}', got %q", empty.String())
	}

	bs := NewBitSet()
	bs.Set(5)
	bs.Set(0)
	bs.Set(2)
	s := bs.String()
	if s != "{0, 2, 5}" {
		t.Fatalf("expected '{0, 2, 5}', got %q", s)
	}
}

// ---------------------------------------------------------------------------
// ScanDirection tests
// ---------------------------------------------------------------------------

func TestScanDirection_Values(t *testing.T) {
	t.Parallel()

	if ScanDirectionForward != 0 {
		t.Fatal("forward should be 0")
	}
	if ScanDirectionReverse != 1 {
		t.Fatal("reverse should be 1")
	}
	if ScanDirectionBoth != 2 {
		t.Fatal("both should be 2")
	}
}

// compile-time check
var _ expressions.RelationalExpression = (*stubRelExpr)(nil)
