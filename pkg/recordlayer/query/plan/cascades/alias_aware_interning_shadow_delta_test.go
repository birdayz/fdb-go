package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// planChainInterning plans an n-table chain and returns the alias-aware dedup
// shadow, the total memo member population, the task count, and whether it
// converged. aliasIdentityBaseline=true suppresses the alias-aware interning
// tier (SetDisableAliasAwareInterning) to measure whether any proposal still
// reaches that tier after prepared admission.
func planChainInterning(t *testing.T, n int, aliasIdentityBaseline bool) (shadow, members, tasks int, converged bool) {
	t.Helper()
	if aliasIdentityBaseline {
		expressions.SetDisableAliasAwareInterning(true)
		defer expressions.SetDisableAliasAwareInterning(false)
	}
	p := fullChainPlanner()
	_, tasks, err := p.Plan(expressions.InitialOf(buildOrdinalChainSelect(t, n)))
	return p.Memo().AliasAwareDedups(), p.Memo().TotalMembers(), tasks, err == nil
}

// TestAliasAwareInterningShadowDelta pins the RFC-232 prepared-admission
// effect on the historical alias-aware shadow. Before staged rules published
// only genuinely inserted proposals, memo-equal proposals still scheduled
// ExploreExpr/OptimizeInputs work; those phantom traversals later produced the
// alias-renamed merge twins counted here (3-chain shadow/delta 2/6, 4-chain
// shadow 32). Prepared admission now drops a deduped proposal before task
// scheduling. Consequently the chain never manufactures those later twins:
// enabling or disabling tier 3 has exactly the same population and task count.
//
// This is not a broken InternsAliasAware/MemoEqual gate. The prepared equality
// unit tests pin an alias-aware-only duplicate directly, while
// TestSelectExpression_InternsAliasAware_GatedToMergeSelects pins that the real
// ordinal merge shapes opt in. This corpus pin proves the stronger scheduling
// invariant: already-deduped proposals cannot re-enter and create work for the
// alias-aware tier to clean up downstream.
//
// NON-PARALLEL: it toggles the process-global alias-identity baseline
// (SetDisableAliasAwareInterning). Go runs non-parallel tests sequentially,
// before the parallel phase, so the toggle is never read concurrently with the
// write (see the var doc in reference.go). Regenerate the exact counts only on
// an intentional interning/planner change — and then the task-count baseline
// (TestPartitionSelect_ChainInterningBaseline) must move in lockstep.
func TestAliasAwareInterningShadowDelta(t *testing.T) {
	// Both scales are exact. A non-zero delta means a proposal absorbed by
	// prepared admission again scheduled downstream work and re-exploded.
	shadow3, mAware3, tasks3, conv3 := planChainInterning(t, 3, false)
	_, mIdentity3, tasksI3, convI3 := planChainInterning(t, 3, true)
	if !conv3 || !convI3 {
		t.Fatalf("3-chain must converge in both modes (aware=%v identity=%v)", conv3, convI3)
	}
	if shadow3 != 0 {
		t.Errorf("3-chain alias-aware dedup shadow = %d, want 0 after prepared admission", shadow3)
	}
	if delta := mIdentity3 - mAware3; delta != 0 {
		t.Errorf("3-chain member-count delta (identity %d − aware %d) = %d, want 0", mIdentity3, mAware3, delta)
	}
	if tasksI3 != tasks3 {
		t.Errorf("3-chain tasks differ with tier disabled: aware=%d identity=%d, want exact equality", tasks3, tasksI3)
	}

	shadow4, mAware4, tasks4, conv4 := planChainInterning(t, 4, false)
	_, mIdentity4, tasksI4, convI4 := planChainInterning(t, 4, true)
	if shadow4 != 0 {
		t.Errorf("4-chain alias-aware dedup shadow = %d, want 0 after prepared admission", shadow4)
	}
	if !conv4 || !convI4 {
		t.Errorf("4-chain must converge in both modes (aware=%v identity=%v)", conv4, convI4)
	}
	if mIdentity4 != mAware4 || tasksI4 != tasks4 {
		t.Errorf("4-chain tier toggle changed work: members aware=%d identity=%d; tasks aware=%d identity=%d; want exact equality",
			mAware4, mIdentity4, tasks4, tasksI4)
	}
	t.Logf("prepared-admission shadow: 3-chain members=%d tasks=%d | 4-chain members=%d tasks=%d (tier toggle exact no-op)",
		mAware3, tasks3, mAware4, tasks4)
}
