package embedded

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// A filter over a table with two indexes reaches rules over references with
// several plan partitions, so Java's per-partition count trips a bound of 2.
func TestMaxNumMatchesPerRuleCallCountsPlanPartitions(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX t_a ON t(a) CREATE INDEX t_b ON t(b)"
	const sql = "SELECT id FROM t WHERE c = 1"
	for _, tc := range []struct {
		bound int
		trips bool
	}{{0, false}, {2, true}, {1000, false}} {
		t.Run(fmt.Sprint(tc.bound), func(t *testing.T) {
			t.Parallel()
			opts := api.NewOptionsBuilder().Set(api.OptMaxNumMatchesPerRuleCall, tc.bound).Build()
			plan, err := PlanPhysicalForTestWithOptions(sql, ddl, nil, opts)
			if tripped := errors.Is(err, cascades.ErrPlannerRuleMatchCapHit); tripped != tc.trips {
				t.Fatalf("bound %d: err = %v, want trip=%v", tc.bound, err, tc.trips)
			}
			if !tc.trips && (err != nil || plan == nil) {
				t.Fatalf("bound %d: plan = %v, err = %v, want a plan", tc.bound, plan, err)
			}
		})
	}
}

// TestMaxNumMatchesPerRuleCallTripsTheCountingRule: one below a rule's Java count
// that rule trips (the observer's last call); at the count the statement plans.
func TestMaxNumMatchesPerRuleCallTripsTheCountingRule(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX t_a ON t(a) CREATE INDEX t_b ON t(b) " +
		"CREATE TABLE u (id BIGINT, a BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE v (id BIGINT, a BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE w (id BIGINT, a BIGINT, PRIMARY KEY (id))"
	const join = "SELECT u.id FROM u, v WHERE u.a = v.a"
	const pushOrdering = "PushRequestedOrderingThroughSelectRule"
	for _, tc := range []struct {
		name, sql string
		dml       bool
		disabled  []string
		rule      string
		count     int
	}{
		{"partition select: 2^3-1 subsets", "SELECT t.id FROM t, u, v WHERE t.id = u.id AND u.id = v.id", false, nil, "PartitionSelectRule", 7},
		{"partition select: 2^4-1 subsets", "SELECT t.id FROM t, u, v, w WHERE t.id = u.id AND u.id = v.id AND v.id = w.id", false, nil, "PartitionSelectRule", 15},
		{"ordering push: ForEach quantifiers", join, false, nil, pushOrdering, 2},
		{"partition binary select: both orders", join, false, []string{pushOrdering}, "PartitionBinarySelectRule", 2},
		{"nested loop join: both orders", join, false, []string{pushOrdering, "PartitionBinarySelectRule"}, "ImplementNestedLoopJoinRule", 2},
		{"simple select: plan partitions", "SELECT id FROM t WHERE c = 1", false, nil, "ImplementSimpleSelectRule", 3},
		{"delete: stored-record partitions", "DELETE FROM t", true, nil, "ImplementDeleteRule", 3},
		{"update: stored-record partitions", "UPDATE t SET c = 2", true, nil, "ImplementUpdateRule", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := func(bound int) (lastRule string, err error) {
				b := api.NewOptionsBuilder().Set(api.OptMaxNumMatchesPerRuleCall, bound)
				if tc.disabled != nil {
					b = b.Set(api.OptDisabledPlannerRules, tc.disabled)
				}
				popts := plannerOptionsFrom(b.Build())
				popts.ruleObserver = func(c cascades.ObservedRuleCall) { lastRule = c.Rule }
				if tc.dml {
					_, err = planPhysicalDMLForTest(tc.sql, ddl, nil, nil, nil, popts)
				} else {
					_, _, err = planPhysicalForTest(tc.sql, ddl, nil, false, nil, popts)
				}
				return lastRule, err
			}
			if last, err := plan(tc.count - 1); !errors.Is(err, cascades.ErrPlannerRuleMatchCapHit) || last != tc.rule {
				t.Fatalf("bound %d: err = %v after %s; want %s to trip", tc.count-1, err, last, tc.rule)
			}
			if _, err := plan(tc.count); err != nil {
				t.Fatalf("bound %d: %v; want a plan", tc.count, err)
			}
		})
	}
}
