package plangen_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
)

// TestEndToEnd_CostExtractionPicksPushedFilter is the canonical Track
// B4 integration test: verify that the C1 → B5 → B4 pipeline
// (Convert + FixpointApply + Reference.GetBest) selects the cheaper
// rule-generated alternative.
//
// Setup: Filter(P, Sort(Scan)). PushFilterThroughSortRule converts to
// Sort(Filter(P, Scan)). The cost model's calibration target — Sort
// over fewer rows beats Sort over the full row set — is unit-tested
// in properties/cost_test.go; this test pins that the same ordering
// holds when the alternatives reach the Reference via the actual
// rule engine, not synthetic construction.
//
// The predicate is a non-foldable ValuePredicate so the
// simplification rules (FilterDropTruePredicates / NoOpFilter) don't
// further collapse the tree before we extract.
func TestEndToEnd_CostExtractionPicksPushedFilter(t *testing.T) {
	t.Parallel()
	// Use a non-foldable ValuePredicate so simplification rules don't
	// collapse the tree before extraction can compare alternatives.
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

	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 32); !converged {
		t.Fatal("FixpointApply did not converge in 32 iters")
	}

	if got := len(ref.Members()); got < 2 {
		t.Fatalf("Reference has %d members; expected ≥2 after PushFilterThroughSort", got)
	}

	best := ref.GetBest(properties.CostLess)
	if best == nil {
		t.Fatal("GetBest returned nil")
	}

	// The cheapest member should be a Sort wrapping a Filter — the
	// pushed shape. Anything else means the cost model picked the
	// pulled (Filter-over-Sort) shape, contradicting the calibration
	// target.
	sort, ok := best.(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("GetBest returned %T, want *LogicalSortExpression (the pushed shape)", best)
	}
	innerExpr := sort.GetInner().GetRangesOver().Get()
	if _, isFilter := innerExpr.(*expressions.LogicalFilterExpression); !isFilter {
		t.Fatalf("Sort's inner = %T, want *LogicalFilterExpression — cost model didn't pick Sort(Filter(...))", innerExpr)
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
	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 64); !converged {
		t.Fatal("FixpointApply did not converge in 64 iters")
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
		t.Fatalf("GetBest returned a LogicalFilterExpression (the un-rewritten shape) — rule chain or cost model failed to prefer Sort/Scan")
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
	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 32); !converged {
		t.Fatal("FixpointApply did not converge")
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
