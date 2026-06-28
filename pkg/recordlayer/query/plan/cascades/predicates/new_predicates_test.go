package predicates

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// PredicateWithValueAndRanges
// ---------------------------------------------------------------------------

func TestPredicateWithValueAndRanges_Construction(t *testing.T) {
	t.Parallel()
	val := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	rc := NewRangeConstraints(
		[]Comparison{NewLiteralComparison(ComparisonGreaterThan, int64(18))},
		nil,
	)
	p := NewPredicateWithValueAndRanges(val, []*RangeConstraints{rc})

	if p.GetValue() != val {
		t.Fatal("value mismatch")
	}
	if len(p.GetRanges()) != 1 {
		t.Fatalf("ranges count = %d", len(p.GetRanges()))
	}
}

func TestPredicateWithValueAndRanges_IsLeafPredicate(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt}, nil)
	if len(p.Children()) != 0 {
		t.Fatal("should be a leaf predicate")
	}
	if got, _ := p.Eval(nil); got != TriUnknown {
		t.Fatal("Eval should return TriUnknown")
	}
}

func TestPredicateWithValueAndRanges_GetComparisons(t *testing.T) {
	t.Parallel()
	c1 := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	c2 := NewLiteralComparison(ComparisonLessThan, int64(100))
	rc1 := NewRangeConstraints([]Comparison{c1}, nil)
	rc2 := NewRangeConstraints([]Comparison{c2}, nil)
	p := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{rc1, rc2},
	)
	comps := p.GetComparisons()
	if len(comps) != 2 {
		t.Fatalf("comparisons count = %d, want 2", len(comps))
	}
}

func TestPredicateWithValueAndRanges_WithValue(t *testing.T) {
	t.Parallel()
	original := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{EmptyRangeConstraints()},
	)
	newVal := &values.FieldValue{Field: "y", Typ: values.TypeString}
	updated := original.WithValue(newVal)

	if updated.GetValue() != newVal {
		t.Fatal("WithValue should replace the value")
	}
	if len(updated.GetRanges()) != len(original.GetRanges()) {
		t.Fatal("ranges should be preserved")
	}
}

func TestPredicateWithValueAndRanges_WithRanges(t *testing.T) {
	t.Parallel()
	original := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		nil,
	)
	rc := NewRangeConstraints(
		[]Comparison{NewLiteralComparison(ComparisonEquals, int64(42))}, nil)
	updated := original.WithRanges([]*RangeConstraints{rc})

	if len(updated.GetRanges()) != 1 {
		t.Fatal("WithRanges should set ranges")
	}
	if updated.GetValue() != original.GetValue() {
		t.Fatal("value should be preserved")
	}
}

func TestPredicateWithValueAndRanges_Explain(t *testing.T) {
	t.Parallel()
	val := &values.FieldValue{Field: "score", Typ: values.TypeInt}
	rc := NewRangeConstraints(
		[]Comparison{NewLiteralComparison(ComparisonGreaterThan, int64(90))},
		nil,
	)
	p := NewPredicateWithValueAndRanges(val, []*RangeConstraints{rc})
	got := p.Explain()
	if !strings.Contains(got, "score") || !strings.Contains(got, ">") {
		t.Fatalf("Explain = %q", got)
	}
}

func TestPredicateWithValueAndRanges_IsCompileTime(t *testing.T) {
	t.Parallel()
	// All literal comparisons → compile time.
	rc := NewRangeConstraints(
		[]Comparison{NewLiteralComparison(ComparisonEquals, int64(5))}, nil)
	p := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{rc},
	)
	if !p.IsCompileTime() {
		t.Fatal("should be compile time with literal comparisons only")
	}

	// Deferred range → not compile time.
	alias := values.NamedCorrelationIdentifier("q")
	corVal := values.NewQuantifiedObjectValue(alias)
	deferred := Comparison{Type: ComparisonEquals, Operand: corVal}
	rc2 := NewRangeConstraints(nil, []Comparison{deferred})
	p2 := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{rc2},
	)
	if p2.IsCompileTime() {
		t.Fatal("should not be compile time with deferred ranges")
	}
}

func TestPredicateWithValueAndRanges_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q1")
	corVal := values.NewQuantifiedObjectValue(alias)
	deferred := Comparison{Type: ComparisonEquals, Operand: corVal}
	rc := NewRangeConstraints(nil, []Comparison{deferred})
	p := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{rc},
	)
	corr := p.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("should contain correlation from deferred range")
	}
}

// ---------------------------------------------------------------------------
// RangeConstraints
// ---------------------------------------------------------------------------

func TestRangeConstraints_Empty(t *testing.T) {
	t.Parallel()
	rc := EmptyRangeConstraints()
	if rc.IsConstraining() {
		t.Fatal("empty should not be constraining")
	}
	if !rc.IsCompileTime() {
		t.Fatal("empty should be compile-time")
	}
	if len(rc.GetComparisons()) != 0 {
		t.Fatalf("comparisons = %d", len(rc.GetComparisons()))
	}
}

func TestRangeConstraints_CompilableAndDeferred(t *testing.T) {
	t.Parallel()
	c1 := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	alias := values.NamedCorrelationIdentifier("q")
	corVal := values.NewQuantifiedObjectValue(alias)
	c2 := Comparison{Type: ComparisonLessThan, Operand: corVal}

	rc := NewRangeConstraints([]Comparison{c1}, []Comparison{c2})
	if !rc.IsConstraining() {
		t.Fatal("should be constraining")
	}
	if rc.IsCompileTime() {
		t.Fatal("should not be compile-time with deferred ranges")
	}
	if len(rc.GetComparisons()) != 2 {
		t.Fatalf("total comparisons = %d, want 2", len(rc.GetComparisons()))
	}
	if len(rc.GetDeferredRanges()) != 1 {
		t.Fatalf("deferred = %d", len(rc.GetDeferredRanges()))
	}
	if len(rc.GetCompilableComparisons()) != 1 {
		t.Fatalf("compilable = %d", len(rc.GetCompilableComparisons()))
	}
}

func TestRangeConstraints_AsComparisonRange(t *testing.T) {
	t.Parallel()
	c := NewLiteralComparison(ComparisonEquals, int64(42))
	rc := NewRangeConstraints([]Comparison{c}, nil)
	cr := rc.AsComparisonRange()
	if !cr.IsEquality() {
		t.Fatal("AsComparisonRange should produce equality range")
	}
}

func TestRangeConstraints_GetCorrelatedTo(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("q")
	corVal := values.NewQuantifiedObjectValue(alias)
	c := Comparison{Type: ComparisonEquals, Operand: corVal}
	rc := NewRangeConstraints(nil, []Comparison{c})
	corr := rc.GetCorrelatedTo()
	if _, ok := corr[alias]; !ok {
		t.Fatal("should contain the deferred range's correlation")
	}
}

func TestRangeConstraintsBuilder(t *testing.T) {
	t.Parallel()
	b := NewRangeConstraintsBuilder()
	c1 := NewLiteralComparison(ComparisonGreaterThan, int64(5))
	b.AddComparisonMaybe(c1)

	alias := values.NamedCorrelationIdentifier("q")
	corVal := values.NewQuantifiedObjectValue(alias)
	c2 := Comparison{Type: ComparisonLessThan, Operand: corVal}
	b.AddComparisonMaybe(c2)

	rc := b.Build()
	if len(rc.GetCompilableComparisons()) != 1 {
		t.Fatalf("compilable = %d", len(rc.GetCompilableComparisons()))
	}
	if len(rc.GetDeferredRanges()) != 1 {
		t.Fatalf("deferred = %d", len(rc.GetDeferredRanges()))
	}
}

// ---------------------------------------------------------------------------
// CompatibleTypeEvolutionPredicate
// ---------------------------------------------------------------------------

func TestCompatibleTypeEvolutionPredicate_Construction(t *testing.T) {
	t.Parallel()
	m := map[string]*FieldAccessTrieNode{
		"MyRecord": {FieldName: "name", Ordinal: 1, TypeName: "string"},
	}
	p := NewCompatibleTypeEvolutionPredicate(m)
	if len(p.RecordTypeNameFieldAccessMap) != 1 {
		t.Fatalf("map size = %d", len(p.RecordTypeNameFieldAccessMap))
	}
}

func TestCompatibleTypeEvolutionPredicate_LeafBehavior(t *testing.T) {
	t.Parallel()
	p := NewCompatibleTypeEvolutionPredicate(nil)
	if len(p.Children()) != 0 {
		t.Fatal("should be a leaf predicate")
	}
	if got, _ := p.Eval(nil); got != TriTrue {
		t.Fatal("Eval should return TriTrue (plan cache not ported)")
	}
}

func TestCompatibleTypeEvolutionPredicate_Explain(t *testing.T) {
	t.Parallel()
	p1 := NewCompatibleTypeEvolutionPredicate(nil)
	if p1.Explain() != "compatibleTypeEvolution()" {
		t.Fatalf("empty Explain = %q", p1.Explain())
	}

	m := map[string]*FieldAccessTrieNode{
		"Order": {FieldName: "id", Ordinal: 1},
	}
	p2 := NewCompatibleTypeEvolutionPredicate(m)
	got := p2.Explain()
	if !strings.Contains(got, "compatibleTypeEvolution(") || !strings.Contains(got, "Order") {
		t.Fatalf("Explain = %q", got)
	}
}

// ---------------------------------------------------------------------------
// DatabaseObjectDependenciesPredicate
// ---------------------------------------------------------------------------

func TestDatabaseObjectDependenciesPredicate_Construction(t *testing.T) {
	t.Parallel()
	indexes := []UsedIndex{
		{Name: "idx_a", LastModifiedVersion: 5},
		{Name: "idx_b", LastModifiedVersion: 12},
	}
	p := NewDatabaseObjectDependenciesPredicate(indexes)
	if len(p.UsedIndexes) != 2 {
		t.Fatalf("indexes = %d", len(p.UsedIndexes))
	}
}

func TestDatabaseObjectDependenciesPredicate_LeafBehavior(t *testing.T) {
	t.Parallel()
	p := NewDatabaseObjectDependenciesPredicate(nil)
	if len(p.Children()) != 0 {
		t.Fatal("should be a leaf predicate")
	}
	if got, _ := p.Eval(nil); got != TriTrue {
		t.Fatal("Eval should return TriTrue (plan cache not ported)")
	}
}

func TestDatabaseObjectDependenciesPredicate_Explain(t *testing.T) {
	t.Parallel()
	p1 := NewDatabaseObjectDependenciesPredicate(nil)
	if p1.Explain() != "databaseObjectDependencies()" {
		t.Fatalf("empty Explain = %q", p1.Explain())
	}

	indexes := []UsedIndex{{Name: "idx_a", LastModifiedVersion: 3}}
	p2 := NewDatabaseObjectDependenciesPredicate(indexes)
	got := p2.Explain()
	want := "databaseObjectDependencies(idx_a@v3)"
	if got != want {
		t.Fatalf("Explain = %q, want %q", got, want)
	}
}

func TestDatabaseObjectDependenciesPredicate_DefensiveCopy(t *testing.T) {
	t.Parallel()
	indexes := []UsedIndex{{Name: "idx", LastModifiedVersion: 1}}
	p := NewDatabaseObjectDependenciesPredicate(indexes)
	// Mutate original slice — predicate should be unaffected.
	indexes[0].Name = "mutated"
	if p.UsedIndexes[0].Name != "idx" {
		t.Fatal("constructor should defensively copy")
	}
}

func TestFoldPredicateWithRanges_NullIsNull(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		values.NewNullValue(values.TypeInt),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{{Type: ComparisonIsNull}}, nil,
		)},
	)
	got := SimplifyPredicateValues(p)
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("NULL IS NULL should fold to TRUE, got %T %v", got, got)
	}
}

func TestFoldPredicateWithRanges_TrueIsNull(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		values.NewBooleanValue(true),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{{Type: ComparisonIsNull}}, nil,
		)},
	)
	got := SimplifyPredicateValues(p)
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("TRUE IS NULL should fold to FALSE, got %T %v", got, got)
	}
}

func TestFoldPredicateWithRanges_TrueEqualsTrue(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		values.NewBooleanValue(true),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{NewLiteralComparison(ComparisonEquals, true)}, nil,
		)},
	)
	got := SimplifyPredicateValues(p)
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("TRUE = TRUE should fold to TRUE, got %T %v", got, got)
	}
}

func TestFoldPredicateWithRanges_NullEqualsAnything(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		values.NewNullValue(values.TypeInt),
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{NewLiteralComparison(ComparisonEquals, true)}, nil,
		)},
	)
	got := SimplifyPredicateValues(p)
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriUnknown {
		t.Fatalf("NULL = TRUE should fold to NULL/UNKNOWN, got %T %v", got, got)
	}
}

func TestFoldPredicateWithRanges_NonConstantNoFold(t *testing.T) {
	t.Parallel()
	p := NewPredicateWithValueAndRanges(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		[]*RangeConstraints{NewRangeConstraints(
			[]Comparison{NewLiteralComparison(ComparisonEquals, int64(5))}, nil,
		)},
	)
	got := SimplifyPredicateValues(p)
	if _, ok := got.(*PredicateWithValueAndRanges); !ok {
		t.Fatalf("non-constant LHS should not fold, got %T", got)
	}
}
