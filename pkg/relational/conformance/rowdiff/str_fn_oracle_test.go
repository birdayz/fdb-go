package rowdiff

// Oracle-isolation validation for string-function predicate leaves
// `<Fn>(s) <cmp> lit`. The oracle uses Go's strings.ToUpper/ToLower/len, which
// match the engine's UPPER/LOWER/LENGTH over the ASCII string domain (verified
// by probe); a NULL column makes the result NULL so the comparison is UNKNOWN.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestStrFnOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{{Name: "S", Type: ColString}},
	}
	//   ID=1 S="alpha" (len 5) ; ID=2 S="beta" (len 4)
	//   ID=3 S=""      (len 0) ; ID=4 S=NULL
	rows := []Row{
		{"ID": int64(1), "S": "alpha"},
		{"ID": int64(2), "S": "beta"},
		{"ID": int64(3), "S": ""},
		{"ID": int64(4), "S": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// UPPER(s) = 'ALPHA' → only id1; NULL (id4) is UNKNOWN.
		{"upper_eq", &Pred{StrFn: &StrFnSpec{Fn: StrFnUpper, Col: "S"}, Op: predicates.ComparisonEquals, Lit: "ALPHA"}, []int64{1}},
		// LOWER(s) = 'beta' → only id2.
		{"lower_eq", &Pred{StrFn: &StrFnSpec{Fn: StrFnLower, Col: "S"}, Op: predicates.ComparisonEquals, Lit: "beta"}, []int64{2}},
		// LENGTH(s) > 3 → id1 (5), id2 (4); id3 (0) no; id4 NULL UNKNOWN.
		{"length_gt", &Pred{StrFn: &StrFnSpec{Fn: StrFnLength, Col: "S"}, Op: predicates.ComparisonGreaterThan, Lit: int64(3)}, []int64{1, 2}},
		// LENGTH(s) = 0 → only the empty string (id3); NULL excluded.
		{"length_zero", &Pred{StrFn: &StrFnSpec{Fn: StrFnLength, Col: "S"}, Op: predicates.ComparisonEquals, Lit: int64(0)}, []int64{3}},
		// UPPER(s) <> 'ALPHA' → id2, id3 (not id1; id4 NULL UNKNOWN).
		{"upper_ne", &Pred{StrFn: &StrFnSpec{Fn: StrFnUpper, Col: "S"}, Op: predicates.ComparisonNotEquals, Lit: "ALPHA"}, []int64{2, 3}},
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
				t.Errorf("string-fn oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
