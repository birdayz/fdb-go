package rowdiff

// Oracle-isolation validation for TRIM over whitespace-padded strings. SQL TRIM
// strips leading/trailing spaces (Go's strings.Trim over " ", verified by
// probe: TRIM(' ab ')='ab'), so a padded value matches its trimmed literal even
// though the RAW value would not (" a" < "a" lexicographically, but
// TRIM(" a")="a"). A NULL column makes TRIM NULL → the comparison is UNKNOWN.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestTrimOracle_HandComputed(t *testing.T) {
	tbl := TableDef{Name: "t", Cols: []ColumnDef{{Name: "S", Type: ColString}}}
	//   ID=1 " a" ; ID=2 "b " ; ID=3 " c " ; ID=4 "alpha" ; ID=5 NULL
	rows := []Row{
		{"ID": int64(1), "S": " a"},
		{"ID": int64(2), "S": "b "},
		{"ID": int64(3), "S": " c "},
		{"ID": int64(4), "S": "alpha"},
		{"ID": int64(5), "S": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	trim := func(op predicates.ComparisonType, lit string) *Pred {
		return &Pred{StrFn: &StrFnSpec{Fn: StrFnTrim, Col: "S"}, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// TRIM(s)='a': " a"→"a" (the padded row matches its trimmed literal).
		{"lead_space", trim(predicates.ComparisonEquals, "a"), []int64{1}},
		// TRIM(s)='b': "b "→"b" (trailing).
		{"trail_space", trim(predicates.ComparisonEquals, "b"), []int64{2}},
		// TRIM(s)='c': " c "→"c" (both).
		{"both_space", trim(predicates.ComparisonEquals, "c"), []int64{3}},
		// TRIM(s)='alpha': the unpadded row.
		{"no_space", trim(predicates.ComparisonEquals, "alpha"), []int64{4}},
		// TRIM(s) < 'b': "a"<"b" (id1) and "alpha"<"b" (id4); "b" and "c" fail;
		// NULL UNKNOWN. Proves the compare is over the TRIMMED value.
		{"lt_trimmed", trim(predicates.ComparisonLessThan, "b"), []int64{1, 4}},
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
				t.Errorf("TRIM oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
