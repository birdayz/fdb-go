package rowdiff

// Oracle-isolation validation for numeric-function predicate leaves
// `ABS(col) op lit` / `MOD(col, k) op lit`. The oracle uses Go's abs and %,
// which match the engine over the BIGINT domain (verified by probe: no ABS
// overflow, and % follows the dividend's sign like SQL MOD, e.g. MOD(-8,3)=-2).
// A NULL column makes the result NULL → the comparison is UNKNOWN.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestNumFnOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{{Name: "A", Type: ColBigint}},
	}
	//   ID=1 A=-3 ; ID=2 A=7 ; ID=3 A=-8 ; ID=4 A=NULL
	rows := []Row{
		{"ID": int64(1), "A": int64(-3)},
		{"ID": int64(2), "A": int64(7)},
		{"ID": int64(3), "A": int64(-8)},
		{"ID": int64(4), "A": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// ABS(A) > 5: |−3|=3 no, |7|=7 yes, |−8|=8 yes, NULL UNKNOWN.
		{"abs_gt", &Pred{NumFn: &NumFnSpec{Fn: NumFnAbs, Col: "A"}, Op: predicates.ComparisonGreaterThan, Lit: int64(5)}, []int64{2, 3}},
		// ABS(A) = 3: |−3|=3.
		{"abs_eq", &Pred{NumFn: &NumFnSpec{Fn: NumFnAbs, Col: "A"}, Op: predicates.ComparisonEquals, Lit: int64(3)}, []int64{1}},
		// MOD(A,3)=1: (-3)%3=0, 7%3=1, (-8)%3=-2 → only id2.
		{"mod_eq_1", &Pred{NumFn: &NumFnSpec{Fn: NumFnMod, Col: "A", Mod: 3}, Op: predicates.ComparisonEquals, Lit: int64(1)}, []int64{2}},
		// MOD(A,3)=-2: the negative remainder — only (-8)%3=-2 (id3).
		{"mod_eq_neg2", &Pred{NumFn: &NumFnSpec{Fn: NumFnMod, Col: "A", Mod: 3}, Op: predicates.ComparisonEquals, Lit: int64(-2)}, []int64{3}},
		// MOD(A,7)=0: 7%7=0 → only id2.
		{"mod_eq_0", &Pred{NumFn: &NumFnSpec{Fn: NumFnMod, Col: "A", Mod: 7}, Op: predicates.ComparisonEquals, Lit: int64(0)}, []int64{2}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{Where: &BoolNode{Leaf: tc.p}}
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
				t.Errorf("num-fn oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
