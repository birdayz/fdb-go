package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestValidatePlanInvariants_NilInnerChild is the committed detection proof for
// RFC-164 WS-2: a non-leaf plan whose required inner is nil (the IN-LIMIT bug
// shape — GetChildren masks the nil as zero children) must be rejected, while a
// genuine leaf and a well-formed operator pass. The end-to-end mutation proof
// (revert the IN-LIMIT relink fix → PlanQueryForTest reports "plan invariant
// violated: ... Fetch(<nil>)") is captured in the PR; this pins the detector.
func TestValidatePlanInvariants_NilInnerChild(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	// Genuine leaf — legitimately childless.
	if err := ValidatePlanInvariants(scan); err != nil {
		t.Fatalf("scan leaf must pass: %v", err)
	}
	// Non-leaf operator with a nil inner — the malformed shape.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(nil, 5, 0)); err == nil {
		t.Fatal("a Limit with a nil inner must violate the no-nil-child invariant")
	}
	// Well-formed operator — passes.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(scan, 5, 0)); err != nil {
		t.Fatalf("well-formed Limit must pass: %v", err)
	}
	// Nested: Limit(Limit(nil)) — the inner malformation is reached by the walk.
	if err := ValidatePlanInvariants(plans.NewRecordQueryLimitPlan(plans.NewRecordQueryLimitPlan(nil, 1, 0), 5, 0)); err == nil {
		t.Fatal("a nested nil inner must be reached and rejected")
	}
}

// FuzzPlanner_Invariants asserts that EVERY successfully-planned random query
// satisfies the WS-2 structural invariants — a relink that drops a child on any
// input shape is caught here, always-on, with no Java/FDB dependency.
func FuzzPlanner_Invariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)
		p := NewPlanner(rules, nil).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 100_000

		plan, _, err := p.Plan(ref)
		if err != nil || plan == nil {
			return
		}
		ppe, ok := plan.(physicalPlanExpression)
		if !ok {
			return
		}
		rqp := ppe.GetRecordQueryPlan()
		if rqp == nil {
			return
		}
		if verr := ValidatePlanInvariants(rqp); verr != nil {
			t.Fatalf("planner produced a malformed plan for input %v: %v", b, verr)
		}
	})
}
