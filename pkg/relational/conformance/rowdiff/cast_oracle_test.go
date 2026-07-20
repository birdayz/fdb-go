package rowdiff

// Oracle-isolation validation for CAST(col AS STRING). int→string is Go's
// FormatInt, bool→string is "true"/"false" (both match the engine, verified by
// probe). The comparison is then LEXICOGRAPHIC over the string image — e.g.
// CAST(12 AS STRING) < '2' is TRUE ("12" < "2"), unlike a numeric 12 < 2 — so
// this pins that the cast produces a string and the compare is string-order. A
// NULL column makes CAST NULL → the comparison is UNKNOWN.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestCastOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "F", Type: ColBoolean},
		},
	}
	//   ID=1 A=7   F=true  ; ID=2 A=-1 F=false
	//   ID=3 A=NULL F=true ; ID=4 A=12 F=NULL
	rows := []Row{
		{"ID": int64(1), "A": int64(7), "F": true},
		{"ID": int64(2), "A": int64(-1), "F": false},
		{"ID": int64(3), "A": nil, "F": true},
		{"ID": int64(4), "A": int64(12), "F": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	castI := func(op predicates.ComparisonType, lit string) *Pred {
		return &Pred{Cast: &CastSpec{Col: "A", FromInt: true}, Op: op, Lit: lit}
	}
	castB := func(op predicates.ComparisonType, lit string) *Pred {
		return &Pred{Cast: &CastSpec{Col: "F", FromInt: false}, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// CAST(a AS STRING) = '7' → only a=7.
		{"int_eq", castI(predicates.ComparisonEquals, "7"), []int64{1}},
		// = '-1' → only a=-1 (image "-1").
		{"int_neg", castI(predicates.ComparisonEquals, "-1"), []int64{2}},
		// LEXICOGRAPHIC: CAST(a AS STRING) < '2' → "-1"<"2" (id2), "12"<"2" (id4);
		// "7"<"2" is FALSE; NULL UNKNOWN. NOT the numeric interpretation.
		{"int_lex", castI(predicates.ComparisonLessThan, "2"), []int64{2, 4}},
		// CAST(f AS STRING) = 'true' → the true rows (id1, id3); NULL-f UNKNOWN.
		{"bool_true", castB(predicates.ComparisonEquals, "true"), []int64{1, 3}},
		// = 'false' → only id2.
		{"bool_false", castB(predicates.ComparisonEquals, "false"), []int64{2}},
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
				t.Errorf("CAST oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
