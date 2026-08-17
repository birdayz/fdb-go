package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// ImplementUnorderedUnionRule — matcher
// ---------------------------------------------------------------------------

func TestImplementUnorderedUnionRule_MatchesLogicalUnionExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(scanRef),
	}))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	if len(bindings) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should match LogicalUnionExpression")
	}
}

// TestPhysicalPlanColumnNames_StreamingAggNotUnwrapped pins the RFC-080 fix:
// physicalPlanColumnNames must NOT unwrap a RecordQueryStreamingAggregationPlan through
// GetInner() to its pre-aggregation input column names. Those input names are NOT the
// aggregate's output names, so a union-branch rename Map built from them would read
// columns absent from the aggregate row → NULLs. It returns nil instead, deferring the
// branch's column normalization to the executor's position-remap (which DOES report a
// StreamingAgg's output schema, RFC-078).
func TestPhysicalPlanColumnNames_StreamingAggNotUnwrapped(t *testing.T) {
	t.Parallel()
	// StreamingAgg over a Project whose output column is [P] — the pre-aggregation
	// input name that must NOT leak out as the aggregate branch's column name.
	scan := unionRulePlanScan("A")
	root := mustUnionRuleQOV(scan.GetResultValue())
	projected := mustUnionRuleConstruct(values.ResolveFieldOrdinals(root, []int{1}))
	innerProj := mustUnionRuleConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{projected}, []string{"P"}, scan,
	))
	agg := mustUnionRuleConstruct(plans.NewRecordQueryStreamingAggregationPlan(
		innerProj, nil,
		[]expressions.AggregateSpec{{Function: expressions.AggCount, Alias: "X"}},
	))
	if got := physicalPlanColumnNames(agg); got != nil {
		t.Fatalf("physicalPlanColumnNames(StreamingAgg) must NOT unwrap to inner names; got %v, want nil", got)
	}
}

func TestImplementUnorderedUnionRule_SkipsNonMatching(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	filter := mustUnionRuleConstruct(expressions.NewLogicalFilterExpression(
		nil, expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("ImplementUnorderedUnionRule should NOT match LogicalFilterExpression")
	}
}

func TestImplementUnorderedUnionRule_SkipsLogicalUniqueExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	unique := mustUnionRuleConstruct(expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	if len(bindings) != 0 {
		t.Fatal("ImplementUnorderedUnionRule should NOT match LogicalUniqueExpression")
	}
}

// ---------------------------------------------------------------------------
// ImplementUnorderedUnionRule — OnMatch
// ---------------------------------------------------------------------------

func TestImplementUnorderedUnionRule_CreatesUnorderedUnionPlan(t *testing.T) {
	t.Parallel()
	// Build two inner references, each holding a bare scan plan.
	scanA := unionRulePlanScan("A")
	scanB := unionRulePlanScan("B")
	wA := scanA
	wB := scanB

	refA := expressions.InitialOf(wA)
	pmA := NewPlanPropertiesMap()
	pmA.Add(wA)
	refA.SetPlanProperties(pmA)

	refB := expressions.InitialOf(wB)
	pmB := NewPlanPropertiesMap()
	pmB.Add(wB)
	refB.SetPlanProperties(pmB)

	// Build the logical union over the two refs.
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should yield at least one expression")
	}

	// The yielded expression should be the bare *RecordQueryUnorderedUnionPlan.
	foundPlan := false
	for _, r := range results {
		if uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			foundPlan = true
			inners := uup.GetInners()
			if len(inners) < 2 {
				t.Fatalf("unordered union should have >= 2 inner plans, got %d", len(inners))
			}
		}
	}
	if !foundPlan {
		t.Fatal("expected at least one *RecordQueryUnorderedUnionPlan in results")
	}
}

func TestImplementUnorderedUnionRule_NoYieldForEmptyQuantifiers(t *testing.T) {
	t.Parallel()
	if union, err := expressions.NewLogicalUnionExpression(nil); err == nil || union != nil {
		t.Fatalf("empty logical union = %T, %v; want atomic constructor rejection", union, err)
	}
}

func TestImplementUnorderedUnionRule_NoYieldForSingleChildWithNoPhysicalPlans(t *testing.T) {
	t.Parallel()
	// Single child ref with only logical expressions (no physical wrappers).
	logicalRef := expressions.InitialOf(unionRuleFullScan("T"))
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(logicalRef),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	// The inner reference holds no physical plan, so ToPlanPartitions rolls up
	// to nothing and the rule declines. Assert the DECLINE rather than merely
	// surviving the call: a rule that yielded a union over an unimplemented leg
	// would produce a plan tree with a logical node inside it.
	for _, r := range results {
		if uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			t.Fatalf("rule yielded %T over a leg with no physical plan", uup)
		}
	}
}

// TestImplementUnorderedUnionRule_SingleLegImplementsAsOneLegConcat pins the
// ARITY of the rule against Java's: RecordQueryUnorderedUnionPlan.fromQuantifiers
// imposes no minimum leg count, so a one-leg logical union implements.
//
// A Go-only two-leg floor used to live here, and it was invisible only because
// UnionSingletonElimRule normally rewrites a singleton union away in REWRITING
// — leaving ImplementUnionRule, since deleted, as the sole implementer of the
// shape that survives when that rewrite does not fire. With the floor and that
// rule both gone the union would have had NO implementer, and the planner
// returned no plan AND no error.
func TestImplementUnorderedUnionRule_SingleLegImplementsAsOneLegConcat(t *testing.T) {
	t.Parallel()
	scan := unionRulePlanScan("A")
	ref := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	ref.SetPlanProperties(pm)

	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(ref),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	found := 0
	for _, r := range results {
		uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan)
		if !ok {
			continue
		}
		found++
		if got := len(uup.GetInners()); got != 1 {
			t.Fatalf("one-leg union implemented with %d inners, want 1", got)
		}
	}
	if found != 1 {
		t.Fatalf("one-leg logical union yielded %d unordered union plans, want 1", found)
	}
}

// ---------------------------------------------------------------------------
// crossProductPartitions
// ---------------------------------------------------------------------------

func TestCrossProductPartitions_Empty(t *testing.T) {
	t.Parallel()
	result := crossProductPartitions(nil)
	if result != nil {
		t.Fatalf("crossProductPartitions(nil) = %v, want nil", result)
	}
}

func TestCrossProductPartitions_SingleChildSinglePartition(t *testing.T) {
	t.Parallel()
	p := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{p}})
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if len(result[0]) != 1 {
		t.Fatalf("expected combination of length 1, got %d", len(result[0]))
	}
	if result[0][0] != p {
		t.Fatal("partition mismatch")
	}
}

func TestCrossProductPartitions_TwoChildrenSinglePartitionEach(t *testing.T) {
	t.Parallel()
	pA := NewPlanPartition(nil, nil)
	pB := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{pA}, {pB}})
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if len(result[0]) != 2 {
		t.Fatalf("expected combination of length 2, got %d", len(result[0]))
	}
}

func TestCrossProductPartitions_TwoChildrenTwoPartitionsEach(t *testing.T) {
	t.Parallel()
	pA1 := NewPlanPartition(nil, nil)
	pA2 := NewPlanPartition(nil, nil)
	pB1 := NewPlanPartition(nil, nil)
	pB2 := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{pA1, pA2}, {pB1, pB2}})
	// 2 * 2 = 4 combinations.
	if len(result) != 4 {
		t.Fatalf("expected 4 combinations, got %d", len(result))
	}
	for _, combo := range result {
		if len(combo) != 2 {
			t.Fatalf("each combination should have 2 partitions, got %d", len(combo))
		}
	}
}

func TestCrossProductPartitions_ThreeChildren(t *testing.T) {
	t.Parallel()
	pA := NewPlanPartition(nil, nil)
	pB1 := NewPlanPartition(nil, nil)
	pB2 := NewPlanPartition(nil, nil)
	pC := NewPlanPartition(nil, nil)
	// 1 * 2 * 1 = 2 combinations.
	result := crossProductPartitions([][]*PlanPartition{{pA}, {pB1, pB2}, {pC}})
	if len(result) != 2 {
		t.Fatalf("expected 2 combinations, got %d", len(result))
	}
	for _, combo := range result {
		if len(combo) != 3 {
			t.Fatalf("each combination should have 3 partitions, got %d", len(combo))
		}
	}
}

// TestPhysicalPlanColumnNames_StopsAtProjection pins that this walker takes its
// names from a PROJECTION and does not descend past one.
//
// It exists because RFC-226 rev 5 asserted the opposite — that this walker and
// the executor's planColumnNamesWithMD both "descend past the projection via
// GetInner() and are therefore unaffected", and that a don't-descend arm needed
// adding to both. That was wrong about both walkers: each already has a
// projection arm at the TOP of its loop, so neither ever reaches the GetInner()
// descent for a projection. The claim has since been restated for this walker
// specifically and was wrong again, so the behaviour is pinned here rather than
// argued about a third time.
//
// The pin is also the reason the walkers are UNAFFECTED by a projection stating
// a row: they never read GetResultType() for one. The tail RecordType read below
// the loop is reached only for a non-projection terminal.
//
// Naming authority is the second half. This walker resolves a projected column's
// name with values.OutputColumnName — the same function values.Projection-
// ResultValue uses to name the fields of the row a projection now states. So the
// executor-visible name and the stated row's field name come from ONE authority
// by construction, not by two implementations agreeing.
func TestPhysicalPlanColumnNames_StopsAtProjection(t *testing.T) {
	t.Parallel()

	innerRow := values.NewRecordType("", true, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "A", FieldType: values.NotNullLong},
	})
	scan := mustUnionRuleConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, innerRow, false))
	root := mustUnionRuleQOV(scan.GetResultValue())
	field := mustUnionRuleConstruct(values.ResolveFieldOrdinals(root, []int{1}))
	proj := mustUnionRuleConstruct(plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{field}, []string{"RENAMED"}, scan))

	got := physicalPlanColumnNames(proj)
	if len(got) != 1 || got[0] != "RENAMED" {
		t.Fatalf("physicalPlanColumnNames(Projection) = %v, want [RENAMED].\n"+
			"  A 2-name [ID A] result means the walker descended PAST the projection to "+
			"its inner's row — the union rename would then be built from columns the "+
			"projection does not output. A nil means the projection arm was removed and "+
			"the walk fell through to the tail RecordType read.", got)
	}
}
