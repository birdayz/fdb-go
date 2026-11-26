package rowdiff

// Oracle-isolation validation for CONCAT — the one string function that does NOT
// propagate NULL. The engine treats a NULL operand as "" (verified by probe:
// CONCAT(NULL,'x')='x'), so the result is never NULL and a NULL-s row is NOT
// UNKNOWN — it compares as suffix-only. That distinguishes CONCAT from
// UPPER/LOWER/LENGTH/SUBSTR and is the axis this test pins.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestConcatOracle_HandComputed(t *testing.T) {
	tbl := TableDef{Name: "t", Cols: []ColumnDef{{Name: "S", Type: ColString}}}
	//   ID=1 "alpha" ; ID=2 "" ; ID=3 NULL
	rows := []Row{
		{"ID": int64(1), "S": "alpha"},
		{"ID": int64(2), "S": ""},
		{"ID": int64(3), "S": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	cc := func(suffix string, op predicates.ComparisonType, lit string) *Pred {
		return &Pred{StrFn: &StrFnSpec{Fn: StrFnConcat, Col: "S", Suffix: suffix}, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// CONCAT(s,'x')='alphax': only alpha.
		{"word_suffix", cc("x", predicates.ComparisonEquals, "alphax"), []int64{1}},
		// CONCAT(s,'x')='x': the empty-string row AND the NULL row (NULL→"") —
		// the NULL-absorption case. NOT alpha.
		{"null_as_empty", cc("x", predicates.ComparisonEquals, "x"), []int64{2, 3}},
		// CONCAT(s,'')='alpha': empty suffix, only alpha (NULL→"" ≠ 'alpha').
		{"empty_suffix", cc("", predicates.ComparisonEquals, "alpha"), []int64{1}},
		// CONCAT(s,'z') <> 'z': alpha→"alphaz"≠"z" ✓; ""→"z"=="z" ✗; NULL→"z" ✗.
		{"ne", cc("z", predicates.ComparisonNotEquals, "z"), []int64{1}},
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
				t.Errorf("CONCAT oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
