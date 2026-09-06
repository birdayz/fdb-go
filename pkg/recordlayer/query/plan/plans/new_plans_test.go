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
	idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("sum_idx", nil, []string{"Sale"}, exactTestRecordType(), false)
	})
	p := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "Sale", values.NotNullLong, "SUM")
	})

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

// TestAggregateIndexPlan_CanonicalAggColumnName pins the one name this plan still
// mints: the canonical aggregate column the aggregateIndexCursor writes the value
// under, "FUNC(*)" for a count-star index and "FUNC(col)" otherwise. A bare
// aggregate-index plan is always unaliased, so no alias is involved; the group
// columns are stated by the plan's result row, not by a name list of their own.
func TestAggregateIndexPlan_CanonicalAggColumnName(t *testing.T) {
	t.Parallel()

	// Grouped COUNT(*): a count-star index names its slot COUNT(*).
	cnt := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan("cnt_by_g", nil, []string{"GA"}, exactTestRecordType(), false)
		}), "GA", exactTestRecordType(), "COUNT",
		)
	}).WithGroupColumns([]string{"G"}, "")
	if got := cnt.CanonicalAggColumnName(); got != "COUNT(*)" {
		t.Fatalf("canonical = %q, want COUNT(*)", got)
	}

	// Grouped SUM(V): a value aggregate names its slot by its operand.
	sum := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan("sum_by_g", nil, []string{"GA"}, exactTestRecordType(), false)
		}), "GA", exactTestRecordType(), "SUM",
		)
	}).WithGroupColumns([]string{"G"}, "V")
	if got := sum.CanonicalAggColumnName(); got != "SUM(V)" {
		t.Fatalf("canonical = %q, want SUM(V)", got)
	}
}

func TestAggregateIndexPlan_LeafPlan(t *testing.T) {
	t.Parallel()
	idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("idx", nil, []string{"T"}, exactTestRecordType(), false)
	})
	p := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "T", exactTestRecordType(), "COUNT")
	})

	if len(p.GetChildren()) != 0 {
		t.Fatal("aggregate index plan should be a leaf (no children)")
	}
	if !exactTestRecordType().Equals(p.GetResultType()) {
		t.Fatalf("result type = %v, want exact fixture type", p.GetResultType())
	}
}

func TestAggregateIndexPlan_EqualityAndHash(t *testing.T) {
	t.Parallel()
	idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, exactTestRecordType(), false)
	})
	a := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "T", exactTestRecordType(), "SUM")
	})
	b := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "T", exactTestRecordType(), "SUM")
	})
	c := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "T", exactTestRecordType(), "COUNT")
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("identical aggregate plans should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("different aggregate functions should not be equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("equal plans should have equal hashes")
	}
}

func TestAggregateIndexPlan_Explain(t *testing.T) {
	t.Parallel()
	idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("sum_idx", nil, []string{"Sale"}, exactTestRecordType(), false)
	})
	p := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(idx, "Sale", exactTestRecordType(), "SUM")
	})
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
	c1 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	c2 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	keys := []values.Value{testField(t, "id", values.NullableLong)}
	p := mustChecked(t, func() (*RecordQueryComparatorPlan, error) {
		return NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, false, true)
	})

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

func TestComparatorPlan_RejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryComparatorPlan(nil, nil, 0, false, false); err == nil {
		t.Fatal("expected empty comparator children to be rejected")
	}
}

func TestComparatorPlan_RejectsBadRefIndex(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if _, err := NewRecordQueryComparatorPlan([]RecordQueryPlan{scan}, nil, 5, false, false); err == nil {
		t.Fatal("expected out-of-range comparator reference index to be rejected")
	}
}

func TestComparatorPlan_EqualityAndExplain(t *testing.T) {
	t.Parallel()
	c1 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	c2 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	keys := []values.Value{testField(t, "id", values.NullableLong)}

	a := mustChecked(t, func() (*RecordQueryComparatorPlan, error) {
		return NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, true, false)
	})
	b := mustChecked(t, func() (*RecordQueryComparatorPlan, error) {
		return NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 0, true, false)
	})
	c := mustChecked(t, func() (*RecordQueryComparatorPlan, error) {
		return NewRecordQueryComparatorPlan([]RecordQueryPlan{c1, c2}, keys, 1, true, false)
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("identical comparator plans should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
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
	c1 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	c2 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	p := mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlanWithProbabilities(
			[]RecordQueryPlan{c1, c2}, []int{70, 30}, false)
	})

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

func TestSelectorPlan_RejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQuerySelectorPlan(nil, nil, false); err == nil {
		t.Fatal("expected empty selector children to be rejected")
	}
}

func TestSelectorPlan_EqualityAndExplain(t *testing.T) {
	t.Parallel()
	c1 := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	sel1 := NewRelativeProbabilityPlanSelector([]int{50, 50})
	sel2 := NewRelativeProbabilityPlanSelector([]int{50, 50})
	sel3 := NewRelativeProbabilityPlanSelector([]int{70, 30})

	a := mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel1, false)
	})
	b := mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel2, false)
	})
	c := mustChecked(t, func() (*RecordQuerySelectorPlan, error) {
		return NewRecordQuerySelectorPlan([]RecordQueryPlan{c1, c1}, sel3, false)
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same probability selectors should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
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
	p := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromKeys(keys, exactTestRecordType())
	})

	if !exactTestRecordType().Equals(p.GetResultType()) {
		t.Fatalf("result type = %v, want exact fixture type", p.GetResultType())
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
	p := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("pk_list", exactTestRecordType())
	})

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
	a := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(1)}}, exactTestRecordType())
	})
	b := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(1)}}, exactTestRecordType())
	})
	c := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromKeys([]tuple.Tuple{{int64(2)}}, exactTestRecordType())
	})
	d := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("p", exactTestRecordType())
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same keys should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("different keys should not be equal")
	}
	if a.EqualsPlanWithoutChildren(d) {
		t.Fatal("keys source vs parameter source should not be equal")
	}
}

func TestLoadByKeysPlan_Explain(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryLoadByKeysPlan, error) {
		return NewRecordQueryLoadByKeysPlanFromParameter("pk_list", exactTestRecordType())
	})
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
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	ranks := []ScoreForRank{
		{BindingName: "score", FunctionName: "rank", IndexName: "rank_idx", Comparisons: []string{"= 5"}},
	}
	p := mustChecked(t, func() (*RecordQueryScoreForRankPlan, error) {
		return NewRecordQueryScoreForRankPlan(inner, ranks)
	})

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
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	r1 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "idx", Comparisons: []string{"= 1"}}
	r2 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "idx", Comparisons: []string{"= 1"}}
	r3 := ScoreForRank{BindingName: "s", FunctionName: "rank", IndexName: "other", Comparisons: []string{"= 1"}}

	a := mustChecked(t, func() (*RecordQueryScoreForRankPlan, error) {
		return NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r1})
	})
	b := mustChecked(t, func() (*RecordQueryScoreForRankPlan, error) {
		return NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r2})
	})
	c := mustChecked(t, func() (*RecordQueryScoreForRankPlan, error) {
		return NewRecordQueryScoreForRankPlan(inner, []ScoreForRank{r3})
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same ranks should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("different index name should break equality")
	}
}

func TestScoreForRankPlan_Explain(t *testing.T) {
	t.Parallel()
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	ranks := []ScoreForRank{
		{BindingName: "s", FunctionName: "rank", IndexName: "idx_a", Comparisons: []string{"= 5", "> 0"}},
	}
	p := mustChecked(t, func() (*RecordQueryScoreForRankPlan, error) {
		return NewRecordQueryScoreForRankPlan(inner, ranks)
	})
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
	p := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("text_idx", scan, exactTestRecordType(), false)
	})

	if p.GetIndexName() != "text_idx" {
		t.Fatalf("index = %q", p.GetIndexName())
	}
	if p.IsReverse() {
		t.Fatal("should not be reverse")
	}
	if !exactTestRecordType().Equals(p.GetResultType()) {
		t.Fatalf("text plan result type = %v, want exact fixture type", p.GetResultType())
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
	a := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("idx", scan, exactTestRecordType(), false)
	})
	b := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("idx", scan, exactTestRecordType(), false)
	})
	c := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("idx", scan, exactTestRecordType(), true)
	})
	d := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("other", scan, exactTestRecordType(), false)
	})

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("identical text plans should be equal")
	}
	if a.EqualsPlanWithoutChildren(c) {
		t.Fatal("reverse flag should break equality")
	}
	if a.EqualsPlanWithoutChildren(d) {
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
	p := mustChecked(t, func() (*RecordQueryTextIndexPlan, error) {
		return NewRecordQueryTextIndexPlan("my_text_idx", scan, exactTestRecordType(), true)
	})
	got := p.Explain()
	want := "TextIndexScan(my_text_idx, TEXT_CONTAINS_ALL 'hello' REVERSE)"
	if got != want {
		t.Fatalf("Explain = %q, want %q", got, want)
	}
}
