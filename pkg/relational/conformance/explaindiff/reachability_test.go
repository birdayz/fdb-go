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
func TestCorpusPlanReachability(t *testing.T) {
	t.Parallel()

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
	report := cascades.ReachabilityReport(20)
	if compared := cascades.ReachabilityComparedEdges(); compared == 0 {
		t.Fatalf("collector compared ZERO edges against a group over %d queries — "+
			"this assertion is blind, not clean\n%s", st.Queries, report)
	}

	if n := cascades.ReachabilityCount(); n != 0 {
		t.Errorf("%d unreachable plan edges across %d planned queries; "+
			"a plan executes a child its quantifier's group cannot produce, "+
			"so the memo is costing an expression that will never run\n\n%s",
			n, st.Queries, report)
	}
}
