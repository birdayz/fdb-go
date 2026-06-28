package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// RecordQueryAggregateIndexPlan
// ---------------------------------------------------------------------------

func TestAggregateIndexPlan_Construction(t *testing.T) {
	t.Parallel()
	idx := NewRecordQueryIndexPlan("sum_idx", nil, []string{"Sale"}, values.UnknownType, false)
	p := NewRecordQueryAggregateIndexPlan(idx, "Sale", values.NotNullLong, "SUM")

	if p.GetIndexPlan() != idx {
		t.Fatal("index plan mismatch")
	}
	if p.GetRecordTypeName() != "Sale" {
		t.Fatalf("record type = %q, want Sale", p.GetRecordTypeName())
	}
	if p.GetAggregateFunction() != "SUM" {
		t.Fatalf("function = %q, want SUM", p.GetAggregateFunction())
	}
	if p.GetIndexName() != "sum_idx" {
		t.Fatalf("index name = %q", p.GetIndexName())
	}
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatal("result type mismatch")
	}
}

// TestAggregateIndexPlan_OutputColumnNames pins the RFC-081 output-naming: a bare
// aggregate-index plan reports group columns (verbatim) + the canonical aggregate name —
// the exact keys aggregateIndexCursor writes (single source so cursor and reporter can't
// drift). A bare aggregate-index plan is always unaliased, so no alias is involved.
func TestAggregateIndexPlan_OutputColumnNames(t *testing.T) {
	t.Parallel()

	// Grouped COUNT(*): [G, COUNT(*)].
	cnt := NewRecordQueryAggregateIndexPlan(
		NewRecordQueryIndexPlan("cnt_by_g", nil, []string{"GA"}, values.UnknownType, false),
		"GA", values.UnknownType, "COUNT",
	).WithGroupColumns([]string{"G"}, "")
	if got := cnt.CanonicalAggColumnName(); got != "COUNT(*)" {
		t.Fatalf("canonical = %q, want COUNT(*)", got)
	}
	if got := cnt.OutputColumnNames(); len(got) != 2 || got[0] != "G" || got[1] != "COUNT(*)" {
		t.Fatalf("output names = %v, want [G COUNT(*)]", got)
	}

	// Grouped SUM(V): [G, SUM(V)].
	sum := NewRecordQueryAggregateIndexPlan(
		NewRecordQueryIndexPlan("sum_by_g", nil, []string{"GA"}, values.UnknownType, false),
		"GA", values.UnknownType, "SUM",
	).WithGroupColumns([]string{"G"}, "V")
	if got := sum.CanonicalAggColumnName(); got != "SUM(V)" {
		t.Fatalf("canonical = %q, want SUM(V)", got)
	}
	if got := sum.OutputColumnNames(); len(got) != 2 || got[0] != "G" || got[1] != "SUM(V)" {
		t.Fatalf("output names = %v, want [G SUM(V)]", got)
	}
}

func TestAggregateIndexPlan_LeafPlan(t *testing.T) {
	t.Parallel()
	idx := NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false)
	p := NewRecordQueryAggregateIndexPlan(idx, "T", nil, "COUNT")

	if len(p.GetChildren()) != 0 {
		t.Fatal("aggregate index plan should be a leaf (no children)")
	}
	// nil resultType falls back to UnknownType.
	if p.GetResultType() != values.UnknownType {
		t.Fatal("nil resultType should become UnknownType")
	}
}

func TestAggregateIndexPlan_EqualityAndHash(t *testing.T) {
	t.Parallel()
	idx := NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false)
	a := NewRecordQueryAggregateIndexPlan(idx, "T", values.UnknownType, "SUM")
	b := NewRecordQueryAggregateIndexPlan(idx, "T", values.UnknownType, "SUM")
	c := NewRecordQueryAggregateIndexPlan(idx, "T", values.UnknownType, "COUNT")

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("identical aggregate plans should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different aggregate functions should not be equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal plans should have equal hashes")
	}
}

func TestAggregateIndexPlan_Explain(t *testing.T) {
	t.Parallel()
	idx := NewRecordQueryIndexPlan("sum_idx", nil, []string{"Sale"}, values.UnknownType, false)
	p := NewRecordQueryAggregateIndexPlan(idx, "Sale", values.UnknownType, "SUM")
	want := "AggregateIndex(SUM, sum_idx, Sale)"
	if got := p.Explain(); got != want {
		t.Fatalf("Explain = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryComparatorPlan
// ---------------------------------------------------------------------------

func TestComparatorPlan_Construction(t *testing.T) {
	t.Parallel()
	c1 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	c2 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.TypeInt}}
	p := NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, false, true)

	if len(p.GetChildren()) != 2 {
		t.Fatalf("children count = %d, want 2", len(p.GetChildren()))
	}
	if p.GetReferencePlanIndex() != 0 {
		t.Fatalf("ref index = %d", p.GetReferencePlanIndex())
	}
	if p.IsReverse() {
		t.Fatal("should not be reverse")
	}
	if !p.AbortOnComparisonFailure() {
		t.Fatal("abort flag mismatch")
	}
	if len(p.GetComparisonKeyValues()) != 1 {
		t.Fatalf("key count = %d", len(p.GetComparisonKeyValues()))
	}
}

func TestComparatorPlan_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty children")
		}
	}()
	NewRecordQueryComparatorPlan(nil, nil, 0, false, false)
}

func TestComparatorPlan_PanicsOnBadRefIndex(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on out-of-range reference index")
		}
	}()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	NewRecordQueryComparatorPlan([]RecordQueryPlan{scan}, nil, 5, false, false)
}

func TestComparatorPlan_EqualityAndExplain(t *testing.T) {
	t.Parallel()
	c1 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	c2 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.TypeInt}}

	a := NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, true, false)
	b := NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, true, false)
	c := NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 1, true, false)

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("identical comparator plans should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different ref index should break equality")
	}

	got := a.Explain()
	if !strings.Contains(got, "Comparator(") || !strings.Contains(got, "DESC") {
		t.Fatalf("Explain = %q", got)
	}
}

// ---------------------------------------------------------------------------
// RecordQuerySelectorPlan + PlanSelector
// ---------------------------------------------------------------------------

func TestSelectorPlan_Construction(t *testing.T) {
	t.Parallel()
	c1 := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	c2 := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	p := NewRecordQuerySelectorPlanWithProbabilities(
		[]RecordQueryPlan{c1, c2}, []int{70, 30}, false)

	if len(p.GetChildren()) != 2 {
		t.Fatalf("children count = %d", len(p.GetChildren()))
	}
	if p.IsReverse() {
		t.Fatal("should not be reverse")
	}
	// Result type comes from first child.
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatal("result type should match first child")
	}
}

func TestSelectorPlan_PanicsOnEmpty(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty children")
		}
	}()
	NewRecordQuerySelectorPlan(nil, nil, false)
}

func TestSelectorPlan_EqualityAndExplain(t *testing.T) {
	t.Parallel()
	c1 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sel1 := NewRelativeProbabilityPlanSelector([]int{50, 50})
	sel2 := NewRelativeProbabilityPlanSelector([]int{50, 50})
	sel3 := NewRelativeProbabilityPlanSelector([]int{70, 30})

	a := NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel1, false)
	b := NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel2, false)
	c := NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel3, false)

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same probability selectors should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different probabilities should break equality")
	}
	got := a.Explain()
	if !strings.Contains(got, "Selector(") || !strings.Contains(got, "RelativeProb") {
		t.Fatalf("Explain = %q", got)
	}
}

func TestRelativeProbabilityPlanSelector_Equality(t *testing.T) {
	t.Parallel()
	a := NewRelativeProbabilityPlanSelector([]int{50, 50})
	b := NewRelativeProbabilityPlanSelector([]int{50, 50})
	c := NewRelativeProbabilityPlanSelector([]int{60, 40})

	if !a.Equals(b) {
		t.Fatal("same probs should be equal")
	}
	if a.Equals(c) {
		t.Fatal("different probs should not be equal")
	}
	if got := a.String(); !strings.Contains(got, "50") {
		t.Fatalf("String = %q", got)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryLoadByKeysPlan + KeysSource
// ---------------------------------------------------------------------------

func TestLoadByKeysPlan_FromKeys(t *testing.T) {
	t.Parallel()
	keys := []tuple.Tuple{{int64(1)}, {int64(2)}, {int64(3)}}
	p := NewRecordQueryLoadByKeysPlanFromKeys(keys)

	if p.GetResultType() != values.UnknownType {
		t.Fatal("result type should be UnknownType")
	}
	if len(p.GetChildren()) != 0 {
		t.Fatal("should be a leaf plan")
	}
	src := p.GetKeysSource().(*PrimaryKeysKeySource)
	if src.MaxCardinality() != 3 {
		t.Fatalf("cardinality = %d", src.MaxCardinality())
	}
}

func TestLoadByKeysPlan_FromParameter(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLoadByKeysPlanFromParameter("pk_list")

	src := p.GetKeysSource().(*ParameterKeySource)
	if src.GetParameter() != "pk_list" {
		t.Fatalf("parameter = %q", src.GetParameter())
	}
	if src.MaxCardinality() != -1 {
		t.Fatalf("parameter cardinality = %d, want -1", src.MaxCardinality())
	}
	if src.GetPrimaryKeys() != nil {
		t.Fatal("parameter source keys should be nil")
	}
}

func TestLoadByKeysPlan_Equality(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(1)}})
	b := NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(1)}})
	c := NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(2)}})
	d := NewRecordQueryLoadByKeysPlanFromParameter("p")

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same keys should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different keys should not be equal")
	}
	if a.EqualsWithoutChildren(d) {
		t.Fatal("keys source vs parameter source should not be equal")
	}
}

func TestLoadByKeysPlan_Explain(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryLoadByKeysPlanFromParameter("pk_list")
	got := p.Explain()
	if !strings.Contains(got, "LoadByKeys") || !strings.Contains(got, "$pk_list") {
		t.Fatalf("Explain = %q", got)
	}
}

func TestKeysSource_PrimaryKeysEquality(t *testing.T) {
	t.Parallel()
	a := NewPrimaryKeysKeySource([]tuple.Tuple{{int64(1), "a"}})
	b := NewPrimaryKeysKeySource([]tuple.Tuple{{int64(1), "a"}})
	c := NewPrimaryKeysKeySource([]tuple.Tuple{{int64(1), "b"}})

	if !a.Equals(b) {
		t.Fatal("same tuples should be equal")
	}
	if a.Equals(c) {
		t.Fatal("different tuples should not be equal")
	}
	if a.Equals(NewParameterKeySource("p")) {
		t.Fatal("different source types should not be equal")
	}
}

// ---------------------------------------------------------------------------
// RecordQueryScoreForRankPlan
// ---------------------------------------------------------------------------

func TestScoreForRankPlan_Construction(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	ranks := []ScoreForRank{
		{BindingName: "score", FunctionName: "rank", IndexName: "rank_idx", Comparisons: []string{"= 5"}},
	}
	p := NewRecordQueryScoreForRankPlan(inner, ranks)

	if p.GetInner() != inner {
		t.Fatal("inner plan mismatch")
	}
	children := p.GetChildren()
	if len(children) != 1 || children[0] != inner {
		t.Fatal("children should contain the inner plan")
	}
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatal("result type should match inner")
	}
	if len(p.GetRanks()) != 1 {
		t.Fatalf("ranks count = %d", len(p.GetRanks()))
	}
}

func TestScoreForRankPlan_Equality(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	r1 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "idx", Comparisons: []string{"= 1"}}
	r2 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "idx", Comparisons: []string{"= 1"}}
	r3 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "other", Comparisons: []string{"= 1"}}

	a := NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r1})
	b := NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r2})
	c := NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r3})

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same ranks should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("different index name should break equality")
	}
}

func TestScoreForRankPlan_Explain(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	ranks := []ScoreForRank{
		{BindingName: "s", FunctionName: "rank", IndexName: "idx_a", Comparisons: []string{"= 5", "> 0"}},
	}
	p := NewRecordQueryScoreForRankPlan(inner, ranks)
	got := p.Explain()
	if !strings.Contains(got, "ScoreForRank(") {
		t.Fatalf("Explain = %q", got)
	}
	if !strings.Contains(got, "idx_a.rank(= 5, > 0)") {
		t.Fatalf("Explain should contain rank call: %q", got)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryTextIndexPlan
// ---------------------------------------------------------------------------

func TestTextIndexPlan_Construction(t *testing.T) {
	t.Parallel()
	scan := TextScan{
		IndexName:           "text_idx",
		GroupingComparisons: "group = A",
		TextComparison:      "TEXT_CONTAINS_ALL 'hello world'",
		SuffixComparisons:   "",
	}
	p := NewRecordQueryTextIndexPlan("text_idx", scan, false)

	if p.GetIndexName() != "text_idx" {
		t.Fatalf("index = %q", p.GetIndexName())
	}
	if p.IsReverse() {
		t.Fatal("should not be reverse")
	}
	if p.GetResultType() != values.UnknownType {
		t.Fatal("text plan result type should be UnknownType")
	}
	if len(p.GetChildren()) != 0 {
		t.Fatal("text index plan should be a leaf")
	}
	if p.GetTextScan() != scan {
		t.Fatal("text scan mismatch")
	}
}

func TestTextIndexPlan_EqualityAndHash(t *testing.T) {
	t.Parallel()
	scan := TextScan{TextComparison: "CONTAINS 'x'"}
	a := NewRecordQueryTextIndexPlan("idx", scan, false)
	b := NewRecordQueryTextIndexPlan("idx", scan, false)
	c := NewRecordQueryTextIndexPlan("idx", scan, true)
	d := NewRecordQueryTextIndexPlan("other", scan, false)

	if !a.EqualsWithoutChildren(b) {
		t.Fatal("identical text plans should be equal")
	}
	if a.EqualsWithoutChildren(c) {
		t.Fatal("reverse flag should break equality")
	}
	if a.EqualsWithoutChildren(d) {
		t.Fatal("different index name should break equality")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal plans should have equal hashes")
	}
	if a.HashCodeWithoutChildren() == c.HashCodeWithoutChildren() {
		t.Fatal("different reverse should have different hashes")
	}
}

func TestTextIndexPlan_Explain(t *testing.T) {
	t.Parallel()
	scan := TextScan{TextComparison: "TEXT_CONTAINS_ALL 'hello'"}
	p := NewRecordQueryTextIndexPlan("my_text_idx", scan, true)
	got := p.Explain()
	want := "TextIndexScan(my_text_idx, TEXT_CONTAINS_ALL 'hello' REVERSE)"
	if got != want {
		t.Fatalf("Explain = %q, want %q", got, want)
	}
}
