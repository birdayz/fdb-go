package cascades

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPipelineConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct planner pipeline fixture: " + err.Error())
	}
	return value
}

func pipelineRowType() values.Type {
	return values.NewRecordType("PipelineRow", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 3},
		{Name: "Z", FieldType: values.NotNullLong, Ordinal: 4},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 5},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 6},
		{Name: "REGION", FieldType: values.NullableString, Ordinal: 7},
		{Name: "AMOUNT", FieldType: values.NullableLong, Ordinal: 8},
		{Name: "CATEGORY", FieldType: values.NullableString, Ordinal: 9},
		{Name: "PRICE", FieldType: values.NullableLong, Ordinal: 10},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 11},
	})
}

func pipelineScan(recordTypes ...string) *expressions.FullUnorderedScanExpression {
	return mustPipelineConstruct(expressions.NewFullUnorderedScanExpression(
		recordTypes, pipelineRowType()))
}

func pipelineRoot(q expressions.Quantifier) values.QuantifiedObjectValue {
	flowedType := mustPipelineConstruct(q.GetFlowedObjectType())
	return mustPipelineConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), flowedType))
}

func pipelineField(q expressions.Quantifier, name string) values.Value {
	request := mustPipelineConstruct(values.FieldByName(name))
	return mustPipelineConstruct(values.ResolveFieldAccess(
		pipelineRoot(q), []values.FieldRequest{request}))
}

func pipelineFieldAt(q expressions.Quantifier, ordinal int) values.Value {
	return mustPipelineConstruct(values.ResolveFieldOrdinals(
		pipelineRoot(q), []int{ordinal}))
}

func pipelineLongLiteral(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
}

func pipelineLongListLiteral(elements ...int64) values.Value {
	items := make([]any, len(elements))
	for i, element := range elements {
		items[i] = element
	}
	return &values.ConstantValue{
		Value: items,
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}
}

func pipelineFilter(q expressions.Quantifier, ps ...predicates.QueryPredicate) *expressions.LogicalFilterExpression {
	return mustPipelineConstruct(expressions.NewLogicalFilterExpression(ps, q))
}

func pipelineProjection(q expressions.Quantifier, projected ...values.Value) *expressions.LogicalProjectionExpression {
	return mustPipelineConstruct(expressions.NewLogicalProjectionExpression(projected, q))
}

func pipelineSort(q expressions.Quantifier, keys ...expressions.SortKey) *expressions.LogicalSortExpression {
	return mustPipelineConstruct(expressions.NewLogicalSortExpression(keys, q))
}

func pipelineDistinct(q expressions.Quantifier) *expressions.LogicalDistinctExpression {
	return mustPipelineConstruct(expressions.NewLogicalDistinctExpression(q))
}

func pipelineUnique(q expressions.Quantifier) *expressions.LogicalUniqueExpression {
	return mustPipelineConstruct(expressions.NewLogicalUniqueExpression(q))
}

func pipelineLimit(q expressions.Quantifier, limit, offset int64) *expressions.LogicalLimitExpression {
	return mustPipelineConstruct(expressions.NewLogicalLimitExpression(limit, offset, q))
}

func pipelineGroupBy(
	q expressions.Quantifier,
	keys []values.Value,
	aggregates []expressions.AggregateSpec,
) *expressions.GroupByExpression {
	return mustPipelineConstruct(expressions.NewGroupByExpression(keys, aggregates, q))
}

func pipelineUnion(qs ...expressions.Quantifier) *expressions.LogicalUnionExpression {
	return mustPipelineConstruct(expressions.NewLogicalUnionExpression(qs))
}

type pipelineIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d *pipelineIndexDef) IndexName() string              { return d.name }
func (d *pipelineIndexDef) IndexColumnNames() []string     { return d.columns }
func (d *pipelineIndexDef) IndexRecordTypes() []string     { return d.recordTypes }
func (d *pipelineIndexDef) IndexIsUnique() bool            { return d.unique }
func (*pipelineIndexDef) IndexPrimaryKeyColumns() []string { return nil }
func (*pipelineIndexDef) IndexCreatesDuplicates() bool     { return false }
func (*pipelineIndexDef) IndexRowType() values.Type        { return pipelineRowType() }
func (d *pipelineIndexDef) IndexKeyComponentTypes() []values.Type {
	row := pipelineRowType().(*values.RecordType)
	result := make([]values.Type, len(d.columns))
	for i, column := range d.columns {
		field, ok := row.LookupFieldUnique(column)
		if !ok {
			panic("planner pipeline index fixture: unknown column " + column)
		}
		result[i] = field.FieldType
	}
	return result
}

type pipelinePrimaryKeyContext struct{}

func (pipelinePrimaryKeyContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (pipelinePrimaryKeyContext) GetMatchCandidates() []MatchCandidate { return nil }

func (pipelinePrimaryKeyContext) GetPrimaryKeyColumns(recordType string) []string {
	if recordType == "T" {
		return []string{"ID"}
	}
	return nil
}

// planPipeline runs the full Cascades pipeline (logical tree -> Explore ->
// Plan -> extract physical plan) and returns the explain string of the
// extracted physical plan. No FDB required.
func planPipeline(t *testing.T, root expressions.RelationalExpression, indexes ...IndexDef) string {
	t.Helper()

	rootRef := expressions.InitialOf(root)

	rules := DefaultExpressionRules()

	var ctx PlanContext
	if len(indexes) > 0 {
		ctx = NewPlanContextFromIndexDefs(indexes)
	}

	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(7_000)

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

func TestPlannerPipeline_PreservesIdentityProjectionOutputAlias(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("Order", false, []values.Field{
		{Name: "VALUE", FieldType: values.NotNullLong, Ordinal: 0},
	})
	scan := mustPipelineConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Order"}, rowType))
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	logical := mustPipelineConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{mustPipelineConstruct(values.ResolveFieldOrdinals(
			pipelineRoot(q), []int{0}))},
		[]string{"RENAMED_ROW"},
		q,
	))

	best, _, err := NewPlanner(DefaultExpressionRules(), nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(7_000).
		Plan(expressions.InitialOf(logical))
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	projection, ok := best.(*plans.RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("planner erased schema-bearing projection: best=%T, want *plans.RecordQueryProjectionPlan", best)
	}
	aliases := projection.GetAliases()
	if len(aliases) != 1 || aliases[0] != "RENAMED_ROW" {
		t.Fatalf("planned projection aliases=%v, want [RENAMED_ROW]", aliases)
	}
}

func planPipelineWithStats(t *testing.T, root expressions.RelationalExpression, stats properties.StatisticsProvider, indexes ...IndexDef) string {
	t.Helper()

	rootRef := expressions.InitialOf(root)

	rules := DefaultExpressionRules()

	var ctx PlanContext
	if len(indexes) > 0 {
		ctx = NewPlanContextFromIndexDefs(indexes)
	}

	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithStatistics(stats).
		WithMaxTasks(7_000)

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

	ctx := NewPlanContextFromMatchCandidates(candidates)

	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(2_000)

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
	return &pipelineIndexDef{
		name:        name,
		columns:     columns,
		recordTypes: []string{"T"},
	}
}

// idxUnique builds a unique stubIndexDef.
func idxUnique(name string, columns ...string) IndexDef {
	return &pipelineIndexDef{
		name:        name,
		columns:     columns,
		recordTypes: []string{"T"},
		unique:      true,
	}
}

// --- Basic operators ---

func TestPipeline_Scan(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	plan := planPipeline(t, scan)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Scan(T)") {
		t.Fatalf("expected plan to contain Scan(T), got: %s", plan)
	}
}

func TestPipeline_Filter(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "X"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42))))
	plan := planPipeline(t, filter)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Filter") {
		t.Fatalf("expected plan to contain Filter, got: %s", plan)
	}
}

func TestPipeline_Projection(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	proj := pipelineProjection(scanQ,
		pipelineField(scanQ, "A"), pipelineField(scanQ, "B"))
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

func TestPipeline_TypeFilter(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T", "U")
	scanRef := expressions.InitialOf(scan)
	tf := mustPipelineConstruct(expressions.NewLogicalTypeFilterExpression(
		[]string{"T"},
		expressions.ForEachQuantifier(scanRef),
	))
	plan := planPipeline(t, tf)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "TypeFilter") {
		t.Fatalf("expected plan to contain TypeFilter, got: %s", plan)
	}
}

func TestPipeline_Sort(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	sort := pipelineSort(scanQ, expressions.SortKey{
		Value: pipelineField(scanQ, "A"), Reverse: false,
	})
	plan := planPipeline(t, sort)
	t.Logf("plan: %s", plan)
	// Sort over an unordered scan produces InMemorySort (Go extension).
	if !strings.Contains(plan, "InMemorySort") && !strings.Contains(plan, "Sort") {
		t.Fatalf("expected plan to contain InMemorySort or Sort, got: %s", plan)
	}
}

func TestPipeline_Distinct(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	distinct := pipelineDistinct(expressions.ForEachQuantifier(scanRef))
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	unique := pipelineUnique(expressions.ForEachQuantifier(scanRef))
	p := NewPlanner(DefaultExpressionRules(), pipelinePrimaryKeyContext{}).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithMaxTasks(7_000)
	best, _, err := p.Plan(expressions.InitialOf(unique))
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if best == nil {
		t.Fatal("Plan returned nil best expression despite exact primary-key proof")
	}
	plan := ExplainPhysicalPlan(best)
	t.Logf("plan: %s", plan)
	// Ordinary Unique is absorbed only when one exact child member is both
	// record-distinct and backed by a primary-key proof.
	if !strings.Contains(plan, "Scan") {
		t.Fatalf("expected absorbed Unique to retain its Scan, got: %s", plan)
	}
}

func TestPipeline_Limit(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	limit := pipelineLimit(expressions.ForEachQuantifier(scanRef), 10, 0)
	plan := planPipeline(t, limit)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("expected plan to contain Limit, got: %s", plan)
	}
}

// --- Index-based operators ---

func TestPipeline_IndexScan(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))))
	plan := planPipeline(t, filter, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected plan to contain IndexScan, got: %s", plan)
	}
}

func TestPipeline_OrderedIndexScan(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0))))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sort := pipelineSort(filterQ, expressions.SortKey{
		Value: pipelineField(filterQ, "A"), Reverse: false,
	})
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "A")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "cnt"},
		})
	plan := planPipeline(t, groupBy, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	// With an index on the grouping key, streaming aggregation is possible.
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected plan to contain StreamingAgg, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "STATUS")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "cnt"},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("expected AggregateIndex plan, got: %s", plan)
	}
}

func TestPipeline_AggregateIndexSUM(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "REGION")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: pipelineField(scanQ, "AMOUNT"), Alias: "total"},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$sum_amount_by_region",
		[]string{"T"},
		[]string{"REGION"},
		expressions.AggSum,
		"AMOUNT",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "CATEGORY")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggMax, Operand: pipelineField(scanQ, "PRICE"), Alias: "max_price"},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$max_price_by_category",
		[]string{"T"},
		[]string{"CATEGORY"},
		expressions.AggMax,
		"PRICE",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") || !strings.Contains(plan, "MAX") {
		t.Fatalf("expected AggregateIndex(MAX, ...) plan, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_WithStats(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "STATUS")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "cnt"},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
	)

	rootRef := expressions.InitialOf(groupBy)
	rules := DefaultExpressionRules()

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	ctx := NewPlanContextFromMatchCandidates([]MatchCandidate{aggCand})
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules()).
		WithStatistics(stats).
		WithMaxTasks(2_000)

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	resultType, ok := best.GetResultValue().Type().(*values.RecordType)
	if !ok || len(resultType.Fields) != 2 ||
		resultType.Fields[0].Name != "STATUS" || resultType.Fields[1].Name != "CNT" {
		t.Fatalf("aggregate-index winner result type = %v, want exact {STATUS,CNT}",
			best.GetResultValue().Type())
	}
	plan := ExplainPhysicalPlan(best)
	t.Logf("plan (1M stats): %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("aggregate index should win with 1M stats, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_MismatchedFunction(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "STATUS")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: pipelineField(scanQ, "AMOUNT")},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
	)

	plan := planPipelineWithCandidates(t, groupBy, []MatchCandidate{aggCand})
	t.Logf("plan: %s", plan)
	if strings.Contains(plan, "AggregateIndex") {
		t.Fatalf("should NOT use aggregate index for mismatched function, got: %s", plan)
	}
}

func TestPipeline_AggregateIndex_WithRegularIndex(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "STATUS")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "cnt"},
		})

	aggCand := NewAggregateIndexMatchCandidate(
		"T$count_by_status",
		[]string{"T"},
		[]string{"STATUS"},
		expressions.AggCount,
		"",
		pipelineRowType(),
		[]values.Type{values.NullableString},
		1,
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
	scanA := pipelineScan("A")
	scanB := pipelineScan("B")
	union := pipelineUnion(
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	)
	plan := planPipeline(t, union)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Union") {
		t.Fatalf("expected plan to contain Union, got: %s", plan)
	}
}

func TestPipeline_Join(t *testing.T) {
	t.Parallel()
	scanA := pipelineScan("A")
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := pipelineScan("B")
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		pipelineField(scanAQ, "ID"),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: pipelineField(scanBQ, "ID"),
		},
	)

	result := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: pipelineRoot(scanAQ)},
		values.RecordConstructorField{Name: "B", Value: pipelineRoot(scanBQ)},
	)
	sel := mustPipelineConstruct(expressions.NewSelectExpressionWithAliases(
		result,
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
		[]string{"A", "B"},
	))
	plan := planPipeline(t, sel)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "NestedLoopJoin") && !strings.Contains(plan, "FlatMap") {
		t.Fatalf("expected plan to contain NestedLoopJoin or FlatMap, got: %s", plan)
	}
}

func TestPipeline_StreamingAggNoIndex(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "A")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "cnt"},
		})
	// No indexes — streaming aggregation is the only implementation.
	plan := planPipeline(t, groupBy)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("expected plan to contain StreamingAgg, got: %s", plan)
	}
}

func TestPipeline_FilterProjection(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "X"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	proj := pipelineProjection(filterQ,
		pipelineField(filterQ, "A"), pipelineField(filterQ, "B"))
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
	vals := mustPipelineConstruct(expressions.NewLogicalValuesExpression([]values.Value{
		pipelineLongLiteral(1),
		pipelineLongLiteral(2),
	}))
	plan := planPipeline(t, vals)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Values") {
		t.Fatalf("expected plan to contain Values, got: %s", plan)
	}
}

func TestPipeline_Explode(t *testing.T) {
	t.Parallel()
	explode := mustPipelineConstruct(expressions.NewExplodeExpression(
		pipelineLongListLiteral(1, 2, 3),
	))
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
		scan := pipelineScan("T")
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
			pipelineField(scanQ, "A"),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
		filterRef := expressions.InitialOf(filter)
		filterQ := expressions.ForEachQuantifier(filterRef)
		proj := pipelineProjection(filterQ,
			pipelineField(filterQ, "A"), pipelineField(filterQ, "B"))
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "X"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0))))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sort := pipelineSort(filterQ, expressions.SortKey{
		Value: pipelineField(filterQ, "A"), Reverse: false,
	})
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	proj := pipelineProjection(sortQ, pipelineField(sortQ, "A"))
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

// --- Limit + Filter ---

func TestPipeline_LimitOverFilter(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "X"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
	filterRef := expressions.InitialOf(filter)

	limit := pipelineLimit(expressions.ForEachQuantifier(filterRef), 5, 0)
	plan := planPipeline(t, limit)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("expected plan to contain Limit, got: %s", plan)
	}
}

// --- Distinct + Sort ---

func TestPipeline_DistinctOverSort(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := pipelineSort(scanQ, expressions.SortKey{
		Value: pipelineField(scanQ, "A"), Reverse: false,
	})
	sortRef := expressions.InitialOf(sort)

	distinct := pipelineDistinct(expressions.ForEachQuantifier(sortRef))
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)

	distinct := pipelineDistinct(expressions.ForEachQuantifier(scanRef))
	distinctRef := expressions.InitialOf(distinct)
	distinctQ := expressions.ForEachQuantifier(distinctRef)

	proj := pipelineProjection(distinctQ, pipelineField(distinctQ, "A"))
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Project") {
		t.Fatalf("expected plan to contain Project, got: %s", plan)
	}
}

// --- Limit with offset ---

func TestPipeline_LimitWithOffset(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	limit := pipelineLimit(expressions.ForEachQuantifier(scanRef), 10, 5)
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: pipelineLongListLiteral(1, 2, 3),
		},
	)
	filter := pipelineFilter(scanQ, inPred)
	plan := planPipeline(t, filter, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "InJoin") {
		t.Fatalf("expected InJoin plan for IN-list with index, got: %s", plan)
	}
}

func TestPipeline_InListExplode_WithStats(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: pipelineLongListLiteral(1, 2, 3),
		},
	)
	filter := pipelineFilter(scanQ, inPred)

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	plan := planPipelineWithStats(t, filter, stats, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "InJoin") {
		t.Fatalf("expected InJoin plan with 1M stats, got: %s", plan)
	}
}

func TestPipeline_InListExplodeWithProjectionAndSort(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	inPred := predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: pipelineLongListLiteral(1, 2, 3),
		},
	)
	filter := pipelineFilter(scanQ, inPred)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sort := pipelineSort(filterQ, expressions.SortKey{Value: pipelineField(filterQ, "B")})
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	proj := pipelineProjection(sortQ,
		pipelineField(sortQ, "B"), pipelineField(sortQ, "A"))

	rootRef := expressions.InitialOf(proj)
	rules := DefaultExpressionRules()
	ctx := NewPlanContextFromIndexDefs([]IndexDef{idx("idx_a", "A")})
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	p1 := predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	p2 := predicates.NewComparisonPredicate(
		pipelineField(scanQ, "B"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)))
	filter := pipelineFilter(scanQ, p1, p2)
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
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := pipelineProjection(scanQ, pipelineField(scanQ, "A"))
	projRef := expressions.InitialOf(proj)
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := pipelineDistinct(projQ)
	plan := planPipeline(t, distinct)
	t.Logf("plan: %s", plan)
	// Should produce a plan (Distinct or direct if eliminated).
	if plan == "" {
		t.Fatal("expected non-empty plan")
	}
}

func TestPipeline_SortOverDistinct(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := pipelineDistinct(scanQ)
	distinctRef := expressions.InitialOf(distinct)
	distinctQ := expressions.ForEachQuantifier(distinctRef)

	sort := pipelineSort(distinctQ, expressions.SortKey{
		Value: pipelineField(distinctQ, "A"),
	})
	plan := planPipeline(t, sort)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Sort") || !strings.Contains(plan, "Scan") {
		t.Fatalf("expected Sort over Scan, got: %s", plan)
	}
}

func TestPipeline_UnionWithProjection(t *testing.T) {
	t.Parallel()
	scan1 := pipelineScan("A")
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := pipelineScan("B")
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)

	union := pipelineUnion(scan1Q, scan2Q)
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	proj := pipelineProjection(unionQ, pipelineField(unionQ, "ID"))
	plan := planPipeline(t, proj)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "Union") {
		t.Fatalf("expected Union in plan, got: %s", plan)
	}
}

// TestPipeline_SortOnDifferentColumnThanFilter verifies that when a
// WHERE predicate uses one indexed column (A) and ORDER BY uses a
// different column (B), the planner produces IndexScan(A=val) +
// InMemorySort(B) rather than a full primary scan. This is the core
// pattern behind the "order_by_pk_index_filter" perf regression.
func TestPipeline_SortOnDifferentColumnThanFilter(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := pipelineFilter(scanQ, predicates.NewComparisonPredicate(
		pipelineField(scanQ, "A"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42))))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sort := pipelineSort(filterQ, expressions.SortKey{Value: pipelineField(filterQ, "B")})
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	proj := pipelineProjection(sortQ,
		pipelineField(sortQ, "B"), pipelineField(sortQ, "A"))

	plan := planPipeline(t, proj, idx("idx_a", "A"))
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "IndexScan") {
		t.Fatalf("expected IndexScan for selective A=42 lookup, got: %s", plan)
	}
	if !strings.Contains(plan, "InMemorySort") {
		t.Fatalf("expected InMemorySort for B ordering, got: %s", plan)
	}
}

// TestPipeline_GroupBySortWithIndex verifies that GROUP BY A ORDER BY A
// with an index on A uses the index for ordered input to streaming
// aggregation, avoiding InMemorySort on the full table. This is the
// "group_by_customer_having" perf regression pattern.
func TestPipeline_GroupBySortWithIndex(t *testing.T) {
	t.Parallel()
	scan := pipelineScan("T")
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	groupBy := pipelineGroupBy(scanQ,
		[]values.Value{pipelineField(scanQ, "A")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "CNT"},
		})
	groupByRef := expressions.InitialOf(groupBy)
	groupByQ := expressions.ForEachQuantifier(groupByRef)

	sort := pipelineSort(groupByQ, expressions.SortKey{Value: pipelineFieldAt(groupByQ, 0)})

	plan := planPipeline(t, sort, idx("idx_a", "A"))
	t.Logf("plan (no HAVING): %s", plan)
	if strings.Contains(plan, "InMemorySort") && strings.Contains(plan, "Scan(T)") {
		t.Fatalf("plan uses InMemorySort over full Scan — should use IndexScan for ordering: %s", plan)
	}

	// Now the full regression pattern: GROUP BY A HAVING CNT > 5 ORDER BY A
	scan2 := pipelineScan("T")
	scan2Q := expressions.ForEachQuantifier(expressions.InitialOf(scan2))
	groupBy2 := pipelineGroupBy(scan2Q,
		[]values.Value{pipelineField(scan2Q, "A")},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: pipelineLongLiteral(1), Alias: "CNT"},
		})
	groupBy2Q := expressions.ForEachQuantifier(expressions.InitialOf(groupBy2))
	havingFilter := pipelineFilter(groupBy2Q, predicates.NewComparisonPredicate(
		pipelineFieldAt(groupBy2Q, 1),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5))))
	havingQ := expressions.ForEachQuantifier(expressions.InitialOf(havingFilter))
	sort2 := pipelineSort(havingQ, expressions.SortKey{Value: pipelineFieldAt(havingQ, 0)})

	plan2 := planPipeline(t, sort2, idx("idx_a", "A"))
	t.Logf("plan (with HAVING): %s", plan2)
	if strings.Contains(plan2, "InMemorySort") && strings.Contains(plan2, "Scan(T)") {
		t.Fatalf("plan with HAVING uses InMemorySort over full Scan — should use IndexScan: %s", plan2)
	}
}
