package rowdiff

// Oracle-isolation validation for MULTI-COLUMN GROUP BY. The aggregate oracle
// is the single reimplemented path (aggregation is not expressible through the
// engine's per-row Comparison eval), so a composite-key mistake here would
// manufacture false findings rather than catch real ones. Each case is
// hand-computed; a NULL in ANY key column must form its own composite group.

import (
	"fmt"
	"sort"
	"testing"
)

func TestAggMultiColOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=1 B=10
	//   ID=2 A=1 B=20
	//   ID=3 A=2 B=10
	//   ID=4 A=NULL B=10   (NULL key → its own composite group)
	//   ID=5 A=1 B=10
	rows := []Row{
		{"ID": int64(1), "A": int64(1), "B": int64(10)},
		{"ID": int64(2), "A": int64(1), "B": int64(20)},
		{"ID": int64(3), "A": int64(2), "B": int64(10)},
		{"ID": int64(4), "A": nil, "B": int64(10)},
		{"ID": int64(5), "A": int64(1), "B": int64(10)},
	}
	c := &Case{Table: tbl, Rows: rows}

	sig := func(r Row) string {
		// Canonical signature over whatever G* / AGG columns the row carries.
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b []string
		for _, k := range keys {
			b = append(b, fmt.Sprintf("%s=%v", k, r[k]))
		}
		return fmt.Sprint(b)
	}
	sigs := func(rs []Row) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, sig(r))
		}
		sort.Strings(out)
		return out
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	cases := []struct {
		name string
		agg  *AggSpec
		want []Row
	}{
		// Two-column COUNT(*): composite groups (A,B). (1,10)={1,5}=2,
		// (1,20)={2}=1, (2,10)={3}=1, (NULL,10)={4}=1.
		{"count_by_a_b", &AggSpec{Func: AggCountStar, GroupBy: []string{"A", "B"}}, []Row{
			{"G": int64(1), "G1": int64(10), "AGG": int64(2)},
			{"G": int64(1), "G1": int64(20), "AGG": int64(1)},
			{"G": int64(2), "G1": int64(10), "AGG": int64(1)},
			{"G": nil, "G1": int64(10), "AGG": int64(1)},
		}},
		// Two-column SUM(B): same groups, sum of B per group.
		{"sum_b_by_a_b", &AggSpec{Func: AggSum, Col: "B", GroupBy: []string{"A", "B"}}, []Row{
			{"G": int64(1), "G1": int64(10), "AGG": int64(20)},
			{"G": int64(1), "G1": int64(20), "AGG": int64(20)},
			{"G": int64(2), "G1": int64(10), "AGG": int64(10)},
			{"G": nil, "G1": int64(10), "AGG": int64(10)},
		}},
		// Single-column control: GROUP BY a stays byte-identical to the old
		// "G"/"AGG" schema. A=1={1,2,5}=3, A=2={3}=1, A=NULL={4}=1.
		{"count_by_a", &AggSpec{Func: AggCountStar, GroupBy: []string{"A"}}, []Row{
			{"G": int64(1), "AGG": int64(3)},
			{"G": int64(2), "AGG": int64(1)},
			{"G": nil, "AGG": int64(1)},
		}},
		// Key ORDER swapped (B,A): the composite still partitions the same rows
		// but the output columns bind G=B, G1=A. (10,1)={1,5}=2, (20,1)={2}=1,
		// (10,2)={3}=1, (10,NULL)={4}=1.
		{"count_by_b_a", &AggSpec{Func: AggCountStar, GroupBy: []string{"B", "A"}}, []Row{
			{"G": int64(10), "G1": int64(1), "AGG": int64(2)},
			{"G": int64(20), "G1": int64(1), "AGG": int64(1)},
			{"G": int64(10), "G1": int64(2), "AGG": int64(1)},
			{"G": int64(10), "G1": nil, "AGG": int64(1)},
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{Agg: tc.agg}
			got, err := OracleRows(c, q, nil)
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			if gs, ws := sigs(got), sigs(tc.want); !eq(gs, ws) {
				t.Errorf("multi-col agg %s:\n got  %v\n want %v\n SQL: %s",
					tc.name, gs, ws, c.SQL(q, nil))
			}
		})
	}
}
