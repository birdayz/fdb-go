package rowdiff

// Oracle-isolation validation for COALESCE — like CONCAT, it ABSORBS NULL: a
// NULL column yields the Default (never NULL, verified by probe:
// COALESCE(NULL,5)=5), so a NULL-column row compares as the default rather than
// dropping as UNKNOWN. That NULL-absorption is the axis this test pins.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestCoalesceOracle_HandComputed(t *testing.T) {
	tbl := TableDef{Name: "t", Cols: []ColumnDef{{Name: "A", Type: ColBigint}}}
	//   ID=1 A=7 ; ID=2 A=3 ; ID=3 A=NULL
	rows := []Row{
		{"ID": int64(1), "A": int64(7)},
		{"ID": int64(2), "A": int64(3)},
		{"ID": int64(3), "A": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	co := func(def int64, op predicates.ComparisonType, lit int64) *Pred {
		return &Pred{NumFn: &NumFnSpec{Fn: NumFnCoalesce, Col: "A", Default: def}, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// COALESCE(a,5)=7: only a=7.
		{"eq_value", co(5, predicates.ComparisonEquals, 7), []int64{1}},
		// COALESCE(a,5)=5: only the NULL row (→5) — the absorption case.
		{"eq_default_null", co(5, predicates.ComparisonEquals, 5), []int64{3}},
		// COALESCE(a,5)>4: 7 and 5(NULL) pass; 3 fails.
		{"gt", co(5, predicates.ComparisonGreaterThan, 4), []int64{1, 3}},
		// COALESCE(a,3)=3: the a=3 row AND the NULL→3 row.
		{"value_meets_default", co(3, predicates.ComparisonEquals, 3), []int64{2, 3}},
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
				t.Errorf("COALESCE oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
