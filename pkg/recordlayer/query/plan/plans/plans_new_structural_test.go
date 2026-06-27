package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// RecordQueryPredicatesFilterPlan
// ---------------------------------------------------------------------------

func TestPredicatesFilterPlan_Construction(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	inner := stub("Inner")
	p := NewRecordQueryPredicatesFilterPlan(inner, []predicates.QueryPredicate{pred})
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

func TestPredicatesFilterPlan_GetResultType_DelegatesInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryPredicatesFilterPlan(scan, []predicates.QueryPredicate{pred})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestPredicatesFilterPlan_GetResultType_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryPredicatesFilterPlan(nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil inner", p.GetResultType())
	}
}

func TestPredicatesFilterPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryPredicatesFilterPlan(inner, nil)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestPredicatesFilterPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryPredicatesFilterPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestPredicatesFilterPlan_Explain_ContainsPredicatesFilter(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryPredicatesFilterPlan(stub("Scan(T)"), []predicates.QueryPredicate{pred})
	got := p.Explain()
	if !strings.Contains(got, "PredicatesFilter") {
		t.Fatalf("Explain = %q, missing 'PredicatesFilter'", got)
	}
	if !strings.Contains(got, "1 preds") {
		t.Fatalf("Explain = %q, missing '1 preds'", got)
	}
}

func TestPredicatesFilterPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryPredicatesFilterPlan(nil, nil)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestPredicatesFilterPlan_Explain_EmptyPredicates(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryPredicatesFilterPlan(stub("Scan(T)"), nil)
	got := p.Explain()
	if !strings.Contains(got, "0 preds") {
		t.Fatalf("Explain = %q, missing '0 preds'", got)
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	a := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	b := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same predicates should be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_DifferentPredicateCount(t *testing.T) {
	t.Parallel()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	a := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{p1})
	b := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{p1, p2})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different predicate counts should not be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_DifferentPredicate(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	})
	b := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriFalse),
	})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different predicates should not be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_BothEmpty(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryPredicatesFilterPlan(nil, nil)
	b := NewRecordQueryPredicatesFilterPlan(nil, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("two PredicatesFilterPlans with nil predicates should be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	pf := NewRecordQueryPredicatesFilterPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if pf.EqualsWithoutChildren(scan) {
		t.Fatal("PredicatesFilterPlan should not equal ScanPlan")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_NotEqualToFilterPlan(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	pf := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	f := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, nil)
	if pf.EqualsWithoutChildren(f) {
		t.Fatal("PredicatesFilterPlan should not equal FilterPlan")
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_DiffersForDifferentPreds(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	})
	b := NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriFalse),
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different predicates should (very likely) produce different hashes")
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_EmptyPreds(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryPredicatesFilterPlan(nil, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic for empty preds: %d vs %d", h1, h2)
	}
}

func TestPredicatesFilterPlan_CopiesPredicateSlice(t *testing.T) {
	t.Parallel()
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	p := NewRecordQueryPredicatesFilterPlan(nil, preds)
	// Mutate the original slice.
	preds[0] = predicates.NewConstantPredicate(predicates.TriFalse)
	// The plan's copy should be unaffected.
	got := p.GetPredicates()[0]
	if predicates.PredicateEquals(got, preds[0]) {
		t.Fatal("predicates filter plan should have an independent copy of the predicate slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryMapPlan
// ---------------------------------------------------------------------------

func TestMapPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	rv := &values.FieldValue{Field: "id", Typ: values.NotNullLong}
	p := NewRecordQueryMapPlan(inner, rv)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
	if p.GetResultValue() != rv {
		t.Fatal("GetResultValue() mismatch")
	}
}

func TestMapPlan_GetResultType_FromResultValue(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "id", Typ: values.NotNullLong}
	p := NewRecordQueryMapPlan(stub("X"), rv)
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from result value)", p.GetResultType())
	}
}

func TestMapPlan_GetResultType_NilResultValue(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryMapPlan(stub("X"), nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil result value", p.GetResultType())
	}
}

func TestMapPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryMapPlan(inner, nil)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestMapPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryMapPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestMapPlan_Explain_ContainsMap(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "id", Typ: values.UnknownType}
	p := NewRecordQueryMapPlan(stub("Scan(T)"), rv)
	got := p.Explain()
	if !strings.Contains(got, "Map") {
		t.Fatalf("Explain = %q, missing 'Map'", got)
	}
	if !strings.Contains(got, "Scan(T)") {
		t.Fatalf("Explain = %q, missing inner label", got)
	}
}

func TestMapPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "id", Typ: values.UnknownType}
	p := NewRecordQueryMapPlan(nil, rv)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestMapPlan_Explain_NilResultValue(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryMapPlan(stub("Scan(T)"), nil)
	got := p.Explain()
	// ExplainValue(nil) returns "", so Map(Scan(T), )
	if !strings.Contains(got, "Map") {
		t.Fatalf("Explain = %q, missing 'Map'", got)
	}
}

func TestMapPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "id", Typ: values.UnknownType}
	a := NewRecordQueryMapPlan(nil, rv)
	b := NewRecordQueryMapPlan(nil, rv)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same result value should be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_DifferentResultValue(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryMapPlan(nil, &values.FieldValue{Field: "id", Typ: values.UnknownType})
	b := NewRecordQueryMapPlan(nil, &values.FieldValue{Field: "name", Typ: values.UnknownType})
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different result values should not be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_BothNilResultValue(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryMapPlan(nil, nil)
	b := NewRecordQueryMapPlan(nil, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("two MapPlans with nil result values should be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	m := NewRecordQueryMapPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if m.EqualsWithoutChildren(scan) {
		t.Fatal("MapPlan should not equal ScanPlan")
	}
}

func TestMapPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "id", Typ: values.UnknownType}
	p := NewRecordQueryMapPlan(nil, rv)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestMapPlan_HashCodeWithoutChildren_DiffersForDifferentValues(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryMapPlan(nil, &values.FieldValue{Field: "id", Typ: values.UnknownType})
	b := NewRecordQueryMapPlan(nil, &values.FieldValue{Field: "name", Typ: values.UnknownType})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different result values should (very likely) produce different hashes")
	}
}

func TestMapPlan_HashCodeWithoutChildren_NilResultValue(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryMapPlan(nil, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic for nil result value: %d vs %d", h1, h2)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryFirstOrDefaultPlan
// ---------------------------------------------------------------------------

func TestFirstOrDefaultPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	dv := values.LiteralValue(int64(0))
	p := NewRecordQueryFirstOrDefaultPlan(inner, dv)
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
	if p.GetDefaultValue() != dv {
		t.Fatal("GetDefaultValue() mismatch")
	}
}

func TestFirstOrDefaultPlan_GetResultType_DelegatesInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	p := NewRecordQueryFirstOrDefaultPlan(scan, nil)
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestFirstOrDefaultPlan_GetResultType_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for nil inner", p.GetResultType())
	}
}

func TestFirstOrDefaultPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := NewRecordQueryFirstOrDefaultPlan(inner, nil)
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestFirstOrDefaultPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestFirstOrDefaultPlan_Explain_ContainsFirstOrDefault(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFirstOrDefaultPlan(stub("Scan(T)"), nil)
	got := p.Explain()
	if !strings.Contains(got, "FirstOrDefault") {
		t.Fatalf("Explain = %q, missing 'FirstOrDefault'", got)
	}
	if !strings.Contains(got, "Scan(T)") {
		t.Fatalf("Explain = %q, missing inner label", got)
	}
}

func TestFirstOrDefaultPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	dv := values.LiteralValue(int64(42))
	a := NewRecordQueryFirstOrDefaultPlan(nil, dv)
	b := NewRecordQueryFirstOrDefaultPlan(nil, dv)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same default value should be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_DifferentDefaultValue(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryFirstOrDefaultPlan(nil, values.LiteralValue(int64(1)))
	b := NewRecordQueryFirstOrDefaultPlan(nil, values.LiteralValue(int64(2)))
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different default values should not be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_BothNilDefaultValue(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	b := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("two FirstOrDefaultPlans with nil default values should be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	fod := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if fod.EqualsWithoutChildren(scan) {
		t.Fatal("FirstOrDefaultPlan should not equal ScanPlan")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_NotEqualToMapPlan(t *testing.T) {
	t.Parallel()
	// Both use ExplainValue for equality, but type discriminator should prevent match.
	fod := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	m := NewRecordQueryMapPlan(nil, nil)
	if fod.EqualsWithoutChildren(m) {
		t.Fatal("FirstOrDefaultPlan should not equal MapPlan")
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	dv := values.LiteralValue(int64(42))
	p := NewRecordQueryFirstOrDefaultPlan(nil, dv)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_DiffersForDifferentDefaults(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryFirstOrDefaultPlan(nil, values.LiteralValue(int64(1)))
	b := NewRecordQueryFirstOrDefaultPlan(nil, values.LiteralValue(int64(2)))
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different default values should (very likely) produce different hashes")
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_NilDefaultValue(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryFirstOrDefaultPlan(nil, nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic for nil default: %d vs %d", h1, h2)
	}
}

// ---------------------------------------------------------------------------
// Cross-type discrimination for new plan types
// ---------------------------------------------------------------------------

func TestNewPlanTypes_DistinctTypeHashes(t *testing.T) {
	t.Parallel()
	hashes := map[string]uint64{
		"PredicatesFilter": NewRecordQueryPredicatesFilterPlan(nil, nil).HashCodeWithoutChildren(),
		"Map":              NewRecordQueryMapPlan(nil, nil).HashCodeWithoutChildren(),
		"FirstOrDefault":   NewRecordQueryFirstOrDefaultPlan(nil, nil).HashCodeWithoutChildren(),
		// Include existing types to verify no collisions with the new ones.
		"Filter":   NewRecordQueryFilterPlan(nil, nil).HashCodeWithoutChildren(),
		"Distinct": NewRecordQueryDistinctPlan(nil).HashCodeWithoutChildren(),
		"Scan":     NewRecordQueryScanPlan(nil, values.UnknownType, false).HashCodeWithoutChildren(),
	}
	seen := make(map[uint64]string)
	for name, h := range hashes {
		if prev, ok := seen[h]; ok {
			t.Fatalf("hash collision between %s and %s: %d", name, prev, h)
		}
		seen[h] = name
	}
}
