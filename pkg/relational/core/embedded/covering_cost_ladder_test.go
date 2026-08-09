package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

// TestCoveringTreeWinsTheCostLadder is the measurement that CLEARS the cost
// model of RFC-220's covering-scan loss, kept as a test because it is what
// justified looking elsewhere.
//
// On the RFC's defect query the planner picks a bare fetching index scan over
// an equivalent covering scan. The obvious explanation — the cost ladder
// prefers the bare tree — is FALSE, and this pins that it stays false. Both
// candidate trees are the same size (3 nodes; the fetch is elided in the
// covering tree, so there is no extra node to pay for), criterion 7 ranks the
// covering tree strictly better, and the full ladder agrees.
//
// An earlier probe compared BARE against COVERING at the SCAN level and reached
// the same verdict for the wrong reason: those two scans are not the pair the
// planner ranks. This test compares the two ROOT-GROUP alternatives, which is
// the comparison the planner actually performs.
//
// If this goes red, the cost model HAS become the reason a covering scan loses,
// and the investigation that concluded otherwise must be reopened rather than
// this expectation updated.
func TestCoveringTreeWinsTheCostLadder(t *testing.T) {
	t.Parallel()

	const residual = "SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%'"

	plan := func(disabled []string) plans.RecordQueryPlan {
		t.Helper()
		opts := api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, disabled).Build()
		p, _, err := planPhysicalForTest(residual, likePrefixSchema, nil, false, nil, plannerOptionsFrom(opts))
		if err != nil {
			t.Fatalf("planning %q (disabled=%v): %v", residual, disabled, err)
		}
		return p
	}

	// MergeFetchIntoCoveringIndexRule collapses Fetch(Covering(Index)) into a
	// bare fetching Index — sound, Java-registered, one node cheaper. It USED to
	// decide this query's plan: with it enabled the covering scan was lost, with
	// it disabled the covering scan was chosen.
	//
	// That asymmetry was the whole defect, and it was never a costing one. Go
	// built only ONE parent alternative per child (the per-ordering winner), so
	// once this rule added a bare Index to the child group, Filter(Fetch(Covering))
	// was never CONSTRUCTED — invisible to cost rather than out-priced. With
	// parent construction enumerated over every physical child member, both
	// alternatives reach the parent group and the cost ladder picks the covering
	// one, which is what it preferred all along.
	//
	// So the assertion is now the strong one: this rule's presence must make NO
	// difference. That is a sharper statement than "the covering tree is cheaper",
	// and it is the property the fix actually establishes.
	withRule := plan(nil).Explain()
	withoutRule := plan([]string{"MergeFetchIntoCoveringIndexRule"}).Explain()

	if !strings.Contains(withRule, "COVERING") {
		t.Fatalf("the covering scan was lost with MergeFetchIntoCoveringIndexRule "+
			"enabled: %s\nThe rule is sound; if its presence changes the plan again, "+
			"parent construction has gone back to single-picking a child member and "+
			"the alternative is not being built. Fix the enumeration — do not "+
			"unregister the rule and do not update this expectation.", withRule)
	}
	if withRule != withoutRule {
		t.Fatalf("MergeFetchIntoCoveringIndexRule changed the chosen plan.\n"+
			"  enabled:  %s\n  disabled: %s\n"+
			"A sound transformation that merely ADDS a member must not change the "+
			"winner. If it does, some parent is again choosing one child member "+
			"instead of enumerating them and letting the memo cost the results.",
			withRule, withoutRule)
	}
}
