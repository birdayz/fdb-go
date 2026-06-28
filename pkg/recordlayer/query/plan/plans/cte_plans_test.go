package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- RecordQueryTempTableScanPlan ---

func TestTempTableScanPlan_ConstructionAndGetTempTableAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tt_scan")
	p := NewRecordQueryTempTableScanPlan(alias)
	if got := p.GetTempTableAlias(); got != alias {
		t.Fatalf("GetTempTableAlias() = %v, want %v", got, alias)
	}
}

func TestTempTableScanPlan_GetChildren_ReturnsNil(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt"))
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestTempTableScanPlan_GetResultType_ReturnsUnknownType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt"))
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestTempTableScanPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tt_same")
	a := NewRecordQueryTempTableScanPlan(alias)
	b := NewRecordQueryTempTableScanPlan(alias)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same-alias TempTableScanPlans should be EqualsWithoutChildren")
	}
}

func TestTempTableScanPlan_EqualsWithoutChildren_DifferentAlias(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_a"))
	b := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_b"))
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different-alias TempTableScanPlans should not be equal")
	}
}

func TestTempTableScanPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt"))
	other := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if scan.EqualsWithoutChildren(other) {
		t.Fatal("TempTableScanPlan should not equal a RecordQueryScanPlan")
	}
}

func TestTempTableScanPlan_HashCodeWithoutChildren_SameAlias(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tt_hash")
	a := NewRecordQueryTempTableScanPlan(alias)
	b := NewRecordQueryTempTableScanPlan(alias)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same-alias TempTableScanPlans should have the same hash code")
	}
}

func TestTempTableScanPlan_HashCodeWithoutChildren_DifferentAlias(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_x"))
	b := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_y"))
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different-alias TempTableScanPlans should (very likely) have different hash codes")
	}
}

func TestTempTableScanPlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("tt_c"))
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestTempTableScanPlan_Explain_ContainsTempTableScan(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableScanPlan(values.NamedCorrelationIdentifier("my_tt"))
	exp := p.Explain()
	if !strings.Contains(exp, "TempTableScan") {
		t.Fatalf("Explain = %q, want it to contain 'TempTableScan'", exp)
	}
	if !strings.Contains(exp, "my_tt") {
		t.Fatalf("Explain = %q, want it to contain 'my_tt'", exp)
	}
}

// --- RecordQueryTempTableInsertPlan ---

func TestTempTableInsertPlan_ConstructionAndAccessors(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	alias := values.NamedCorrelationIdentifier("tti")
	p := NewRecordQueryTempTableInsertPlan(inner, alias, true)
	if got := p.GetInner(); got != inner {
		t.Fatalf("GetInner() = %v, want inner scan", got)
	}
	if got := p.GetTempTableAlias(); got != alias {
		t.Fatalf("GetTempTableAlias() = %v, want %v", got, alias)
	}
	if !p.IsOwning() {
		t.Fatal("IsOwning() should be true")
	}
}

func TestTempTableInsertPlan_NotOwning(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p := NewRecordQueryTempTableInsertPlan(inner, values.NamedCorrelationIdentifier("tti"), false)
	if p.IsOwning() {
		t.Fatal("IsOwning() should be false")
	}
}

func TestTempTableInsertPlan_GetChildren_ReturnsInner(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p := NewRecordQueryTempTableInsertPlan(inner, values.NamedCorrelationIdentifier("tti"), true)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestTempTableInsertPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("tti"), true)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() with nil inner = %v, want nil", cs)
	}
}

func TestTempTableInsertPlan_GetResultType_ReturnsUnknownType(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p := NewRecordQueryTempTableInsertPlan(inner, values.NamedCorrelationIdentifier("tti"), true)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestTempTableInsertPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tti_eq")
	a := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	b := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same alias+owning TempTableInsertPlans should be equal")
	}
}

func TestTempTableInsertPlan_EqualsWithoutChildren_DifferentAlias(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("a"), true)
	b := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("b"), true)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different-alias TempTableInsertPlans should not be equal")
	}
}

func TestTempTableInsertPlan_EqualsWithoutChildren_DifferentOwning(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tti_ow")
	a := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	b := NewRecordQueryTempTableInsertPlan(nil, alias, false)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different-owning TempTableInsertPlans should not be equal")
	}
}

func TestTempTableInsertPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	ins := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("tti"), true)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if ins.EqualsWithoutChildren(scan) {
		t.Fatal("TempTableInsertPlan should not equal a RecordQueryScanPlan")
	}
}

func TestTempTableInsertPlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tti_h")
	a := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	b := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same alias+owning should produce same hash code")
	}
}

func TestTempTableInsertPlan_HashCodeWithoutChildren_DifferentOwning(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tti_hd")
	a := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	b := NewRecordQueryTempTableInsertPlan(nil, alias, false)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different owning should (very likely) produce different hash codes")
	}
}

func TestTempTableInsertPlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("tti_con"), true)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestTempTableInsertPlan_Explain_ContainsTempTableInsert(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p := NewRecordQueryTempTableInsertPlan(inner, values.NamedCorrelationIdentifier("my_tti"), true)
	exp := p.Explain()
	if !strings.Contains(exp, "TempTableInsert") {
		t.Fatalf("Explain = %q, want it to contain 'TempTableInsert'", exp)
	}
	if !strings.Contains(exp, "my_tti") {
		t.Fatalf("Explain = %q, want it to contain 'my_tti'", exp)
	}
	if !strings.Contains(exp, "Scan(T)") {
		t.Fatalf("Explain = %q, want it to contain the inner scan", exp)
	}
}

func TestTempTableInsertPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryTempTableInsertPlan(nil, values.NamedCorrelationIdentifier("tti_nil"), true)
	exp := p.Explain()
	if !strings.Contains(exp, "TempTableInsert") {
		t.Fatalf("Explain = %q, want it to contain 'TempTableInsert'", exp)
	}
	if !strings.Contains(exp, "<nil>") {
		t.Fatalf("Explain = %q, want it to contain '<nil>' for nil inner", exp)
	}
}

// --- RecordQueryRecursiveDfsJoinPlan ---

func TestRecursiveDfsJoinPlan_ConstructionAndAccessors(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"Root"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"Child"}, values.UnknownType, false)
	corr := values.NamedCorrelationIdentifier("prior")
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, corr, DfsPreorder)

	if got := p.GetRoot(); got != root {
		t.Fatalf("GetRoot() = %v, want root", got)
	}
	if got := p.GetChild(); got != child {
		t.Fatalf("GetChild() = %v, want child", got)
	}
	if got := p.GetPriorCorrelation(); got != corr {
		t.Fatalf("GetPriorCorrelation() = %v, want %v", got, corr)
	}
	if got := p.GetTraversalStrategy(); got != DfsPreorder {
		t.Fatalf("GetTraversalStrategy() = %v, want DfsPreorder", got)
	}
}

func TestRecursiveDfsJoinPlan_GetChildren_ReturnsRootAndChild(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, values.NamedCorrelationIdentifier("p"), DfsPreorder)
	cs := p.GetChildren()
	if len(cs) != 2 {
		t.Fatalf("GetChildren() len = %d, want 2", len(cs))
	}
	if cs[0] != root {
		t.Fatalf("GetChildren()[0] = %v, want root", cs[0])
	}
	if cs[1] != child {
		t.Fatalf("GetChildren()[1] = %v, want child", cs[1])
	}
}

func TestRecursiveDfsJoinPlan_GetResultType_ReturnsUnknownType(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, values.NamedCorrelationIdentifier("p"), DfsPreorder)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestRecursiveDfsJoinPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	corr := values.NamedCorrelationIdentifier("prior")
	a := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPreorder)
	b := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPreorder)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same correlation+strategy RecursiveDfsJoinPlans should be equal")
	}
}

func TestRecursiveDfsJoinPlan_EqualsWithoutChildren_DifferentCorrelation(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, values.NamedCorrelationIdentifier("c1"), DfsPreorder)
	b := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, values.NamedCorrelationIdentifier("c2"), DfsPreorder)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different-correlation RecursiveDfsJoinPlans should not be equal")
	}
}

func TestRecursiveDfsJoinPlan_EqualsWithoutChildren_DifferentStrategy(t *testing.T) {
	t.Parallel()
	corr := values.NamedCorrelationIdentifier("prior")
	a := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPreorder)
	b := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPostorder)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different-strategy RecursiveDfsJoinPlans should not be equal")
	}
}

func TestRecursiveDfsJoinPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, values.NamedCorrelationIdentifier("c"), DfsPreorder)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if p.EqualsWithoutChildren(scan) {
		t.Fatal("RecursiveDfsJoinPlan should not equal a RecordQueryScanPlan")
	}
}

func TestRecursiveDfsJoinPlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	corr := values.NamedCorrelationIdentifier("ph")
	a := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPostorder)
	b := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPostorder)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same correlation+strategy should produce same hash code")
	}
}

func TestRecursiveDfsJoinPlan_HashCodeWithoutChildren_DifferentStrategy(t *testing.T) {
	t.Parallel()
	corr := values.NamedCorrelationIdentifier("ph")
	a := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPreorder)
	b := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, corr, DfsPostorder)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different strategy should (very likely) produce different hash codes")
	}
}

func TestRecursiveDfsJoinPlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, values.NamedCorrelationIdentifier("c"), DfsPreorder)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestDfsTraversalStrategy_Preorder_String(t *testing.T) {
	t.Parallel()
	if got := DfsPreorder.String(); got != "PREORDER" {
		t.Fatalf("DfsPreorder.String() = %q, want PREORDER", got)
	}
}

func TestDfsTraversalStrategy_Postorder_String(t *testing.T) {
	t.Parallel()
	if got := DfsPostorder.String(); got != "POSTORDER" {
		t.Fatalf("DfsPostorder.String() = %q, want POSTORDER", got)
	}
}

func TestDfsTraversalStrategy_Unknown_String(t *testing.T) {
	t.Parallel()
	if got := DfsTraversalStrategy(99).String(); got != "UNKNOWN" {
		t.Fatalf("DfsTraversalStrategy(99).String() = %q, want UNKNOWN", got)
	}
}

func TestRecursiveDfsJoinPlan_Explain_ContainsRecursiveDfsJoin(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, values.NamedCorrelationIdentifier("prior"), DfsPreorder)
	exp := p.Explain()
	if !strings.Contains(exp, "RecursiveDfsJoin") {
		t.Fatalf("Explain = %q, want it to contain 'RecursiveDfsJoin'", exp)
	}
	if !strings.Contains(exp, "PREORDER") {
		t.Fatalf("Explain = %q, want it to contain 'PREORDER'", exp)
	}
	if !strings.Contains(exp, "Scan(R)") {
		t.Fatalf("Explain = %q, want it to contain the root scan", exp)
	}
	if !strings.Contains(exp, "Scan(C)") {
		t.Fatalf("Explain = %q, want it to contain the child scan", exp)
	}
}

func TestRecursiveDfsJoinPlan_Explain_Postorder(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, values.NamedCorrelationIdentifier("prior"), DfsPostorder)
	exp := p.Explain()
	if !strings.Contains(exp, "POSTORDER") {
		t.Fatalf("Explain = %q, want it to contain 'POSTORDER'", exp)
	}
}

// --- RecordQueryRecursiveLevelUnionPlan ---

func TestRecursiveLevelUnionPlan_Construction(t *testing.T) {
	t.Parallel()
	init := NewRecordQueryScanPlan([]string{"Init"}, values.UnknownType, false)
	rec := NewRecordQueryScanPlan([]string{"Rec"}, values.UnknownType, false)
	scanAlias := values.NamedCorrelationIdentifier("scan")
	insertAlias := values.NamedCorrelationIdentifier("insert")
	p := NewRecordQueryRecursiveLevelUnionPlan(init, rec, scanAlias, insertAlias)
	if p.GetInitialState() != init {
		t.Fatal("GetInitialState mismatch")
	}
	if p.GetRecursiveState() != rec {
		t.Fatal("GetRecursiveState mismatch")
	}
	if p.GetTempTableScanAlias() != scanAlias {
		t.Fatal("GetTempTableScanAlias mismatch")
	}
	if p.GetTempTableInsertAlias() != insertAlias {
		t.Fatal("GetTempTableInsertAlias mismatch")
	}
}

func TestRecursiveLevelUnionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	init := NewRecordQueryScanPlan([]string{"I"}, values.UnknownType, false)
	rec := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveLevelUnionPlan(init, rec,
		values.NamedCorrelationIdentifier("s"),
		values.NamedCorrelationIdentifier("i"))
	cs := p.GetChildren()
	if len(cs) != 2 {
		t.Fatalf("GetChildren() len = %d, want 2", len(cs))
	}
	if cs[0] != init || cs[1] != rec {
		t.Fatal("children order mismatch")
	}
}

func TestRecursiveLevelUnionPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("s"),
		values.NamedCorrelationIdentifier("i"))
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestRecursiveLevelUnionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	sa := values.NamedCorrelationIdentifier("scan")
	ia := values.NamedCorrelationIdentifier("insert")
	a := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, ia)
	b := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, ia)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same aliases should be equal")
	}
}

func TestRecursiveLevelUnionPlan_EqualsWithoutChildren_DifferentScanAlias(t *testing.T) {
	t.Parallel()
	ia := values.NamedCorrelationIdentifier("insert")
	a := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, values.NamedCorrelationIdentifier("s1"), ia)
	b := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, values.NamedCorrelationIdentifier("s2"), ia)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different scan alias should not be equal")
	}
}

func TestRecursiveLevelUnionPlan_EqualsWithoutChildren_DifferentInsertAlias(t *testing.T) {
	t.Parallel()
	sa := values.NamedCorrelationIdentifier("scan")
	a := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, values.NamedCorrelationIdentifier("i1"))
	b := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, values.NamedCorrelationIdentifier("i2"))
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different insert alias should not be equal")
	}
}

func TestRecursiveLevelUnionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("s"),
		values.NamedCorrelationIdentifier("i"))
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if p.EqualsWithoutChildren(scan) {
		t.Fatal("should not equal a different plan type")
	}
}

func TestRecursiveLevelUnionPlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	sa := values.NamedCorrelationIdentifier("scan")
	ia := values.NamedCorrelationIdentifier("insert")
	a := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, ia)
	b := NewRecordQueryRecursiveLevelUnionPlan(nil, nil, sa, ia)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same aliases should produce same hash")
	}
}

func TestRecursiveLevelUnionPlan_HashCodeWithoutChildren_Different(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("s1"),
		values.NamedCorrelationIdentifier("i1"))
	b := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("s2"),
		values.NamedCorrelationIdentifier("i2"))
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different aliases should (very likely) produce different hashes")
	}
}

func TestRecursiveLevelUnionPlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("s"),
		values.NamedCorrelationIdentifier("i"))
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestRecursiveLevelUnionPlan_Explain(t *testing.T) {
	t.Parallel()
	init := NewRecordQueryScanPlan([]string{"I"}, values.UnknownType, false)
	rec := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveLevelUnionPlan(init, rec,
		values.NamedCorrelationIdentifier("sc"),
		values.NamedCorrelationIdentifier("ins"))
	exp := p.Explain()
	if !strings.Contains(exp, "RecursiveLevelUnion") {
		t.Fatalf("Explain = %q, want 'RecursiveLevelUnion'", exp)
	}
	if !strings.Contains(exp, "Scan(I)") {
		t.Fatalf("Explain = %q, want 'Scan(I)'", exp)
	}
	if !strings.Contains(exp, "Scan(R)") {
		t.Fatalf("Explain = %q, want 'Scan(R)'", exp)
	}
	if !strings.Contains(exp, "scan=sc") {
		t.Fatalf("Explain = %q, want 'scan=sc'", exp)
	}
	if !strings.Contains(exp, "insert=ins") {
		t.Fatalf("Explain = %q, want 'insert=ins'", exp)
	}
}

// --- Cross-type discrimination ---

func TestCTEPlan_DistinctHashes(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("tt")
	scan := NewRecordQueryTempTableScanPlan(alias)
	insert := NewRecordQueryTempTableInsertPlan(nil, alias, true)
	dfs := NewRecordQueryRecursiveDfsJoinPlan(nil, nil, alias, DfsPreorder)

	scanH := scan.HashCodeWithoutChildren()
	insertH := insert.HashCodeWithoutChildren()
	dfsH := dfs.HashCodeWithoutChildren()

	if scanH == insertH || scanH == dfsH || insertH == dfsH {
		t.Fatalf("CTE plan hashes collide: scan=%d insert=%d dfs=%d", scanH, insertH, dfsH)
	}
}

func TestCTEPlan_Equals_Full_Tree(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	alias := values.NamedCorrelationIdentifier("tt")
	insertA := NewRecordQueryTempTableInsertPlan(inner, alias, true)
	insertB := NewRecordQueryTempTableInsertPlan(inner, alias, true)
	if !Equals(insertA, insertB) {
		t.Fatal("structurally identical TempTableInsertPlans should be Equals")
	}

	insertC := NewRecordQueryTempTableInsertPlan(inner, values.NamedCorrelationIdentifier("other"), true)
	if Equals(insertA, insertC) {
		t.Fatal("different-alias TempTableInsertPlans should not be Equals")
	}
}

func TestRecursiveDfsJoinPlan_Size(t *testing.T) {
	t.Parallel()
	root := NewRecordQueryScanPlan([]string{"R"}, values.UnknownType, false)
	child := NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	p := NewRecordQueryRecursiveDfsJoinPlan(root, child, values.NamedCorrelationIdentifier("p"), DfsPreorder)
	if got := Size(p); got != 3 {
		t.Fatalf("Size(RecursiveDfsJoin(Scan, Scan)) = %d, want 3", got)
	}
}
