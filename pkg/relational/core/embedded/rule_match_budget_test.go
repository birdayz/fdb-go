package embedded

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// MAX_NUM_MATCHES_PER_RULE_CALL bounds the bindings Java's matcher would make
// per rule call. A filter over a table with two indexes reaches rules whose
// inputs hold several plan partitions (the scan and each index's ordering), so
// a bound of 2 now trips where a Go root binding used to count 1; the same
// query plans with the bound unset and with a generous one.
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
