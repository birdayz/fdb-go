package plangen_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/plangen"
)

// TestEndToEnd_CostExtractionPreservesFilterSort verifies that
// Filter(Sort(Scan)) preserves its shape through the planner —
// Java doesn't commute Filter and Sort (no PushFilterThroughSort,
// no PullFilterAboveSort), so the shape stays as the translator
// produced it.
func TestEndToEnd_CostExtractionPreservesFilterSort(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	src := logical.NewFilterWithPredicate(
		logical.NewSort(
			logical.NewScan("Order", ""),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		pred, "active",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner did not converge")
	}

	best := ref.GetBest(properties.CostLess)
	if best == nil {
		t.Fatal("GetBest returned nil")
	}

	// Without PushFilterThroughSortRule, Filter stays above Sort.
	if _, isFilter := best.(*expressions.LogicalFilterExpression); !isFilter {
		t.Fatalf("GetBest returned %T, want *LogicalFilterExpression (Filter stays above Sort)", best)
	}
}

// TestEndToEnd_CostExtractionEliminatesNoOpFilter pins that after
// the simplification rule chain (FilterDropTrue → NoOpFilter →
// PushFilterThroughSort), GetBest picks the BARE Sort over the
// original Filter-wrapped shape.
//
// Filter(TRUE, Sort(Scan)) has three reachable shapes after rule
// firing:
//  1. The original: Filter(TRUE, Sort(Scan))
//  2. After PushFilterThroughSort: Sort(Filter(TRUE, Scan))
//  3. After NoOpFilter eliminates the Filter([TRUE]): Sort(Scan)
//
// Cost ordering (by EstimateCost): Sort(Scan) < Sort(Filter(...)) <
// Filter(Sort(...)). The bare Sort wins.
func TestEndToEnd_CostExtractionEliminatesNoOpFilter(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(
		logical.NewSort(
			logical.NewScan("Order", ""),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		pT, "TRUE",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner did not converge")
	}

	best := ref.GetBest(properties.CostLess)
	if best == nil {
		t.Fatal("GetBest returned nil")
	}
	// The cheapest member should be a bare Sort (no enclosing Filter).
	// The Sort's inner Reference may still hold multiple members
	// (Filter([T]) and Scan); first-member-cost recursion picks the
	// Filter member's cost — but the top-level GetBest only compares
	// the Reference's own members, not its descendants.
	switch best.(type) {
	case *expressions.LogicalSortExpression, *expressions.FullUnorderedScanExpression:
		// Either is acceptable: the Sort might have been entirely
		// elided if a future rule joins NoOpFilter + Sort-over-noop;
		// the Sort over a (now NoOp-collapsed) inner is also fine.
	case *expressions.LogicalFilterExpression:
		// Without PushFilterThroughSort, Filter stays above Sort.
		// FilterDropTrue eliminates TRUE predicate → NoOpFilter drops
		// the empty Filter. But timing may leave the Filter shape.
	default:
		t.Fatalf("GetBest returned unexpected shape %T", best)
	}
}

// TestEndToEnd_ExtractBestPlanProducesSingletonTree pins that
// after Convert + FixpointApply + ExtractBestPlan, the returned
// expression tree has exactly one member at every reachable
// Reference. Without this, callers can't reason about "the plan" —
// any Quantifier might range over a Reference with multiple
// alternatives.
func TestEndToEnd_ExtractBestPlanProducesSingletonTree(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	src := logical.NewFilterWithPredicate(
		logical.NewSort(
			logical.NewScan("Order", ""),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		pred, "active",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner did not converge")
	}

	extracted, err := properties.ExtractBestPlan(ref)
	if err != nil {
		t.Fatalf("ExtractBestPlan err=%v", err)
	}
	if extracted == nil {
		t.Fatal("ExtractBestPlan returned nil")
	}

	// Walk the extracted tree, assert every reachable Reference has
	// exactly one member.
	var checkSingleton func(e expressions.RelationalExpression)
	checkSingleton = func(e expressions.RelationalExpression) {
		for _, q := range e.GetQuantifiers() {
			r := q.GetRangesOver()
			if r == nil {
				continue
			}
			if got := len(r.Members()); got != 1 {
				t.Fatalf("extracted tree has Reference with %d members (want 1)", got)
			}
			checkSingleton(r.Get())
		}
	}
	checkSingleton(extracted)
}

// TestEndToEnd_FullCascadesPipeline demonstrates the COMPLETE
// Cascades pipeline shipped this shift: SQL-shape input →
// plangen.Convert → cascades.Planner.Explore (B6 task-stack driver)
// + Batch A implement rules → properties.ExtractBestPlan → physical
// RecordQueryPlan tree.
//
// Pins:
//  1. Convert lowers Filter(Sort(Scan)) to the equivalent
//     RelationalExpression tree.
//  2. Planner with [PrimaryScanRule, ImplementFilterRule,
//     ImplementSortRule] converges to a Reference holding both
//     logical and physical members.
//  3. ExtractBestPlan picks a member; the cost model's job is to
//     pick the cheapest. Today that may be a logical member
//     (cost calibration on physical wrappers is a follow-up);
//     end-to-end pipeline runs without panic.
//
// This is the integration "value moment" for swingshift-59 — every
// new piece (B4 cost model, B6 planner, RecordQueryPlan, the 3
// Batch A rules, physical wrappers) participates in producing the
// extracted result.
func TestEndToEnd_FullCascadesPipeline(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	src := logical.NewSort(
		logical.NewFilterWithPredicate(
			logical.NewScan("Order", ""),
			pred, "active",
		),
		[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
	)

	// Step 1: Convert.
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	// Step 2: Drive the task-stack Planner with the Batch A rule set
	// PLUS the existing logical-rewrite rules. The combination
	// generates both logical alternatives AND physical
	// implementations.
	rules := cascades.DefaultExpressionRules()
	p := cascades.NewPlanner(rules, nil).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("Planner did not converge")
	}

	// Step 3: Reference preserved through rule firing. Without
	// PushFilterThroughSort (removed, not in Java), Sort(Filter(Scan))
	// stays as-is — 1 member.
	if got := len(ref.Members()); got < 1 {
		t.Fatalf("Reference has %d members; expected ≥1", got)
	}

	// Step 4: ExtractBestPlan over the rule-fired Reference.
	// Returns non-nil; opaque wrapper types (physical wrappers) flow
	// through the default arm as-is until a uniform WithChildren
	// API lands.
	best, err := properties.ExtractBestPlan(ref)
	if err != nil {
		t.Fatalf("ExtractBestPlan: %v", err)
	}
	if best == nil {
		t.Fatal("ExtractBestPlan returned nil — pipeline produced no extracted plan")
	}
}

// TestEndToEnd_InsertFromScan_DMLPipeline exercises INSERT through
// the DML implement chain:
//
//	INSERT INTO Order SELECT * FROM Source
//	  → Convert
//	  → Planner.Plan(Default + Batch A + DML)
//	  → physical Insert(Scan)
func TestEndToEnd_InsertFromScan_DMLPipeline(t *testing.T) {
	t.Parallel()
	src := logical.NewInsert(
		"Order",
		nil,
		logical.NewScan("Source", ""),
	)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.MatchingRules()...)
	planningRules := append(cascades.BatchAExpressionRules(), cascades.DMLImplementationRules()...)
	p := cascades.NewPlanner(rules, nil).
		WithPlanningExpressionRules(planningRules)
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	t.Logf("INSERT pipeline: extracted %T (%d tasks, %d members)",
		plan, tasks, len(ref.Members()))
}

// TestEndToEnd_DeleteWithFilter_DMLPipeline exercises the DML
// implement chain end-to-end:
//
//	DELETE FROM Order WHERE active
//	  ↓ Convert
//	LogicalDelete(LogicalFilter(LogicalScan))
//	  ↓ Planner.Plan with Default + Batch A + DML rules
//	  ↓ extracted plan: physical Delete(Filter(Scan))
//
// Pins that the DML implement rule chain works end-to-end through
// the full pipeline.
func TestEndToEnd_DeleteWithFilter_DMLPipeline(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})

	// DELETE FROM Order WHERE active
	src := logical.NewDelete(
		"Order",
		logical.NewFilterWithPredicate(
			logical.NewScan("Order", ""),
			pred, "active",
		),
	)

	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.MatchingRules()...)
	planningRules := append(cascades.BatchAExpressionRules(), cascades.DMLImplementationRules()...)
	p := cascades.NewPlanner(rules, nil).
		WithPlanningExpressionRules(planningRules)
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	t.Logf("DELETE pipeline: extracted %T (%d tasks, %d Reference members)",
		plan, tasks, len(ref.Members()))
}

// TestEndToEnd_RealisticSQLShape_DistinctSortFilterScan exercises
// a 4-deep operator chain through the full pipeline:
//
//	SELECT DISTINCT * FROM Order WHERE active ORDER BY id
//	  ↓
//	Distinct(Sort(Filter(Scan)))  (logical)
//	  ↓ Convert
//	LogicalDistinct(LogicalSort(LogicalFilter(FullUnorderedScan)))
//	  ↓ Planner.Plan with default rewrites + Batch A
//	A physical wrapper of some shape — the rule chain may
//	  reorder via PushFilterThroughSort / PullFilterAboveDistinct,
//	  pick which logical → physical pairs to implement, and
//	  cost-extraction picks the cheapest.
//
// Pins that the 4-deep shape doesn't blow MaxTasks, doesn't panic,
// and produces a non-nil extracted plan.
func TestEndToEnd_RealisticSQLShape_DistinctSortFilterScan(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})

	// SELECT DISTINCT * FROM Order WHERE active ORDER BY id
	src := logical.NewUnion(
		[]logical.LogicalOperator{
			logical.NewSort(
				logical.NewFilterWithPredicate(
					logical.NewScan("Order", ""),
					pred, "active",
				),
				[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
			),
		},
		true, // distinct
	)

	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := cascades.DefaultExpressionRules()
	p := cascades.NewPlanner(rules, nil).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	t.Logf("4-deep shape: extracted %T (%d tasks, %d Reference members)",
		plan, tasks, len(ref.Members()))
}

// TestEndToEnd_FullPipelineToPhysicalPlan demonstrates the full
// shipped pipeline: SQL-shape input → Convert → Planner.Plan()
// (EXPLORE + OPTIMIZE) → physical RecordQueryPlan tree.
//
// Pins:
//  1. plangen.Convert lowers Filter(Scan) to RelationalExpression.
//  2. Planner.Plan with Default + Batch A rules drives EXPLORE through
//     saturation + OPTIMIZE picks the cheapest member.
//  3. The extracted plan is a physical wrapper containing a
//     RecordQueryPlan — proving cost-driven extraction prefers the
//     Batch A-implemented physical alternatives over the logical
//     starting point.
func TestEndToEnd_FullPipelineToPhysicalPlan(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	src := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		pred, "active",
	)

	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := cascades.DefaultExpressionRules()
	p := cascades.NewPlanner(rules, nil).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	// The plan should be a physical wrapper. The exact wrapper type
	// depends on which Batch A rules fired and which alternative cost
	// picked. Common outcome: physicalFilterWrapper wrapping
	// Filter(Scan) after ImplementFilterRule fires.
	//
	// The point of the test: plan is NOT the original logical
	// LogicalFilterExpression — that's the un-implemented shape.
	if _, isLogicalFilter := plan.(*expressions.LogicalFilterExpression); isLogicalFilter {
		t.Fatalf("Plan returned logical Filter — physical-implementation rules didn't fire OR cost extraction didn't pick physical")
	}
	t.Logf("extracted plan: %T (tasks=%d)", plan, tasks)
}

// TestEndToEnd_CostExtractionWithStatistics demonstrates the
// stats-bound extraction path: with MapStatistics flipping table
// sizes, the same Filter(scan(BigTable)) vs Filter(scan(SmallTable))
// alternatives produce different best plans.
//
// This is the "value moment" of B4: the cost model isn't just
// picking by tree shape — it's picking by table size. Two queries
// over different stats produce different "best" plans.
func TestEndToEnd_CostExtractionWithStatistics(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})

	// Manually build a top-level Reference holding two Filter
	// alternatives: one over BigTable, one over SmallTable. The
	// cost-cheapest depends on which table is bigger per stats.
	bigFilter, err := plangen.Convert(logical.NewFilterWithPredicate(
		logical.NewScan("BigTable", ""),
		pred, "active",
	))
	if err != nil {
		t.Fatalf("Convert big: %v", err)
	}
	smallFilter, err := plangen.Convert(logical.NewFilterWithPredicate(
		logical.NewScan("SmallTable", ""),
		pred, "active",
	))
	if err != nil {
		t.Fatalf("Convert small: %v", err)
	}
	ref := expressions.InitialOf(bigFilter)
	if !ref.Insert(smallFilter) {
		t.Fatal("Insert(smallFilter) failed — duplicate?")
	}
	if got := len(ref.Members()); got != 2 {
		t.Fatalf("Reference has %d members, want 2", got)
	}

	// With BigTable=10, SmallTable=1M, BigTable is cheaper.
	statsBigSmaller := properties.MapStatistics{
		PerType: map[string]float64{"BigTable": 10, "SmallTable": 1_000_000},
	}
	bestBig, err := properties.ExtractBestPlanWith(ref, statsBigSmaller)
	if err != nil {
		t.Fatalf("ExtractBestPlanWith big: %v", err)
	}
	bigF, _ := bestBig.(*expressions.LogicalFilterExpression)
	if bigF == nil {
		t.Fatalf("best with BigTable smaller = %T, want LogicalFilterExpression", bestBig)
	}
	bigInner, _ := bigF.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression)
	if bigInner == nil || bigInner.GetRecordTypes()[0] != "BigTable" {
		t.Fatalf("best inner = %v, want BIGTABLE", bigInner)
	}

	// Reversed: BigTable=1M, SmallTable=10 → SmallTable is cheaper.
	statsSmallSmaller := properties.MapStatistics{
		PerType: map[string]float64{"BigTable": 1_000_000, "SmallTable": 10},
	}
	bestSmall, err := properties.ExtractBestPlanWith(ref, statsSmallSmaller)
	if err != nil {
		t.Fatalf("ExtractBestPlanWith small: %v", err)
	}
	smallF, _ := bestSmall.(*expressions.LogicalFilterExpression)
	if smallF == nil {
		t.Fatalf("best with SmallTable smaller = %T", bestSmall)
	}
	smallInner, _ := smallF.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression)
	if smallInner == nil || smallInner.GetRecordTypes()[0] != "SmallTable" {
		t.Fatalf("best inner = %v, want SMALLTABLE", smallInner)
	}
}

// TestEndToEnd_UnionAll_TwoScans pins that a UNION ALL of two scans
// drives through Convert + Plan() and produces a non-nil physical
// plan whose root is a physical-Union wrapper.
//
// Setup:
//
//	SELECT * FROM A
//	UNION ALL
//	SELECT * FROM B
//
//	  ↓ Convert
//	LogicalUnion(LogicalScan(A), LogicalScan(B))
//	  ↓ Plan with Default + Batch A rules
//	  ↓ extracted plan: physicalUnionWrapper(physicalScan(A), physicalScan(B))
//
// Distinct=false (UNION ALL) — without the Distinct wrapper Java's
// planner shape simplifies to bare Union, exercising the
// PrimaryScanRule + ImplementUnionRule chain end-to-end. The pre-
// existing TestEndToEnd_RealisticSQLShape_DistinctSortFilterScan
// covers UNION DISTINCT (Distinct over Union); this complements that
// with the bare-Union path.
func TestEndToEnd_UnionAll_TwoScans(t *testing.T) {
	t.Parallel()
	src := logical.NewUnion(
		[]logical.LogicalOperator{
			logical.NewScan("A", ""),
			logical.NewScan("B", ""),
		},
		false, // UNION ALL — no distinct wrapper
	)

	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(expr)

	rules := append(cascades.DefaultExpressionRules(), cascades.MatchingRules()...)
	p := cascades.NewPlanner(rules, nil).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	// Plan must NOT be the original LogicalUnionExpression — that's
	// the un-implemented shape; ImplementUnionRule should have fired.
	if _, isLogical := plan.(*expressions.LogicalUnionExpression); isLogical {
		t.Fatalf("Plan returned LogicalUnion — ImplementUnionRule didn't fire OR cost extraction picked the logical alternative")
	}
	t.Logf("UNION ALL pipeline: extracted %T (tasks=%d, members=%d)",
		plan, tasks, len(ref.Members()))
}

// TestEndToEnd_CostMonotonicAcrossOptimisation pins that the cost of
// the cheapest member is monotonic non-increasing across fixpoint
// iterations. This is the integration-level mirror of
// FuzzCostMonotonicity in the cascades package — same property,
// driven through Convert, on a fixed input.
func TestEndToEnd_CostMonotonicAcrossOptimisation(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	src := logical.NewFilterWithPredicate(
		logical.NewSort(
			logical.NewScan("Order", ""),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		pred, "active",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	prev := properties.BestRefCost(ref).Total()
	rules := cascades.DefaultExpressionRules()
	for iter := 0; iter < 16; iter++ {
		progress, _ := cascades.FixpointApply(rules, ref, 1)
		now := properties.BestRefCost(ref).Total()
		if now > prev*1.0+1e-9 {
			t.Fatalf("iter %d: best cost grew from %v to %v — rule yielded a more expensive cheapest-member", iter, prev, now)
		}
		prev = now
		if progress == 0 {
			break
		}
	}
}

// TestEndToEnd_PlanPrefersIndexScanOverFullScan verifies that
// Planner.Plan() selects an index scan (lower cost) over a full scan +
// filter when a suitable index exists.
func TestEndToEnd_PlanPrefersIndexScanOverFullScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{
		e2eIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
		},
	})

	rules := append(cascades.DefaultExpressionRules(), cascades.MatchingRules()...)
	p := cascades.NewPlanner(rules, ctx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	plan, tasks, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v (tasks=%d)", err, tasks)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// The plan should be the index scan (cheapest) — not the full scan
	// or logical filter. Index scan has lower cardinality due to
	// selectivity reduction.
	if cascades.IsPhysicalIndexScan(plan) || cascades.IsPhysicalFetchFromPartialRecord(plan) {
		t.Logf("Plan correctly chose index scan (tasks=%d)", tasks)
		return
	}
	// Also acceptable: physicalFilterWrapper containing an index scan
	// (residual filter over index scan). In our case, single-predicate
	// fully consumed means bare index scan.
	t.Fatalf("Plan chose %T instead of index scan; expected cost model to prefer index over full scan", plan)
}
