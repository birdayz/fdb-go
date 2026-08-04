package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMultiIntersectionHintCost_DrivingBranchIsLive pins the RFC-209
// group-existence merge's cost formula against being deleted as dead code.
//
// It has been argued that the driving-leg branch is unreachable, on the grounds
// that neutralizing it does not change any plan choice. That inference does not
// hold: the branch executes during ordinary planning of a grouped-aggregate
// query, and it is inert as a DECIDER only because two earlier, purely
// structural rungs of the planning cost model already express the same
// preference (see the concreteCountMultiIntersection arm in
// planning_cost_model.go). "Never marginal" is not "never runs", and the
// distinction matters here because the fallback is worse, not merely different:
// without this branch the plan falls to IntersectionCost, whose min-of-legs
// cardinality UNDERSTATES an outer merge by exactly the groups the merge exists
// to add back, and whose CPU stops summing the companion leg. Deleting the
// branch would make the merge look CHEAPER — the opposite of the concern that
// motivated calling it dead.
//
// The two properties asserted are the two the formula exists to provide:
//
//	cardinality — the DRIVING leg's, not the minimum over legs, because an outer
//	              merge emits one row per driving-stream group whether or not
//	              the other legs have a matching entry;
//	CPU         — the SUM over every leg, because the companion is a genuine
//	              BY_GROUP index scan, not a constant folded in at zero.
func TestMultiIntersectionHintCost_DrivingBranchIsLive(t *testing.T) {
	t.Parallel()

	keys := []values.Value{&values.FieldValue{Field: "gk", Typ: values.NotNullLong}}
	base := NewRecordQueryMultiIntersectionOnValuesPlan(
		[]RecordQueryPlan{stub("COUNT_LEG"), stub("SUM_LEG")}, keys, nil)

	qs := base.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("fixture needs two legs, got %d", len(qs))
	}
	// Drive from leg 0 — the companion COUNT(*) stream, exactly as the
	// group-existence merge designates it.
	outer := base.WithDrivingStream(qs[0].GetAlias())
	if outer.DrivingStreamIndex() != 0 {
		t.Fatalf("driving stream did not resolve to leg 0, got %d", outer.DrivingStreamIndex())
	}

	// Deliberately asymmetric: the driving leg has the LARGER cardinality (it
	// reports every group that ever existed, including the ones the SUM leg has
	// no entry for). A min-of-legs formula would report 40 here, which is the
	// specific understatement this branch exists to prevent.
	child := []properties.Cost{
		{Cardinality: 100, CPU: 1000},
		{Cardinality: 40, CPU: 300},
	}

	got := outer.HintCost(child, properties.DefaultStatistics{})

	if got.Cardinality != 100 {
		t.Errorf("cardinality = %v, want the DRIVING leg's 100. A value of 40 means the formula "+
			"fell back to min-of-legs, which drops exactly the groups the outer merge adds back.",
			got.Cardinality)
	}

	wantCPU := 1000.0 + 300.0 + 100*properties.IntersectionCPU*2
	if got.CPU != wantCPU {
		t.Errorf("CPU = %v, want %v (sum of every leg's work plus the per-row merge charge). "+
			"A smaller value means the companion leg stopped being charged as a real scan.",
			got.CPU, wantCPU)
	}

	// The inner (non-driving) spelling must NOT take this branch — otherwise the
	// assertions above would pass for a plan that has no driving stream at all,
	// and the test would not be distinguishing the two formulas.
	inner := base.HintCost(child, properties.DefaultStatistics{})
	if inner.Cardinality == got.Cardinality && inner.CPU == got.CPU {
		t.Errorf("inner intersection and outer merge priced identically (%v); the driving-leg "+
			"branch is not being taken, so this test cannot detect its removal", inner)
	}
}
