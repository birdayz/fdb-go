package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// planChainInterning plans an n-table chain and returns the alias-aware dedup
// shadow, the total memo member population, the task count, and whether it
// converged. aliasIdentityBaseline=true suppresses the alias-aware interning
// tier (SetDisableAliasAwareInterning) to recover the pre-interning population.
func planChainInterning(t *testing.T, n int, aliasIdentityBaseline bool) (shadow, members, tasks int, converged bool) {
	t.Helper()
	if aliasIdentityBaseline {
		expressions.SetDisableAliasAwareInterning(true)
		defer expressions.SetDisableAliasAwareInterning(false)
	}
	p := fullChainPlanner()
	_, tasks, err := p.Plan(expressions.InitialOf(buildOrdinalChainSelect(n)))
	return p.Memo().AliasAwareDedups(), p.Memo().TotalMembers(), tasks, err == nil
}

// TestAliasAwareInterningShadowDelta pins the alias-aware interning tier's
// dedup accounting. The tier (Reference.Insert's MemoEqual branch, gated to
// merge re-enumeration selects) collapses merge
// sub-products that are equal up to a consistent quantifier-alias renaming — the
// dedup the task-count baseline depends on but never measures DIRECTLY. This
// promotes that measurement from a t.Logf to EXACT assertions, two scales:
//
//   - 3-chain (both modes converge, so the member-count delta is well-defined):
//     the alias-aware tier performs exactly 2 direct dedups (the shadow) and
//     that collapses exactly 6 memo members (54 → 60 with the tier off). shadow
//     < delta because each collapsed merge sub-product would otherwise
//     RE-EXPLODE — the direct dedup is the seed of a larger population delta.
//     This is the shadow↔delta equivalence in its exact, convergent form.
//     (Pre-RFC-181-stage-(b) values: shadow 4, delta 20 (68 → 88) — the
//     finals-only landing plus epoch-bounded rounds shrank both.)
//   - 4-chain (the scale where the tier becomes load-bearing): the shadow is
//     exactly 32 (was 56 before the stage-(b) flip), and turning the tier
//     off still costs a MATERIAL multiple of the deduped task count
//     (asserted as the ≥1.25x ratio below; observed 1.58x). Before the
//     epoch model the un-deduped re-explosion was super-linear (≥2x,
//     the 29915→60044 class; before the root-operator rule index it was
//     outright non-convergence against the 100k task budget) — epoch-
//     bounded rounds now damp the blow-up, but the tier remains
//     load-bearing, not cosmetic.
//
// NON-PARALLEL: it toggles the process-global alias-identity baseline
// (SetDisableAliasAwareInterning). Go runs non-parallel tests sequentially,
// before the parallel phase, so the toggle is never read concurrently with the
// write (see the var doc in reference.go). Regenerate the exact counts only on
// an intentional interning/planner change — and then the task-count baseline
// (TestPartitionSelect_ChainInterningBaseline) must move in lockstep.
func TestAliasAwareInterningShadowDelta(t *testing.T) {
	// 3-chain: exact shadow + exact member-count delta, both modes converge.
	shadow3, mAware3, _, conv3 := planChainInterning(t, 3, false)
	_, mIdentity3, _, convI3 := planChainInterning(t, 3, true)
	if !conv3 || !convI3 {
		t.Fatalf("3-chain must converge in both modes (aware=%v identity=%v)", conv3, convI3)
	}
	if shadow3 != 2 {
		t.Errorf("3-chain alias-aware dedup shadow = %d, want 2 (exact — interning behaviour changed)", shadow3)
	}
	if delta := mIdentity3 - mAware3; delta != 6 {
		t.Errorf("3-chain member-count delta (identity %d − aware %d) = %d, want 6 (exact)", mIdentity3, mAware3, delta)
	}
	if shadow3 <= 0 {
		t.Errorf("3-chain shadow must be > 0 — the alias-aware tier must actually fire")
	}
	if mIdentity3-mAware3 <= shadow3 {
		t.Errorf("3-chain delta (%d) must EXCEED the direct shadow (%d) — each collapsed merge "+
			"sub-product re-explodes when not deduped (cascade)", mIdentity3-mAware3, shadow3)
	}

	// 4-chain: exact shadow, and the tier is LOAD-BEARING for the task
	// budget. Before the root-operator rule index this was observable as
	// outright non-convergence (the un-deduped population blew the 100k
	// task cap); the index cut every configuration's task count ~7x and
	// the assertion became a ≥2x super-linear ratio. The RFC-181 WS-P
	// stage (b) epoch model bounds re-exploration rounds, so turning the
	// tier off no longer RE-EXPLODES super-linearly — but the un-deduped
	// population still costs a MATERIAL multiple of the deduped task
	// count (observed 21685 vs 13721, 1.58x). Assert the tier saves at
	// least a quarter of the OFF-mode work (≥1.25x): a regression that
	// makes the tier cosmetic (ratio → 1x) still fails loudly, while the
	// epoch-damped ratio has real headroom.
	shadow4, _, tasks4, conv4 := planChainInterning(t, 4, false)
	_, _, tasksI4, convI4 := planChainInterning(t, 4, true)
	if shadow4 != 32 {
		t.Errorf("4-chain alias-aware dedup shadow = %d, want 32 (exact; was 56 pre-RFC-181-stage-(b))", shadow4)
	}
	if !conv4 {
		t.Errorf("4-chain must CONVERGE with alias-aware interning on (tasks=%d)", tasks4)
	}
	if convI4 && tasksI4 < tasks4+tasks4/4 {
		t.Errorf("4-chain with alias-aware interning OFF ran %d tasks vs %d on — the shadow's "+
			"dedup no longer buys a material saving; interning behaviour changed", tasksI4, tasks4)
	}
	t.Logf("shadow-delta: 3-chain shadow=%d delta=%d (aware=%d identity=%d) | 4-chain shadow=%d tasks(on)=%d tasks(off)=%d conv(on)=%v conv(off)=%v",
		shadow3, mIdentity3-mAware3, mAware3, mIdentity3, shadow4, tasks4, tasksI4, conv4, convI4)
}
