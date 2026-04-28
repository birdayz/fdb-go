package plangen_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
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
	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 16); !converged {
		t.Fatal("FixpointApply didn't converge in 16 iters")
	}
	// The Reference should contain the bare-Scan member after the rules
	// fire. Don't enforce a particular shape on .Get() (Reference's
	// best-member contract is decided in B3+).
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
	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 32); !converged {
		t.Fatal("FixpointApply didn't converge in 32 iters")
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
	if _, converged := cascades.FixpointApply(cascades.DefaultExpressionRules(), ref, 32); !converged {
		t.Fatal("FixpointApply didn't converge in 32 iters")
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
