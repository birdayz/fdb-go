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
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(inner, []predicates.QueryPredicate{pred})
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(scan, []predicates.QueryPredicate{pred})
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestPredicatesFilterPlan_ConstructorRejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryPredicatesFilterPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

func TestPredicatesFilterPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(inner, nil)
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestPredicatesFilterPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	// Package-local zero values remain useful adversaries for method totality,
	// but they are not valid constructor inputs.
	p := &RecordQueryPredicatesFilterPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestPredicatesFilterPlan_Explain_ContainsPredicatesFilter(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Scan(T)"), []predicates.QueryPredicate{pred})
	})
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
	p := &RecordQueryPredicatesFilterPlan{}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestPredicatesFilterPlan_Explain_EmptyPredicates(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Scan(T)"), nil)
	})
	got := p.Explain()
	if !strings.Contains(got, "0 preds") {
		t.Fatalf("Explain = %q, missing '0 preds'", got)
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	a := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("A"), []predicates.QueryPredicate{pred})
	})
	b := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("B"), []predicates.QueryPredicate{pred})
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same predicates should be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_DifferentPredicateCount(t *testing.T) {
	t.Parallel()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	a := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("A"), []predicates.QueryPredicate{p1})
	})
	b := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("B"), []predicates.QueryPredicate{p1, p2})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different predicate counts should not be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_DifferentPredicate(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("A"), []predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		})
	})
	b := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("B"), []predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriFalse),
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different predicates should not be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_BothEmpty(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("A"), nil)
	})
	b := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("B"), nil)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two PredicatesFilterPlans with nil predicates should be equal")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	pf := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Inner"), nil)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if pf.EqualsPlanWithoutChildren(scan) {
		t.Fatal("PredicatesFilterPlan should not equal ScanPlan")
	}
}

func TestPredicatesFilterPlan_EqualsWithoutChildren_NotEqualToFilterPlan(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	pf := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("PredicatesInner"), []predicates.QueryPredicate{pred})
	})
	f := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("FilterInner"))
	})
	if pf.EqualsPlanWithoutChildren(f) {
		t.Fatal("PredicatesFilterPlan should not equal FilterPlan")
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Inner"), []predicates.QueryPredicate{pred})
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_DiffersForDifferentPreds(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("A"), []predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		})
	})
	b := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("B"), []predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriFalse),
		})
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different predicates should (very likely) produce different hashes")
	}
}

func TestPredicatesFilterPlan_HashCodeWithoutChildren_EmptyPreds(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Inner"), nil)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic for empty preds: %d vs %d", h1, h2)
	}
}

func TestPredicatesFilterPlan_CopiesPredicateSlice(t *testing.T) {
	t.Parallel()
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	p := mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
		return NewRecordQueryPredicatesFilterPlan(stub("Inner"), preds)
	})
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
	rv := testField(t, "id", values.NotNullLong)
	p := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(inner, rv)
	})
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
	rv := testField(t, "id", values.NotNullLong)
	p := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("X"), rv)
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from result value)", p.GetResultType())
	}
}

func TestMapPlan_ConstructorRejectsNilResultValue(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryMapPlan(stub("X"), nil); err == nil {
		t.Fatal("constructor accepted a nil result value")
	}
}

func TestMapPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(inner, testField(t, "id", values.NotNullLong))
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestMapPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryMapPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestMapPlan_Explain_ContainsMap(t *testing.T) {
	t.Parallel()
	rv := testField(t, "id", values.NullableLong)
	p := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("Scan(T)"), rv)
	})
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
	rv := testField(t, "id", values.NullableLong)
	p := &RecordQueryMapPlan{resultValue: rv}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestMapPlan_Explain_NilResultValue(t *testing.T) {
	t.Parallel()
	p := &RecordQueryMapPlan{innerQ: QuantifierOverPlan(stub("Scan(T)"))}
	got := p.Explain()
	// ExplainValue(nil) returns "", so Map(Scan(T), )
	if !strings.Contains(got, "Map") {
		t.Fatalf("Explain = %q, missing 'Map'", got)
	}
}

func TestMapPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	rv := testField(t, "id", values.NullableLong)
	a := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("A"), rv)
	})
	b := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("B"), rv)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same result value should be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_DifferentResultValue(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("A"), testField(t, "id", values.NullableLong))
	})
	b := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("B"), testField(t, "name", values.NullableLong))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different result values should not be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_BothNilResultValue(t *testing.T) {
	t.Parallel()
	a := &RecordQueryMapPlan{}
	b := &RecordQueryMapPlan{}
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two MapPlans with nil result values should be equal")
	}
}

func TestMapPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	m := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("Inner"), testField(t, "id", values.NullableLong))
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if m.EqualsPlanWithoutChildren(scan) {
		t.Fatal("MapPlan should not equal ScanPlan")
	}
}

func TestMapPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	rv := testField(t, "id", values.NullableLong)
	p := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("Inner"), rv)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestMapPlan_HashCodeWithoutChildren_DiffersForDifferentValues(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("A"), testField(t, "id", values.NullableLong))
	})
	b := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("B"), testField(t, "name", values.NullableLong))
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different result values should (very likely) produce different hashes")
	}
}

func TestMapPlan_HashCodeWithoutChildren_NilResultValue(t *testing.T) {
	t.Parallel()
	p := &RecordQueryMapPlan{}
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
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(inner, dv)
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(scan, nil)
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestFirstOrDefaultPlan_ConstructorRejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryFirstOrDefaultPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

func TestFirstOrDefaultPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(inner, nil)
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestFirstOrDefaultPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryFirstOrDefaultPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestFirstOrDefaultPlan_Explain_ContainsFirstOrDefault(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("Scan(T)"), nil)
	})
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
	p := &RecordQueryFirstOrDefaultPlan{}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	dv := values.LiteralValue(int64(42))
	a := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("A"), dv)
	})
	b := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("B"), dv)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same default value should be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_DifferentDefaultValue(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("A"), values.LiteralValue(int64(1)))
	})
	b := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("B"), values.LiteralValue(int64(2)))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different default values should not be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_BothNilDefaultValue(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("A"), nil)
	})
	b := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("B"), nil)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two FirstOrDefaultPlans with nil default values should be equal")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	fod := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("Inner"), nil)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if fod.EqualsPlanWithoutChildren(scan) {
		t.Fatal("FirstOrDefaultPlan should not equal ScanPlan")
	}
}

func TestFirstOrDefaultPlan_EqualsWithoutChildren_NotEqualToMapPlan(t *testing.T) {
	t.Parallel()
	// Both compare their single Value semantically (RFC-176 P2), but the
	// concrete plan type is the first discriminator and must prevent a match.
	fod := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("FirstInner"), nil)
	})
	m := mustChecked(t, func() (*RecordQueryMapPlan, error) {
		return NewRecordQueryMapPlan(stub("MapInner"), &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong})
	})
	if fod.EqualsPlanWithoutChildren(m) {
		t.Fatal("FirstOrDefaultPlan should not equal MapPlan")
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	dv := values.LiteralValue(int64(42))
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("Inner"), dv)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_DiffersForDifferentDefaults(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("A"), values.LiteralValue(int64(1)))
	})
	b := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("B"), values.LiteralValue(int64(2)))
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different default values should (very likely) produce different hashes")
	}
}

func TestFirstOrDefaultPlan_HashCodeWithoutChildren_NilDefaultValue(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
		return NewRecordQueryFirstOrDefaultPlan(stub("Inner"), nil)
	})
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
		"PredicatesFilter": mustChecked(t, func() (*RecordQueryPredicatesFilterPlan, error) {
			return NewRecordQueryPredicatesFilterPlan(stub("PredicatesInner"), nil)
		}).HashCodeWithoutChildren(),
		"Map": mustChecked(t, func() (*RecordQueryMapPlan, error) {
			return NewRecordQueryMapPlan(stub("MapInner"), &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong})
		}).HashCodeWithoutChildren(),
		"FirstOrDefault": mustChecked(t, func() (*RecordQueryFirstOrDefaultPlan, error) {
			return NewRecordQueryFirstOrDefaultPlan(stub("FirstInner"), nil)
		}).HashCodeWithoutChildren(),
		// Include existing types to verify no collisions with the new ones.
		"Filter": mustChecked(t, func() (*RecordQueryFilterPlan, error) {
			return NewRecordQueryFilterPlan(nil, stub("FilterInner"))
		}).HashCodeWithoutChildren(),
		"Distinct": mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
			return NewRecordQueryDistinctPlan(stub("DistinctInner"))
		}).HashCodeWithoutChildren(),
		"Scan": mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
		}).HashCodeWithoutChildren(),
	}
	seen := make(map[uint64]string)
	for name, h := range hashes {
		if prev, ok := seen[h]; ok {
			t.Fatalf("hash collision between %s and %s: %d", name, prev, h)
		}
		seen[h] = name
	}
}
