package explaindiff_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// TestCorpusPlanReachability pins RFC-183's central invariant across the whole
// yamsql corpus: every child a physical plan executes must be an expression
// the corresponding quantifier's group can actually produce.
//
// This is a ratchet, and it guards a property NOTHING ELSE IN THE SUITE CAN
// SEE. Plan extraction reads the PLAN and never consults the memo, so when the
// two disagree the executed rows stay correct and every row-level test, plan
// snapshot and the corpus explain-differ all stay green — while the optimizer
// costs an expression that will never run. The whole defect class shipped
// green for exactly that reason.
//
// It is a test rather than an always-on runtime check because enforcing it in
// verifyChildrenMemoized would run a deep plans.Equals against every group
// member at every yield: real per-query cost in production planning, for a
// property that is static given the rules.
//
// If this fails, do NOT relax the assertion. A regression means some rule
// builds a plan around a child its quantifier's group cannot produce; the
// report names the plan type and the first structurally differing field. Note
// that rendered EXPLAIN frequently shows the two sides as IDENTICAL — it does
// not print scan-comparison operands — which is why the report carries a
// field-level dump.
//
// Deliberately ONE test over one corpus run, holding both assertions: the "is
// it clean" and "is it actually looking" checks are two readings of the SAME
// measurement, and splitting them would plan the corpus twice to answer one
// question.
//
// The collector is OWNED BY THIS TEST and threaded down through
// GenerateBaselineWithReachability -> planGuarded ->
// embedded.PlanPhysicalForTestWithReachability -> the Planner, so t.Parallel is
// safe and correct here. It was not always: the tally used to be package state
// in cascades, and several sibling tests in this package plan the SAME corpus
// (TestNoPlanPanics, TestShapeAccompaniesEverySuccessfulPlan,
// TestBaselineIsDeterministic, ...). Under t.Parallel their planning
// accumulated into this tally and the full-package run reported edges=53748 /
// no-quantifier=96 — exactly 3x the true 17916/32 — while every standalone
// `-run TestCorpusPlanReachability` invocation passed, because then only one
// test was planning.
//
// That was patched by dropping t.Parallel, which bought exclusivity by
// serialising the whole package against this one test — a workaround that left
// the shared instrument in place. Scoping the collector to a Planner deletes
// the sharing instead of scheduling around it, so the number is now the number
// no matter who else is planning.
func TestCorpusPlanReachability(t *testing.T) {
	t.Parallel()

	reach := cascades.NewReachabilityCollector()

	// Planning the corpus is what populates the tally; the baseline text
	// itself is checked by the explain-differ gate, not here.
	_, st, err := explaindiff.GenerateBaselineWithReachability(corpusDir, reach)
	if err != nil {
		t.Fatalf("generate baseline: %v", err)
	}

	// Sample-size guard. A zero that means "collected nothing" is
	// indistinguishable from a zero that means "clean" unless the sample is
	// asserted too — so assert it, and fail loudly rather than pass vacuously.
	if st.Queries == 0 {
		t.Fatal("no queries planned — the reachability tally would be vacuously zero")
	}
	// Assert on COMPARED edges, not total edges: total counts every plan child
	// unconditionally, before any comparison, so it stays in the thousands even
	// if the checker silently stopped comparing.
	//
	// PROPORTIONAL, not "> 0". A floor of one is barely a floor: 17884 edges
	// collapsing to 3 would still pass while the checker had gone almost
	// entirely blind. The compared count should track the total edge count
	// closely — everything except the known no-quantifier adapters — so
	// require it to stay within 10%.
	report := reach.Report(20)
	edges, compared := reach.Edges(), reach.ComparedEdges()
	// Log the headline counts unconditionally. A ratchet that only speaks when
	// it fails cannot be checked for having drifted into measuring the wrong
	// population — which is exactly how the 3x inflation stayed invisible: the
	// test was green, so nobody read the numbers. `-v` now prints them.
	t.Logf("reachability: edges=%d compared=%d unreachable=%d no-quantifier=%d over %d queries",
		edges, compared, reach.Count(), reach.NoQuantifierCount(), st.Queries)
	if compared < edges*9/10 {
		t.Fatalf("collector compared only %d of %d edges over %d queries — "+
			"this assertion is blind, not clean\n%s", compared, edges, st.Queries, report)
	}

	// The no-quantifier class is EXCLUDED from reach.Count (leaf
	// adapters model no quantifier by design) but is ratcheted here rather
	// than merely reported. It is not a benign category: the two-leg
	// intersection that executed with zero memo edges was a no-quantifier
	// bug, so leaving the class unasserted means the ratchet cannot catch
	// that defect recurring.
	//
	// 32 is the current population. It must only ever fall — closing it needs
	// plans to carry the correlation and ordering properties the wrappers
	// carry (RFC-183 §15, RFC-184 W2/W3).
	//
	// The report below labels all 32 as "TypeFilterPlan" while this is
	// elsewhere described as a scanPlanExpression problem. Both are right and
	// they are not the same axis: the EXPRESSION is scanPlanExpression (the
	// adapter that reports no quantifiers, so the memo models no child), and
	// the report groups by planTypeName of the WRAPPED PLAN, which for this
	// adapter is the TypeFilter(Scan) it holds. Do not read the two as
	// contradicting each other, and do not "correct" one to match the other.
	const knownNoQuantifierEdges = 32
	if n := reach.NoQuantifierCount(); n > knownNoQuantifierEdges {
		t.Errorf("no-quantifier edges rose to %d (known baseline %d) — a rule is "+
			"constructing a plan whose children the memo does not model.\n\n"+
			"ONE LEGITIMATE CAUSE, before you go hunting a regression: fixing the "+
			"N-way projected-EXISTS arm so it starts firing will ADD "+
			"scanPlanExpression wrappers and push this count UP. That is the "+
			"ratchet barking at a real fix, not at a defect — raise the baseline "+
			"in that case, and only in that case.\n\n%s",
			n, knownNoQuantifierEdges, report)
	}

	if n := reach.Count(); n != 0 {
		t.Errorf("%d unreachable plan edges across %d planned queries; "+
			"a plan executes a child its quantifier's group cannot produce, "+
			"so the memo is costing an expression that will never run\n\n%s",
			n, st.Queries, report)
	}
}
