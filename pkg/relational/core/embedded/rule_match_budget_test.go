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
