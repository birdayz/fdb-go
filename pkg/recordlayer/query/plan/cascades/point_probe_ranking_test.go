package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPlanningCostModel_CorrelatedPointProbeRanksAsUnbounded carries the
// point-probe identity fix up to the level where it actually costs something:
// a RANKING.
//
// point_probe_identity_test.go proves the predicate answers correctly. That is
// not the same claim as "a plan gets ranked differently", because criterion #2
// is DOUBLY GATED and both gates are tripped by the very shape the fix
// corrects: the criterion is skipped unless at least one side's whole-plan max
// cardinality is known (planning_cost_model.go's outer guard), and a side's
// data-access maximum collapses to -1 the moment ANY access on it is unbounded
// (concretePlanCounts). Correcting the probe makes the access unbounded, so it
// is fair to ask whether the corrected bound can ever reach a comparison at
// all.
//
// It can, and this is the construction. The two candidates are the SAME plan
// shape — a full ORDERS scan under a single equality filter — differing only in
// whose row the equality operand reads:
//
//	probe:      ORDERS.ID = ?   (this scan's own primary key: a real point probe)
//	correlated: PRODUCTS.ID = ? (an outer reference that binds nothing of ORDERS)
//
// Under the name-keyed answer both spelled their operand "ID", both were
// declared one-record probes, and both therefore carried a data-access maximum
// of 1. Under the identity answer only the first does.
//
// The opponent's only job is to unlock the outer guard. A FirstOrDefault is
// bounded at exactly one row as a WHOLE PLAN (Java's CardinalitiesProperty
// bounds it regardless of its child) while its data ACCESS stays unbounded, so
// it satisfies `wholePlanMaxCardinalityKnown(b)` without itself supplying a
// bounded access — precisely the all-bounded-except-one asymmetry criterion #2
// needs in order to run.
func TestPlanningCostModel_CorrelatedPointProbeRanksAsUnbounded(t *testing.T) {
	t.Parallel()

	ctx := &pkGateTestCtx{pk: []string{"ID"}}
	ordersAlias := values.NamedCorrelationIdentifier("O")
	productsAlias := values.NamedCorrelationIdentifier("P")

	probe, _ := pointProbeEqFilter(t, "ORDERS", ordersAlias,
		pointProbeRef(t, "ORDERS", ordersAlias, "ID"))

	// Two flavours of the corrected operand, so BOTH identity elements are
	// load-bearing at ranking level and not merely at predicate level. The
	// first differs from the accepted operand in its DOMAIN (baked against
	// PRODUCTS' layout); the second is baked against ORDERS' OWN layout at
	// ORDERS' own primary-key ordinal and differs ONLY in the quantifier — the
	// self-join shape, which the domain check cannot reach.
	corrByDomain, _ := pointProbeEqFilter(t, "ORDERS", ordersAlias,
		pointProbeRef(t, "PRODUCTS", productsAlias, "ID"))
	corrByLeg, _ := pointProbeEqFilter(t, "ORDERS", ordersAlias,
		pointProbeRef(t, "ORDERS", productsAlias, "ID"))

	opponent := plans.NewRecordQueryFirstOrDefaultPlan(
		plans.NewRecordQueryScanPlan([]string{"PRODUCTS"}, pointProbeIdentityLayouts["PRODUCTS"], false), nil)

	// --- the criterion is actually reachable, stated as assertions rather than
	// assumed by the comparisons below.
	if !wholePlanMaxCardinalityKnown(opponent) {
		t.Fatal("setup: the opponent must have a KNOWN whole-plan max cardinality, or criterion #2's " +
			"outer guard skips the comparison and this test proves nothing")
	}
	oppCounts := concretePlanCounts(opponent, ctx)
	if oppCounts.maxDataAccessCardinality >= 0 {
		t.Fatalf("setup: the opponent's data access must stay UNBOUNDED (got %v) — with both sides "+
			"bounded the criterion ties and never adjudicates", oppCounts.maxDataAccessCardinality)
	}

	probeCounts := concretePlanCounts(probe, ctx)
	if probeCounts.maxDataAccessCardinality != 1 {
		t.Fatalf("a genuine `o.ID = ?` filter over a full scan lost its provable bound (got %v) — "+
			"the correction must not over-decline", probeCounts.maxDataAccessCardinality)
	}

	// --- the ranking of the accepted shape, which the flip below is measured
	// against.
	probeVsOpp := planningCostModelCompareWith(probe, opponent, nil, ctx)
	if probeVsOpp >= 0 {
		t.Fatalf("compare(real point probe, opponent) = %d, want < 0 — a provably one-record access "+
			"must still win criterion #2 against an unbounded one", probeVsOpp)
	}
	probeResid := countResidualPredicatesWithContext(probe, ctx)
	oppResid := countResidualPredicatesWithContext(opponent, ctx)
	if oppResid >= probeResid {
		t.Fatalf("the opponent's residual count (%d) does not beat the candidates' (%d) — the "+
			"attribution argument needs every rung BELOW criterion #2 to prefer the opponent",
			oppResid, probeResid)
	}

	for _, tc := range []struct {
		name      string
		plan      *plans.RecordQueryPredicatesFilterPlan
		element   string
		conflated string
	}{
		{
			name:      "another DOMAIN",
			plan:      corrByDomain,
			element:   "domain",
			conflated: "`p.ID` baked against PRODUCTS' layout",
		},
		{
			name:      "another CORRELATION, domain held equal",
			plan:      corrByLeg,
			element:   "correlation",
			conflated: "an ORDERS-layout ordinal 0 read off the PRODUCTS quantifier",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			corrCounts := concretePlanCounts(tc.plan, ctx)
			if corrCounts.maxDataAccessCardinality >= 0 {
				t.Fatalf("a filter whose only equality is %s still carries a data-access bound of %v — "+
					"a full ORDERS scan is not a one-record access", tc.conflated,
					corrCounts.maxDataAccessCardinality)
			}

			// Attribution. Criterion #2 is the FIRST rung that can discriminate
			// here — the only rung above it separates physical expressions from
			// logical ones, and all three plans are physical. The rung
			// immediately below it counts residual predicates, and it prefers
			// the OPPONENT over BOTH candidates: each carries one residual, the
			// opponent none. So every rung from the residual count downwards
			// ranks both candidates the same way, and the candidate that
			// nevertheless WINS can only have won at criterion #2.
			if resid := countResidualPredicatesWithContext(tc.plan, ctx); resid != probeResid {
				t.Fatalf("this candidate carries %d residual(s) against the accepted shape's %d — "+
					"they must agree below criterion #2, or the ranking difference is not "+
					"attributable to it", resid, probeResid)
			}

			corrVsOpp := planningCostModelCompareWith(tc.plan, opponent, nil, ctx)
			if corrVsOpp <= 0 {
				t.Fatalf("compare(candidate, opponent) = %d, want > 0 — a full ORDERS scan whose only "+
					"equality is %s was ranked as if it read a single record. That mis-ranking is "+
					"REACHABLE: criterion #2's outer guard is open (the opponent's whole-plan max is "+
					"known) and the %s element of the identity is what decides",
					corrVsOpp, tc.conflated, tc.element)
			}
			if corrVsOpp == probeVsOpp {
				t.Fatalf("this candidate and the genuine point probe both ranked %d against the same "+
					"opponent — two plans differing only in the %s of their equality operand are "+
					"indistinguishable to the cost model, which is the conflation RFC-197 item 2 "+
					"removed", probeVsOpp, tc.element)
			}

			// Antisymmetry, so the flip is a real reordering and not a
			// one-sided artifact of argument order.
			if oppVsCorr := planningCostModelCompareWith(opponent, tc.plan, nil, ctx); oppVsCorr >= 0 {
				t.Fatalf("compare(opponent, candidate) = %d, want < 0 — the comparator disagrees "+
					"with itself on the same pair", oppVsCorr)
			}
		})
	}
}
