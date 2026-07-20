package rowdiff

// Oracle-isolation validation for UNION [ALL]. UNION dedups the combined output
// over the PROJECTED columns; UNION ALL keeps every row. A dup-A row makes the
// projection matter: projecting only A collapses two rows that projecting the pk
// keeps distinct. Each case is hand-computed.

import (
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestUnionOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{{Name: "A", Type: ColBigint}},
	}
	//   ID=1 A=5 ; ID=2 A=7 ; ID=3 A=5  (id1,id3 share A=5)
	rows := []Row{
		{"ID": int64(1), "A": int64(5)},
		{"ID": int64(2), "A": int64(7)},
		{"ID": int64(3), "A": int64(5)},
	}
	c := &Case{Table: tbl, Rows: rows}

	leaf := func(op predicates.ComparisonType, lit int64) *BoolNode {
		return &BoolNode{Leaf: &Pred{Col: "A", Op: op, Lit: lit}}
	}
	// signature multiset over a projected row's columns.
	sig := func(r Row, cols []string) string {
		parts := make([]string, len(cols))
		for i, col := range cols {
			parts[i] = fmt.Sprintf("%s=%v", col, r[col])
		}
		return fmt.Sprint(parts)
	}
	sigs := func(rs []Row, cols []string) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, sig(r, cols))
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
		u    *UnionSpec
		proj []string
		want []Row // expected multiset (projected)
	}{
		// UNION, overlapping branches, project (ID,A): dedup by pk → 3 rows.
		{
			"union_overlap", &UnionSpec{Left: leaf(predicates.ComparisonEquals, 5), Right: leaf(predicates.ComparisonGreaterThanEq, 5)},
			[]string{"ID", "A"},
			[]Row{{"ID": int64(1), "A": int64(5)}, {"ID": int64(2), "A": int64(7)}, {"ID": int64(3), "A": int64(5)}},
		},
		// UNION ALL, same: 5 rows (the two overlap rows appear twice).
		{
			"unionall_overlap", &UnionSpec{Left: leaf(predicates.ComparisonEquals, 5), Right: leaf(predicates.ComparisonGreaterThanEq, 5), All: true},
			[]string{"ID", "A"},
			[]Row{{"ID": int64(1), "A": int64(5)}, {"ID": int64(3), "A": int64(5)}, {"ID": int64(1), "A": int64(5)}, {"ID": int64(2), "A": int64(7)}, {"ID": int64(3), "A": int64(5)}},
		},
		// UNION project only A: the two A=5 rows collapse → {5,7} = 2 rows.
		{
			"union_proj_a", &UnionSpec{Left: leaf(predicates.ComparisonEquals, 5), Right: leaf(predicates.ComparisonEquals, 7)},
			[]string{"A"},
			[]Row{{"A": int64(5)}, {"A": int64(7)}},
		},
		// UNION ALL project only A: [5,5,7] = 3 rows.
		{
			"unionall_proj_a", &UnionSpec{Left: leaf(predicates.ComparisonEquals, 5), Right: leaf(predicates.ComparisonEquals, 7), All: true},
			[]string{"A"},
			[]Row{{"A": int64(5)}, {"A": int64(5)}, {"A": int64(7)}},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{Union: tc.u}
			out, err := OracleRows(c, q, tc.proj)
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			if g, w := sigs(out, tc.proj), sigs(tc.want, tc.proj); !eq(g, w) {
				t.Errorf("UNION oracle %s:\n got  %v\n want %v\n SQL: %s", tc.name, g, w, c.SQL(q, tc.proj))
			}
		})
	}
}
