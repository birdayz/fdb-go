package plans

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// stubPlan is a minimal RecordQueryPlan for use as an inner child.
type stubPlan struct{ label string }

func (s *stubPlan) GetResultType() values.Type                 { return values.UnknownType }
func (s *stubPlan) GetChildren() []RecordQueryPlan             { return nil }
func (s *stubPlan) Explain() string                            { return s.label }
func (s *stubPlan) EqualsWithoutChildren(RecordQueryPlan) bool { return true }
func (s *stubPlan) HashCodeWithoutChildren() uint64            { return 0 }

func stub(label string) *stubPlan { return &stubPlan{label: label} }

// ---------------------------------------------------------------------------
// RecordQueryLimitPlan
// ---------------------------------------------------------------------------

func TestLimitPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryLimitPlan(inner, 10, 0)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetLimit() != 10 {
		t.Fatalf("GetLimit() = %d, want 10", p.GetLimit())
	}
	if p.GetOffset() != 0 {
		t.Fatalf("GetOffset() = %d, want 0", p.GetOffset())
	}
}

func TestLimitPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLimitPlan(stub("X"), 5, 0)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestLimitPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryLimitPlan(inner, 10, 0)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestLimitPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLimitPlan(nil, 10, 0)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil for nil inner", cs)
	}
}

func TestLimitPlan_Explain_NoOffset(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLimitPlan(stub("Scan(T)"), 10, 0)
	got := p.Explain()
	if !strings.Contains(got, "Limit") {
		t.Fatalf("Explain = %q, missing 'Limit'", got)
	}
	if !strings.Contains(got, "10") {
		t.Fatalf("Explain = %q, missing limit value", got)
	}
	if strings.Contains(got, "offset") {
		t.Fatalf("Explain = %q, should not contain 'offset' when offset=0", got)
	}
}

func TestLimitPlan_Explain_WithOffset(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLimitPlan(stub("Scan(T)"), 5, 20)
	got := p.Explain()
	if !strings.Contains(got, "Limit") {
		t.Fatalf("Explain = %q, missing 'Limit'", got)
	}
	if !strings.Contains(got, "offset=20") {
		t.Fatalf("Explain = %q, missing 'offset=20'", got)
	}
}

func TestLimitPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryLimitPlan(nil, 10, 5)
	b := NewRecordQueryLimitPlan(nil, 10, 5)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same limit+offset should be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_DifferentLimit(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryLimitPlan(nil, 10, 0)
	b := NewRecordQueryLimitPlan(nil, 20, 0)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different limits should not be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_DifferentOffset(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryLimitPlan(nil, 10, 0)
	b := NewRecordQueryLimitPlan(nil, 10, 5)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different offsets should not be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	lim := NewRecordQueryLimitPlan(nil, 10, 0)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if lim.EqualsWithoutChildren(scan) {
		t.Fatal("LimitPlan should not equal ScanPlan")
	}
}

func TestLimitPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLimitPlan(nil, 10, 5)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestLimitPlan_HashCodeWithoutChildren_DiffersForDifferentParams(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryLimitPlan(nil, 10, 0)
	b := NewRecordQueryLimitPlan(nil, 20, 0)
	c := NewRecordQueryLimitPlan(nil, 10, 5)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different limits should (very likely) have different hashes")
	}
	if a.HashCodeWithoutChildren() == c.HashCodeWithoutChildren() {
		t.Fatal("different offsets should (very likely) have different hashes")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryFilterPlan
// ---------------------------------------------------------------------------

func TestFilterPlan_Construction(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	inner := stub("Inner")
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, inner)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetPredicates()) != 1 {
		t.Fatalf("GetPredicates() len = %d, want 1", len(p.GetPredicates()))
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
}

func TestFilterPlan_GetResultType_DelegatesInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestFilterPlan_GetResultType_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFilterPlan(nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil inner", p.GetResultType())
	}
}

func TestFilterPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFilterPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestFilterPlan_Explain_ContainsFilter(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Scan(T)"))
	got := p.Explain()
	if !strings.Contains(got, "Filter") {
		t.Fatalf("Explain = %q, missing 'Filter'", got)
	}
	if !strings.Contains(got, "1 preds") {
		t.Fatalf("Explain = %q, missing '1 preds'", got)
	}
}

func TestFilterPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFilterPlan(nil, nil)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestFilterPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	a := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, nil)
	b := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same predicates should be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_DifferentPredicateCount(t *testing.T) {
	t.Parallel()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	a := NewRecordQueryFilterPlan([]predicates.QueryPredicate{p1}, nil)
	b := NewRecordQueryFilterPlan([]predicates.QueryPredicate{p1, p2}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different predicate counts should not be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_DifferentPredicate(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryFilterPlan([]predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	}, nil)
	b := NewRecordQueryFilterPlan([]predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriFalse),
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different predicates should not be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	f := NewRecordQueryFilterPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if f.EqualsWithoutChildren(scan) {
		t.Fatal("FilterPlan should not equal ScanPlan")
	}
}

func TestFilterPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestFilterPlan_HashCodeWithoutChildren_DiffersForDifferentPreds(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryFilterPlan([]predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	}, nil)
	b := NewRecordQueryFilterPlan([]predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriFalse),
	}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different predicates should (very likely) produce different hashes")
	}
}

func TestFilterPlan_CopiesPredicateSlice(t *testing.T) {
	t.Parallel()
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	p := NewRecordQueryFilterPlan(preds, nil)
	// Mutate the original slice.
	preds[0] = predicates.NewConstantPredicate(predicates.TriFalse)
	// The plan's copy should be unaffected.
	got := p.GetPredicates()[0]
	if predicates.PredicateEquals(got, preds[0]) {
		t.Fatal("filter plan should have an independent copy of the predicate slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQuerySortPlan
// ---------------------------------------------------------------------------

func TestSortPlan_Construction(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}
	inner := stub("Inner")
	p := NewRecordQuerySortPlan(keys, inner)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetSortKeys()) != 1 {
		t.Fatalf("GetSortKeys() len = %d, want 1", len(p.GetSortKeys()))
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
}

func TestSortPlan_GetResultType_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQuerySortPlan(nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil inner", p.GetResultType())
	}
}

func TestSortPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQuerySortPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestSortPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}, Reverse: true},
	}
	p := NewRecordQuerySortPlan(keys, stub("Scan(T)"))
	got := p.Explain()
	if !strings.Contains(got, "Sort") {
		t.Fatalf("Explain = %q, missing 'Sort'", got)
	}
	if !strings.Contains(got, "2 keys") {
		t.Fatalf("Explain = %q, missing '2 keys'", got)
	}
}

func TestSortPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}
	a := NewRecordQuerySortPlan(keys, nil)
	b := NewRecordQuerySortPlan(keys, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same sort keys should be equal")
	}
}

func TestSortPlan_EqualsWithoutChildren_DifferentKeyCount(t *testing.T) {
	t.Parallel()
	a := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}, nil)
	b := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}},
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different key counts should not be equal")
	}
}

func TestSortPlan_EqualsWithoutChildren_DifferentReverse(t *testing.T) {
	t.Parallel()
	a := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}, nil)
	b := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: true},
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different reverse flags should not be equal")
	}
}

func TestSortPlan_EqualsWithoutChildren_DifferentValue(t *testing.T) {
	t.Parallel()
	a := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}, nil)
	b := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}},
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different values should not be equal")
	}
}

func TestSortPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := NewRecordQuerySortPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if s.EqualsWithoutChildren(scan) {
		t.Fatal("SortPlan should not equal ScanPlan")
	}
}

func TestSortPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	p := NewRecordQuerySortPlan(keys, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestSortPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}, nil)
	b := NewRecordQuerySortPlan([]expressions.SortKey{
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}},
	}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different sort keys should (very likely) have different hashes")
	}
}

func TestSortPlan_CopiesKeySlice(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	p := NewRecordQuerySortPlan(keys, nil)
	keys[0].Reverse = true
	if p.GetSortKeys()[0].Reverse {
		t.Fatal("sort plan should have an independent copy of the key slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryDistinctPlan
// ---------------------------------------------------------------------------

func TestDistinctPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryDistinctPlan(inner)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
}

func TestDistinctPlan_GetResultType_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryDistinctPlan(nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil inner", p.GetResultType())
	}
}

func TestDistinctPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryDistinctPlan(nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil for nil inner", cs)
	}
}

func TestDistinctPlan_Explain(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryDistinctPlan(stub("Scan(T)"))
	got := p.Explain()
	if !strings.Contains(got, "Distinct") {
		t.Fatalf("Explain = %q, missing 'Distinct'", got)
	}
	if !strings.Contains(got, "Scan(T)") {
		t.Fatalf("Explain = %q, missing inner label", got)
	}
}

func TestDistinctPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryDistinctPlan(nil)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>'", got)
	}
}

func TestDistinctPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryDistinctPlan(nil)
	b := NewRecordQueryDistinctPlan(nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("two DistinctPlans should be equal (type-only discriminator)")
	}
}

func TestDistinctPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	d := NewRecordQueryDistinctPlan(nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if d.EqualsWithoutChildren(scan) {
		t.Fatal("DistinctPlan should not equal ScanPlan")
	}
}

func TestDistinctPlan_HashCodeWithoutChildren_SameAcrossInstances(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryDistinctPlan(nil)
	b := NewRecordQueryDistinctPlan(stub("X"))
	// Distinct has no node-info params, so all instances hash the same.
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("all DistinctPlan instances should have the same hash (no params)")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryProjectionPlan
// ---------------------------------------------------------------------------

func TestProjectionPlan_Construction(t *testing.T) {
	t.Parallel()
	projs := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.NotNullLong},
		&values.FieldValue{Field: "name", Typ: values.NotNullString},
	}
	inner := stub("Inner")
	p := NewRecordQueryProjectionPlan(projs, inner)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetProjections()) != 2 {
		t.Fatalf("GetProjections() len = %d, want 2", len(p.GetProjections()))
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
}

func TestProjectionPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryProjectionPlan(nil, stub("X"))
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestProjectionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryProjectionPlan(nil, inner)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestProjectionPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryProjectionPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestProjectionPlan_Explain(t *testing.T) {
	t.Parallel()
	projs := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}
	p := NewRecordQueryProjectionPlan(projs, stub("Scan(T)"))
	got := p.Explain()
	if !strings.Contains(got, "Project") {
		t.Fatalf("Explain = %q, missing 'Project'", got)
	}
}

func TestProjectionPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryProjectionPlan(nil, nil)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>'", got)
	}
}

func TestProjectionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	projs := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	a := NewRecordQueryProjectionPlan(projs, nil)
	b := NewRecordQueryProjectionPlan(projs, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same projections should be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_DifferentColumns(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}, nil)
	b := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different projection columns should not be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_DifferentCount(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}, nil)
	b := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different projection counts should not be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryProjectionPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if p.EqualsWithoutChildren(scan) {
		t.Fatal("ProjectionPlan should not equal ScanPlan")
	}
}

func TestProjectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	projs := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	p := NewRecordQueryProjectionPlan(projs, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestProjectionPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	}, nil)
	b := NewRecordQueryProjectionPlan([]values.Value{
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
	}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different projections should (very likely) produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryUnionPlan
// ---------------------------------------------------------------------------

func TestUnionPlan_Construction(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	p := NewRecordQueryUnionPlan([]RecordQueryPlan{a, b})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetInners()) != 2 {
		t.Fatalf("GetInners() len = %d, want 2", len(p.GetInners()))
	}
}

func TestUnionPlan_GetResultType_FirstInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	p := NewRecordQueryUnionPlan([]RecordQueryPlan{scan, stub("B")})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from first inner)", p.GetResultType())
	}
}

func TestUnionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	p := NewRecordQueryUnionPlan([]RecordQueryPlan{a, b})
	cs := p.GetChildren()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatal("GetChildren() mismatch")
	}
}

func TestUnionPlan_Explain(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnionPlan([]RecordQueryPlan{stub("A"), stub("B")})
	got := p.Explain()
	if !strings.Contains(got, "Union") {
		t.Fatalf("Explain = %q, missing 'Union'", got)
	}
}

func TestUnionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryUnionPlan(nil)
	b := NewRecordQueryUnionPlan([]RecordQueryPlan{stub("X")})
	// Union equality is type-only (no operator params).
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("any two UnionPlans should be EqualsWithoutChildren")
	}
}

func TestUnionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	u := NewRecordQueryUnionPlan(nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if u.EqualsWithoutChildren(scan) {
		t.Fatal("UnionPlan should not equal ScanPlan")
	}
}

func TestUnionPlan_CopiesInnerSlice(t *testing.T) {
	t.Parallel()
	inners := []RecordQueryPlan{stub("A")}
	p := NewRecordQueryUnionPlan(inners)
	inners[0] = stub("B")
	if p.GetInners()[0].Explain() != "A" {
		t.Fatal("union should have an independent copy of the inner slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryIntersectionPlan
// ---------------------------------------------------------------------------

func TestIntersectionPlan_Construction(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	keys := []values.Value{&values.FieldValue{Field: "pk", Typ: values.NotNullLong}}
	p := NewRecordQueryIntersectionPlan([]RecordQueryPlan{a, b}, keys)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetInners()) != 2 {
		t.Fatalf("GetInners() len = %d, want 2", len(p.GetInners()))
	}
	if len(p.GetComparisonKeyValues()) != 1 {
		t.Fatalf("GetComparisonKeyValues() len = %d, want 1", len(p.GetComparisonKeyValues()))
	}
}

func TestIntersectionPlan_GetResultType_FirstInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	p := NewRecordQueryIntersectionPlan([]RecordQueryPlan{scan}, nil)
	if !values.NotNullString.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullString (from first inner)", p.GetResultType())
	}
}

func TestIntersectionPlan_GetResultType_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryIntersectionPlan(nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestIntersectionPlan_Explain(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("A"), stub("B")}, nil)
	got := p.Explain()
	if !strings.Contains(got, "Intersection") {
		t.Fatalf("Explain = %q, missing 'Intersection'", got)
	}
}

func TestIntersectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	i := NewRecordQueryIntersectionPlan(nil, nil)
	u := NewRecordQueryUnionPlan(nil)
	if i.EqualsWithoutChildren(u) {
		t.Fatal("IntersectionPlan should not equal UnionPlan")
	}
}

func TestIntersectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "pk", Typ: values.UnknownType}}
	p := NewRecordQueryIntersectionPlan(nil, keys)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestIntersectionPlan_HashCodeWithoutChildren_DiffersForDifferentKeyCount(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryIntersectionPlan(nil, []values.Value{
		&values.FieldValue{Field: "pk", Typ: values.UnknownType},
	})
	b := NewRecordQueryIntersectionPlan(nil, []values.Value{
		&values.FieldValue{Field: "pk", Typ: values.UnknownType},
		&values.FieldValue{Field: "sk", Typ: values.UnknownType},
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different key counts should (very likely) produce different hashes")
	}
}

func TestIntersectionPlan_CopiesSlices(t *testing.T) {
	t.Parallel()
	inners := []RecordQueryPlan{stub("A")}
	keys := []values.Value{&values.FieldValue{Field: "pk", Typ: values.UnknownType}}
	p := NewRecordQueryIntersectionPlan(inners, keys)
	inners[0] = stub("B")
	keys[0] = &values.FieldValue{Field: "xx", Typ: values.UnknownType}
	if p.GetInners()[0].Explain() != "A" {
		t.Fatal("intersection should have an independent copy of inners")
	}
	if values.ExplainValue(p.GetComparisonKeyValues()[0]) != values.ExplainValue(&values.FieldValue{Field: "pk", Typ: values.UnknownType}) {
		t.Fatal("intersection should have an independent copy of comparison keys")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryValuesPlan
// ---------------------------------------------------------------------------

func TestValuesPlan_Construction(t *testing.T) {
	t.Parallel()
	cols := []values.Value{
		values.LiteralValue(int64(1)),
		values.LiteralValue("hello"),
	}
	p := NewRecordQueryValuesPlan(cols)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetColumns()) != 2 {
		t.Fatalf("GetColumns() len = %d, want 2", len(p.GetColumns()))
	}
}

func TestValuesPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryValuesPlan(nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestValuesPlan_GetChildren_Nil(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryValuesPlan(nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil (leaf plan)", cs)
	}
}

func TestValuesPlan_Explain(t *testing.T) {
	t.Parallel()
	cols := []values.Value{values.LiteralValue(int64(42))}
	p := NewRecordQueryValuesPlan(cols)
	got := p.Explain()
	if !strings.Contains(got, "Values") {
		t.Fatalf("Explain = %q, missing 'Values'", got)
	}
}

func TestValuesPlan_Explain_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryValuesPlan(nil)
	got := p.Explain()
	if got != "Values()" {
		t.Fatalf("Explain = %q, want 'Values()'", got)
	}
}

func TestValuesPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	cols := []values.Value{values.LiteralValue(int64(1))}
	a := NewRecordQueryValuesPlan(cols)
	b := NewRecordQueryValuesPlan(cols)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same columns should be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_DifferentValues(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	b := NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(2))})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different column values should not be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_DifferentCount(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	b := NewRecordQueryValuesPlan([]values.Value{
		values.LiteralValue(int64(1)),
		values.LiteralValue(int64(2)),
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different column counts should not be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	v := NewRecordQueryValuesPlan(nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if v.EqualsWithoutChildren(scan) {
		t.Fatal("ValuesPlan should not equal ScanPlan")
	}
}

func TestValuesPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	cols := []values.Value{values.LiteralValue(int64(1))}
	p := NewRecordQueryValuesPlan(cols)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestValuesPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(1))})
	b := NewRecordQueryValuesPlan([]values.Value{values.LiteralValue(int64(2))})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different values should (very likely) produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryScanPlan
// ---------------------------------------------------------------------------

func TestScanPlan_Construction(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"Order", "Customer"}, values.NotNullLong, true)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if !p.IsReverse() {
		t.Fatal("IsReverse() should be true")
	}
	rts := p.GetRecordTypes()
	if len(rts) != 2 || rts[0] != "Customer" || rts[1] != "Order" {
		t.Fatalf("GetRecordTypes() = %v, want [Customer, Order] (sorted)", rts)
	}
}

func TestScanPlan_NilFlowedType_DefaultsToUnknown(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, nil, false)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType when nil passed", p.GetResultType())
	}
}

func TestScanPlan_GetFlowedType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	if !values.NotNullString.Equals(p.GetFlowedType()) {
		t.Fatalf("GetFlowedType() = %v, want NotNullString", p.GetFlowedType())
	}
}

func TestScanPlan_GetChildren_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil (leaf plan)", cs)
	}
}

func TestScanPlan_Explain_Forward(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	got := p.Explain()
	if got != "Scan(T)" {
		t.Fatalf("Explain = %q, want 'Scan(T)'", got)
	}
}

func TestScanPlan_Explain_Reverse(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, true)
	got := p.Explain()
	if got != "Scan(T) REVERSE" {
		t.Fatalf("Explain = %q, want 'Scan(T) REVERSE'", got)
	}
}

func TestScanPlan_Explain_MultiType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"B", "A"}, values.UnknownType, false)
	got := p.Explain()
	// Sorted, so A comes first.
	if got != "Scan(A, B)" {
		t.Fatalf("Explain = %q, want 'Scan(A, B)'", got)
	}
}

func TestScanPlan_Explain_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan(nil, values.UnknownType, false)
	got := p.Explain()
	if got != "Scan()" {
		t.Fatalf("Explain = %q, want 'Scan()'", got)
	}
}

func TestScanPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	b := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same params should be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentTypes(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	b := NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different record types should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentReverse(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	b := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, true)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different reverse flags should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentFlowedType(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	b := NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different flowed types should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	d := NewRecordQueryDistinctPlan(nil)
	if s.EqualsWithoutChildren(d) {
		t.Fatal("ScanPlan should not equal DistinctPlan")
	}
}

func TestScanPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestScanPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	b := NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
	c := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, true)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different record types should (very likely) produce different hashes")
	}
	if a.HashCodeWithoutChildren() == c.HashCodeWithoutChildren() {
		t.Fatal("different reverse should (very likely) produce different hashes")
	}
}

func TestScanPlan_DedupAndSort(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"C", "A", "B", "A"}, values.UnknownType, false)
	rts := p.GetRecordTypes()
	if len(rts) != 3 || rts[0] != "A" || rts[1] != "B" || rts[2] != "C" {
		t.Fatalf("record types = %v, want [A, B, C]", rts)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryHashAggregationPlan
// ---------------------------------------------------------------------------

func TestHashAggPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	p := NewRecordQueryHashAggregationPlan(inner, keys, aggs)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
	if len(p.GetGroupingKeys()) != 1 {
		t.Fatalf("GetGroupingKeys() len = %d, want 1", len(p.GetGroupingKeys()))
	}
	if len(p.GetAggregates()) != 1 {
		t.Fatalf("GetAggregates() len = %d, want 1", len(p.GetAggregates()))
	}
}

func TestHashAggPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryHashAggregationPlan(stub("X"), nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestHashAggPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryHashAggregationPlan(inner, nil, nil)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestHashAggPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryHashAggregationPlan(nil, nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestHashAggPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	p := NewRecordQueryHashAggregationPlan(stub("Scan(T)"), keys, nil)
	got := p.Explain()
	if !strings.Contains(got, "HashAgg") {
		t.Fatalf("Explain = %q, missing 'HashAgg'", got)
	}
}

func TestHashAggPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	a := NewRecordQueryHashAggregationPlan(nil, keys, aggs)
	b := NewRecordQueryHashAggregationPlan(nil, keys, aggs)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same keys+aggs should be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_DifferentGroupingKeys(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different grouping keys should not be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_DifferentGroupingKeyCount(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{
			&values.FieldValue{Field: "dept", Typ: values.UnknownType},
			&values.FieldValue{Field: "region", Typ: values.UnknownType},
		}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different grouping key counts should not be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_DifferentAggFunction(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	operand := &values.FieldValue{Field: "val", Typ: values.UnknownType}
	a := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: operand},
	})
	b := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: operand},
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different aggregate functions should not be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_DifferentAggOperand(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	a := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	})
	b := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "name", Typ: values.UnknownType}},
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different aggregate operands should not be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_DifferentAggCount(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	agg := expressions.AggregateSpec{
		Function: expressions.AggCount,
		Operand:  &values.FieldValue{Field: "id", Typ: values.UnknownType},
	}
	a := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{agg})
	b := NewRecordQueryHashAggregationPlan(nil, keys, []expressions.AggregateSpec{agg, agg})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different aggregate counts should not be equal")
	}
}

func TestHashAggPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	h := NewRecordQueryHashAggregationPlan(nil, nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if h.EqualsWithoutChildren(scan) {
		t.Fatal("HashAggPlan should not equal ScanPlan")
	}
}

func TestHashAggPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	p := NewRecordQueryHashAggregationPlan(nil, keys, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestHashAggPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryHashAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different grouping keys should (very likely) produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryStreamingAggregationPlan
// ---------------------------------------------------------------------------

func TestStreamingAggPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
	}
	p := NewRecordQueryStreamingAggregationPlan(inner, keys, aggs)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
	if len(p.GetGroupingKeys()) != 1 {
		t.Fatalf("GetGroupingKeys() len = %d, want 1", len(p.GetGroupingKeys()))
	}
	if len(p.GetAggregates()) != 1 {
		t.Fatalf("GetAggregates() len = %d, want 1", len(p.GetAggregates()))
	}
}

func TestStreamingAggPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryStreamingAggregationPlan(stub("X"), nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestStreamingAggPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryStreamingAggregationPlan(inner, nil, nil)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestStreamingAggPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestStreamingAggPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	p := NewRecordQueryStreamingAggregationPlan(stub("Scan(T)"), keys, nil)
	got := p.Explain()
	if !strings.Contains(got, "StreamingAgg") {
		t.Fatalf("Explain = %q, missing 'StreamingAgg'", got)
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
	}
	a := NewRecordQueryStreamingAggregationPlan(nil, keys, aggs)
	b := NewRecordQueryStreamingAggregationPlan(nil, keys, aggs)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same keys+aggs should be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentGroupingKeys(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryStreamingAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryStreamingAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}}, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different grouping keys should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentAggFunction(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	operand := &values.FieldValue{Field: "val", Typ: values.UnknownType}
	a := NewRecordQueryStreamingAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: operand},
	})
	b := NewRecordQueryStreamingAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggMax, Operand: operand},
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different aggregate functions should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentAggOperand(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	a := NewRecordQueryStreamingAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
	})
	b := NewRecordQueryStreamingAggregationPlan(nil, keys, []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "qty", Typ: values.UnknownType}},
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different aggregate operands should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	h := NewRecordQueryHashAggregationPlan(nil, nil, nil)
	if s.EqualsWithoutChildren(h) {
		t.Fatal("StreamingAgg should not equal HashAgg")
	}
}

func TestStreamingAggPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	p := NewRecordQueryStreamingAggregationPlan(nil, keys, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestStreamingAggPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryStreamingAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryStreamingAggregationPlan(nil,
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different grouping keys should (very likely) produce different hashes")
	}
}

func TestStreamingAggPlan_HashDistinctFromHashAgg(t *testing.T) {
	t.Parallel()
	// With same keys+aggs, streaming vs hash should have different hashes.
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	s := NewRecordQueryStreamingAggregationPlan(nil, keys, aggs)
	h := NewRecordQueryHashAggregationPlan(nil, keys, aggs)
	if s.HashCodeWithoutChildren() == h.HashCodeWithoutChildren() {
		t.Fatal("streaming agg and hash agg with same params should have different hashes")
	}
}

// ---------------------------------------------------------------------------
// Cross-type hash discrimination
// ---------------------------------------------------------------------------

func TestAllPlanTypes_DistinctTypeHashes(t *testing.T) {
	t.Parallel()
	// Plans with no operator-specific params, all should have distinct
	// type-discriminator hashes.
	hashes := map[string]uint64{
		"Limit":     NewRecordQueryLimitPlan(nil, 0, 0).HashCodeWithoutChildren(),
		"Filter":    NewRecordQueryFilterPlan(nil, nil).HashCodeWithoutChildren(),
		"Sort":      NewRecordQuerySortPlan(nil, nil).HashCodeWithoutChildren(),
		"Distinct":  NewRecordQueryDistinctPlan(nil).HashCodeWithoutChildren(),
		"Project":   NewRecordQueryProjectionPlan(nil, nil).HashCodeWithoutChildren(),
		"Union":     NewRecordQueryUnionPlan(nil).HashCodeWithoutChildren(),
		"Intersect": NewRecordQueryIntersectionPlan(nil, nil).HashCodeWithoutChildren(),
		"Values":    NewRecordQueryValuesPlan(nil).HashCodeWithoutChildren(),
		"Scan":      NewRecordQueryScanPlan(nil, values.UnknownType, false).HashCodeWithoutChildren(),
		"HashAgg":   NewRecordQueryHashAggregationPlan(nil, nil, nil).HashCodeWithoutChildren(),
		"StreamAgg": NewRecordQueryStreamingAggregationPlan(nil, nil, nil).HashCodeWithoutChildren(),
	}
	seen := make(map[uint64]string)
	for name, h := range hashes {
		if prev, ok := seen[h]; ok {
			t.Fatalf("hash collision between %s and %s: %d", name, prev, h)
		}
		seen[h] = name
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkLimitPlan_Explain(b *testing.B) {
	p := NewRecordQueryLimitPlan(stub("Scan(T)"), 100, 50)
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkLimitPlan_HashCodeWithoutChildren(b *testing.B) {
	p := NewRecordQueryLimitPlan(nil, 100, 50)
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkFilterPlan_Explain(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Scan(T)"))
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkFilterPlan_HashCodeWithoutChildren(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, nil)
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkScanPlan_Explain(b *testing.B) {
	p := NewRecordQueryScanPlan([]string{"Order", "Customer"}, values.UnknownType, true)
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkScanPlan_HashCodeWithoutChildren(b *testing.B) {
	p := NewRecordQueryScanPlan([]string{"Order", "Customer"}, values.UnknownType, true)
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkHashAggPlan_Explain(b *testing.B) {
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	p := NewRecordQueryHashAggregationPlan(stub("Scan(T)"), keys, aggs)
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkHashAggPlan_HashCodeWithoutChildren(b *testing.B) {
	keys := []values.Value{&values.FieldValue{Field: "dept", Typ: values.UnknownType}}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
	}
	p := NewRecordQueryHashAggregationPlan(nil, keys, aggs)
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkValuesPlan_Explain(b *testing.B) {
	cols := []values.Value{
		values.LiteralValue(int64(1)),
		values.LiteralValue("hello"),
		values.LiteralValue(int64(42)),
	}
	p := NewRecordQueryValuesPlan(cols)
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkProjectionPlan_HashCodeWithoutChildren(b *testing.B) {
	projs := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
		&values.FieldValue{Field: "name", Typ: values.UnknownType},
		&values.FieldValue{Field: "age", Typ: values.UnknownType},
	}
	p := NewRecordQueryProjectionPlan(projs, nil)
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}
