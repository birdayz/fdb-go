package plangen_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/plangen"
)

// findBareScan reports whether ref holds a bare FullUnorderedScan
// member.
func findBareScan(ref *expressions.Reference) bool {
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			return true
		}
	}
	return false
}

// TestEndToEnd_ConvertThenOptimise verifies the Convert → rule-engine
// boundary: a redundant LogicalFilter(TRUE) wrapping a LogicalScan
// converts to LogicalFilterExpression([TRUE], FullUnorderedScan),
// which FilterDropTruePredicatesRule + NoOpFilterRule then collapse
// to the bare FullUnorderedScan. Pins that the converter's output is
// bindable by the rules' matchers. (The same composition under the
// production task-stack driver is pinned by the cascades package's
// rule tests.)
func TestEndToEnd_ConvertThenOptimise(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		pT, "TRUE",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)
	// FilterDropTrue yields Filter([], Scan); NoOpFilter then yields
	// the bare Scan.
	cascades.FireExpressionRule(cascades.NewFilterDropTruePredicatesRule(), ref)
	cascades.FireExpressionRule(cascades.NewNoOpFilterRule(), ref)
	if !findBareScan(ref) {
		t.Fatalf("rule chain did not yield a bare FullUnorderedScan after Filter([TRUE]) — got %d members", len(ref.Members()))
	}
}

// TestEndToEnd_NestedFilterCollapses — Filter(TRUE, Filter(TRUE, Scan))
// should collapse to Scan via FilterMergeRule + FilterDropTrue +
// NoOpFilter. Multi-rule cooperation on Convert output.
func TestEndToEnd_NestedFilterCollapses(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	inner := logical.NewFilterWithPredicate(
		logical.NewScan("Order", ""),
		pT, "TRUE",
	)
	outer := logical.NewFilterWithPredicate(inner, pT, "TRUE")
	got, err := plangen.Convert(outer)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)
	// FilterMerge collapses the adjacent filters into Filter([T,T]);
	// DropTrue empties the predicate list; NoOpFilter elides it.
	cascades.FireExpressionRule(cascades.NewFilterMergeRule(), ref)
	cascades.FireExpressionRule(cascades.NewFilterDropTruePredicatesRule(), ref)
	cascades.FireExpressionRule(cascades.NewNoOpFilterRule(), ref)
	if !findBareScan(ref) {
		t.Fatal("nested Filter([TRUE]) did not collapse to bare Scan after rule chain")
	}
}

// TestEndToEnd_StackedProjectionsCollapse — Project([id]) over
// Project([id, name]) over Scan collapses to Project([id]) over Scan
// via ProjectionMergeRule fired against the Convert output. Pins that
// the converter's projection shape is bindable by the rule's matcher
// (the multi-step composition itself is pinned in the cascades
// package's rule_projection_merge tests).
func TestEndToEnd_StackedProjectionsCollapse(t *testing.T) {
	t.Parallel()
	src := logical.NewProject(
		logical.NewProject(
			logical.NewScan("Order", ""),
			[]string{"id", "name"},
			[]string{"", ""},
		),
		[]string{"id"},
		[]string{""},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)
	if yielded := cascades.FireExpressionRule(cascades.NewProjectionMergeRule(), ref); len(yielded) == 0 {
		t.Fatal("ProjectionMergeRule did not fire on Convert output")
	}
	// Look for a 1-deep Projection (over Scan) in the members.
	foundFlat := false
	for _, m := range ref.Members() {
		p, ok := m.(*expressions.LogicalProjectionExpression)
		if !ok {
			continue
		}
		if _, scanOK := p.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); scanOK {
			foundFlat = true
			break
		}
	}
	if !foundFlat {
		t.Fatal("ProjectionMergeRule did not collapse stacked projections to 1-deep over Scan")
	}
}

// TestEndToEnd_PushFilterThroughChain exercises the Push-Filter-
// Through-X family across a Quantifier boundary. Starting from
//
//	Filter(TRUE, Sort([id], Filter(TRUE, Scan)))
//
// the exploration driven by the production planner applies:
//  1. PushFilterThroughSort on the outer Filter → yields a Sort-
//     rooted alternative whose inner is Filter(TRUE, Filter(TRUE,
//     Scan))
//  2. FilterMerge on the inner adjacent filters → Filter([T,T])
//  3. FilterDropTrue → Filter([])
//  4. NoOpFilter → eliminates the trivial filter, yielding Scan
//
// Pin that a bare-Scan member appears in a NON-LEAF Reference (one
// that started with only Filter/Sort members) — proving the chain
// composed through the sub-Reference descent on Convert output. The
// leaf scan Reference itself is excluded: it trivially contains a
// Scan.
func TestEndToEnd_PushFilterThroughChain(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(
		logical.NewSort(
			logical.NewFilterWithPredicate(
				logical.NewScan("Order", ""),
				pT, "TRUE",
			),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		pT, "TRUE",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ref := expressions.InitialOf(got)

	// Snapshot the References that hold non-Scan members BEFORE
	// exploration; the rule chain must land a Scan inside one of them.
	nonLeafBefore := map[*expressions.Reference]bool{}
	var snapshot func(r *expressions.Reference)
	snapshot = func(r *expressions.Reference) {
		if r == nil || nonLeafBefore[r] {
			return
		}
		hasNonScan := false
		for _, m := range r.Members() {
			if _, ok := m.(*expressions.FullUnorderedScanExpression); !ok {
				hasNonScan = true
			}
			for _, q := range m.GetQuantifiers() {
				snapshot(q.GetRangesOver())
			}
		}
		if hasNonScan {
			nonLeafBefore[r] = true
		}
	}
	snapshot(ref)

	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundBareScan := false
	for r := range nonLeafBefore {
		for _, m := range r.AllMembers() {
			if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
				foundBareScan = true
			}
		}
	}
	if !foundBareScan {
		t.Fatal("rule chain did not produce a bare Scan member in any non-leaf Reference — push/merge/drop/noop composition broke")
	}
}

// FuzzConvertAndPlan drives the Convert → Planner pipeline end-to-end
// on random LogicalOperator trees. Pins three properties:
//
//  1. Convert may return ErrUnsupported but MUST NOT panic.
//  2. When Convert succeeds, the resulting RelationalExpression is a
//     valid Plan() input — the planner must not panic on any shape.
//  3. The planner terminates by convergence, never by the MaxTasks
//     cap: ErrPlannerCapHit on Convert output means a rule interaction
//     is non-terminating (the class of bug that hit
//     DistinctOverUnionDedupRule).
func FuzzConvertAndPlan(f *testing.F) {
	f.Add(uint64(0), "Order", "", uint8(0))
	f.Add(uint64(1), "T", "x", uint8(1))
	f.Add(uint64(2), "A", "B", uint8(2))
	f.Add(uint64(3), "x", "y", uint8(3))
	f.Add(uint64(0xff), "", "", uint8(255))
	f.Add(uint64(42), "42", "'hello'", uint8(10))
	f.Add(uint64(7), "UPPER(x)", "1 + 2", uint8(4))
	rules := cascades.DefaultExpressionRules()
	planningRules := append(cascades.BatchAExpressionRules(), cascades.DMLImplementationRules()...)
	f.Fuzz(func(t *testing.T, seed uint64, name1, name2 string, shape uint8) {
		op := buildFuzzOp(seed, name1, name2, shape)
		if op == nil {
			return
		}
		got, err := plangen.Convert(op)
		if err != nil {
			return
		}
		ref := expressions.InitialOf(got)
		p := cascades.NewPlanner(rules, cascades.EmptyPlanContext()).
			WithPlanningExpressionRules(planningRules)
		if _, _, err := p.Plan(ref); err == cascades.ErrPlannerCapHit {
			t.Fatalf("Plan hit the MaxTasks cap on Convert output of seed=%d shape=%d (op=%T) — non-terminating rule interaction", seed, shape, op)
		}
	})
}
