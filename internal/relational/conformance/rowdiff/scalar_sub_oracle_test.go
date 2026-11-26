package rowdiff

// Oracle-isolation validation for the non-correlated aggregate scalar subquery.
// This is a Go read-side extension with no Java equivalent, so the oracle is the
// sole authority — it must be proven correct WITHOUT the engine. The axis most
// worth pinning is NULL-when-empty: an empty MIN/MAX subquery yields NULL, and
// `col <op> NULL` is UNKNOWN so every row drops (the documented past defect),
// whereas an empty COUNT yields 0 and compares normally.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestScalarSubOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=1   B=10
	//   ID=2 A=5   B=20
	//   ID=3 A=8   B=30
	//   ID=4 A=nil B=40
	rows := []Row{
		{"ID": int64(1), "A": int64(1), "B": int64(10)},
		{"ID": int64(2), "A": int64(5), "B": int64(20)},
		{"ID": int64(3), "A": int64(8), "B": int64(30)},
		{"ID": int64(4), "A": nil, "B": int64(40)},
	}
	c := &Case{Table: tbl, Rows: rows}

	gtA100 := &Pred{Col: "A", Op: predicates.ComparisonGreaterThan, Lit: int64(100)}

	cases := []struct {
		name string
		s    *ScalarSubSpec
		want []int64
	}{
		// A > MIN(A)=1: 1>1 no, 5>1 yes, 8>1 yes, NULL>1 UNKNOWN → {2,3}.
		{"gt_min", &ScalarSubSpec{OuterCol: "A", Op: predicates.ComparisonGreaterThan, Func: AggMin, Col: "A"}, []int64{2, 3}},
		// A = MAX(A)=8: only ID 3.
		{"eq_max", &ScalarSubSpec{OuterCol: "A", Op: predicates.ComparisonEquals, Func: AggMax, Col: "A"}, []int64{3}},
		// A > MAX(A WHERE A>100): subquery EMPTY → MAX NULL → A > NULL UNKNOWN
		// for every row → {} (the NULL-when-empty trap).
		{"gt_empty_max", &ScalarSubSpec{OuterCol: "A", Op: predicates.ComparisonGreaterThan, Func: AggMax, Col: "A", Filter: gtA100}, []int64{}},
		// A >= COUNT(* WHERE A>100): empty COUNT is 0 (not NULL); A>=0 for
		// 1,5,8; NULL>=0 UNKNOWN → {1,2,3}.
		{"ge_empty_count", &ScalarSubSpec{OuterCol: "A", Op: predicates.ComparisonGreaterThanEq, Func: AggCountStar, Filter: gtA100}, []int64{1, 2, 3}},
		// A < COUNT(*)=4 (whole table): 1<4 yes; 5,8 no; NULL UNKNOWN → {1}.
		{"lt_count", &ScalarSubSpec{OuterCol: "A", Op: predicates.ComparisonLessThan, Func: AggCountStar}, []int64{1}},
		// COUNT(A)=3 (non-NULL count, skips ID4). B >= 3 for all B → {1,2,3,4}.
		{"ge_count_col", &ScalarSubSpec{OuterCol: "B", Op: predicates.ComparisonGreaterThanEq, Func: AggCountCol, Col: "A"}, []int64{1, 2, 3, 4}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{ScalarSub: tc.s}
			out, err := OracleRows(c, q, []string{"ID"})
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			got := make([]int64, 0, len(out))
			for _, r := range out {
				got = append(got, r["ID"].(int64))
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if !equalInt64(got, tc.want) {
				t.Errorf("scalar-sub oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
