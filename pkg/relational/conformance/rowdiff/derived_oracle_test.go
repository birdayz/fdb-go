package rowdiff

// Oracle-isolation validation for derived-table queries. The inner is SELECT *,
// so the derived table flattens to `WHERE Inner AND Outer`; the oracle filters
// by both and finalizes via the shared single-table tail. Each case is
// hand-computed.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestDerivedOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=3 B=10 ; ID=2 A=7 B=20 ; ID=3 A=3 B=5 ; ID=4 A=9 B=1
	rows := []Row{
		{"ID": int64(1), "A": int64(3), "B": int64(10)},
		{"ID": int64(2), "A": int64(7), "B": int64(20)},
		{"ID": int64(3), "A": int64(3), "B": int64(5)},
		{"ID": int64(4), "A": int64(9), "B": int64(1)},
	}
	c := &Case{Table: tbl, Rows: rows}

	leaf := func(col string, op predicates.ComparisonType, lit int64) *BoolNode {
		return &BoolNode{Leaf: &Pred{Col: col, Op: op, Lit: lit}}
	}

	cases := []struct {
		name string
		q    Query
		proj []string
		want []int64 // by ID, or by projected value for the DISTINCT-A case
	}{
		// inner A>2 (all), outer B<15 → B=10,5,1 → id 1,3,4.
		{"inner_outer", Query{Derived: &DerivedSpec{Inner: leaf("A", predicates.ComparisonGreaterThan, 2), Outer: leaf("B", predicates.ComparisonLessThan, 15)}}, []string{"ID"}, []int64{1, 3, 4}},
		// inner A=3 → {1,3}, outer B>6 → only id1 (B=10).
		{"inner_and_outer", Query{Derived: &DerivedSpec{Inner: leaf("A", predicates.ComparisonEquals, 3), Outer: leaf("B", predicates.ComparisonGreaterThan, 6)}}, []string{"ID"}, []int64{1}},
		// inner B>=5, no outer → id 1,2,3.
		{"inner_only", Query{Derived: &DerivedSpec{Inner: leaf("B", predicates.ComparisonGreaterThanEq, 5)}}, []string{"ID"}, []int64{1, 2, 3}},
		// DISTINCT over [A]: inner A>0 (all), project A → distinct {3,7,9}.
		{"distinct_a", Query{Distinct: true, Derived: &DerivedSpec{Inner: leaf("A", predicates.ComparisonGreaterThan, 0)}}, []string{"A"}, []int64{3, 7, 9}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, err := OracleRows(c, tc.q, tc.proj)
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			key := tc.proj[0]
			got := make([]int64, 0, len(out))
			for _, r := range out {
				got = append(got, r[key].(int64))
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if !equalInt64(got, tc.want) {
				t.Errorf("derived oracle %s: got %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(tc.q, tc.proj))
			}
		})
	}
}
