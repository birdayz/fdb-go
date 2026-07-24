package cascades

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type lockedDiagnosticBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedDiagnosticBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedDiagnosticBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newCostModelDiagnosticLogger(level slog.Leveler) (*slog.Logger, *lockedDiagnosticBuffer) {
	buf := &lockedDiagnosticBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})), buf
}

func diagnosticLogLines(buf *lockedDiagnosticBuffer) []string {
	output := strings.TrimSpace(buf.String())
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}

func diagnosticLogicalCounts(plan plans.RecordQueryPlan, ctx PlanContext) expressionCounts {
	logical := expressions.NewLogicalProjectionExpression(
		nil,
		expressions.ForEachQuantifier(
			expressions.FinalOfAtStage(plan, expressions.StageCanonical),
		),
	)
	return findExpressionsByType(logical, nil, ctx)
}

func diagnosticEqualityRange(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to construct equality comparison range")
	}
	return merged.Range
}

type diagnosticPKContext struct {
	emptyPlanContext
	pk []string
}

func (c *diagnosticPKContext) GetPrimaryKeyColumns(string) []string {
	return c.pk
}

// unclassifiedCostModelTestPlan deliberately has neither an RFC-190.14
// taxonomy entry nor HintCost. It is the future-plan-type adversary for all
// three cost-model walks.
type unclassifiedCostModelTestPlan struct {
	plans.PlanExprBase
	children []plans.RecordQueryPlan
}

func (p *unclassifiedCostModelTestPlan) GetResultType() values.Type {
	return values.UnknownType
}

func (p *unclassifiedCostModelTestPlan) GetChildren() []plans.RecordQueryPlan {
	return p.children
}

func (p *unclassifiedCostModelTestPlan) EqualsPlanWithoutChildren(other plans.RecordQueryPlan) bool {
	_, ok := other.(*unclassifiedCostModelTestPlan)
	return ok
}

func (p *unclassifiedCostModelTestPlan) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	_, ok := other.(*unclassifiedCostModelTestPlan)
	return ok
}

func (p *unclassifiedCostModelTestPlan) HashCodeWithoutChildren() uint64 {
	return 0x19014
}

func (p *unclassifiedCostModelTestPlan) WithQuantifiers(
	_ []expressions.Quantifier,
) expressions.RelationalExpression {
	return p
}

func (p *unclassifiedCostModelTestPlan) Explain() string {
	return "SECRET-EXPLAIN-PAYLOAD"
}

func (p *unclassifiedCostModelTestPlan) GetRecordQueryPlan() plans.RecordQueryPlan {
	return p
}

var (
	_ plans.RecordQueryPlan  = (*unclassifiedCostModelTestPlan)(nil)
	_ physicalPlanExpression = (*unclassifiedCostModelTestPlan)(nil)
)

func TestCostModelDiagnosticsDeduplicateConcurrentWalks(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	plan := &unclassifiedCostModelTestPlan{}

	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range 4 {
				_ = concretePlanCounts(plan, ctx)
				_ = concreteResidualPredicatesWithContext(plan, ctx)
				_ = concretePlanCost(plan, properties.DefaultStatistics{}, ctx)
			}
		}()
	}
	wg.Wait()

	lines := diagnosticLogLines(buf)
	if len(lines) != 3 {
		t.Fatalf("diagnostic records = %d, want one per walk (3):\n%s", len(lines), buf.String())
	}
	for _, walk := range []costModelDiagnosticWalk{
		costModelDiagnosticCounts,
		costModelDiagnosticResidual,
		costModelDiagnosticCost,
	} {
		token := "walk=" + string(walk)
		if got := strings.Count(buf.String(), token); got != 1 {
			t.Fatalf("%s records = %d, want 1:\n%s", token, got, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "plan_type=*cascades.unclassifiedCostModelTestPlan") {
		t.Fatalf("diagnostic omitted concrete Go type:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "SECRET-EXPLAIN-PAYLOAD") {
		t.Fatalf("diagnostic called or exposed Explain():\n%s", buf.String())
	}
}

func TestCostModelDiagnosticsAreInstanceScoped(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	plan := &unclassifiedCostModelTestPlan{}
	first := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	second := WithCostModelDiagnostics(EmptyPlanContext(), logger)

	for range 2 {
		_ = concretePlanCounts(plan, first)
		_ = concretePlanCounts(plan, second)
	}

	lines := diagnosticLogLines(buf)
	if len(lines) != 2 {
		t.Fatalf("diagnostic records = %d, want one per wrapper instance (2):\n%s", len(lines), buf.String())
	}
}

func TestCostModelDiagnosticsNilLoggerMasksInnerSink(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	enabled := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	masked := WithCostModelDiagnostics(enabled, nil)
	plan := &unclassifiedCostModelTestPlan{}

	_ = concretePlanCounts(plan, masked)
	_ = concreteResidualPredicatesWithContext(plan, masked)
	_ = concretePlanCost(plan, properties.DefaultStatistics{}, masked)

	if lines := diagnosticLogLines(buf); len(lines) != 0 {
		t.Fatalf("nil logger failed to mask inner sink:\n%s", buf.String())
	}
}

func TestCostModelDiagnosticsDisabledLevelDoesNotConsumeWarning(t *testing.T) {
	t.Parallel()

	var level slog.LevelVar
	level.Set(slog.LevelError)
	logger, buf := newCostModelDiagnosticLogger(&level)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	plan := &unclassifiedCostModelTestPlan{}

	_ = concretePlanCounts(plan, ctx)
	_ = concreteResidualPredicatesWithContext(plan, ctx)
	_ = concretePlanCost(plan, properties.DefaultStatistics{}, ctx)
	if lines := diagnosticLogLines(buf); len(lines) != 0 {
		t.Fatalf("WARN-disabled logger emitted records:\n%s", buf.String())
	}

	level.Set(slog.LevelWarn)
	_ = concretePlanCounts(plan, ctx)
	_ = concreteResidualPredicatesWithContext(plan, ctx)
	_ = concretePlanCost(plan, properties.DefaultStatistics{}, ctx)
	if lines := diagnosticLogLines(buf); len(lines) != 3 {
		t.Fatalf("disabled calls consumed dedupe keys; records = %d, want 3:\n%s", len(lines), buf.String())
	}
}

func TestCostModelDiagnosticsKnownNeutralPlanIsQuiet(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	plan := plans.NewRecordQueryValuesPlan(nil)

	_ = concretePlanCounts(plan, ctx)
	_ = concreteResidualPredicatesWithContext(plan, ctx)
	_ = concretePlanCost(plan, properties.DefaultStatistics{}, ctx)

	if lines := diagnosticLogLines(buf); len(lines) != 0 {
		t.Fatalf("known-neutral plan emitted diagnostics:\n%s", buf.String())
	}
}

func TestCostModelDiagnosticsCoverLogicalFallbackWalks(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	unknown := &unclassifiedCostModelTestPlan{}
	logical := expressions.NewLogicalProjectionExpression(
		nil,
		expressions.ForEachQuantifier(
			expressions.FinalOfAtStage(unknown, expressions.StageCanonical),
		),
	)

	_ = findExpressionsByType(logical, nil, ctx)
	_ = countResidualPredicatesWithContext(logical, ctx)

	lines := diagnosticLogLines(buf)
	if len(lines) != 2 {
		t.Fatalf("logical fallback records = %d, want count + residual (2):\n%s", len(lines), buf.String())
	}
	for _, walk := range []costModelDiagnosticWalk{
		costModelDiagnosticCounts,
		costModelDiagnosticResidual,
	} {
		if !strings.Contains(buf.String(), "walk="+string(walk)) {
			t.Fatalf("logical fallback omitted %s:\n%s", walk, buf.String())
		}
	}
}

func TestCostModelDiagnosticsPreserveLogicalCountPolicy(t *testing.T) {
	t.Parallel()

	t.Run("unique index remains plan-local without context", func(t *testing.T) {
		t.Parallel()
		index := plans.NewRecordQueryIndexPlan(
			"idx_unique",
			[]*predicates.ComparisonRange{
				diagnosticEqualityRange(t, int64(1)),
				diagnosticEqualityRange(t, int64(2)),
			},
			[]string{"T"},
			values.UnknownType,
			false,
		).WithIndexMetadata([]string{"A", "B"}, nil, true)

		counts := diagnosticLogicalCounts(index, nil)
		if counts.indexScanCount != 1 ||
			counts.maxDataAccessCardinality != 1 ||
			counts.unboundedDataAccess {
			t.Fatalf("logical unique-index counts changed: %+v", counts)
		}
	})

	t.Run("unmatched index fields remain plan-local", func(t *testing.T) {
		t.Parallel()
		index := plans.NewRecordQueryIndexPlan(
			"idx_partial",
			[]*predicates.ComparisonRange{diagnosticEqualityRange(t, int64(1))},
			[]string{"T"},
			values.UnknownType,
			false,
		).WithIndexMetadata([]string{"A", "B", "C"}, nil, false)

		counts := diagnosticLogicalCounts(index, nil)
		if counts.indexScanCount != 1 ||
			counts.unmatchedFieldCount != 2 ||
			!counts.unboundedDataAccess {
			t.Fatalf("logical partial-index counts changed: %+v", counts)
		}
	})

	t.Run("PK filter still descends into its scan", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		filter := plans.NewRecordQueryPredicatesFilterPlan(
			scan,
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "ID", Typ: values.UnknownType},
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
				),
			},
		)

		counts := diagnosticLogicalCounts(filter, &diagnosticPKContext{pk: []string{"ID"}})
		if counts.predicatesFilterCount != 1 ||
			counts.scanCount != 1 ||
			!counts.unboundedDataAccess {
			t.Fatalf("logical PK-filter counts changed: %+v", counts)
		}
	})

	t.Run("multi-intersection still counts each aggregate leg", func(t *testing.T) {
		t.Parallel()
		aggregate := func(name string) plans.RecordQueryPlan {
			index := plans.NewRecordQueryIndexPlan(
				name,
				nil,
				[]string{"T"},
				values.UnknownType,
				false,
			)
			return plans.NewRecordQueryAggregateIndexPlan(index, "T", values.UnknownType, "COUNT")
		}
		multiIntersection := plans.NewRecordQueryMultiIntersectionOnValuesPlan(
			[]plans.RecordQueryPlan{aggregate("agg_a"), aggregate("agg_b")},
			nil,
			nil,
		)

		counts := diagnosticLogicalCounts(multiIntersection, nil)
		if counts.coveringIndexCount != 2 || !counts.unboundedDataAccess {
			t.Fatalf("logical multi-intersection counts changed: %+v", counts)
		}
	})

	t.Run("text index remains logical-count neutral", func(t *testing.T) {
		t.Parallel()
		text := plans.NewRecordQueryTextIndexPlan(
			"idx_text",
			plans.TextScan{IndexName: "idx_text", TextComparison: "contains"},
			false,
		)

		counts := diagnosticLogicalCounts(text, nil)
		if counts.indexScanCount != 0 ||
			counts.coveringIndexCount != 0 ||
			counts.unboundedDataAccess {
			t.Fatalf("logical text-index counts changed: %+v", counts)
		}
	})
}

func TestCostModelDiagnosticsValidateFoldedCountChildren(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	multiIntersection := plans.NewRecordQueryMultiIntersectionOnValuesPlan(
		[]plans.RecordQueryPlan{&unclassifiedCostModelTestPlan{}},
		nil,
		nil,
	)

	_ = concretePlanCounts(multiIntersection, ctx)

	lines := diagnosticLogLines(buf)
	if len(lines) != 1 || !strings.Contains(lines[0], "walk=operator_counts") {
		t.Fatalf("folded child diagnostics = %d, want one count warning:\n%s", len(lines), buf.String())
	}
}

func TestCostModelDiagnosticsRejectUnhandledClassificationKinds(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
	plan := &unclassifiedCostModelTestPlan{}
	counts := expressionCounts{}

	_ = countClassifiedConcreteNode(
		plan,
		concretePlanClassification{count: concreteCountKind(255)},
		&counts,
		ctx,
	)
	_ = countClassifiedResidualPredicates(
		plan,
		concretePlanClassification{residual: concreteResidualKind(255)},
		ctx,
	)

	lines := diagnosticLogLines(buf)
	if len(lines) != 2 {
		t.Fatalf("unhandled classification records = %d, want count + residual (2):\n%s", len(lines), buf.String())
	}
	for _, fallback := range []string{
		"classified count kind has no concrete count policy",
		"classified residual kind has no predicate-count policy",
	} {
		if !strings.Contains(buf.String(), fallback) {
			t.Fatalf("missing fallback %q:\n%s", fallback, buf.String())
		}
	}
}

func TestCostModelDiagnosticPlumbingPreservesComparatorContextSemantics(t *testing.T) {
	t.Parallel()

	logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
	wrapped := WithCostModelDiagnostics(&prefTestCtx{pref: PreferIndex}, logger)
	primary := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	index := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false)

	noStatsBaseline := NewPlanningCostModelLess(nil)
	noStatsComparators := []struct {
		name string
		less func(expressions.RelationalExpression, expressions.RelationalExpression) bool
	}{
		{name: "planner", less: NewPlanner(nil, wrapped).costModel},
		{name: "expression rule", less: (&ExpressionRuleCall{Context: wrapped}).CostModel()},
		{name: "implementation rule", less: (&ImplementationRuleCall{Context: wrapped}).CostModel()},
	}
	for _, tc := range noStatsComparators {
		if got, want := tc.less(primary, index), noStatsBaseline(primary, index); got != want {
			t.Fatalf("%s no-stats primary<index = %t, want historical nil-context %t", tc.name, got, want)
		}
		if got, want := tc.less(index, primary), noStatsBaseline(index, primary); got != want {
			t.Fatalf("%s no-stats index<primary = %t, want historical nil-context %t", tc.name, got, want)
		}
	}

	stats := properties.DefaultStatistics{}
	withStatsComparators := []struct {
		name string
		less func(expressions.RelationalExpression, expressions.RelationalExpression) bool
	}{
		{name: "planner", less: NewPlanner(nil, wrapped).WithStatistics(stats).costModel},
		{name: "expression rule", less: (&ExpressionRuleCall{Context: wrapped, Stats: stats}).CostModel()},
		{name: "implementation rule", less: (&ImplementationRuleCall{Context: wrapped, Stats: stats}).CostModel()},
	}
	for _, tc := range withStatsComparators {
		if !tc.less(index, primary) || tc.less(primary, index) {
			t.Fatalf("%s with-stats comparator did not preserve wrapped PreferIndex context", tc.name)
		}
	}

	if lines := diagnosticLogLines(buf); len(lines) != 0 {
		t.Fatalf("known plans emitted diagnostics during plumbing test:\n%s", buf.String())
	}
}

func TestCostModelDiagnosticSinkSurvivesNoStatsPlumbing(t *testing.T) {
	t.Parallel()

	paths := []struct {
		name       string
		comparator func(PlanContext) func(
			expressions.RelationalExpression,
			expressions.RelationalExpression,
		) bool
	}{
		{
			name: "planner",
			comparator: func(ctx PlanContext) func(
				expressions.RelationalExpression,
				expressions.RelationalExpression,
			) bool {
				return NewPlanner(nil, ctx).costModel
			},
		},
		{
			name: "expression rule",
			comparator: func(ctx PlanContext) func(
				expressions.RelationalExpression,
				expressions.RelationalExpression,
			) bool {
				return (&ExpressionRuleCall{Context: ctx}).CostModel()
			},
		},
		{
			name: "implementation rule",
			comparator: func(ctx PlanContext) func(
				expressions.RelationalExpression,
				expressions.RelationalExpression,
			) bool {
				return (&ImplementationRuleCall{Context: ctx}).CostModel()
			},
		},
	}

	for _, path := range paths {
		logger, buf := newCostModelDiagnosticLogger(slog.LevelWarn)
		ctx := WithCostModelDiagnostics(EmptyPlanContext(), logger)
		less := path.comparator(ctx)
		unknown := &unclassifiedCostModelTestPlan{}
		known := plans.NewRecordQueryValuesPlan(nil)
		outerAlias := values.NamedCorrelationIdentifier("diagnostic_outer")
		innerAlias := values.NamedCorrelationIdentifier("diagnostic_inner")
		left := plans.NewRecordQueryFlatMapPlan(
			unknown,
			known,
			outerAlias,
			innerAlias,
			values.LiteralValue(int64(1)),
			false,
		)
		right := plans.NewRecordQueryFlatMapPlan(
			known,
			unknown,
			innerAlias,
			outerAlias,
			values.LiteralValue(int64(1)),
			false,
		)
		_ = less(left, right)

		lines := diagnosticLogLines(buf)
		if len(lines) != 3 {
			t.Fatalf("%s diagnostic records = %d, want all three walks:\n%s", path.name, len(lines), buf.String())
		}
		for _, walk := range []costModelDiagnosticWalk{
			costModelDiagnosticCounts,
			costModelDiagnosticResidual,
			costModelDiagnosticCost,
		} {
			if !strings.Contains(buf.String(), "walk="+string(walk)) {
				t.Fatalf("%s dropped %s sink:\n%s", path.name, walk, buf.String())
			}
		}
	}
}

func TestClassifyConcretePlanExhaustiveTaxonomy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		plan     plans.RecordQueryPlan
		count    concreteCountKind
		residual concreteResidualKind
	}{
		{name: "AggregateIndex", plan: (*plans.RecordQueryAggregateIndexPlan)(nil), count: concreteCountAggregateIndex},
		{name: "Comparator", plan: (*plans.RecordQueryComparatorPlan)(nil)},
		{name: "DefaultOnEmpty", plan: (*plans.RecordQueryDefaultOnEmptyPlan)(nil), count: concreteCountDefaultOnEmpty},
		{name: "Delete", plan: (*plans.RecordQueryDeletePlan)(nil)},
		{name: "Distinct", plan: (*plans.RecordQueryDistinctPlan)(nil)},
		{name: "Explode", plan: (*plans.RecordQueryExplodePlan)(nil)},
		{name: "FetchFromPartialRecord", plan: (*plans.RecordQueryFetchFromPartialRecordPlan)(nil), count: concreteCountFetch},
		{name: "Filter", plan: (*plans.RecordQueryFilterPlan)(nil), residual: concreteResidualPredicateCNF},
		{name: "FirstOrDefault", plan: (*plans.RecordQueryFirstOrDefaultPlan)(nil)},
		{name: "FlatMap", plan: (*plans.RecordQueryFlatMapPlan)(nil), count: concreteCountFlatMap},
		{name: "InJoin", plan: (*plans.RecordQueryInJoinPlan)(nil), count: concreteCountInJoin},
		{name: "InMemorySort", plan: (*plans.RecordQueryInMemorySortPlan)(nil), count: concreteCountInMemorySort},
		{name: "InUnion", plan: (*plans.RecordQueryInUnionPlan)(nil), count: concreteCountInUnion},
		{name: "Index", plan: (*plans.RecordQueryIndexPlan)(nil), count: concreteCountIndex},
		{name: "Insert", plan: (*plans.RecordQueryInsertPlan)(nil)},
		{name: "Intersection", plan: (*plans.RecordQueryIntersectionPlan)(nil)},
		{name: "Limit", plan: (*plans.RecordQueryLimitPlan)(nil)},
		{name: "LoadByKeys", plan: (*plans.RecordQueryLoadByKeysPlan)(nil)},
		{name: "Map", plan: (*plans.RecordQueryMapPlan)(nil), count: concreteCountMap},
		{name: "MergeSortUnion", plan: (*plans.RecordQueryMergeSortUnionPlan)(nil)},
		{name: "MultiIntersectionOnValues", plan: (*plans.RecordQueryMultiIntersectionOnValuesPlan)(nil), count: concreteCountMultiIntersection},
		{
			name:     "NestedLoopJoin",
			plan:     (*plans.RecordQueryNestedLoopJoinPlan)(nil),
			count:    concreteCountNestedLoopJoin,
			residual: concreteResidualPredicateCNF,
		},
		{
			name:     "PredicatesFilter",
			plan:     (*plans.RecordQueryPredicatesFilterPlan)(nil),
			count:    concreteCountPredicatesFilter,
			residual: concreteResidualPredicateCNF,
		},
		{name: "Projection", plan: (*plans.RecordQueryProjectionPlan)(nil)},
		{name: "RecursiveDfsJoin", plan: (*plans.RecordQueryRecursiveDfsJoinPlan)(nil)},
		{name: "RecursiveLevelUnion", plan: (*plans.RecordQueryRecursiveLevelUnionPlan)(nil)},
		{name: "Scan", plan: (*plans.RecordQueryScanPlan)(nil), count: concreteCountScan},
		{name: "ScoreForRank", plan: (*plans.RecordQueryScoreForRankPlan)(nil)},
		{name: "Selector", plan: (*plans.RecordQuerySelectorPlan)(nil)},
		{name: "StreamingAggregation", plan: (*plans.RecordQueryStreamingAggregationPlan)(nil)},
		{name: "TableFunction", plan: (*plans.RecordQueryTableFunctionPlan)(nil)},
		{name: "TempTableInsert", plan: (*plans.RecordQueryTempTableInsertPlan)(nil)},
		{name: "TempTableScan", plan: (*plans.RecordQueryTempTableScanPlan)(nil)},
		{name: "TextIndex", plan: (*plans.RecordQueryTextIndexPlan)(nil), count: concreteCountTextIndex},
		{name: "TypeFilter", plan: (*plans.RecordQueryTypeFilterPlan)(nil), count: concreteCountTypeFilter},
		{name: "Union", plan: (*plans.RecordQueryUnionPlan)(nil)},
		{name: "UnorderedPrimaryKeyDistinct", plan: (*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)(nil)},
		{name: "UnorderedUnion", plan: (*plans.RecordQueryUnorderedUnionPlan)(nil)},
		{name: "Update", plan: (*plans.RecordQueryUpdatePlan)(nil)},
		{name: "Values", plan: (*plans.RecordQueryValuesPlan)(nil)},
		{name: "VectorIndex", plan: (*plans.RecordQueryVectorIndexPlan)(nil), count: concreteCountVectorIndex},
	}

	if len(cases) != 41 {
		t.Fatalf("taxonomy fixture contains %d plan types, want 41", len(cases))
	}
	for _, tc := range cases {
		classification, known := classifyConcretePlan(tc.plan)
		if !known {
			t.Fatalf("%s is not classified", tc.name)
		}
		if classification.count != tc.count || classification.residual != tc.residual {
			t.Fatalf(
				"%s classification = {count:%d residual:%d}, want {count:%d residual:%d}",
				tc.name,
				classification.count,
				classification.residual,
				tc.count,
				tc.residual,
			)
		}
	}

	if _, known := classifyConcretePlan(&unclassifiedCostModelTestPlan{}); known {
		t.Fatal("future-plan adversary unexpectedly classified")
	}
}
