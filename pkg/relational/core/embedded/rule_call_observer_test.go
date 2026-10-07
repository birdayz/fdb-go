package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// observeRuleCalls plans sql with the given rules disabled and counts the
// rule calls the W6 step 1 observer reports, per rule.
func observeRuleCalls(t *testing.T, sql, schema string, disabled ...string) map[string]int {
	t.Helper()
	calls := map[string]int{}
	if _, err := PlanQueryForTestObservingRules(sql, schema, disabled, func(c cascades.ObservedRuleCall) {
		calls[c.Rule]++
	}); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return calls
}

// The conditional SelectMerge → PredicatePushDown chain through the rule-call
// observer (ws-f-design.md 2.7): disabling SelectMergeRule leaves pushdown
// running; disabling the wrapper's own name, ConditionalCascadesRule, turns
// the whole chain off, as Java's isRuleEnabled does for the wrapper
// (CascadesPlanner.pushTransformExpressionIfNeeded checks the wrapper before
// its inner rules).
func TestRuleCallObserver_SelectMergeAndPushDownChain(t *testing.T) {
	t.Parallel()
	const schema = "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE U (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))"
	const query = "SELECT T.id FROM T, U WHERE T.a = U.a AND T.b > 5 AND U.b < 3"

	all := observeRuleCalls(t, query, schema)
	if all["SelectMergeRule"] == 0 || all["PredicatePushDownRule"] == 0 {
		t.Fatalf("the chain did not run with every rule enabled: %v", all)
	}

	noMerge := observeRuleCalls(t, query, schema, "SelectMergeRule")
	if noMerge["SelectMergeRule"] != 0 {
		t.Errorf("a disabled SelectMergeRule ran %d times", noMerge["SelectMergeRule"])
	}
	if noMerge["PredicatePushDownRule"] == 0 {
		t.Errorf("with SelectMergeRule disabled, pushdown must still run: %v", noMerge)
	}

	noChain := observeRuleCalls(t, query, schema, "ConditionalCascadesRule")
	for _, r := range []string{"SelectMergeRule", "PredicatePushDownRule", "DecorrelateValuesRule", "QueryPredicateSimplificationRule"} {
		if noChain[r] != 0 {
			t.Errorf("with ConditionalCascadesRule disabled, %s ran %d times; every conditional chain is off", r, noChain[r])
		}
	}
	if noChain["FinalizeExpressionsRule"] == 0 {
		t.Error("disabling the conditional wrapper must leave the other rules running")
	}
}
