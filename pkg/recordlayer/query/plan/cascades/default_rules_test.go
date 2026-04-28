package cascades

import (
	"fmt"
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

// TestDefaultRules_NoNil verifies every rule in the default set is
// non-nil and has a non-nil Matcher. Caught a bug class where a rule
// constructor accidentally returns nil under some conditions.
func TestDefaultRules_NoNil(t *testing.T) {
	t.Parallel()
	for i, r := range DefaultExpressionRules() {
		if r == nil {
			t.Fatalf("default rule at index %d is nil", i)
		}
		if r.Matcher() == nil {
			t.Fatalf("default rule at index %d (%T) has nil Matcher", i, r)
		}
	}
}

// TestDefaultRules_DistinctTypes verifies no duplicate rule types in
// the default set. Same rule registered twice would double the
// per-iteration work.
func TestDefaultRules_DistinctTypes(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	for i, r := range DefaultExpressionRules() {
		k := typeName(r)
		if prev, ok := seen[k]; ok {
			t.Fatalf("default rule %s appears twice (indices %d and %d)", k, prev, i)
		}
		seen[k] = i
	}
}

// typeName returns the rule's concrete type name (Go's %T format).
// Used for collision detection in DistinctTypes test — works on
// any rule type, including future ones, without per-rule
// maintenance.
func typeName(r ExpressionRule) string {
	if r == nil {
		return "<nil>"
	}
	return reflectTypeName(r)
}

func reflectTypeName(r ExpressionRule) string {
	return fmt.Sprintf("%T", r)
}

// TestDefaultRules_EndToEndOptimisation drives a multi-rule rewrite
// chain through the default rule set:
//
//	Filter(TRUE) over Filter(TRUE) over Distinct over Distinct over Scan
//
// Each rule fires in turn and each yield grows the Reference because
// Reference.Insert's children-aware dedup distinguishes shapes that
// share EqualsWithoutChildren but range over different inner
// References (the dedup contract documented on Reference.Insert).
//
// Expected fires (over 2-3 iterations):
//   - FilterMerge on outer Filter — yields Filter([T,T]) over outerD's Q.
//   - NoOpFilter on outer Filter — yields innerF.
//   - NoOpFilter on the merged Filter([T,T]) — yields outerD.
//   - DistinctMerge on outerD — yields Distinct over scanQ.
//
// Test pins that the optimisation chain produces a Distinct(Scan)
// member somewhere in the resulting Reference.
func TestDefaultRules_EndToEndOptimisation(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerD := expressions.NewLogicalDistinctExpression(scanQ)
	innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
	outerD := expressions.NewLogicalDistinctExpression(innerDQ)
	outerDQ := expressions.ForEachQuantifier(expressions.InitialOf(outerD))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, outerDQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerFQ)
	ref := expressions.InitialOf(outerF)

	progress, converged := FixpointApply(DefaultExpressionRules(), ref, 50)
	if !converged {
		t.Fatalf("did not converge — progress=%d", progress)
	}
	if progress < 4 {
		t.Fatalf("progress=%d, want at least 4 (FilterMerge + 2× NoOpFilter + DistinctMerge)", progress)
	}

	// Find the most-optimised member: Distinct directly over Scan.
	foundShape := false
	for _, m := range ref.Members() {
		d, ok := m.(*expressions.LogicalDistinctExpression)
		if !ok {
			continue
		}
		inner := d.GetInner().GetRangesOver().Get()
		if _, ok := inner.(*expressions.FullUnorderedScanExpression); ok {
			foundShape = true
			break
		}
	}
	if !foundShape {
		t.Fatalf("after fixpoint, Reference has no Distinct(Scan) member — members=%d", len(ref.Members()))
	}
}
