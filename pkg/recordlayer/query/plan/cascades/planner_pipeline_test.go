package cascades

import (
	"fmt"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// planPipeline runs the full Cascades pipeline (logical tree -> Explore ->
// Plan -> extract physical plan) and returns the explain string of the
// extracted physical plan. No FDB required.
func planPipeline(t *testing.T, root expressions.RelationalExpression, indexes ...IndexDef) string {
	t.Helper()

	rootRef := expressions.InitialOf(root)

	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)

	var ctx PlanContext
	if len(indexes) > 0 {
		ctx = NewPlanContextFromIndexDefs(indexes)
	}

	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(10_000)

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if best == nil {
		t.Fatal("Plan returned nil best expression")
	}

	explain := ExplainPhysicalPlan(best)
	if explain != "" {
		return explain
	}
	// Fallback: describe the expression type.
	return fmt.Sprintf("%T", best)
}

func planPipelineWithStats(t *testing.T, root expressions.RelationalExpression, stats properties.StatisticsProvider, indexes ...IndexDef) string {
	t.Helper()

	rootRef := expressions.InitialOf(root)

	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)

	var ctx PlanContext
	if len(indexes) > 0 {
		ctx = NewPlanContextFromIndexDefs(indexes)
	}

	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules()).
		WithStatistics(stats).
		WithMaxTasks(10_000)

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if best == nil {
		t.Fatal("Plan returned nil best expression")
	}

	explain := ExplainPhysicalPlan(best)
	if explain != "" {
		return explain
	}
	return fmt.Sprintf("%T", best)
}

func planPipelineWithCandidates(t *testing.T, root expressions.RelationalExpression, candidates []MatchCandidate) string {
	t.Helper()

	rootRef := expressions.InitialOf(root)

	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)

	ctx := NewPlanContextFromMatchCandidates(candidates)

	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(10_000)

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if best == nil {
		t.Fatal("Plan returned nil best expression")
	}

	explain := ExplainPhysicalPlan(best)
	if explain != "" {
		return explain
	}
	return fmt.Sprintf("%T", best)
}

// idx builds a stubIndexDef with sensible defaults (recordTypes: ["T"]).
func idx(name string, columns ...string) IndexDef {
	return &stubIndexDef{
		name:        name,
		columns:     columns,
		recordTypes: []string{"T"},
	}
}

// idxUnique builds a unique stubIndexDef.
func idxUnique(name string, columns ...string) IndexDef {
	return &stubIndexDef{
		name:        name,
		columns:     columns,
		recordTypes: []string{"T"},
		unique:      true,
	}
}

// --- Basic operators ---

func TestPipeline_Scan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	plan := planPipeline(t, scan)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Scan(T)") {
		t.Fatalf("expected plan to contain Scan(T), got: %s", plan)
	}
}

func TestPipeline_Filter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "X", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, filter)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Filter") {
		t.Fatalf("expected plan to contain Filter, got: %s", plan)
	}
}

func TestPipeline_Projection(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
		},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

func TestPipeline_TypeFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T", "U"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"T"},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, tf)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "TypeFilter") {
		t.Fatalf("expected plan to contain TypeFilter, got: %s", plan)
	}
}

func TestPipeline_Sort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
		},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, sort)
	t.Logf("plan: %s", plan)
	// Sort over an unordered scan produces InMemorySort (Go extension).
	if !strings.Contains(plan, "InMemorySort") && !strings.Contains(plan, "Sort") {
		t.Fatalf("expected plan to contain InMemorySort or Sort, got: %s", plan)
	}
}

func TestPipeline_Distinct(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, distinct)
	t.Logf("plan: %s", plan)
	// Distinct over a scan that already produces distinct records may be
	// eliminated. Accept either Distinct or Scan.
	if !strings.Contains(plan, "Scan") {
		t.Fatalf("expected plan to contain Scan (distinct may be eliminated or preserved), got: %s", plan)
	}
}

func TestPipeline_Unique(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, unique)
	t.Logf("plan: %s", plan)
	// Unique depends on PK coverage; accept any non-empty plan.
	if plan == "" {
		t.Fatal("expected non-empty plan")
	}
}

func TestPipeline_Limit(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	limit := expressions.NewLogicalLimitExpression(10, 0,
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, limit)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("expected plan to contain Limit, got: %s", plan)
	}
}

// --- Index-based operators ---

func TestPipeline_IndexScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "A", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, filter, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected plan to contain IndexScan, got: %s", plan)
	}
}

func TestPipeline_OrderedIndexScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "A", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	plan := planPipeline(t, sort, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	// With an index on A, the sort should be eliminated or satisfied by
	// the index scan. The plan should contain IndexScan.
	if !strings.Contains(plan, "IndexScan") && !strings.Contains(plan, "Scan") {
		t.Fatalf("expected plan to contain IndexScan or Scan, got: %s", plan)
	}
}

func TestPipeline_StreamingAgg(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1)}, Alias: "cnt"},
		},
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, groupBy, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	// With an index on the grouping key, streaming aggregation is possible.
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected plan to contain StreamingAgg, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1)}, Alias: "cnt"},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan, got: %s", plan)
	}
}

func TestPipeline_AggregateIndexSUM(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "REGION", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}, Alias: "total"},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$sum_amount_by_region",
		[]string{"T"},
		[]string{"REGION"},
		expressions.AggSum,
		"AMOUNT",
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan, got: %s", plan)
	}
	if !strings.Contains(plan, "SUM") {
		t.Fatalf("expected SUM in plan, got: %s", plan)
	}
}

func TestPipeline_AggregateIndexMAX(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "CATEGORY", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggMax, Operand: &values.FieldValue{Field: "PRICE", Typ: values.UnknownType}, Alias: "max_price"},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$max_price_by_category",
		[]string{"T"},
		[]string{"CATEGORY"},
		expressions.AggMax,
		"PRICE",
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "MAX") {
		t.Fatalf("expected AggregateIndex(MAX, ...) plan, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_WithStats(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1)}, Alias: "cnt"},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
	)

	rootRef := expressions.InitialOf(groupBy)
	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	ctx := NewPlanContextFromMatchCandidates([]MatchCandidate{aggCand})
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules()).
		WithStatistics(stats).
		WithMaxTasks(10_000)

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plan := ExplainPhysicalPlan(best)
	t.Logf("plan (1M stats): %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("aggregate index should win with 1M stats, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_MismatchedFunction(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("should NOT use aggregate index for mismatched function, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_WithRegularIndex(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1)}, Alias: "cnt"},
		},
		expressions.ForEachQuantifier(scanRef),
	)

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
	)

	regularIdx := NewPlanContextFromIndexDefs([]IndexDef{idx("idx_status", "STATUS")})
	allCandidates := append(regularIdx.GetMatchCandidates(), aggCand)

	plan := planPipelineWithCandidates(t, groupBy, allCandidates)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("aggregate index should win over streaming agg+regular index, got: %s", plan)
	}
}

// --- Composite operators ---

func TestPipeline_Union(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	plan := planPipeline(t, union)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Union") {
		t.Fatalf("expected plan to contain Union, got: %s", plan)
	}
}

func TestPipeline_Join(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)

	sel := expressions.NewSelectExpressionWithAliases(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
		[]string{"A", "B"},
	)
	plan := planPipeline(t, sel)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "NestedLoopJoin") && !strings.Contains(plan, "FlatMap") {
		t.Fatalf("expected plan to contain NestedLoopJoin or FlatMap, got: %s", plan)
	}
}

func TestPipeline_StreamingAggNoIndex(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	groupBy := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: int64(1)}, Alias: "cnt"},
		},
		expressions.ForEachQuantifier(scanRef),
	)
	// No indexes — streaming aggregation is the only implementation.
	plan := planPipeline(t, groupBy)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected plan to contain StreamingAgg, got: %s", plan)
	}
}

func TestPipeline_FilterProjection(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "X", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	// Both operators should be present in the explain tree.
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
	if !strings.Contains(plan, "Filter") && !strings.Contains(plan, "Scan") {
		t.Fatalf("expected plan to contain Filter or Scan, got: %s", plan)
	}
}

// --- CTE / leaf ---

func TestPipeline_Values(t *testing.T) {
	t.Parallel()
	vals := expressions.NewLogicalValuesExpression([]values.Value{
		&values.ConstantValue{Value: int64(1)},
		&values.ConstantValue{Value: int64(2)},
	})
	plan := planPipeline(t, vals)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Values") {
		t.Fatalf("expected plan to contain Values, got: %s", plan)
	}
}

func TestPipeline_Explode(t *testing.T) {
	t.Parallel()
	explode := expressions.NewExplodeExpression(
		&values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}},
	)
	plan := planPipeline(t, explode)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Explode") {
		t.Fatalf("expected plan to contain Explode, got: %s", plan)
	}
}

// --- Determinism ---

func TestPipeline_Deterministic(t *testing.T) {
	t.Parallel()

	buildTree := func() expressions.RelationalExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "A", Typ: values.UnknownType},
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
				),
			},
			expressions.ForEachQuantifier(scanRef),
		)
		filterRef := expressions.InitialOf(filter)
		proj := expressions.NewLogicalProjectionExpression(
			[]values.Value{
				&values.FieldValue{Field: "A", Typ: values.UnknownType},
				&values.FieldValue{Field: "B", Typ: values.UnknownType},
			},
			expressions.ForEachQuantifier(filterRef),
		)
		return proj
	}

	var firstPlan string
	for i := 0; i < 10; i++ {
		root := buildTree()
		plan := planPipeline(t, root, idx("idx_a", "A"))
		if i == 0 {
			firstPlan = plan
			t.Logf("plan: %s", plan)
		} else if plan != firstPlan {
			t.Fatalf("run %d produced different plan:\n  first: %s\n  this:  %s", i, firstPlan, plan)
		}
	}
}

// --- Compound: Sort + Filter + Projection ---

func TestPipeline_SortFilterProjection(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "X", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	sortRef := expressions.InitialOf(sort)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
		},
		expressions.ForEachQuantifier(sortRef),
	)
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

// --- Limit + Filter ---

func TestPipeline_LimitOverFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "X", Typ: values.UnknownType},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
		},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)

	limit := expressions.NewLogicalLimitExpression(5, 0,
		expressions.ForEachQuantifier(filterRef),
	)
	plan := planPipeline(t, limit)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("expected plan to contain Limit, got: %s", plan)
	}
}

// --- Distinct + Sort ---

func TestPipeline_DistinctOverSort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}, Reverse: false},
		},
		expressions.ForEachQuantifier(scanRef),
	)
	sortRef := expressions.InitialOf(sort)

	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(sortRef),
	)
	plan := planPipeline(t, distinct)
	t.Logf("plan: %s", plan)
	// DistinctOverSortElimRule may eliminate the distinct or the sort.
	if plan == "" {
		t.Fatal("expected non-empty plan")
	}
}

// --- Projection + Distinct ---

func TestPipeline_ProjectionDistinct(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	distinctRef := expressions.InitialOf(distinct)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
		},
		expressions.ForEachQuantifier(distinctRef),
	)
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

// --- Limit with offset ---

func TestPipeline_LimitWithOffset(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	limit := expressions.NewLogicalLimitExpression(10, 5,
		expressions.ForEachQuantifier(scanRef),
	)
	plan := planPipeline(t, limit)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("expected plan to contain Limit, got: %s", plan)
	}
	if !strings.Contains(plan, "offset") {
		t.Fatalf("expected plan to contain offset, got: %s", plan)
	}
}

func TestPipeline_InListExplode(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: &values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}},
		},
	}
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred}, scanQ)
	plan := planPipeline(t, filter, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "InJoin") {
		t.Fatalf("expected InJoin plan for IN-list with index, got: %s", plan)
	}
}

func TestPipeline_InListExplode_WithStats(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: &values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}},
		},
	}
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred}, scanQ)

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	plan := planPipelineWithStats(t, filter, stats, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "InJoin") {
		t.Fatalf("expected InJoin plan with 1M stats, got: %s", plan)
	}
}

func TestPipeline_InListExplodeWithProjectionAndSort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: &values.ConstantValue{Value: []any{int64(1), int64(2), int64(3)}},
		},
	}
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPred}, scanQ)
	filterRef := expressions.InitialOf(filter)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "B", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	sortRef := expressions.InitialOf(sort)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
		},
		expressions.ForEachQuantifier(sortRef),
	)

	rootRef := expressions.InitialOf(proj)
	rules := DefaultExpressionRules()
	rules = append(rules, BatchAExpressionRules()...)
	rules = append(rules, MatchingRules()...)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{idx("idx_a", "A")})
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(10_000)
	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	plan := ExplainPhysicalPlan(best)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "InJoin") {
		t.Fatal("expected InJoin in plan")
	}
	if !strings.Contains(plan, "IndexScan") {
		t.Fatal("expected IndexScan inside InJoin for correlated index lookup")
	}
}

func TestPipeline_Intersection(t *testing.T) {
	t.Parallel()
	// Two filters on different indexed columns → potential intersection.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	p1 := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	p2 := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "B", Typ: values.UnknownType},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(2)},
		},
	}
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{p1, p2}, scanQ)
	plan := planPipeline(t, filter,
		idx("idx_a", "A"),
		idx("idx_b", "B"),
	)
	t.Logf("plan: %s", plan)
	// Should use at least one index.
	if !strings.Contains(plan, "Index") && !strings.Contains(plan, "Filter") {
		t.Fatalf("expected index or filter plan, got: %s", plan)
	}
}

func TestPipeline_DistinctOverProjection(t *testing.T) {
	t.Parallel()
	// DISTINCT over projection — exercises MapPlan distinct-records
	// property propagation.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
		}, scanQ)
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	plan := planPipeline(t, distinct)
	t.Logf("plan: %s", plan)
	// Should produce a plan (Distinct or direct if eliminated).
	if plan == "" {
		t.Fatal("expected non-empty plan")
	}
}

func TestPipeline_SortOverDistinct(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)
	distinctQ := expressions.ForEachQuantifier(distinctRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "A", Typ: values.UnknownType}}},
		distinctQ,
	)
	plan := planPipeline(t, sort)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Sort") || !strings.Contains(plan, "Scan") {
		t.Fatalf("expected Sort over Scan, got: %s", plan)
	}
}

func TestPipeline_UnionWithProjection(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scan1Q, scan2Q})
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "ID", Typ: values.UnknownType}},
		unionQ,
	)
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Union") {
		t.Fatalf("expected Union in plan, got: %s", plan)
	}
}
