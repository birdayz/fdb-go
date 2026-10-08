package sqltest

import (
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestAggregateIndexRowsAgree(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		idx, oracle []string
		nAgg        int
		want        bool
	}{
		{"equal", []string{"1|2", "3|4"}, []string{"1|2", "3|4"}, 1, true},
		{"vacated_residue", []string{"1|2", "2|0", "3|4"}, []string{"1|2", "3|4"}, 1, true},
		{"all_null_group_absent", []string{"1|2"}, []string{"1|2", "3|NULL"}, 1, true},
		{"sum_residue", []string{"1|0"}, []string{"1|NULL"}, 1, true},
		{"intersection_drops_all_null_sum", []string{"1|5|2"}, []string{"1|5|2", "2|NULL|3"}, 2, true},
		{"live_value_differs", []string{"1|3"}, []string{"1|2"}, 1, false},
		{"order_differs", []string{"3|4", "1|2"}, []string{"1|2", "3|4"}, 1, false},
		{"extra_nonzero_group", []string{"1|2", "2|7"}, []string{"1|2"}, 1, false},
		{"missing_live_group", []string{"1|2"}, []string{"1|2", "3|4"}, 1, false},
		{"zero_for_live_value", []string{"1|0"}, []string{"1|2"}, 1, false},
	} {
		if got := testkit.MmAggregateIndexRowsAgree(c.idx, c.oracle, c.nAgg); got != c.want {
			t.Errorf("%s: agree = %v, want %v", c.name, got, c.want)
		}
	}
	if n := testkit.MmTrailingAggregates("SELECT c.id, (SELECT SUM(v) FROM t WHERE x) FROM cust"); n != 1 {
		t.Errorf("trailing aggregates = %d, want 1", n)
	}
}
