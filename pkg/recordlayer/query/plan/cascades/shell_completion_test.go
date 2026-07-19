package cascades

// Bounds and correctness pins for the recursive shell completion in
// findPhysicalPlan.
//
// The FIRST attempt at this shipped a depth cap that never fired: its
// recursion re-entered indirectly through WithChildren → findPhysicalPlan,
// which restarted at depth 0, so the parameter was dead and a cyclic memo
// would have blown the stack. It was deleted rather than patched. The
// current implementation recurses only between completeShellPlan and
// resolveInnerPlan, rebuilding through each plan's WithInner and never
// crossing the WithChildren boundary — which is what makes the bound real.
//
// Nothing pinned that bound, so a second fake could ship the same way.
// These tests pin it directly.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// shellChain builds a chain of `depth` nil-inner PredicatesFilter shells
// over a concrete leaf scan, and returns the reference holding the
// outermost shell. Each level's quantifier ranges over the level below, so
// completion must walk the whole chain.
func shellChain(t *testing.T, depth int) *expressions.Reference {
	t.Helper()
	cur := concreteScanRef(t)
	for i := 0; i < depth; i++ {
		// nil inner: the shell form the extraction relink must complete.
		shellPlan := plans.NewRecordQueryPredicatesFilterPlan(
			nil, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		)
		w := NewPhysicalPredicatesFilterWrapper(shellPlan, expressions.ForEachQuantifier(cur))
		cur = expressions.InitialOf(w)
	}
	return cur
}

// concreteScanRef returns a reference holding one VALID physical scan —
// the leaf every shell chain bottoms out at.
func concreteScanRef(t *testing.T) *expressions.Reference {
	t.Helper()
	ref := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	FireExpressionRule(NewPrimaryScanRule(), ref)
	if findPhysicalExpr(ref) == nil {
		t.Fatal("setup: no physical scan yielded")
	}
	return ref
}

func TestShellCompletion_WithinBoundCompletes(t *testing.T) {
	t.Parallel()
	// A chain shorter than the cap must complete into a fully-linked plan.
	got := findPhysicalPlan(shellChain(t, maxShellCompletionDepth-2))
	if got == nil {
		t.Fatal("a shell chain within the depth bound must complete, got nil")
	}
	if err := ValidatePlanInvariants(got); err != nil {
		t.Fatalf("completed plan is malformed: %v", err)
	}
}

func TestShellCompletion_BoundIsReal(t *testing.T) {
	t.Parallel()
	// Beyond the cap the completion must DECLINE (nil) rather than recurse.
	// If the bound were fake — as the first implementation's was — this
	// either returns a plan or overflows the stack.
	for _, depth := range []int{maxShellCompletionDepth + 5, maxShellCompletionDepth * 8} {
		if got := findPhysicalPlan(shellChain(t, depth)); got != nil {
			t.Errorf("chain of %d shells: expected a decline past the depth bound, got %T",
				depth, got)
		}
	}
}

func TestShellCompletion_PrefersValidMemberOverShell(t *testing.T) {
	t.Parallel()
	// A reference holding BOTH a shell and a valid member must yield the
	// valid one — completion is the fallback, not the default.
	ref := concreteScanRef(t)
	shellPlan := plans.NewRecordQueryPredicatesFilterPlan(
		nil, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	)
	ref.Insert(NewPhysicalPredicatesFilterWrapper(shellPlan, expressions.ForEachQuantifier(ref)))

	got := findPhysicalPlan(ref)
	if got == nil {
		t.Fatal("expected the valid member, got nil")
	}
	if planIsShell(got) {
		t.Fatalf("picked a shell over a valid member: %T", got)
	}
}
