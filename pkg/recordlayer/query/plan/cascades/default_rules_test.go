package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestDefaultRules_NotEmpty(t *testing.T) {
	t.Parallel()
	if len(DefaultExpressionRules()) == 0 {
		t.Fatal("DefaultExpressionRules returned empty slice")
	}
}

// TestDefaultRules_EndToEndOptimisation drives a multi-rule rewrite
// through the default rule set on a Filter(TRUE) over Filter(FALSE)
// over Scan. Two distinct predicates so seed Reference dedup
// (EqualsWithoutChildren on predicate-list equality) doesn't merge
// them.
//
// FilterMerge fires once: → Filter([TRUE, FALSE]) over Scan added to
// Reference. (NoOpFilterRule does NOT fire on this merged shape since
// FALSE is not TRUE.)
//
// Pins that the default rule set (a) wires up correctly, (b) at least
// FilterMerge fires through FixpointApply.
//
// NOTE: This test deliberately does NOT chain past the merge — the
// seed Reference dedup is per-Java-class structural-only and doesn't
// distinguish "Filter([T]) over X" from "Filter([T]) over Y" by their
// inner X/Y. Full multi-step optimisation chains need the proper
// Memo (B3 follow-on) where Reference dedup considers child
// references too. Pin only the legitimate seed-level behaviour.
func TestDefaultRules_EndToEndOptimisation(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pInner := predicates.NewConstantPredicate(predicates.TriFalse) // distinct
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pInner}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	pOuter := predicates.NewConstantPredicate(predicates.TriTrue)
	outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pOuter}, innerFQ)
	ref := expressions.InitialOf(outerF)

	progress, converged := FixpointApply(DefaultExpressionRules(), ref, 50)
	if !converged {
		t.Fatalf("did not converge — progress=%d", progress)
	}
	if progress < 1 {
		t.Fatalf("progress=%d, want at least 1 (FilterMergeRule fires)", progress)
	}

	// Find the merged Filter(TRUE, FALSE) member — proves
	// FilterMergeRule went through the default rule set + fixpoint.
	foundMerged := false
	for _, m := range ref.Members() {
		f, ok := m.(*expressions.LogicalFilterExpression)
		if !ok {
			continue
		}
		if len(f.GetPredicates()) == 2 {
			foundMerged = true
			break
		}
	}
	if !foundMerged {
		t.Fatalf("after fixpoint, no merged-2-predicate Filter member — Reference has %d members", len(ref.Members()))
	}
}
