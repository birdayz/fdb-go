package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// RFC-220 made EVERY index-backed access a covering scan under a fetch, so the
// covering arms added to combineConcreteCostUnclamped and scanLikeCostSpecForPlan
// re-price the DOMINANT access-path shape — and not one existing cost assertion
// moved. That is the finding, not the reassurance: a repricing that moves
// nothing is either a no-op nobody proved is a no-op, or an untested win.
//
// MEASURED, and the answer differs per arm. Both figures below come from
// driving the two functions directly and from deleting each arm to see what it
// had been doing:
//
//   - combineConcreteCostUnclamped's arm is NUMERICALLY A NO-OP today. Deleting
//     it sends a covering scan to the default, which dispatches to
//     RecordQueryCoveringIndexPlan.HintCost, which DELEGATES to the wrapped
//     scan's HintCost — and that already computes what scanLikeCost computes.
//     Identical across {unique, non-unique} x {ctx-unique, ctx-non-unique} and
//     across partial and full equality binds: {10000.000000000002, 454.5...}
//     every cell. The arm unifies two paths that already agreed.
//   - scanLikeCostSpecForPlan's arm is NOT a no-op. Its second consumer is
//     fkChainInnerFixedCPU, and there a covering inner went from (0, false) to
//     (4.5, true) — with the production Fetch(Covering(scan)) shape going from
//     (0, false) to (4.05, true). false means "fail closed", and the caller's
//     response to it is `fixedCPU = innerCost.CPU`, i.e. charge the capped hop
//     the FULL 454.5 CPU of the uncapped scan. So the arm makes FK-chain-capped
//     joins materially cheaper, which moves winners.
//
// Both facts are pinned below, because the no-op is exactly as worth pinning as
// the change: if the two models ever stop agreeing, the first pin is what says
// so, rather than a silent divergence between two shapes reading one index.

// repricingScan builds a bare index scan on idx_a over key [A] with pk [ID].
// bindPK adds the second equality so the bind is FULL, which is what a point
// probe requires; unique sets the index's UNIQUE flag.
func repricingScan(t *testing.T, unique, bindPK bool) *plans.RecordQueryIndexPlan {
	t.Helper()
	mk := func(v int64) *predicates.ComparisonRange {
		comp := predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: v, Typ: values.NullableLong},
		}
		mr := predicates.EmptyComparisonRange().Merge(&comp)
		if !mr.Ok {
			t.Fatal("premise broken: could not build an equality comparison range")
		}
		return mr.Range
	}
	comps := []*predicates.ComparisonRange{mk(42)}
	if bindPK {
		comps = append(comps, mk(7))
	}
	return plans.NewRecordQueryIndexPlan(
		"idx_a", comps, []string{"T"}, values.UnknownType, false,
	).WithIndexMetadata([]string{"A"}, []string{"ID"}, unique)
}

func repricingCtx(unique bool) *indexTestPlanContext {
	return &indexTestPlanContext{candidates: []MatchCandidate{
		newKnownDistinctValueIndexCandidate(
			"idx_a", []string{"T"}, []string{"A"}, nil, values.UnknownType, unique, nil),
	}}
}

// TestCoveringScanIsPricedByTheSameModelAsTheScanItWraps pins the equality of
// MODEL: a covering scan reads the same index, the same ranges, one row per
// entry, so it must cost exactly what the scan it wraps costs.
//
// Written against combineConcreteCostUnclamped rather than against a winner,
// because a winner assertion cannot distinguish "priced the same" from "priced
// differently, but the difference did not flip this particular pair".
func TestCoveringScanIsPricedByTheSameModelAsTheScanItWraps(t *testing.T) {
	t.Parallel()

	st := properties.DefaultStatistics{}
	for _, unique := range []bool{true, false} {
		for _, ctxUnique := range []bool{true, false} {
			for _, fullBind := range []bool{true, false} {
				inner := repricingScan(t, unique, fullBind)
				cov := plans.NewRecordQueryCoveringIndexPlan(inner)
				ctx := repricingCtx(ctxUnique)

				bareCost := combineConcreteCostUnclamped(inner, nil, st, ctx)
				covCost := combineConcreteCostUnclamped(cov, nil, st, ctx)

				if bareCost != covCost {
					t.Errorf("unique=%v ctxUnique=%v fullBind=%v: covering priced %+v, "+
						"wrapped scan priced %+v — they read the SAME index range and must "+
						"be priced by the same model. A divergence means one side fell to "+
						"the generic path while the other kept the selectivity formula, so "+
						"any comparison between the two shapes is an artifact of which "+
						"model each landed in", unique, ctxUnique, fullBind, covCost, bareCost)
				}
				// Anti-vacuity: the equality is satisfied trivially if BOTH sides
				// are the zero cost, which is what a failed spec lookup returns.
				if covCost == (properties.Cost{}) {
					t.Errorf("unique=%v ctxUnique=%v fullBind=%v: covering priced at the "+
						"ZERO cost, so the equality above is vacuous — the spec lookup "+
						"returned ok=false and this pin proves nothing",
						unique, ctxUnique, fullBind)
				}
			}
		}
	}
}

// TestCoveringScanCombineArmAgreesWithTheDelegatingHintCost records that the
// combineConcreteCostUnclamped arm is, today, numerically a NO-OP — and pins it
// so that stops being true silently.
//
// The arm's value is structural rather than numeric: it stops the covering
// shape's price from depending on HintCost delegation continuing to mirror
// scanLikeCost. This test is the tripwire on that mirroring. If it goes red,
// the two models have diverged and the arm has started to matter — at which
// point the right response is to work out WHICH is correct, not to relax the
// assertion.
func TestCoveringScanCombineArmAgreesWithTheDelegatingHintCost(t *testing.T) {
	t.Parallel()

	st := properties.DefaultStatistics{}
	cov := plans.NewRecordQueryCoveringIndexPlan(repricingScan(t, true, true))

	priced := combineConcreteCostUnclamped(cov, nil, st, repricingCtx(true))
	delegated := cov.HintCost(nil, st)

	if priced != delegated {
		t.Errorf("the covering cost arm produced %+v but the delegating HintCost path "+
			"produces %+v. These agreed when the arm was written, which is why the arm "+
			"moved no existing assertion. They no longer do — so the covering shape's "+
			"price now DEPENDS on this arm, and the divergence needs adjudicating "+
			"rather than accepting", priced, delegated)
	}
}

// TestCoveringInnerIsReachableForTheFkChainFixedCPU pins the arm that is NOT a
// no-op, and names what it was doing before.
//
// fkChainInnerFixedCPU is scanLikeCostSpecForPlan's second consumer. Without a
// covering arm it answered (0, false) for a covering inner — "fail closed" —
// and combineConcreteCostUnclamped's FlatMap case responds to false with
// `fixedCPU = innerCost.CPU`, charging a capped hop the FULL CPU of the
// uncapped scan. Since RFC-220 the inner of an FK-chain-capped join is always
// Fetch(Covering(scan)), so that fail-closed branch was the ONLY branch.
func TestCoveringInnerIsReachableForTheFkChainFixedCPU(t *testing.T) {
	t.Parallel()

	ctx := repricingCtx(true)
	inner := repricingScan(t, true, true)
	cov := plans.NewRecordQueryCoveringIndexPlan(inner)

	bareCPU, bareOK := fkChainInnerFixedCPU(inner, ctx)
	if !bareOK {
		t.Fatal("premise broken: a BARE index scan must decompose, else the covering " +
			"comparison below has no baseline")
	}

	covCPU, covOK := fkChainInnerFixedCPU(cov, ctx)
	if !covOK {
		t.Fatal("a COVERING scan does not decompose into fixed per-execution CPU. " +
			"That is the fail-closed answer, and its consequence is not neutral: the " +
			"caller responds by charging the capped hop the inner's FULL CPU, so every " +
			"FK-chain-capped join is priced as though the cap had not been proven")
	}
	if covCPU != bareCPU {
		t.Errorf("covering fixed CPU %v != wrapped scan's %v; the covering plan performs "+
			"the same range read, so its fixed per-execution cost is the same", covCPU, bareCPU)
	}

	// The PRODUCTION shape, which is what the access path actually builds.
	fetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
		cov, nil, nil, plans.FetchIndexRecordsPrimaryKey)
	fetchCPU, fetchOK := fkChainInnerFixedCPU(fetch, ctx)
	if !fetchOK {
		t.Fatal("Fetch(Covering(scan)) — the shape the access path emits for every " +
			"index-backed access — does not decompose. The recursion descends the fetch " +
			"to its child and the child is a covering plan, so this is the same miss one " +
			"level up, and it is the shape that actually occurs in production")
	}
	if fetchCPU <= 0 {
		t.Errorf("Fetch(Covering(scan)) fixed CPU = %v, want a positive cost", fetchCPU)
	}
}

// The isProvablePointProbe half of this repricing is pinned inside package
// plans, where that predicate lives unexported — see
// TestCoveringScanPointProbeProvability in that package.
