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
// Deliberately ONE test over one corpus run, holding both assertions. The
// collector is process-global, so splitting the "is it clean" and "is it
// actually looking" checks into two t.Parallel tests let either one's
// ResetReachability clear the other's tally mid-flight — a race whose most
// likely symptom is a SILENT PASS, which is worse than the flake.
// DELIBERATELY NOT t.Parallel — the one place in this repo where that is
// correct, and it was found by this test failing rather than by reasoning.
//
// The collector is process-global. Several other tests in this package plan
// the SAME corpus (TestNoPlanPanics, TestShapeAccompaniesEverySuccessfulPlan,
// TestBaselineIsDeterministic, ...), and with t.Parallel their planning
// accumulated into this tally: the full-package run reported edges=53748 and
// no-quantifier=96 — exactly 3x the true 17916/32 — while every standalone
// `-run TestCorpusPlanReachability` invocation passed, because then only one
// test was planning.
//
// A non-parallel test runs to completion before the parallel ones resume, so
// this one owns the instrument while it measures. The proper fix is to thread
// the collector instead of sharing it (the plan_harness globals got exactly
// that treatment); until the collector is threaded, exclusivity here is what
// keeps the number meaningful.
//
// Do NOT add t.Parallel to this test. It will not fail loudly — it will
// silently inflate every count in proportion to how many other corpus tests
// happen to be running.
func TestCorpusPlanReachability(t *testing.T) {
	restore := cascades.EnableReachabilityCollection()
	defer restore()

	// Planning the corpus is what populates the tally; the baseline text
	// itself is checked by the explain-differ gate, not here.
	_, st, err := explaindiff.GenerateBaseline(corpusDir)
	if err != nil {
		t.Fatalf("generate baseline: %v", err)
	}

	// Sample-size guard. A zero that means "collected nothing" is
	// indistinguishable from a zero that means "clean" unless the sample is
	// asserted too — so assert it, and fail loudly rather than pass vacuously.
	if st.Queries == 0 {
		t.Fatal("no queries planned — the reachability tally would be vacuously zero")
	}
	// Assert on COMPARED edges, not total edges. Total edges counts every plan
	// child unconditionally, before any comparison happens, so it stays in the
	// thousands even if the checker silently stopped comparing anything — a
	// guard that cannot fail. Only the compared count separates "clean" from
	// "not looking".
	// PROPORTIONAL, not "> 0". A floor of one is barely a floor: 17884 edges
	// collapsing to 3 would still pass while the checker had gone almost
	// entirely blind. The compared count should track the total edge count
	// closely — everything except the known no-quantifier adapters — so
	// require it to stay within 10%.
	report := cascades.ReachabilityReport(20)
	edges, compared := cascades.ReachabilityEdges(), cascades.ReachabilityComparedEdges()
	if compared < edges*9/10 {
		t.Fatalf("collector compared only %d of %d edges over %d queries — "+
			"this assertion is blind, not clean\n%s", compared, edges, st.Queries, report)
	}

	// The no-quantifier class is EXCLUDED from ReachabilityCount (leaf
	// adapters model no quantifier by design) but is ratcheted here rather
	// than merely reported. It is not a benign category: the two-leg
	// intersection that executed with zero memo edges was a no-quantifier
	// bug, so leaving the class unasserted means the ratchet cannot catch
	// that defect recurring.
	//
	// 32 is the current population, all scanPlanExpression. It must only ever
	// fall — closing it needs plans to carry the correlation and ordering
	// properties the wrappers carry (RFC-183 §15, RFC-184 W2/W3).
	const knownNoQuantifierEdges = 32
	if n := cascades.NoQuantifierCount(); n > knownNoQuantifierEdges {
		t.Errorf("no-quantifier edges rose to %d (known baseline %d) — a rule is "+
			"constructing a plan whose children the memo does not model\n\n%s",
			n, knownNoQuantifierEdges, report)
	}

	if n := cascades.ReachabilityCount(); n != 0 {
		t.Errorf("%d unreachable plan edges across %d planned queries; "+
			"a plan executes a child its quantifier's group cannot produce, "+
			"so the memo is costing an expression that will never run\n\n%s",
			n, st.Queries, report)
	}
}
