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
	_, tasks, err := p.Plan(expressions.InitialOf(buildChainSelect(n)))
	return p.Memo().AliasAwareDedups(), p.Memo().TotalMembers(), tasks, err == nil
}

// TestRFC173S3_AliasAwareInterningShadowDelta is the RFC-173 Slice-3 Q6
// shadow-delta gate. The alias-aware interning tier (Reference.Insert's
// MemoEqual branch, gated to merge re-enumeration selects) collapses merge
// sub-products that are equal up to a consistent quantifier-alias renaming — the
// dedup the task-count baseline depends on but never measures DIRECTLY. This
// promotes that measurement from a t.Logf to EXACT assertions, two scales:
//
//   - 3-chain (both modes converge, so the member-count delta is well-defined):
//     the alias-aware tier performs exactly 4 direct dedups (the shadow) and
//     that collapses exactly 20 memo members (68 → 88 with the tier off). shadow
//     < delta because each collapsed merge sub-product would otherwise
//     RE-EXPLODE — the direct dedup is the seed of a larger population delta.
//     This is the shadow↔delta equivalence in its exact, convergent form.
//   - 4-chain (the scale where the tier becomes load-bearing): the shadow is
//     exactly 56, planning CONVERGES with the tier on, and does NOT converge
//     with it off (the alias-identity population blows the 100k task budget —
//     the 29915→60044-class super-linear re-explosion the tier exists to
//     prevent). The shadow is not cosmetic: it is exactly what keeps the join
//     re-enumeration tractable.
//
// NON-PARALLEL: it toggles the process-global alias-identity baseline
// (SetDisableAliasAwareInterning). Go runs non-parallel tests sequentially,
// before the parallel phase, so the toggle is never read concurrently with the
// write (see the var doc in reference.go). Regenerate the exact counts only on
// an intentional interning/planner change — and then the task-count baseline
// (TestPartitionSelect_ChainInterningBaseline) must move in lockstep.
func TestRFC173S3_AliasAwareInterningShadowDelta(t *testing.T) {
	// 3-chain: exact shadow + exact member-count delta, both modes converge.
	shadow3, mAware3, _, conv3 := planChainInterning(t, 3, false)
	_, mIdentity3, _, convI3 := planChainInterning(t, 3, true)
	if !conv3 || !convI3 {
		t.Fatalf("3-chain must converge in both modes (aware=%v identity=%v)", conv3, convI3)
	}
	if shadow3 != 4 {
		t.Errorf("3-chain alias-aware dedup shadow = %d, want 4 (exact — interning behaviour changed)", shadow3)
	}
	if delta := mIdentity3 - mAware3; delta != 20 {
		t.Errorf("3-chain member-count delta (identity %d − aware %d) = %d, want 20 (exact)", mIdentity3, mAware3, delta)
	}
	if shadow3 <= 0 {
		t.Errorf("3-chain shadow must be > 0 — the alias-aware tier must actually fire")
	}
	if mIdentity3-mAware3 <= shadow3 {
		t.Errorf("3-chain delta (%d) must EXCEED the direct shadow (%d) — each collapsed merge "+
			"sub-product re-explodes when not deduped (cascade)", mIdentity3-mAware3, shadow3)
	}

	// 4-chain: exact shadow, and the tier is LOAD-BEARING for convergence.
	shadow4, _, tasks4, conv4 := planChainInterning(t, 4, false)
	_, _, tasksI4, convI4 := planChainInterning(t, 4, true)
	if shadow4 != 56 {
		t.Errorf("4-chain alias-aware dedup shadow = %d, want 56 (exact)", shadow4)
	}
	if !conv4 {
		t.Errorf("4-chain must CONVERGE with alias-aware interning on (tasks=%d)", tasks4)
	}
	if convI4 {
		t.Errorf("4-chain must NOT converge with alias-aware interning OFF (tasks=%d) — the shadow's "+
			"dedup is load-bearing; without it the alias-identity population blows the task budget", tasksI4)
	}
	t.Logf("shadow-delta: 3-chain shadow=%d delta=%d (aware=%d identity=%d) | 4-chain shadow=%d conv(on)=%v conv(off)=%v",
		shadow3, mIdentity3-mAware3, mAware3, mIdentity3, shadow4, conv4, convI4)
}
