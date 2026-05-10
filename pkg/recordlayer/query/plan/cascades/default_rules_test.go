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

// TestDefaultRules_ExpectedCount pins the rule count as a regression
// guard — accidental removal during a refactor would silently shrink
// the optimiser's reach. CLAUDE.md / TODO.md document the count;
// keep this test in sync with both.
func TestDefaultRules_ExpectedCount(t *testing.T) {
	t.Parallel()
	const expected = 44
	if got := len(DefaultExpressionRules()); got != expected {
		t.Fatalf("DefaultExpressionRules count = %d, want %d (update CLAUDE.md / TODO.md if intentional)", got, expected)
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

// TestDefaultRules_AutoRegistered pins that the package init
// hook (registerDefaultRules) registers every default rule's short
// type name in the registry. Diagnostic / explain output relies on
// LookupRule(name) → rule, so a regression here breaks rule-trace
// logs without a clear failure.
func TestDefaultRules_AutoRegistered(t *testing.T) {
	t.Parallel()
	wantNames := []string{
		"FilterMergeRule",
		"FilterDropTruePredicatesRule",
		"DistinctMergeRule",
		"TypeFilterMergeRule",
		"UnionMergeRule",
		"IntersectionMergeRule",
		"NoOpFilterRule",
		"ProjectionElimRule",
		"UnsortedSortElimRule",
		"UnionSingletonElimRule",
		"IntersectionSingletonElimRule",
	}
	for _, n := range wantNames {
		if got := LookupRule(n); got == nil {
			t.Errorf("LookupRule(%q) = nil after package init — registerDefaultRules didn't include it", n)
		}
	}
}

// TestDefaultRules_StableOrder pins that DefaultExpressionRules
// returns the rules in the same order on every call. The fixpoint
// driver iterates rules in this order, so a rule reordering would
// change which equivalent expressions land first in the Reference's
// member list — fine but worth pinning so nothing accidentally
// shuffles them.
func TestDefaultRules_StableOrder(t *testing.T) {
	t.Parallel()
	first := DefaultExpressionRules()
	for trial := 0; trial < 5; trial++ {
		next := DefaultExpressionRules()
		if len(first) != len(next) {
			t.Fatalf("trial %d: length differs (first=%d, next=%d)", trial, len(first), len(next))
		}
		for i := range first {
			if typeName(first[i]) != typeName(next[i]) {
				t.Fatalf("trial %d: index %d differs (first=%s, next=%s)", trial, i, typeName(first[i]), typeName(next[i]))
			}
		}
	}
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
