package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- TempTableScanExpression ---

func TestTempTableScan_ConstructionAndAccessor(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	expr := NewTempTableScanExpression(alias)
	if expr.GetTempTableAlias() != alias {
		t.Fatalf("GetTempTableAlias()=%v, want %v", expr.GetTempTableAlias(), alias)
	}
}

func TestTempTableScan_GetQuantifiers_Nil(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	expr := NewTempTableScanExpression(alias)
	if q := expr.GetQuantifiers(); q != nil {
		t.Fatalf("GetQuantifiers()=%v, want nil (leaf node)", q)
	}
}

func TestTempTableScan_CanCorrelate_False(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	expr := NewTempTableScanExpression(alias)
	if expr.CanCorrelate() {
		t.Fatal("CanCorrelate() should be false")
	}
}

func TestTempTableScan_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	expr := NewTempTableScanExpression(alias)
	corr := expr.GetCorrelatedToWithoutChildren()
	if _, ok := corr[alias]; !ok {
		t.Fatalf("GetCorrelatedToWithoutChildren() should contain the alias %v", alias)
	}
	if len(corr) != 1 {
		t.Fatalf("GetCorrelatedToWithoutChildren() size=%d, want 1", len(corr))
	}
}

func TestTempTableScan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	a := NewTempTableScanExpression(alias)
	b := NewTempTableScanExpression(alias)
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("same-alias TempTableScanExpressions should be equal")
	}
}

func TestTempTableScan_EqualsWithoutChildren_Different(t *testing.T) {
	t.Parallel()
	a := NewTempTableScanExpression(values.UniqueCorrelationIdentifier())
	b := NewTempTableScanExpression(values.UniqueCorrelationIdentifier())
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different-alias TempTableScanExpressions should not be equal")
	}
}

func TestTempTableScan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	scan := NewTempTableScanExpression(alias)
	other := &leafScan{name: "T"}
	if scan.EqualsWithoutChildren(other, EmptyAliasMap()) {
		t.Fatal("TempTableScanExpression should not equal a different expression type")
	}
}

func TestTempTableScan_HashCodeWithoutChildren_SameAlias(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	a := NewTempTableScanExpression(alias)
	b := NewTempTableScanExpression(alias)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same-alias TempTableScanExpressions should have the same hash code")
	}
}

func TestTempTableScan_HashCodeWithoutChildren_DifferentAlias(t *testing.T) {
	t.Parallel()
	a := NewTempTableScanExpression(values.NamedCorrelationIdentifier("alias_a"))
	b := NewTempTableScanExpression(values.NamedCorrelationIdentifier("alias_b"))
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different-alias TempTableScanExpressions should (very likely) have different hash codes")
	}
}

func TestTempTableScan_GetResultValue(t *testing.T) {
	t.Parallel()
	alias := values.UniqueCorrelationIdentifier()
	expr := NewTempTableScanExpression(alias)
	if expr.GetResultValue() == nil {
		t.Fatal("GetResultValue() should not be nil")
	}
}

// --- TempTableInsertExpression ---

func TestTempTableInsert_ConstructionAndAccessors(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	expr := NewTempTableInsertExpression(q, alias, true)
	if expr.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("GetInner() alias mismatch")
	}
	if expr.GetTempTableAlias() != alias {
		t.Fatalf("GetTempTableAlias()=%v, want %v", expr.GetTempTableAlias(), alias)
	}
	if !expr.IsOwning() {
		t.Fatal("IsOwning() should be true")
	}
}

func TestTempTableInsert_NotOwning(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	expr := NewTempTableInsertExpression(q, alias, false)
	if expr.IsOwning() {
		t.Fatal("IsOwning() should be false")
	}
}

func TestTempTableInsert_GetQuantifiers(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	expr := NewTempTableInsertExpression(q, alias, true)
	qs := expr.GetQuantifiers()
	if len(qs) != 1 {
		t.Fatalf("GetQuantifiers() len=%d, want 1", len(qs))
	}
	if qs[0].GetAlias() != q.GetAlias() {
		t.Fatal("GetQuantifiers()[0] alias mismatch")
	}
}

func TestTempTableInsert_CanCorrelate_False(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	expr := NewTempTableInsertExpression(q, values.UniqueCorrelationIdentifier(), true)
	if expr.CanCorrelate() {
		t.Fatal("CanCorrelate() should be false")
	}
}

func TestTempTableInsert_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	expr := NewTempTableInsertExpression(q, alias, true)
	corr := expr.GetCorrelatedToWithoutChildren()
	if _, ok := corr[alias]; !ok {
		t.Fatal("GetCorrelatedToWithoutChildren() should contain the alias")
	}
	if len(corr) != 1 {
		t.Fatalf("GetCorrelatedToWithoutChildren() size=%d, want 1", len(corr))
	}
}

func TestTempTableInsert_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	a := NewTempTableInsertExpression(q1, alias, true)
	b := NewTempTableInsertExpression(q2, alias, true)
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("same alias+owning TempTableInsertExpressions should be equal")
	}
}

func TestTempTableInsert_EqualsWithoutChildren_DifferentAlias(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))

	a := NewTempTableInsertExpression(q1, values.UniqueCorrelationIdentifier(), true)
	b := NewTempTableInsertExpression(q2, values.UniqueCorrelationIdentifier(), true)
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different-alias TempTableInsertExpressions should not be equal")
	}
}

func TestTempTableInsert_EqualsWithoutChildren_DifferentOwning(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	alias := values.UniqueCorrelationIdentifier()

	a := NewTempTableInsertExpression(q1, alias, true)
	b := NewTempTableInsertExpression(q2, alias, false)
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different-owning TempTableInsertExpressions should not be equal")
	}
}

func TestTempTableInsert_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	ins := NewTempTableInsertExpression(q, values.UniqueCorrelationIdentifier(), true)
	other := &leafScan{name: "T"}
	if ins.EqualsWithoutChildren(other, EmptyAliasMap()) {
		t.Fatal("TempTableInsertExpression should not equal a different expression type")
	}
}

func TestTempTableInsert_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	alias := values.UniqueCorrelationIdentifier()
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	a := NewTempTableInsertExpression(q1, alias, true)
	b := NewTempTableInsertExpression(q2, alias, true)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same alias+owning should produce same hash code")
	}
}

func TestTempTableInsert_HashCodeWithoutChildren_DifferentOwning(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	alias := values.UniqueCorrelationIdentifier()
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	a := NewTempTableInsertExpression(q1, alias, true)
	b := NewTempTableInsertExpression(q2, alias, false)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different owning flag should (very likely) produce different hash codes")
	}
}

func TestTempTableInsert_GetResultValue(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	q := ForEachQuantifier(InitialOf(leaf))
	expr := NewTempTableInsertExpression(q, values.UniqueCorrelationIdentifier(), true)
	if expr.GetResultValue() == nil {
		t.Fatal("GetResultValue() should not be nil")
	}
}

// --- RecursiveUnionExpression ---

func TestRecursiveUnion_ConstructionAndAccessors(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	initial := ForEachQuantifier(InitialOf(leaf))
	recursive := ForEachQuantifier(InitialOf(leaf))
	scanAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	expr := NewRecursiveUnionExpression(initial, recursive, scanAlias, insertAlias, TraversalPreorder)
	if expr.GetInitialState().GetAlias() != initial.GetAlias() {
		t.Fatal("GetInitialState() alias mismatch")
	}
	if expr.GetRecursiveState().GetAlias() != recursive.GetAlias() {
		t.Fatal("GetRecursiveState() alias mismatch")
	}
	if expr.GetTempTableScanAlias() != scanAlias {
		t.Fatal("GetTempTableScanAlias() mismatch")
	}
	if expr.GetTempTableInsertAlias() != insertAlias {
		t.Fatal("GetTempTableInsertAlias() mismatch")
	}
	if expr.GetTraversalStrategy() != TraversalPreorder {
		t.Fatalf("GetTraversalStrategy()=%v, want PREORDER", expr.GetTraversalStrategy())
	}
}

func TestRecursiveUnion_GetQuantifiers(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	initial := ForEachQuantifier(InitialOf(leaf))
	recursive := ForEachQuantifier(InitialOf(leaf))

	expr := NewRecursiveUnionExpression(initial, recursive,
		values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier(), TraversalAny)
	qs := expr.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("GetQuantifiers() len=%d, want 2", len(qs))
	}
	if qs[0].GetAlias() != initial.GetAlias() {
		t.Fatal("GetQuantifiers()[0] should be initialState")
	}
	if qs[1].GetAlias() != recursive.GetAlias() {
		t.Fatal("GetQuantifiers()[1] should be recursiveState")
	}
}

func TestRecursiveUnion_CanCorrelate_True(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalAny,
	)
	if !expr.CanCorrelate() {
		t.Fatal("CanCorrelate() should be true for RecursiveUnionExpression")
	}
}

func TestRecursiveUnion_GetCorrelatedToWithoutChildren_Empty(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalAny,
	)
	corr := expr.GetCorrelatedToWithoutChildren()
	if len(corr) != 0 {
		t.Fatalf("GetCorrelatedToWithoutChildren() size=%d, want 0", len(corr))
	}
}

func TestRecursiveUnion_TraversalStrategy_Any(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalAny,
	)
	if !expr.PreOrderAllowed() {
		t.Fatal("ANY should allow preorder")
	}
	if !expr.PostOrderAllowed() {
		t.Fatal("ANY should allow postorder")
	}
	if !expr.DfsAllowed() {
		t.Fatal("ANY should allow DFS")
	}
	if !expr.LevelAllowed() {
		t.Fatal("ANY should allow level")
	}
}

func TestRecursiveUnion_TraversalStrategy_Preorder(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalPreorder,
	)
	if !expr.PreOrderAllowed() {
		t.Fatal("PREORDER should allow preorder")
	}
	if expr.PostOrderAllowed() {
		t.Fatal("PREORDER should not allow postorder")
	}
	if !expr.DfsAllowed() {
		t.Fatal("PREORDER should allow DFS (via preorder)")
	}
	if expr.LevelAllowed() {
		t.Fatal("PREORDER should not allow level")
	}
}

func TestRecursiveUnion_TraversalStrategy_Level(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalLevel,
	)
	if expr.PreOrderAllowed() {
		t.Fatal("LEVEL should not allow preorder")
	}
	if expr.PostOrderAllowed() {
		t.Fatal("LEVEL should not allow postorder")
	}
	if expr.DfsAllowed() {
		t.Fatal("LEVEL should not allow DFS")
	}
	if !expr.LevelAllowed() {
		t.Fatal("LEVEL should allow level")
	}
}

func TestRecursiveUnion_TraversalStrategy_Postorder(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalPostorder,
	)
	if expr.PreOrderAllowed() {
		t.Fatal("POSTORDER should not allow preorder")
	}
	if !expr.PostOrderAllowed() {
		t.Fatal("POSTORDER should allow postorder")
	}
	if !expr.DfsAllowed() {
		t.Fatal("POSTORDER should allow DFS (via postorder)")
	}
	if expr.LevelAllowed() {
		t.Fatal("POSTORDER should not allow level")
	}
}

func TestRecursiveUnion_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	scanAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPreorder,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPreorder,
	)
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("same aliases+strategy should be equal")
	}
}

func TestRecursiveUnion_EqualsWithoutChildren_DifferentStrategy(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	scanAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPreorder,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPostorder,
	)
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different strategy should not be equal")
	}
}

func TestRecursiveUnion_EqualsWithoutChildren_DifferentScanAlias(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	insertAlias := values.UniqueCorrelationIdentifier()

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(), insertAlias, TraversalAny,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(), insertAlias, TraversalAny,
	)
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different scan aliases should not be equal")
	}
}

func TestRecursiveUnion_EqualsWithoutChildren_DifferentInsertAlias(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	scanAlias := values.UniqueCorrelationIdentifier()

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, values.UniqueCorrelationIdentifier(), TraversalAny,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, values.UniqueCorrelationIdentifier(), TraversalAny,
	)
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("different insert aliases should not be equal")
	}
}

func TestRecursiveUnion_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalAny,
	)
	other := &leafScan{name: "T"}
	if expr.EqualsWithoutChildren(other, EmptyAliasMap()) {
		t.Fatal("RecursiveUnionExpression should not equal a different expression type")
	}
}

func TestRecursiveUnion_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	scanAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalLevel,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalLevel,
	)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same aliases+strategy should produce same hash code")
	}
}

func TestRecursiveUnion_HashCodeWithoutChildren_DifferentStrategy(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")

	a := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPreorder,
	)
	b := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		scanAlias, insertAlias, TraversalPostorder,
	)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different strategies should (very likely) produce different hash codes")
	}
}

func TestRecursiveUnion_GetResultValue(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	expr := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		TraversalAny,
	)
	if expr.GetResultValue() == nil {
		t.Fatal("GetResultValue() should not be nil")
	}
}

func TestTempTableScan_ChildrenAsSet_False(t *testing.T) {
	t.Parallel()
	e := NewTempTableScanExpression(values.NamedCorrelationIdentifier("tt"))
	if e.ChildrenAsSet() {
		t.Fatal("TempTableScan.ChildrenAsSet should be false")
	}
}

func TestTempTableInsert_ChildrenAsSet_False(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	e := NewTempTableInsertExpression(ForEachQuantifier(InitialOf(leaf)), values.NamedCorrelationIdentifier("tt"), true)
	if e.ChildrenAsSet() {
		t.Fatal("TempTableInsert.ChildrenAsSet should be false")
	}
}

func TestRecursiveUnion_ChildrenAsSet_False(t *testing.T) {
	t.Parallel()
	leaf := NewLogicalValuesExpression(nil)
	e := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		TraversalAny,
	)
	if e.ChildrenAsSet() {
		t.Fatal("RecursiveUnion.ChildrenAsSet should be false")
	}
}

func TestRecursiveUnion_HashCodeDistinctAcrossStrategies(t *testing.T) {
	t.Parallel()
	scan := values.NamedCorrelationIdentifier("s")
	ins := values.NamedCorrelationIdentifier("i")
	leaf := NewLogicalValuesExpression(nil)
	hashes := make(map[uint64]TraversalStrategy)
	for _, s := range []TraversalStrategy{TraversalAny, TraversalPreorder, TraversalLevel, TraversalPostorder} {
		e := NewRecursiveUnionExpression(
			ForEachQuantifier(InitialOf(leaf)),
			ForEachQuantifier(InitialOf(leaf)),
			scan, ins, s,
		)
		h := e.HashCodeWithoutChildren()
		if prev, ok := hashes[h]; ok {
			t.Errorf("hash collision: %v and %v both produce %d", prev, s, h)
		}
		hashes[h] = s
	}
}

func BenchmarkRecursiveUnion_HashCodeWithoutChildren(b *testing.B) {
	leaf := NewLogicalValuesExpression(nil)
	e := NewRecursiveUnionExpression(
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		TraversalLevel,
	)
	for b.Loop() {
		e.HashCodeWithoutChildren()
	}
}

func BenchmarkTempTableScan_HashCodeWithoutChildren(b *testing.B) {
	e := NewTempTableScanExpression(values.NamedCorrelationIdentifier("tt"))
	for b.Loop() {
		e.HashCodeWithoutChildren()
	}
}

func TestTraversalStrategy_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    TraversalStrategy
		want string
	}{
		{TraversalAny, "ANY"},
		{TraversalPreorder, "PREORDER"},
		{TraversalLevel, "LEVEL"},
		{TraversalPostorder, "POSTORDER"},
		{TraversalStrategy(99), "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("TraversalStrategy(%d).String()=%q, want %q", int(tc.s), got, tc.want)
		}
	}
}
