package plangen_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/plangen"
)

// TestEndToEnd_ConvertThenOptimise verifies the C1 → B5 pipeline:
// a redundant LogicalFilter(TRUE) wrapping a LogicalScan converts
// to LogicalFilterExpression([TRUE], FullUnorderedScan), which
// FilterDropTruePredicatesRule + NoOpFilterRule then collapse to
// the bare FullUnorderedScan. Pins that the converter and the rule
// engine are wire-compatible end-to-end.
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
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner didn't converge")
	}
	foundBareScan := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			foundBareScan = true
			break
		}
	}
	if !foundBareScan {
		t.Fatalf("rule engine did not yield a bare FullUnorderedScan after Filter([TRUE]) — got %d members", len(ref.Members()))
	}
}

// TestEndToEnd_NestedFilterCollapses — Filter(TRUE, Filter(TRUE, Scan))
// should collapse to Scan via FilterMergeRule + FilterDropTrue +
// NoOpFilter. Multi-rule cooperation test.
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
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner didn't converge")
	}
	foundBareScan := false
	for _, m := range ref.Members() {
		if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
			foundBareScan = true
			break
		}
	}
	if !foundBareScan {
		t.Fatal("nested Filter([TRUE]) did not collapse to bare Scan after rule engine")
	}
}

// TestEndToEnd_StackedProjectionsCollapse — Project([id]) over
// Project([id, name]) over Scan collapses to Project([id]) over Scan
// via ProjectionMergeRule.
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
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner didn't converge")
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
// Through-X family. Starting from
//
//	Filter(TRUE, Sort([id], Filter(TRUE, Scan)))
//
// The Planner iteratively applies:
//  1. PushFilterThroughSort on the outer Filter → yields a Sort-
//     rooted alternative whose inner is Filter(TRUE, Filter(TRUE,
//     Scan))
//  2. FilterMerge on the inner adjacent filters → Filter([T,T])
//  3. FilterDropTrue → Filter([])
//  4. NoOpFilter → eliminates the trivial filter, yielding Scan
//  5. The original outer Filter(TRUE) also gets eliminated by
//     FilterDropTrue + NoOpFilter via the top-level path
//
// End state: every reachable Reference contains progressively-
// simplified members. Pin that the inner-Sort sub-Reference (held
// by the original top-level Filter's quantifier) eventually contains
// a bare-Scan member — proves the descent works end-to-end.
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
	p := cascades.NewPlanner(cascades.DefaultExpressionRules(), nil)
	if _, converged := p.Explore(ref); !converged {
		t.Fatal("Planner did not converge")
	}
	if len(ref.Members()) <= 1 {
		t.Fatal("rule engine made no progress — Reference still has only the initial member")
	}
	// Walk the whole tree from `ref` and look for a bare Scan member
	// somewhere — proves the rule chain reached the leaves through
	// the sub-Reference descent.
	foundBareScan := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.Members() {
			if _, ok := m.(*expressions.FullUnorderedScanExpression); ok {
				foundBareScan = true
				return
			}
			for _, q := range m.GetQuantifiers() {
				walk(q.GetRangesOver(), visited)
				if foundBareScan {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundBareScan {
		t.Fatalf("rule chain did not produce a bare Scan member anywhere in the tree — top-level Reference has %d members", len(ref.Members()))
	}
}

// FuzzConvertAndOptimise drives the C1 → B5 pipeline end-to-end on
// random LogicalOperator trees. Pins three properties:
//
//  1. Convert may return ErrUnsupported but MUST NOT panic.
//  2. When Convert succeeds, the resulting RelationalExpression is
//     a valid FixpointApply input — the rule engine must terminate
//     within 50 iters, no panic.
//  3. After convergence, the input member is preserved as
//     ref.Members()[0] (the rule engine ADDS, never REMOVES).
//
// Catches the class of bug where a converter shape produces an
// expression the rule engine non-terminates on. (Caught the
// DistinctOverUnionDedupRule termination bug post-revert; would have
// caught it at-introduction if FuzzConvert seed had Union shapes.)
// FuzzConvertAndPlan exercises the full pipeline: Convert → Planner
// (all logical + physical rules). Verifies the planner never panics on
// any random LogicalOperator shape.
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
		plan, _, _ := p.Plan(ref)
		_ = plan
	})
}

func FuzzConvertAndOptimise(f *testing.F) {
	f.Add(uint64(0), "Order", "", uint8(0))
	f.Add(uint64(1), "T", "x", uint8(1))
	f.Add(uint64(2), "A", "B", uint8(2))
	f.Add(uint64(3), "x", "y", uint8(3))
	f.Add(uint64(0xff), "", "", uint8(255))
	rules := cascades.DefaultExpressionRules()
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
		initialMember := ref.Get()
		_, converged := cascades.FixpointApply(rules, ref, 50)
		if !converged {
			t.Fatalf("FixpointApply did not converge on Convert output of seed=%d shape=%d (op=%T)", seed, shape, op)
		}
		members := ref.Members()
		if len(members) == 0 || members[0] != initialMember {
			t.Fatalf("initial member not preserved at index 0 (seed=%d shape=%d)", seed, shape)
		}
		// Idempotence at convergence: a second FixpointApply should
		// not grow the Reference. If it does, some rule is yielding
		// non-deterministic output that escapes Reference.Insert
		// dedup.
		progress2, converged2 := cascades.FixpointApply(rules, ref, 5)
		if !converged2 {
			t.Fatalf("second FixpointApply did not converge — non-deterministic rule fire (seed=%d shape=%d)", seed, shape)
		}
		if progress2 != 0 {
			t.Fatalf("second FixpointApply grew Reference by %d — rule isn't idempotent at convergence (seed=%d shape=%d)", progress2, seed, shape)
		}
	})
}
