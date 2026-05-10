package values

import "testing"

// ---------------------------------------------------------------------------
// RankValue — edge cases not covered in value_windowed_test.go
// ---------------------------------------------------------------------------

func TestRankValue_NewCopiesPartitionSlice(t *testing.T) {
	t.Parallel()
	p := []Value{LiteralValue("a"), LiteralValue("b")}
	r := NewRankValue(p)
	// Mutate the original slice — must not affect the RankValue.
	p[0] = LiteralValue("MUTATED")
	if r.PartitioningValues[0] == p[0] {
		t.Fatal("NewRankValue did not copy the partition slice; caller mutation leaked")
	}
	if len(r.PartitioningValues) != 2 {
		t.Fatalf("PartitioningValues len = %d, want 2", len(r.PartitioningValues))
	}
}

func TestRankValue_NewNilPartition(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if r.PartitioningValues != nil {
		t.Fatalf("PartitioningValues = %v, want nil for nil input", r.PartitioningValues)
	}
	if r.ArgumentValues != nil {
		t.Fatalf("ArgumentValues = %v, want nil (RANK takes no arguments)", r.ArgumentValues)
	}
}

func TestRankValue_EvaluateNonMap(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	if got := r.Evaluate(42); got != nil {
		t.Fatalf("Evaluate(42) = %v, want nil", got)
	}
}

func TestRankValue_EvaluateEmptyMap(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	ctx := map[string]any{}
	if got := r.Evaluate(ctx); got != nil {
		t.Fatalf("Evaluate({}) = %v, want nil", got)
	}
}

func TestRankValue_EvaluateStringRankValue(t *testing.T) {
	t.Parallel()
	// The harness stores whatever the caller puts in _rank.
	// Evaluate should return it verbatim (no type assertion on the value).
	r := NewRankValue(nil)
	ctx := map[string]any{"_rank": "not-a-number"}
	got := r.Evaluate(ctx)
	if got != "not-a-number" {
		t.Fatalf("Evaluate returned %v, want the raw value from _rank", got)
	}
}

func TestRankValue_WithChildrenZeroPartitions(t *testing.T) {
	t.Parallel()
	r := NewRankValue(nil)
	rebuilt := r.WithChildren(nil)
	if len(rebuilt.PartitioningValues) != 0 {
		t.Fatalf("WithChildren(nil) PartitioningValues len = %d, want 0", len(rebuilt.PartitioningValues))
	}
	if rebuilt.ArgumentValues != nil {
		t.Fatalf("WithChildren(nil) ArgumentValues = %v, want nil", rebuilt.ArgumentValues)
	}
}

func TestRankValue_WithChildrenUpdatesPartitions(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("old")
	r := NewRankValue([]Value{p1})

	newP := LiteralValue("new")
	rebuilt := r.WithChildren([]Value{newP})

	if len(rebuilt.PartitioningValues) != 1 {
		t.Fatalf("rebuilt PartitioningValues len = %d, want 1", len(rebuilt.PartitioningValues))
	}
	if rebuilt.PartitioningValues[0] != newP {
		t.Fatal("rebuilt PartitioningValues[0] is not the new child")
	}
	// ArgumentValues stays empty — RANK has no operand arguments.
	if rebuilt.ArgumentValues != nil {
		t.Fatalf("rebuilt ArgumentValues = %v, want nil", rebuilt.ArgumentValues)
	}
}

func TestRankValue_WithChildrenMultiplePartitions(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("a")
	p2 := LiteralValue("b")
	r := NewRankValue([]Value{p1, p2})

	np1 := LiteralValue("x")
	np2 := LiteralValue("y")
	rebuilt := r.WithChildren([]Value{np1, np2})

	if len(rebuilt.PartitioningValues) != 2 {
		t.Fatalf("rebuilt PartitioningValues len = %d, want 2", len(rebuilt.PartitioningValues))
	}
	if rebuilt.PartitioningValues[0] != np1 || rebuilt.PartitioningValues[1] != np2 {
		t.Fatal("rebuilt PartitioningValues do not match new children")
	}
}

// ---------------------------------------------------------------------------
// GetCorrelatedToOfValue
// ---------------------------------------------------------------------------

func TestGetCorrelatedToOfValue_Nil(t *testing.T) {
	t.Parallel()
	got := GetCorrelatedToOfValue(nil)
	if got != nil {
		t.Fatalf("GetCorrelatedToOfValue(nil) = %v, want nil", got)
	}
}

func TestGetCorrelatedToOfValue_ConstantNoCorrelations(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(1), Typ: TypeInt}
	got := GetCorrelatedToOfValue(v)
	if got == nil {
		t.Fatal("GetCorrelatedToOfValue(ConstantValue) returned nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("GetCorrelatedToOfValue(ConstantValue) = %v, want empty map", got)
	}
}

func TestGetCorrelatedToOfValue_SingleQuantifiedObject(t *testing.T) {
	t.Parallel()
	c1 := NamedCorrelationIdentifier("c1")
	v := NewQuantifiedObjectValue(c1)
	got := GetCorrelatedToOfValue(v)
	if len(got) != 1 {
		t.Fatalf("got %d correlations, want 1", len(got))
	}
	if _, ok := got[c1]; !ok {
		t.Fatalf("expected correlation %v in set %v", c1, got)
	}
}

func TestGetCorrelatedToOfValue_TreeWithMultipleCorrelations(t *testing.T) {
	t.Parallel()
	c1 := NamedCorrelationIdentifier("left_alias")
	c2 := NamedCorrelationIdentifier("right_alias")
	// Build a tree: ArithmeticValue(QOV(c1), QOV(c2))
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(c1),
		Right: NewQuantifiedObjectValue(c2),
	}
	got := GetCorrelatedToOfValue(v)
	if len(got) != 2 {
		t.Fatalf("got %d correlations, want 2", len(got))
	}
	if _, ok := got[c1]; !ok {
		t.Fatalf("missing correlation %v", c1)
	}
	if _, ok := got[c2]; !ok {
		t.Fatalf("missing correlation %v", c2)
	}
}

func TestGetCorrelatedToOfValue_DuplicateCorrelationDedups(t *testing.T) {
	t.Parallel()
	c := NamedCorrelationIdentifier("dup")
	// Both children reference the same correlation.
	v := &ArithmeticValue{
		Op:    OpMul,
		Left:  NewQuantifiedObjectValue(c),
		Right: NewQuantifiedObjectValue(c),
	}
	got := GetCorrelatedToOfValue(v)
	if len(got) != 1 {
		t.Fatalf("got %d correlations, want 1 (dedup)", len(got))
	}
	if _, ok := got[c]; !ok {
		t.Fatalf("missing expected correlation %v", c)
	}
}

func TestGetCorrelatedToOfValue_MixedTreeConstantAndCorrelated(t *testing.T) {
	t.Parallel()
	c1 := NamedCorrelationIdentifier("x")
	// Tree: ArithmeticValue( ConstantValue(42), QOV(x) )
	v := &ArithmeticValue{
		Op:    OpSub,
		Left:  &ConstantValue{Value: int64(42), Typ: NullableLong},
		Right: NewQuantifiedObjectValue(c1),
	}
	got := GetCorrelatedToOfValue(v)
	if len(got) != 1 {
		t.Fatalf("got %d correlations, want 1", len(got))
	}
	if _, ok := got[c1]; !ok {
		t.Fatalf("missing expected correlation %v", c1)
	}
}

func TestGetCorrelatedToOfValue_DeeperNesting(t *testing.T) {
	t.Parallel()
	c1 := NamedCorrelationIdentifier("deep")
	// Nest: ArithmeticValue( ArithmeticValue( QOV(deep), Const(1) ), Const(2) )
	inner := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(c1),
		Right: &ConstantValue{Value: int64(1), Typ: NullableLong},
	}
	outer := &ArithmeticValue{
		Op:    OpMul,
		Left:  inner,
		Right: &ConstantValue{Value: int64(2), Typ: NullableLong},
	}
	got := GetCorrelatedToOfValue(outer)
	if len(got) != 1 {
		t.Fatalf("got %d correlations, want 1", len(got))
	}
	if _, ok := got[c1]; !ok {
		t.Fatalf("missing expected correlation %v", c1)
	}
}

func TestGetCorrelatedToOfValue_NullValue(t *testing.T) {
	t.Parallel()
	v := NewNullValue(UnknownType)
	got := GetCorrelatedToOfValue(v)
	if got == nil {
		t.Fatal("want non-nil empty map for NullValue, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("got %d correlations for NullValue, want 0", len(got))
	}
}

func TestGetCorrelatedToOfValue_BooleanValue(t *testing.T) {
	t.Parallel()
	v := NewBooleanValue(true)
	got := GetCorrelatedToOfValue(v)
	if got == nil {
		t.Fatal("want non-nil empty map for BooleanValue, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("got %d correlations for BooleanValue, want 0", len(got))
	}
}

func TestGetCorrelatedToOfValue_ExistsValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("exists_q")
	v := &ExistsValue{Alias: alias}
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[alias]; !ok {
		t.Fatal("ExistsValue alias not in correlation set")
	}
}

func TestGetCorrelatedToOfValue_ScalarSubqueryValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("ssq")
	v := &ScalarSubqueryValue{Alias: alias}
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[alias]; !ok {
		t.Fatal("ScalarSubqueryValue alias not in correlation set")
	}
}

func TestGetCorrelatedToOfValue_UnmatchedAggregateValue(t *testing.T) {
	t.Parallel()
	id := NamedCorrelationIdentifier("unmatched_1")
	v := NewUnmatchedAggregateValue(id)
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[id]; !ok {
		t.Fatal("UnmatchedAggregateValue ID not in correlation set")
	}
}

func TestGetCorrelatedToOfValue_QuantifiedRecordValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("qrv")
	v := &QuantifiedRecordValue{Alias: alias}
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[alias]; !ok {
		t.Fatal("QuantifiedRecordValue alias not in correlation set")
	}
}

func TestGetCorrelatedToOfValue_ObjectValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("obj")
	v := &ObjectValue{Alias: alias}
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[alias]; !ok {
		t.Fatal("ObjectValue alias not in correlation set")
	}
}

func TestGetCorrelatedToOfValue_ConstantObjectValue(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("const_obj")
	v := &ConstantObjectValue{Alias: alias, ConstantID: "test"}
	got := GetCorrelatedToOfValue(v)
	if _, ok := got[alias]; !ok {
		t.Fatal("ConstantObjectValue alias not in correlation set")
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkRankValue_Evaluate(b *testing.B) {
	r := NewRankValue([]Value{LiteralValue("region")})
	ctx := map[string]any{"_rank": int64(7)}
	for b.Loop() {
		r.Evaluate(ctx)
	}
}

func BenchmarkGetCorrelatedToOfValue_Leaf(b *testing.B) {
	v := &ConstantValue{Value: int64(1), Typ: NullableLong}
	for b.Loop() {
		GetCorrelatedToOfValue(v)
	}
}

func BenchmarkGetCorrelatedToOfValue_Tree(b *testing.B) {
	c1 := NamedCorrelationIdentifier("a")
	c2 := NamedCorrelationIdentifier("b")
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(c1),
		Right: NewQuantifiedObjectValue(c2),
	}
	for b.Loop() {
		GetCorrelatedToOfValue(v)
	}
}
