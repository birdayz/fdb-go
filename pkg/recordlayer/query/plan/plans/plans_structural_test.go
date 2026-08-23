package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRecordQueryScanPlan_WithPrimaryKey_PreservesComparisons pins the
// builder-preservation footgun fixed in RFC-189 B1: WithPrimaryKey dropped the
// scan comparisons on copy (asymmetric with WithScanComparisons, which preserves
// the PK), silently un-narrowing a scan when the PK was set AFTER the
// comparisons. Production set the PK first so it was latent, but a copy must
// carry every scan field.
func TestRecordQueryScanPlan_WithPrimaryKey_PreservesComparisons(t *testing.T) {
	t.Parallel()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7))
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build equality range")
	}
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	}).
		WithScanComparisons([]*predicates.ComparisonRange{res.Range}).
		WithPrimaryKey([]values.Value{&values.ConstantValue{Value: int64(7), Typ: values.NullableLong}})

	if got := scan.GetScanComparisons(); len(got) != 1 {
		t.Fatalf("WithPrimaryKey dropped the scan comparisons: got %d, want 1", len(got))
	}
	if got := scan.GetPrimaryKeyValues(); len(got) != 1 {
		t.Fatalf("expected 1 PK value after WithPrimaryKey, got %d", len(got))
	}
}

func TestRecordQueryIndexPlan_FanOutOrderingHintsAbstain(t *testing.T) {
	t.Parallel()

	empty := predicates.EmptyComparisonRange()
	indexRowType := values.NewRecordType("index_test_row", false, []values.Field{
		{Name: "TAGS", FieldType: values.NullableString, Ordinal: 0},
		{Name: "SCORE", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	fanOut := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"idx_tags",
			[]*predicates.ComparisonRange{empty},
			[]string{"T"}, indexRowType, false,
		)
	}).WithIndexMetadata(
		[]string{"TAGS"},
		[]string{"ID"},
		false,
	).WithDistinctRecordsSignal(true).
		WithKeyComponentTypes([]values.Type{values.NullableString})

	if ordering := fanOut.HintOrdering(); ordering.IsKnown ||
		len(ordering.Keys) != 0 {
		t.Fatalf(
			"fan-out HintOrdering() = %#v, want conservative empty ordering",
			ordering,
		)
	}
	if rich := fanOut.HintRichOrdering(); len(rich.GetKeys()) != 0 {
		t.Fatalf(
			"fan-out HintRichOrdering() keys = %#v, want empty",
			rich.GetKeys(),
		)
	}

	scalar := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"idx_score",
			[]*predicates.ComparisonRange{empty},
			[]string{"T"}, indexRowType, false,
		)
	}).WithIndexMetadata(
		[]string{"SCORE"},
		[]string{"ID"},
		false,
	).WithDistinctRecordsSignal(false).
		WithKeyComponentTypes([]values.Type{values.NullableLong}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
	if ordering := scalar.HintOrdering(); !ordering.IsKnown ||
		len(ordering.Keys) != 2 {
		t.Fatalf(
			"scalar HintOrdering() = %#v, want SCORE + ID ordering",
			ordering,
		)
	}
	if rich := scalar.HintRichOrdering(); len(rich.GetKeys()) != 2 {
		t.Fatalf(
			"scalar HintRichOrdering() keys = %#v, want SCORE + ID",
			rich.GetKeys(),
		)
	}
}

func TestRecordQueryIndexPlan_ExpressionKeyOrderingHintsAbstain(t *testing.T) {
	t.Parallel()

	expressionKey := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"idx_cardinality_tags",
			[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
			[]string{"T"}, exactTestRecordType(), false,
		)
	}).WithIndexMetadata(
		[]string{"TAGS"},
		[]string{"ID"},
		false,
	).WithDistinctRecordsSignal(false).
		WithOrderingKeyNamesUnavailable()

	if ordering := expressionKey.HintOrdering(); ordering.IsKnown ||
		len(ordering.Keys) != 0 {
		t.Fatalf(
			"expression-key HintOrdering() = %#v, want conservative empty ordering",
			ordering,
		)
	}
	if rich := expressionKey.HintRichOrdering(); len(rich.GetKeys()) != 0 {
		t.Fatalf(
			"expression-key HintRichOrdering() keys = %#v, want empty",
			rich.GetKeys(),
		)
	}

	// A later metadata-preserving stamp must not erase the explicit unsafe
	// state and reintroduce TAGS/ID ordering.
	restamped := expressionKey.WithIndexMetadata(
		[]string{"TAGS"},
		[]string{"ID"},
		false,
	)
	if restamped.HintOrdering().IsKnown ||
		len(restamped.HintRichOrdering().GetKeys()) != 0 {
		t.Fatal("WithIndexMetadata re-enabled expression-key ordering")
	}
}

// stubPlan is a minimal RecordQueryPlan for use as an inner child.
type stubPlan struct {
	PlanExprBase
	label string
}

func (s *stubPlan) GetResultType() values.Type                     { return values.NotNullLong }
func (s *stubPlan) GetChildren() []RecordQueryPlan                 { return nil }
func (s *stubPlan) Explain() string                                { return s.label }
func (s *stubPlan) EqualsPlanWithoutChildren(RecordQueryPlan) bool { return true }
func (s *stubPlan) HashCodeWithoutChildren() uint64                { return 0 }

func stub(label string) *stubPlan {
	base, err := newPlanExprBaseForType("stubPlan", values.NotNullLong)
	if err != nil {
		panic(err)
	}
	return &stubPlan{PlanExprBase: base, label: label}
}

func structuralLong(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
}

func structuralString(value string) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullString}
}

// ---------------------------------------------------------------------------
// RecordQueryLimitPlan
// ---------------------------------------------------------------------------

func TestLimitPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(inner, 10, 0)
	})
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
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("X"), 5, 0)
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong from inner", p.GetResultType())
	}
}

func TestLimitPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(inner, 10, 0)
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestLimitPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryLimitPlan{limit: 10}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil for nil inner", cs)
	}
	if _, err := NewRecordQueryLimitPlan(nil, 10, 0); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

func TestLimitPlan_Explain_NoOffset(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Scan(T)"), 10, 0)
	})
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
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Scan(T)"), 5, 20)
	})
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
	a := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("A"), 10, 5)
	})
	b := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("B"), 10, 5)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same limit+offset should be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_DifferentLimit(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("A"), 10, 0)
	})
	b := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("B"), 20, 0)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different limits should not be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_DifferentOffset(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("A"), 10, 0)
	})
	b := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("B"), 10, 5)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different offsets should not be equal")
	}
}

func TestLimitPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	lim := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Inner"), 10, 0)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if lim.EqualsPlanWithoutChildren(scan) {
		t.Fatal("LimitPlan should not equal ScanPlan")
	}
}

func TestLimitPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Inner"), 10, 5)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestLimitPlan_HashCodeWithoutChildren_DiffersForDifferentParams(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("A"), 10, 0)
	})
	b := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("B"), 20, 0)
	})
	c := mustChecked(t, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("C"), 10, 5)
	})
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
	p := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, inner)
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

func TestFilterPlan_GetResultType_DelegatesInner(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestFilterPlan_ConstructorRejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryFilterPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

func TestFilterPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryFilterPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestFilterPlan_Explain_ContainsFilter(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Scan(T)"))
	})
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
	p := &RecordQueryFilterPlan{}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestFilterPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	a := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("B"))
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same predicates should be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_DifferentPredicateCount(t *testing.T) {
	t.Parallel()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	a := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{p1}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{p1, p2}, stub("B"))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different predicate counts should not be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_DifferentPredicate(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriFalse),
		}, stub("B"))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different predicates should not be equal")
	}
}

func TestFilterPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	f := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan(nil, stub("Inner"))
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if f.EqualsPlanWithoutChildren(scan) {
		t.Fatal("FilterPlan should not equal ScanPlan")
	}
}

func TestFilterPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Inner"))
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestFilterPlan_HashCodeWithoutChildren_DiffersForDifferentPreds(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
		}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriFalse),
		}, stub("B"))
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different predicates should (very likely) produce different hashes")
	}
}

func TestFilterPlan_CopiesPredicateSlice(t *testing.T) {
	t.Parallel()
	preds := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	p := mustChecked(t, func() (*RecordQueryFilterPlan,
		// Mutate the original slice.
		error,
	) {
		return NewRecordQueryFilterPlan(preds, stub("Inner"))
	})

	preds[0] = predicates.NewConstantPredicate(predicates.TriFalse)
	// The plan's copy should be unaffected.
	got := p.GetPredicates()[0]
	if predicates.PredicateEquals(got, preds[0]) {
		t.Fatal("filter plan should have an independent copy of the predicate slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryInMemorySortPlan (the live Go-only physical sort; the legacy
// RecordQuerySortPlan was removed as producer-less dead code)
// ---------------------------------------------------------------------------

func TestInMemorySortPlan_Construction(t *testing.T) {
	t.Parallel()
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong), Desc: false},
	}
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(inner, keys)
	})
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

func TestInMemorySortPlan_ConstructorRejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryInMemorySortPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

// TestInMemorySortPlan_GetResultType_PreservesInnerType (F40) pins that a sort
// FLOWS THROUGH its inner's result type — a sort reorders rows but preserves the
// row shape. This is the assertion the deleted RecordQuerySortPlan's
// PreservesInnerType test couldn't migrate because GetResultType() returned
// UnknownType unconditionally.
func TestInMemorySortPlan_GetResultType_PreservesInnerType(t *testing.T) {
	t.Parallel()
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
	}
	p := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(inner, keys)
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from inner)", p.GetResultType())
	}
}

func TestInMemorySortPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryInMemorySortPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestInMemorySortPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
		{Field: "name", ValueExpr: testField(t, "name", values.NullableLong), Desc: true},
	}
	p := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("Scan(T)"), keys)
	})
	got := p.Explain()
	if !strings.Contains(got, "InMemorySort") {
		t.Fatalf("Explain = %q, missing 'InMemorySort'", got)
	}
	// The key is RENDERED FROM ValueExpr, so what appears is the read —
	// source and ordinal included — rather than the Field text the key was
	// constructed with. Field is the spelling BEFORE re-anchoring and stops
	// describing the read once the key is bound to its input, which is how
	// EXPLAIN came to print a minted correlation counter.
	if !strings.Contains(got, "id#0 ASC") {
		t.Fatalf("Explain = %q, missing 'id#0 ASC'", got)
	}
	if !strings.Contains(got, "name#0 DESC") {
		t.Fatalf("Explain = %q, missing 'name#0 DESC'", got)
	}
}

func TestInMemorySortPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong), Desc: false},
	}
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), keys)
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), keys)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same sort keys should be equal")
	}
}

func TestInMemorySortPlan_EqualsWithoutChildren_DifferentKeyCount(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
			{Field: "name", ValueExpr: testField(t, "name", values.NullableLong)},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different key counts should not be equal")
	}
}

func TestInMemorySortPlan_EqualsWithoutChildren_DifferentDesc(t *testing.T) {
	t.Parallel()
	// Isolate the direction: share one semantically-equal ValueExpr + Field so
	// only Desc differs.
	fv := testField(t, "id", values.NullableLong)
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "id", ValueExpr: fv, Desc: false},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "id", ValueExpr: fv, Desc: true},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different direction flags should not be equal")
	}
}

func TestInMemorySortPlan_EqualsWithoutChildren_DifferentValue(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "name", ValueExpr: testField(t, "name", values.NullableLong)},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different keys should not be equal")
	}
}

func TestInMemorySortPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("Inner"), nil)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if s.EqualsPlanWithoutChildren(scan) {
		t.Fatal("InMemorySortPlan should not equal ScanPlan")
	}
}

func TestInMemorySortPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
	}
	p := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("Inner"), keys)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestInMemorySortPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "name", ValueExpr: testField(t, "name", values.NullableLong)},
		})
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different sort keys should (very likely) have different hashes")
	}
}

// TestInMemorySortPlan_SemanticSortKeyIdentity (F41) pins that sort-plan
// identity is SEMANTIC on the ValueExpr, not pointer identity. Two plans whose
// sort keys carry DISTINCT but semantically-equal ValueExpr instances must be
// EqualsWithoutChildren-equal (was FALSE before the fix — pointer compare via
// struct `!=`) and hash identically. This is the incomplete-F21 case: pointer
// identity would split a semantically-single sort into two memo members.
func TestInMemorySortPlan_SemanticSortKeyIdentity(t *testing.T) {
	t.Parallel()
	// Distinct FieldValue instances, same semantics + direction.
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong), Desc: true, NullsFirst: true},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "id", ValueExpr: testField(t, "id", values.NullableLong), Desc: true, NullsFirst: true},
		})
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("semantically-equal sort keys with distinct ValueExpr instances must be EqualsWithoutChildren-equal (F41)")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("semantically-equal sort keys must hash identically (equal⟹same-hash)")
	}
}

// TestInMemorySortPlan_SortKeyValueDistinguishes (F41) pins that the semantic
// ValueExpr is actually PART of identity: same display Field but a different
// underlying sort Value ⇒ NOT equal, and (very likely) hashes apart.
func TestInMemorySortPlan_SortKeyValueDistinguishes(t *testing.T) {
	t.Parallel()
	// Field is display-only — hold it constant so only the Value differs.
	a := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("A"), []SortKey{
			{Field: "k", ValueExpr: testField(t, "id", values.NullableLong)},
		})
	})
	b := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("B"), []SortKey{
			{Field: "k", ValueExpr: testField(t, "name", values.NullableLong)},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different sort Values (same display Field) must NOT be EqualsWithoutChildren-equal (F41)")
	}
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different sort Values should (very likely) hash apart")
	}
}

func TestInMemorySortPlan_CopiesKeySlice(t *testing.T) {
	t.Parallel()
	keys := []SortKey{
		{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)},
	}
	p := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(stub("Inner"), keys)
	})
	keys[0].Desc = true
	if p.GetSortKeys()[0].Desc {
		t.Fatal("sort plan should have an independent copy of the key slice")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryDistinctPlan
// ---------------------------------------------------------------------------

func TestDistinctPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(inner)
	})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if p.GetInner() != inner {
		t.Fatal("GetInner() mismatch")
	}
}

func TestDistinctPlan_ConstructorRejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryDistinctPlan(nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
}

func TestDistinctPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryDistinctPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil for nil inner", cs)
	}
}

func TestDistinctPlan_Explain(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("Scan(T)"))
	})
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
	p := &RecordQueryDistinctPlan{}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>'", got)
	}
}

func TestDistinctPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("B"))
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two DistinctPlans should be equal (type-only discriminator)")
	}
}

func TestDistinctPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	d := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("Inner"))
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if d.EqualsPlanWithoutChildren(scan) {
		t.Fatal("DistinctPlan should not equal ScanPlan")
	}
}

func TestDistinctPlan_HashCodeWithoutChildren_SameAcrossInstances(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("X"))
	})
	// Distinct has no node-info params, so all instances hash the same.
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("all DistinctPlan instances should have the same hash (no params)")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryUnorderedPrimaryKeyDistinctPlan
// ---------------------------------------------------------------------------

func TestUnorderedPrimaryKeyDistinctPlan_QuantifierAndRelink(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	alias := values.NamedCorrelationIdentifier("pk_distinct_inner")
	innerRef := expressions.FinalOfAtStage(inner, expressions.StageCanonical)
	innerQ := expressions.NamedPhysicalQuantifier(alias, innerRef)
	p := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		return NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(innerQ)
	})

	if p.GetInner() != inner {
		t.Fatal("GetInner() did not resolve the supplied quantifier")
	}
	if got := p.GetInnerQuantifier(); got.GetRangesOver() != innerRef ||
		got.Kind() != expressions.QuantifierPhysical ||
		got.GetAlias() != alias {
		t.Fatalf("inner quantifier = %#v, want supplied physical quantifier", got)
	}
	resultQOV, ok := values.AsQuantifiedObjectValue(p.GetResultValue())
	layout := requireProvidedLayout(t, p)
	if !ok || resultQOV != layout.Carrier() {
		t.Fatalf("GetResultValue() = %#v, want provided-layout carrier %#v", p.GetResultValue(), layout.Carrier())
	}

	replacement := stub("Replacement")
	replacementQ := QuantifierOverPlan(replacement)
	relinkedExpr, err := p.WithChildren([]expressions.Quantifier{replacementQ})
	if err != nil {
		t.Fatalf("WithChildren: %v", err)
	}
	relinked, ok := relinkedExpr.(*RecordQueryUnorderedPrimaryKeyDistinctPlan)
	if !ok || relinked == p || relinked.GetInner() != replacement {
		t.Fatalf("relinked plan = %#v, want fresh plan over replacement", relinkedExpr)
	}
	if p.GetInner() != inner {
		t.Fatal("WithChildren mutated the receiver")
	}
	for _, invalid := range [][]expressions.Quantifier{
		nil,
		{innerQ, replacementQ},
	} {
		if _, err := p.WithChildren(invalid); err == nil {
			t.Fatalf("WithChildren accepted %d children", len(invalid))
		}
	}

	withInner := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		return p.WithInner(replacement)
	})
	if withInner == p || withInner.GetInner() != replacement || p.GetInner() != inner {
		t.Fatal("WithInner did not copy-preservingly relink the child")
	}
}

func TestUnorderedPrimaryKeyDistinctPlan_Hints(t *testing.T) {
	t.Parallel()
	pk := testField(t, "ID", values.NotNullLong)
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(
			[]string{"T"}, exactTestRecordType(), true,
		)
	}).WithPrimaryKey([]values.Value{pk}).
		WithKeyComponentTypes([]values.Type{values.NotNullLong})
	p := mustChecked(t, func() (*RecordQueryUnorderedPrimaryKeyDistinctPlan, error) {
		return NewRecordQueryUnorderedPrimaryKeyDistinctPlan(inner)
	})

	childCost := properties.Cost{Cardinality: 100, CPU: 7}
	if got, want := p.HintCost(
		[]properties.Cost{childCost},
		nil,
	), properties.DistinctCost(childCost); got != want {
		t.Fatalf("HintCost() = %#v, want %#v", got, want)
	}
	if got := p.HintCost(nil, nil); got != (properties.Cost{}) {
		t.Fatalf("HintCost(nil) = %#v, want zero", got)
	}

	if p.OrderingSourceRef() != p.GetInnerQuantifier().GetRangesOver() {
		t.Fatal("ordering source is not the child reference")
	}
	ordering := p.HintOrdering()
	if !ordering.IsKnown ||
		len(ordering.Keys) != 1 ||
		ordering.Keys[0] != pk ||
		!ordering.DescendingAt(0) {
		t.Fatalf("HintOrdering() = %#v, want reverse primary-key ordering", ordering)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryProjectionPlan
// ---------------------------------------------------------------------------

func TestProjectionPlan_Construction(t *testing.T) {
	t.Parallel()
	projs := []values.Value{testField(t, "id", values.NotNullLong), testField(t, "name", values.NotNullString)}
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, inner)
	})
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

func TestProjectionPlan_DefensiveCopy(t *testing.T) {
	t.Parallel()
	originalProjection := testField(t, "id", values.NotNullLong)
	projections := []values.Value{originalProjection}
	aliases := []string{"ID_ALIAS"}
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(projections, aliases, stub("Inner"))
	})

	projections[0] = testField(t, "mutated", values.NotNullLong)
	aliases[0] = "MUTATED_INPUT"
	if got := p.GetProjections(); len(got) != 1 || got[0] != originalProjection {
		t.Fatalf("constructor retained mutable projection input: got %v", got)
	}
	if got := p.GetAliases(); len(got) != 1 || got[0] != "ID_ALIAS" {
		t.Fatalf("constructor retained mutable alias input: got %v", got)
	}

	gotProjections := p.GetProjections()
	gotAliases := p.GetAliases()
	gotProjections[0] = values.NewBooleanValue(false)
	gotAliases[0] = "MUTATED_GETTER"
	if got := p.GetProjections(); len(got) != 1 || got[0] != originalProjection {
		t.Fatalf("GetProjections exposed mutable semantic identity: got %v", got)
	}
	if got := p.GetAliases(); len(got) != 1 || got[0] != "ID_ALIAS" {
		t.Fatalf("GetAliases exposed mutable semantic identity: got %v", got)
	}
}

func TestProjectionPlan_AliasesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	readA := testField(t, "A", values.NullableLong)
	readB := readA
	if !values.SemanticEqualsUnderAliasMap(readA, readB, values.EmptyAliasMap()) {
		t.Fatal("test requires the shared exact projection Value to be semantically equal")
	}
	aliased := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{readA}, []string{"OUTPUT_ALIAS"}, stub("Inner"))
	})
	aliasedTwin := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{readB}, []string{"OUTPUT_ALIAS"}, stub("OtherInner"))
	})
	renamed := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{readB}, []string{"OTHER_ALIAS"}, stub("Inner"))
	})

	// TWO SPELLINGS OF AN ALIAS ARE TWO ALIASES — the plan-side twin of the
	// same inversion in TestLogicalProjection_AliasesAreSemanticIdentity. The
	// output-name authority no longer folds, so a surviving case difference
	// came from two different QUOTED aliases and names two different columns.
	caseTwin := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{readB}, []string{"output_alias"}, stub("Inner"))
	})
	if aliased.EqualsPlanWithoutChildren(caseTwin) {
		t.Fatal(`AS "output_alias" and AS "OUTPUT_ALIAS" name different columns and must not be one plan identity`)
	}

	if !aliased.EqualsPlanWithoutChildren(aliasedTwin) {
		t.Fatal("aliases producing the same executor-visible output name reported unequal")
	}
	if aliased.HashCodeWithoutChildren() != aliasedTwin.HashCodeWithoutChildren() {
		t.Fatal("equal aliased projection plans produced different hash codes")
	}
	if aliased.EqualsPlanWithoutChildren(renamed) {
		t.Fatal("projection plans with different executor-visible output names reported equal")
	}
	if aliased.HashCodeWithoutChildren() == renamed.HashCodeWithoutChildren() {
		t.Fatal("different executor-visible output names produced the same projection-plan hash")
	}
}

func TestProjectionPlan_EmptyAliasesAreEquivalent(t *testing.T) {
	t.Parallel()
	projection := []values.Value{testField(t, "id", values.NotNullLong)}
	withoutAliases := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projection, stub("Inner"))
	})
	emptyAliases := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			projection, []string{}, stub("Inner"))
	})
	blankPlaceholder := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			projection, []string{""}, stub("Inner"))
	})

	for _, candidate := range []*RecordQueryProjectionPlan{emptyAliases, blankPlaceholder} {
		if !withoutAliases.EqualsPlanWithoutChildren(candidate) {
			t.Fatal("missing and empty aliases must use the same derived output-name semantics")
		}
		if withoutAliases.HashCodeWithoutChildren() != candidate.HashCodeWithoutChildren() {
			t.Fatal("semantically equal empty-alias representations produced different hashes")
		}
	}
}

func TestProjectionPlan_DerivedOutputNamesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	readA := testFieldAt(t, "A", 0, values.NullableLong)
	readB := testFieldAt(t, "B", 0, values.NullableLong)

	testCases := []struct {
		name        string
		aliases     []string
		useAliasAPI bool
	}{
		{name: "nil"},
		{name: "empty slice", aliases: []string{}, useAliasAPI: true},
		{name: "trailing empty", aliases: []string{""}, useAliasAPI: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			build := func(v values.Value) *RecordQueryProjectionPlan {
				if tc.useAliasAPI {
					return mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
						return NewRecordQueryProjectionPlanWithAliases(
							[]values.Value{v}, tc.aliases, stub("Inner"))
					})
				}
				return mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
					return NewRecordQueryProjectionPlan([]values.Value{v}, stub("Inner"))
				})
			}
			left, right := build(readA), build(readB)
			if left.EqualsPlanWithoutChildren(right) {
				t.Fatal("semantic-equal reads with different derived output names reported equal")
			}
			if left.HashCodeWithoutChildren() == right.HashCodeWithoutChildren() {
				t.Fatal("different derived output names produced the same projection-plan hash")
			}
		})
	}
}

func TestProjectionPlan_NestedFieldNamesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	one := func() values.Value {
		return &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	}
	leftValue := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  testFieldAt(t, "A", 0, values.NullableLong),
		Right: one(),
	}
	rightValue := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  testFieldAt(t, "B", 0, values.NullableLong),
		Right: one(),
	}
	left := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{leftValue}, stub("Inner"))
	})
	right := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{rightValue}, stub("Inner"))
	})
	if left.EqualsPlanWithoutChildren(right) {
		t.Fatal("nested baked field display names changed output schema but compared equal")
	}
	if left.HashCodeWithoutChildren() == right.HashCodeWithoutChildren() {
		t.Fatal("different nested field display names produced the same projection-plan hash")
	}
}

func TestProjectionPlan_IsIdentityChecksSchemaAndInnerCorrelation(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	identityValue := mustChecked(t, func() (values.Value, error) {
		return innerQ.RequireFlowedObjectValue()
	})

	// IsIdentity remains total on the historical malformed shape so rule code
	// can fail closed while inspecting package-local adversaries. The public
	// constructor must reject that shape because the executor would wrap the
	// whole row into one output slot.
	identity := &RecordQueryProjectionPlan{
		projections: []values.Value{identityValue},
		innerQ:      innerQ,
	}
	if !identity.IsIdentity() {
		t.Fatal("unaliased QOV over the projection's inner quantifier must be identity")
	}
	if !(&RecordQueryProjectionPlan{
		projections: []values.Value{identityValue},
		aliases:     []string{""},
		innerQ:      innerQ,
	}).IsIdentity() {
		t.Fatal("an explicit empty alias must preserve identity")
	}
	if (&RecordQueryProjectionPlan{
		projections: []values.Value{identityValue},
		aliases:     []string{"RENAMED"},
		innerQ:      innerQ,
	}).IsIdentity() {
		t.Fatal("a schema-renaming alias must not be identity")
	}
	if (&RecordQueryProjectionPlan{
		projections: []values.Value{identityValue},
		aliases:     []string{"", ""},
		innerQ:      innerQ,
	}).IsIdentity() {
		t.Fatal("a malformed alias list must fail identity closed")
	}
	otherAlias := values.NamedCorrelationIdentifier("OTHER")
	if (&RecordQueryProjectionPlan{
		projections: []values.Value{mustTestQOV(t, otherAlias.Name(), exactTestRecordType())},
		innerQ:      innerQ,
	}).IsIdentity() {
		t.Fatal("a QOV over a different quantifier must not be identity")
	}
	if _, err := NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{identityValue}, nil, innerQ); err == nil {
		t.Fatal("constructor accepted a one-slot whole-row projection")
	}
}

// TestProjectionPlan_Identity_ResolvedOrdinal pins plan-level identity for
// plan-time-resolved ordinal accessors: two
// projection plans whose reads differ ONLY by resolved ordinal (the
// recursive-CTE duplicate-alias wrap — two slots both named X) must NOT be
// memo-identical. Under the RFC-176 semantic identity the ordinal is a
// structural discriminator (FieldValue's EqualsWithoutChildren and
// writeSemanticHash arms both fold it — Java: distinct ofOrdinalNumber
// ordinals are distinct FieldPaths); unifying them would let extraction pick
// the plan reading the WRONG slot. The Explain assertion additionally pins
// the explain-format rendering ("X#0"/"X#1", Java's FieldPath `#ordinal`
// syntax): debug output rendering both as bare "X" would make different
// plans read identically.
func TestProjectionPlan_Identity_ResolvedOrdinal(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	// Reads of slot 0 and slot 1 of a duplicate-named [X, X] row.
	// Duplicate names are machinery-owned ordinal rows and deliberately bypass
	// NewRecordType's user-schema duplicate-name rejection.
	duplicateLayout := &values.RecordType{RecordName: "duplicate_x", Nullable: false, Fields: []values.Field{
		{Name: "X", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "X", FieldType: values.NullableLong, Ordinal: 1},
	}}
	read := func(ordinal int) values.Value {
		request, err := values.FieldByOrdinal(ordinal)
		if err != nil {
			t.Fatalf("field request X#%d: %v", ordinal, err)
		}
		field, err := values.ResolveFieldAccess(
			mustTestQOV(t, "duplicate_x", duplicateLayout), []values.FieldRequest{request})
		if err != nil {
			t.Fatalf("field X#%d: %v", ordinal, err)
		}
		return field
	}
	read01 := []values.Value{read(0), read(1)}
	read00 := []values.Value{read(0), read(0)}
	p01 := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(read01, []string{"A", "B"}, inner)
	})
	p00 := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(read00, []string{"A", "B"}, inner)
	})

	// The renderings carry the ordinal — the identity discriminator.
	if !strings.Contains(p01.Explain(), "X#0") || !strings.Contains(p01.Explain(), "X#1") {
		t.Fatalf("explain must render resolved ordinals (X#0, X#1), got %q", p01.Explain())
	}
	if p01.EqualsPlanWithoutChildren(p00) {
		t.Fatal("plans reading (slot0,slot1) vs (slot0,slot0) must NOT compare equal — memo unification would let extraction pick the wrong slot")
	}
	if p01.HashCodeWithoutChildren() == p00.HashCodeWithoutChildren() {
		t.Fatal("plans reading (slot0,slot1) vs (slot0,slot0) must not hash equal")
	}

	// Same reads ⟹ equal and hash-equal.
	p01b := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{read(0), read(1)}, []string{"A", "B"}, inner)
	})
	if !p01.EqualsPlanWithoutChildren(p01b) {
		t.Fatal("identical ordinal reads must compare equal")
	}
	if p01.HashCodeWithoutChildren() != p01b.HashCodeWithoutChildren() {
		t.Fatal("identical ordinal reads must hash equal")
	}
}

// TestProjectionPlan_Identity_OrdinalVsLiteralHashField pins that an ORDINAL
// read of X at slot 0 and a plain NAME-read of a field literally named "X#0"
// (a quoted identifier may legally contain '#') are distinct plan identities.
// Under the RFC-176 semantic model this holds structurally (FieldValue
// equality compares field text + resolved-accessor presence + ordinal) and in the
// hash (writeSemanticHash's FieldValue arm doubles raw '#', keeping its
// discriminator injective over (field text, ordinal)). Historic origin:
// when identity was keyed on ExplainValue
// renderings, both rendered "X#0" pre-escape and the plans memo-unified —
// the '#'-doubling ("X##0") now also serves as the explain-format
// injectivity pin (see values.ExplainValue's FieldValue arm).
func TestProjectionPlan_Identity_OrdinalVsLiteralHashField(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	ordinalPlan := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{testFieldAt(t, "X", 0, values.NullableLong)},
			[]string{"A"}, inner)
	})
	literalPlan := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{testField(t, "X#0", values.NullableLong)},
			[]string{"A"}, inner)
	})

	if ordinalPlan.EqualsPlanWithoutChildren(literalPlan) {
		t.Fatal("ordinal read of X@0 and a name-read of a field literally named X#0 must NOT compare equal")
	}
	if ordinalPlan.HashCodeWithoutChildren() == literalPlan.HashCodeWithoutChildren() {
		t.Fatal("ordinal read of X@0 and a name-read of field X#0 must not hash equal")
	}
}

// TestProjectionPlan_GetResultType pins that a projection STATES the row it
// produces rather than declining with UnknownType.
//
// This assertion is inverted from what it used to be. It required UnknownType,
// which was the decline that made every consumer re-derive the row by NAME —
// the supply side of two live wrong-behaviour defects. A projection's row is
// its columns, derived from the result value exactly as Java derives it
// (RelationalExpression.java:194-197, which no Java plan overrides).
func TestProjectionPlan_GetResultType(t *testing.T) {
	t.Parallel()

	// Empty projection list: a record with no fields, NOT UnknownType. The
	// arity is the claim — an empty record says "zero columns", where
	// UnknownType said "cannot tell" and every reader failed closed on it.
	empty := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(nil, stub("X"))
	})
	emptyRT, ok := empty.GetResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("GetResultType() = %v (%T), want a *values.RecordType", empty.GetResultType(), empty.GetResultType())
	}
	if len(emptyRT.Fields) != 0 {
		t.Errorf("empty projection stated %d field(s), want 0", len(emptyRT.Fields))
	}

	// Two columns: one aliased, one not. The stated row must have one field
	// per projected column, in order, and NO field may be unnamed — an unnamed
	// field reaches the name-keyed readers as "" and resolves to nothing.
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(
			[]values.Value{testField(t, "A", values.NullableLong), testField(t, "B", values.NullableLong)},
			[]string{"OUT", ""}, stub("X"))
	})
	rt, ok := p.GetResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("GetResultType() = %v (%T), want a *values.RecordType", p.GetResultType(), p.GetResultType())
	}
	if len(rt.Fields) != 2 {
		t.Fatalf("stated %d field(s), want 2: %v", len(rt.Fields), rt.Fields)
	}
	if rt.Fields[0].Name != "OUT" {
		t.Errorf("slot 0 named %q, want %q (the user's AS wins)", rt.Fields[0].Name, "OUT")
	}
	for i, f := range rt.Fields {
		if f.Name == "" {
			t.Errorf("slot %d is UNNAMED; every slot must carry a name "+
				"(the user's alias, the column's own name, or Java's \"_\"+ordinal)", i)
		}
		if f.Ordinal != i {
			t.Errorf("slot %d carries ordinal %d, want %d", i, f.Ordinal, i)
		}
	}
}

// TestProjectionPlan_ResultTypeMatchesLogicalTwin pins that the physical
// projection and its logical twin state the SAME row for the same columns.
// They share values.ProjectionResultValue precisely so they cannot drift; this
// is the test that notices if one of them stops using it.
func TestProjectionPlan_ResultTypeMatchesLogicalTwin(t *testing.T) {
	t.Parallel()

	projections := []values.Value{testField(t, "A", values.NullableLong), testField(t, "B", values.NullableLong)}
	aliases := []string{"OUT", ""}

	physical := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlanWithAliases(projections, aliases, stub("X"))
	})
	logical, err := values.ProjectionResultValue(projections, aliases)
	if err != nil {
		t.Fatalf("ProjectionResultValue: %v", err)
	}
	if !logical.Type().Equals(physical.GetResultType()) {
		t.Errorf("logical twin states %v, physical states %v — the two projections "+
			"must state the same row", logical.Type(), physical.GetResultType())
	}
}

// TestProjectionResultValue_RejectsWholeRowProjection pins the DERIVATION's
// refusal to synthesise a row for a one-slot whole-row projection.
//
// SCOPE, stated precisely because an earlier revision of this comment
// overstated it as an "unbuildability pin". It is not one. This drives
// values.ProjectionResultValue, a derivation; the LogicalProjectionExpression
// constructors do not otherwise validate the projection list. What this pins is
// narrower and still worth having: the derivation must keep refusing, because
// the executor emits one positional slot per projection, so that shape WRAPS
// its inner's row rather than passing it through and has no name to give its
// single field.
//
// WHAT "WHOLE-ROW" MEANS HERE IS AN IDENTITY, NOT A TYPE, and the two arms
// below are what separate them. A machinery correlation — `_current`, or a
// minted `q$N` — names a row the SQL never named, so a one-slot wrap of it has
// nothing to call its field. A NAMED correlation is a source the user wrote,
// and projecting it whole is `SELECT x`, whose column is x. Deciding this on
// the QOV's TYPE instead split one shape in half: `SELECT x FROM t, t.arr AS x`
// planned for a scalar element and was refused for a STRUCT one.
//
// The alarm direction is REVIVAL: a success on the first arm means the
// derivation started synthesising a row for a shape that cannot name one, and
// every consumer keyed on "unstated" would begin trusting it.
func TestProjectionResultValue_RejectsWholeRowProjection(t *testing.T) {
	t.Parallel()

	machineryRow, err := values.NewQuantifiedObjectValue(
		values.UniqueCorrelationIdentifier(), exactTestRecordType())
	if err != nil {
		t.Fatalf("machinery-row QOV: %v", err)
	}
	if _, err := values.ProjectionResultValue([]values.Value{machineryRow}, nil); err == nil {
		t.Fatal("the derivation SYNTHESISED a row for a one-slot whole-row projection. " +
			"The alarm is REVIVAL: this shape must keep declining, because the executor " +
			"emits one positional slot per projection, so it wraps a row the SQL never " +
			"named and has no name for its single field.")
	}

	// Precision: the guard must reject only the machinery whole-row shape. A
	// one-slot projection of an actual column is ordinary and must build.
	if _, err := values.ProjectionResultValue(
		[]values.Value{testField(t, "A", values.NullableLong)}, nil); err != nil {
		t.Errorf("a one-column projection of a real column must build, got: %v", err)
	}

	// A scalar quantified object is the value of one scalar input (not a
	// whole record wrapped into one slot), as produced by UNNEST/Explode.
	scalarQOV := mustTestQOV(t, "X", values.NotNullLong)
	if _, err := values.ProjectionResultValue([]values.Value{scalarQOV}, []string{"X"}); err != nil {
		t.Errorf("a one-slot projection of an exact scalar QOV must build, got: %v", err)
	}

	// And its STRUCT twin, which is the same SQL. A record-typed QOV over a
	// NAMED source is one projected column whose value happens to be a row.
	if _, err := values.ProjectionResultValue(
		[]values.Value{mustTestQOV(t, "X", exactTestRecordType())}, []string{"X"}); err != nil {
		t.Errorf("a one-slot projection of a STRUCT element must build, got: %v", err)
	}
}

func TestProjectionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(nil, inner)
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestProjectionPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryProjectionPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
	if _, err := NewRecordQueryProjectionPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
	if _, err := NewRecordQueryProjectionPlanFromQuantifier(nil, nil, expressions.Quantifier{}); err == nil {
		t.Fatal("quantifier constructor accepted an empty inner quantifier")
	}
}

func TestProjectionPlan_Explain(t *testing.T) {
	t.Parallel()
	projs := []values.Value{testField(t, "id", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, stub("Scan(T)"))
	})
	got := p.Explain()
	if !strings.Contains(got, "Project") {
		t.Fatalf("Explain = %q, missing 'Project'", got)
	}
}

func TestProjectionPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryProjectionPlan{}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>'", got)
	}
}

func TestProjectionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	projs := []values.Value{testField(t, "id", values.NullableLong)}
	a := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, stub("B"))
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same projections should be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_DifferentColumns(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "id", values.NullableLong)}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "name", values.NullableLong)}, stub("B"))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different projection columns should not be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_DifferentCount(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "id", values.NullableLong)}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "id", values.NullableLong), testField(t, "name", values.NullableLong)}, stub("B"))
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different projection counts should not be equal")
	}
}

func TestProjectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(nil, stub("Inner"))
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if p.EqualsPlanWithoutChildren(scan) {
		t.Fatal("ProjectionPlan should not equal ScanPlan")
	}
}

func TestProjectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	projs := []values.Value{testField(t, "id", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, stub("Inner"))
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestProjectionPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "id", values.NullableLong)}, stub("A"))
	})
	b := mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan([]values.Value{testField(t, "name", values.NullableLong)}, stub("B"))
	})
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
	p := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{a, b})
	})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetInners()) != 2 {
		t.Fatalf("GetInners() len = %d, want 2", len(p.GetInners()))
	}
}

func TestUnionPlan_GetResultType_FirstInner(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	p := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{scan, stub("B")})
	})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from first inner)", p.GetResultType())
	}
}

func TestUnionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	p := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{a, b})
	})
	cs := p.GetChildren()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatal("GetChildren() mismatch")
	}
}

func TestUnionPlan_Explain(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("A"), stub("B")})
	})
	got := p.Explain()
	if !strings.Contains(got, "Union") {
		t.Fatalf("Explain = %q, missing 'Union'", got)
	}
}

func TestUnionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("A")})
	})
	b := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("X")})
	})
	// Union equality is type-only (no operator params).
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("any two UnionPlans should be EqualsWithoutChildren")
	}
}

func TestUnionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("Inner")})
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if u.EqualsPlanWithoutChildren(scan) {
		t.Fatal("UnionPlan should not equal ScanPlan")
	}
}

func TestUnionPlan_ConstructorRejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryUnionPlan(nil); err == nil {
		t.Fatal("constructor accepted an empty input list")
	}
}

func TestUnionPlan_CopiesInnerSlice(t *testing.T) {
	t.Parallel()
	inners := []RecordQueryPlan{stub("A")}
	p := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan(inners)
	})
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
	keys := []values.Value{testField(t, "pk", values.NotNullLong)}
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{a, b}, keys)
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	})
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{scan}, nil)
	})
	if !values.NotNullString.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullString (from first inner)", p.GetResultType())
	}
}

func TestIntersectionPlan_ConstructorRejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryIntersectionPlan(nil, nil); err == nil {
		t.Fatal("constructor accepted an empty input list")
	}
}

func TestIntersectionPlan_Explain(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("A"), stub("B")}, nil)
	})
	got := p.Explain()
	if !strings.Contains(got, "Intersection") {
		t.Fatalf("Explain = %q, missing 'Intersection'", got)
	}
}

func TestIntersectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	i := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("IntersectionInner")}, nil)
	})
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("UnionInner")})
	})
	if i.EqualsPlanWithoutChildren(u) {
		t.Fatal("IntersectionPlan should not equal UnionPlan")
	}
}

func TestIntersectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "pk", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("Inner")}, keys)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestIntersectionPlan_HashCodeWithoutChildren_DiffersForDifferentKeyCount(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("A")}, []values.Value{testField(t, "pk", values.NullableLong)})
	})
	b := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("B")}, []values.Value{testField(t, "pk", values.NullableLong), testField(t, "sk", values.NullableLong)})
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different key counts should (very likely) produce different hashes")
	}
}

func TestIntersectionPlan_CopiesSlices(t *testing.T) {
	t.Parallel()
	inners := []RecordQueryPlan{stub("A")}
	keys := []values.Value{testField(t, "pk", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan(inners, keys)
	})
	inners[0] = stub("B")
	keys[0] = testField(t, "xx", values.NullableLong)
	if p.GetInners()[0].Explain() != "A" {
		t.Fatal("intersection should have an independent copy of inners")
	}
	if values.ExplainValue(p.GetComparisonKeyValues()[0]) != values.ExplainValue(testField(t, "pk", values.NullableLong)) {
		t.Fatal("intersection should have an independent copy of comparison keys")
	}
}

func TestIntersectionPlan_ReverseOrderingContractSurvivesCopies(t *testing.T) {
	t.Parallel()

	key := testField(t, "pk", values.NullableLong)
	parts := []properties.ProvidedOrderingPart{{
		Value:     key,
		SortOrder: properties.ProvidedSortOrderDescending,
	}}
	p := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlanWithOrdering(
			[]RecordQueryPlan{stub("A"), stub("B")},
			parts,
			true,
		)
	})
	if p == nil {
		t.Fatal("reverse descending intersection constructor declined a naturally executable key")
	}
	if !p.IsReverse() {
		t.Fatal("reverse flag was not retained")
	}
	if !strings.Contains(p.Explain(), "REVERSE") {
		t.Fatalf("Explain() = %q, want a reverse marker", p.Explain())
	}
	hint := p.HintOrdering()
	if !hint.IsKnown || len(hint.Keys) != 1 || !hint.Descending[0] || hint.NullsFirstAt(0) {
		t.Fatalf("HintOrdering() = %#v, want PK DESC NULLS LAST", hint)
	}

	relinkedExpr, err := p.WithQuantifiers(QuantifiersOverPlans([]RecordQueryPlan{stub("C"), stub("D")}))
	if err != nil {
		t.Fatalf("WithQuantifiers(): %v", err)
	}
	relinked, ok := relinkedExpr.(*RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("WithQuantifiers() = %T, want *RecordQueryIntersectionPlan", relinkedExpr)
	}
	if !relinked.IsReverse() ||
		len(relinked.GetComparisonKeyOrderingParts()) != 1 ||
		relinked.GetComparisonKeyOrderingParts()[0].SortOrder != properties.ProvidedSortOrderDescending {
		t.Fatalf("WithQuantifiers() lost reverse ordering: %#v", relinked)
	}
	if relinked.GetInners()[0].Explain() != "C" {
		t.Fatalf("WithQuantifiers() did not relink children: %s", relinked.GetInners()[0].Explain())
	}

	forward := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlanWithOrdering(
			[]RecordQueryPlan{stub("ForwardA"), stub("ForwardB")},
			[]properties.ProvidedOrderingPart{{
				Value:     key,
				SortOrder: properties.ProvidedSortOrderAscending,
			}},
			false,
		)
	})
	if forward == nil {
		t.Fatal("forward ascending intersection constructor declined a natural key")
	}
	if p.EqualsPlanWithoutChildren(forward) {
		t.Fatal("opposite merge directions must not be structurally equal")
	}
	if p.HashCodeWithoutChildren() == forward.HashCodeWithoutChildren() {
		t.Fatal("opposite merge directions should produce different structural hashes")
	}

	parts[0].Value = testField(t, "mutated", values.NullableLong)
	if values.ExplainValue(p.GetComparisonKeyOrderingParts()[0].Value) != values.ExplainValue(key) {
		t.Fatal("intersection should have an independent copy of semantic ordering parts")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryValuesPlan
// ---------------------------------------------------------------------------

func TestValuesPlan_Construction(t *testing.T) {
	t.Parallel()
	cols := []values.Value{
		structuralLong(1),
		structuralString("hello"),
	}
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetColumns()) != 2 {
		t.Fatalf("GetColumns() len = %d, want 2", len(p.GetColumns()))
	}
}

func TestValuesPlan_GetResultType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(nil)
	})
	want := values.NewRecordType("", false, nil)
	if !want.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want exact empty record %v", p.GetResultType(), want)
	}
}

func TestValuesPlan_GetChildren_Nil(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(nil)
	})
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil (leaf plan)", cs)
	}
}

func TestValuesPlan_Explain(t *testing.T) {
	t.Parallel()
	cols := []values.Value{structuralLong(42)}
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	got := p.Explain()
	if !strings.Contains(got, "Values") {
		t.Fatalf("Explain = %q, missing 'Values'", got)
	}
}

func TestValuesPlan_Explain_Empty(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(nil)
	})
	got := p.Explain()
	if got != "Values()" {
		t.Fatalf("Explain = %q, want 'Values()'", got)
	}
}

func TestValuesPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	cols := []values.Value{structuralLong(1)}
	a := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	b := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same columns should be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_DifferentValues(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{structuralLong(1)})
	})
	b := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{structuralLong(2)})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different column values should not be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_DifferentCount(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{structuralLong(1)})
	})
	b := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{
			structuralLong(1),
			structuralLong(2),
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different column counts should not be equal")
	}
}

func TestValuesPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	v := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(nil)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if v.EqualsPlanWithoutChildren(scan) {
		t.Fatal("ValuesPlan should not equal ScanPlan")
	}
}

func TestValuesPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	cols := []values.Value{structuralLong(1)}
	p := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestValuesPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{structuralLong(1)})
	})
	b := mustChecked(t, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan([]values.Value{structuralLong(2)})
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different values should (very likely) produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryScanPlan
// ---------------------------------------------------------------------------

func TestScanPlan_Construction(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order", "Customer"}, values.NotNullLong, true)
	})
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

func TestScanPlan_ConstructorRejectsNilFlowedType(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryScanPlan([]string{"T"}, nil, false); err == nil {
		t.Fatal("constructor accepted a nil flowed type")
	}
}

func TestScanPlan_GetFlowedType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	})
	if !values.NotNullString.Equals(p.GetFlowedType()) {
		t.Fatalf("GetFlowedType() = %v, want NotNullString", p.GetFlowedType())
	}
}

func TestScanPlan_GetChildren_Empty(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil (leaf plan)", cs)
	}
}

func TestScanPlan_Explain_Forward(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	got := p.Explain()
	if got != "Scan(T)" {
		t.Fatalf("Explain = %q, want 'Scan(T)'", got)
	}
}

func TestScanPlan_Explain_Reverse(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), true)
	})
	got := p.Explain()
	if got != "Scan(T) REVERSE" {
		t.Fatalf("Explain = %q, want 'Scan(T) REVERSE'", got)
	}
}

func TestScanPlan_Explain_MultiType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"B", "A"}, exactTestRecordType(), false)
	})
	got := p.Explain()
	// Sorted, so A comes first.
	if got != "Scan(A, B)" {
		t.Fatalf("Explain = %q, want 'Scan(A, B)'", got)
	}
}

func TestScanPlan_Explain_Empty(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
	})
	got := p.Explain()
	if got != "Scan()" {
		t.Fatalf("Explain = %q, want 'Scan()'", got)
	}
}

func TestScanPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	b := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same params should be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentTypes(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	b := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"U"}, exactTestRecordType(), false)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different record types should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentReverse(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	b := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), true)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different reverse flags should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_DifferentFlowedType(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	b := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullString, false)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different flowed types should not be equal")
	}
}

func TestScanPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	d := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(stub("Inner"))
	})
	if s.EqualsPlanWithoutChildren(d) {
		t.Fatal("ScanPlan should not equal DistinctPlan")
	}
}

func TestScanPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestScanPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	b := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"U"}, exactTestRecordType(), false)
	})
	c := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), true)
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different record types should (very likely) produce different hashes")
	}
	if a.HashCodeWithoutChildren() == c.HashCodeWithoutChildren() {
		t.Fatal("different reverse should (very likely) produce different hashes")
	}
}

func TestScanPlan_DedupAndSort(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"C", "A", "B", "A"}, exactTestRecordType(), false)
	})
	rts := p.GetRecordTypes()
	if len(rts) != 3 || rts[0] != "A" || rts[1] != "B" || rts[2] != "C" {
		t.Fatalf("record types = %v, want [A, B, C]", rts)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryStreamingAggregationPlan
// ---------------------------------------------------------------------------

func TestStreamingAggPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: testField(t, "amount", values.NullableLong)},
	}
	p := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(inner, keys, aggs)
	})
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
	p := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("X"), nil, nil)
	})
	want := values.NewRecordType("", false, nil)
	if !want.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want exact empty record %v", p.GetResultType(), want)
	}
}

func TestStreamingAggPlan_GetChildren(t *testing.T) {
	t.Parallel()
	inner := stub("Inner")
	p := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(inner, nil, nil)
	})
	cs := p.GetChildren()
	if len(cs) != 1 || cs[0] != inner {
		t.Fatalf("GetChildren() = %v, want [inner]", cs)
	}
}

func TestStreamingAggPlan_GetChildren_NilInner(t *testing.T) {
	t.Parallel()
	p := &RecordQueryStreamingAggregationPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
	if _, err := NewRecordQueryStreamingAggregationPlan(nil, nil, nil); err == nil {
		t.Fatal("constructor accepted a nil inner plan")
	}
	if _, err := NewRecordQueryStreamingAggregationPlanFromQuantifier(expressions.Quantifier{}, nil, nil); err == nil {
		t.Fatal("quantifier constructor accepted an empty inner quantifier")
	}
}

func TestStreamingAggPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Scan(T)"), keys, nil)
	})
	got := p.Explain()
	if !strings.Contains(got, "StreamingAgg") {
		t.Fatalf("Explain = %q, missing 'StreamingAgg'", got)
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: testField(t, "amount", values.NullableLong)},
	}
	a := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("A"), keys, aggs)
	})
	b := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("B"), keys, aggs)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same keys+aggs should be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentGroupingKeys(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("A"),
			[]values.Value{testField(t, "dept", values.NullableLong)}, nil)
	})
	b := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("B"),
			[]values.Value{testField(t, "region", values.NullableLong)}, nil)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different grouping keys should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentAggFunction(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	operand := testField(t, "val", values.NullableLong)
	a := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("A"), keys, []expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: operand},
		})
	})
	b := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("B"), keys, []expressions.AggregateSpec{
			{Function: expressions.AggMax, Operand: operand},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different aggregate functions should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_DifferentAggOperand(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	a := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("A"), keys, []expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: testField(t, "amount", values.NullableLong)},
		})
	})
	b := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("B"), keys, []expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: testField(t, "qty", values.NullableLong)},
		})
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different aggregate operands should not be equal")
	}
}

func TestStreamingAggPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	s := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Inner"), nil, nil)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if s.EqualsPlanWithoutChildren(scan) {
		t.Fatal("StreamingAgg should not equal ScanPlan")
	}
}

func TestStreamingAggPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Inner"), keys, nil)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestStreamingAggPlan_HashCodeWithoutChildren_Differs(t *testing.T) {
	t.Parallel()
	a := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("A"),
			[]values.Value{testField(t, "dept", values.NullableLong)}, nil)
	})
	b := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("B"),
			[]values.Value{testField(t, "region", values.NullableLong)}, nil)
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different grouping keys should (very likely) produce different hashes")
	}
}

func TestStreamingAggPlan_HashDistinctFromScan(t *testing.T) {
	t.Parallel()
	// StreamingAgg and Scan should have different hashes.
	keys := []values.Value{testField(t, "dept", values.NullableLong)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: testField(t, "id", values.NullableLong)},
	}
	s := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Inner"), keys, aggs)
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if s.HashCodeWithoutChildren() == scan.HashCodeWithoutChildren() {
		t.Fatal("streaming agg and scan should have different hashes")
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
		"Limit": mustChecked(t, func() (*RecordQueryLimitPlan, error) {
			return NewRecordQueryLimitPlan(stub("LimitInner"), 0, 0)
		}).HashCodeWithoutChildren(),
		"Filter": mustChecked(t, func() (*RecordQueryFilterPlan, error) {
			return NewRecordQueryFilterPlan(nil, stub("FilterInner"))
		}).HashCodeWithoutChildren(),
		"InMemorySort": mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
			return NewRecordQueryInMemorySortPlan(stub("SortInner"), nil)
		}).HashCodeWithoutChildren(),
		"Distinct": mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
			return NewRecordQueryDistinctPlan(stub("DistinctInner"))
		}).HashCodeWithoutChildren(),
		"Project": mustChecked(t, func() (*RecordQueryProjectionPlan, error) {
			return NewRecordQueryProjectionPlan(nil, stub("ProjectInner"))
		}).HashCodeWithoutChildren(),
		"Union": mustChecked(t, func() (*RecordQueryUnionPlan, error) {
			return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("UnionInner")})
		}).HashCodeWithoutChildren(),
		"Intersect": mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
			return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("IntersectionInner")}, nil)
		}).HashCodeWithoutChildren(),
		"MultiIntersect": mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
			return NewRecordQueryMultiIntersectionOnValuesPlan(
				[]RecordQueryPlan{stub("MultiIntersectionInner")},
				nil,
				values.NewRecordConstructorValue(),
			)
		}).HashCodeWithoutChildren(),
		"Values": mustChecked(t, func() (*RecordQueryValuesPlan, error) {
			return NewRecordQueryValuesPlan(nil)
		}).HashCodeWithoutChildren(),
		"Scan": mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan(nil, exactTestRecordType(), false)
		}).HashCodeWithoutChildren(),
		"StreamAgg": mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
			return NewRecordQueryStreamingAggregationPlan(stub("StreamingAggInner"), nil, nil)
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

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkLimitPlan_Explain(b *testing.B) {
	p := mustChecked(b, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Scan(T)"), 100, 50)
	})
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkLimitPlan_HashCodeWithoutChildren(b *testing.B) {
	p := mustChecked(b, func() (*RecordQueryLimitPlan, error) {
		return NewRecordQueryLimitPlan(stub("Scan(T)"), 100, 50)
	})
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkFilterPlan_Explain(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(b, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Scan(T)"))
	})
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkFilterPlan_HashCodeWithoutChildren(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	p := mustChecked(b, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, stub("Scan(T)"))
	})
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkScanPlan_Explain(b *testing.B) {
	p := mustChecked(b, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order", "Customer"}, exactTestRecordType(), true)
	})
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkScanPlan_HashCodeWithoutChildren(b *testing.B) {
	p := mustChecked(b, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order", "Customer"}, exactTestRecordType(), true)
	})
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func BenchmarkStreamingAggPlan_Explain(b *testing.B) {
	keys := []values.Value{testField(b, "dept", values.NullableLong)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: testField(b, "id", values.NullableLong)},
	}
	p := mustChecked(b, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Scan(T)"), keys, aggs)
	})
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkStreamingAggPlan_HashCodeWithoutChildren(b *testing.B) {
	keys := []values.Value{testField(b, "dept", values.NullableLong)}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: testField(b, "id", values.NullableLong)},
	}
	p := mustChecked(b, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(stub("Scan(T)"), keys, aggs)
	})
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
	p := mustChecked(b, func() (*RecordQueryValuesPlan, error) {
		return NewRecordQueryValuesPlan(cols)
	})
	for b.Loop() {
		_ = p.Explain()
	}
}

func BenchmarkProjectionPlan_HashCodeWithoutChildren(b *testing.B) {
	projs := []values.Value{testField(b, "id", values.NullableLong), testField(b, "name", values.NullableLong), testField(b, "age", values.NullableLong)}
	p := mustChecked(b, func() (*RecordQueryProjectionPlan, error) {
		return NewRecordQueryProjectionPlan(projs, stub("Inner"))
	})
	for b.Loop() {
		_ = p.HashCodeWithoutChildren()
	}
}

func (s *stubPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(s, other)
}

func (s *stubPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("stubPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return s, nil
}

// The getOnlyElement guard on a plan's child dereference must actually FIRE.
//
// It cannot be reached through any production path — QuantifierOverPlan mints a
// fresh singleton per child, so a plan never points at a shared memo group (see
// planFromQuantifier). That is exactly why it needs a direct test: a guard whose
// only justification is "this state is unreachable" is also a guard nobody has
// ever seen work, and the day the provenance changes is the wrong day to find
// out it was mis-wired.
func TestPlanFromQuantifier_PanicsOnMultipleFinalPlans(t *testing.T) {
	t.Parallel()

	a, b := distinctStub("A"), distinctStub("B")

	// A reference holding TWO distinct plan-typed FINAL members — the shared-memo-
	// group shape a plan quantifier is not supposed to be able to see.
	ref := expressions.FinalOfAtStage(a, expressions.StageCanonical)
	if !ref.InsertFinal(b) {
		t.Fatal("setup: second final member was not inserted")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("dereferencing a two-final-plan reference must panic, but did not")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "plan-invariant") {
			t.Errorf("panic message = %v, want one tagged plan-invariant", r)
		}
	}()

	planFromQuantifier(expressions.NewPhysicalQuantifier(ref))
}

// The same two members in the EXPLORATORY set must NOT panic: a group holding
// several alternatives before it is pruned is the normal mid-planning state, and
// Java's getOnlyElement contract applies only to the final set.
func TestPlanFromQuantifier_ToleratesMultipleExploratoryPlans(t *testing.T) {
	t.Parallel()

	a, b := distinctStub("A"), distinctStub("B")

	ref := expressions.InitialOf(a)
	if !ref.Insert(b) {
		t.Fatal("setup: second exploratory member was not inserted")
	}

	got := planFromQuantifier(expressions.NewPhysicalQuantifier(ref))
	if got != RecordQueryPlan(a) {
		t.Errorf("exploratory fallback returned %v, want the first member A", got)
	}
}

// One plan reachable through two DISTINCT final members (a wrapper plus the plan
// it wraps) is one answer, not an ambiguity — the transitional shape the wrapper
// retirement produces must not trip the guard.
func TestPlanFromQuantifier_SamePlanViaWrapperAndPlanIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	a := distinctStub("A")

	ref := expressions.FinalOfAtStage(a, expressions.StageCanonical)
	if !ref.InsertFinal(&stubPlanWrapper{wrapped: a}) {
		t.Fatal("setup: wrapper member was not inserted")
	}

	got := planFromQuantifier(expressions.NewPhysicalQuantifier(ref))
	if got != RecordQueryPlan(a) {
		t.Errorf("got %v, want the single underlying plan A", got)
	}
}

// stubPlanWrapper is a RelationalExpression that is NOT itself a
// RecordQueryPlan but exposes one — the shape of the physical wrappers in the
// cascades package, which this package cannot import.
type stubPlanWrapper struct {
	wrapped RecordQueryPlan
}

func (w *stubPlanWrapper) GetRecordQueryPlan() RecordQueryPlan { return w.wrapped }
func (w *stubPlanWrapper) GetResultValue() values.Value        { return nil }
func (w *stubPlanWrapper) GetQuantifiers() []expressions.Quantifier {
	return nil
}
func (w *stubPlanWrapper) CanCorrelate() bool  { return false }
func (w *stubPlanWrapper) ChildrenAsSet() bool { return false }
func (w *stubPlanWrapper) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (w *stubPlanWrapper) EqualsWithoutChildren(expressions.RelationalExpression, *expressions.AliasMap) bool {
	return false
}
func (w *stubPlanWrapper) HashCodeWithoutChildren() uint64 { return 0 }
func (w *stubPlanWrapper) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("stubPlanWrapper", len(qs), 0); err != nil {
		return nil, err
	}
	return w, nil
}

// distinctStubPlan is stubPlan with REAL equality. stubPlan reports every
// instance equal, which is fine for the child-slot tests above but makes a
// Reference dedup two instances into one member — defeating any test that needs
// a group to genuinely hold two.
type distinctStubPlan struct {
	PlanExprBase
	label string
}

func (s *distinctStubPlan) GetResultType() values.Type     { return values.NotNullLong }
func (s *distinctStubPlan) GetChildren() []RecordQueryPlan { return nil }
func (s *distinctStubPlan) Explain() string                { return s.label }
func (s *distinctStubPlan) HashCodeWithoutChildren() uint64 {
	var h uint64 = 14695981039346656037
	for _, c := range s.label {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

func (s *distinctStubPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*distinctStubPlan)
	return ok && o.label == s.label
}

func distinctStub(label string) *distinctStubPlan {
	return &distinctStubPlan{PlanExprBase: PlanExprBase{resultValue: &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong}}, label: label}
}

func (s *distinctStubPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(s, other)
}

func (s *distinctStubPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("distinctStubPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return s, nil
}
