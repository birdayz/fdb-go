package rowdiff

// Oracle-isolation validation for searched-CASE predicate leaves
// `(CASE WHEN cond THEN a ELSE b END) <cmp> lit`. The branch pick is the only
// restated logic; it must match SQL exactly — the WHEN arm is taken ONLY when
// the condition is TRUE (a FALSE or UNKNOWN/NULL condition falls to ELSE), and
// a selected NULL column makes the whole CASE NULL so the comparison is UNKNOWN.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestCaseOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
			{Name: "C", Type: ColBigint},
		},
	}
	//   ID=1 A=10  B=9 C=1
	//   ID=2 A=2   B=1 C=nil
	//   ID=3 A=nil B=9 C=1
	//   ID=4 A=2   B=1 C=9
	rows := []Row{
		{"ID": int64(1), "A": int64(10), "B": int64(9), "C": int64(1)},
		{"ID": int64(2), "A": int64(2), "B": int64(1), "C": nil},
		{"ID": int64(3), "A": nil, "B": int64(9), "C": int64(1)},
		{"ID": int64(4), "A": int64(2), "B": int64(1), "C": int64(9)},
	}
	c := &Case{Table: tbl, Rows: rows}

	when := func(col string, op predicates.ComparisonType, lit int64) *Pred {
		return &Pred{Col: col, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// (CASE WHEN A>5 THEN B ELSE C END) > 4:
		//   id1 A>5 T → B=9 >4 ✓ ; id2 A>5 F → C=NULL → UNKNOWN ✗ ;
		//   id3 A>5 UNKNOWN → C=1 >4? no ✗ ; id4 A>5 F → C=9 >4 ✓
		{"cols_arms", &Pred{
			Case: &CaseSpec{When: when("A", predicates.ComparisonGreaterThan, 5), ThenCol: "B", ElseCol: "C"},
			Op:   predicates.ComparisonGreaterThan, Lit: 4,
		}, []int64{1, 4}},
		// (CASE WHEN B>5 THEN 10 ELSE 0 END) > 5: TRUE for B>5 (id1,id3).
		{"lit_arms", &Pred{
			Case: &CaseSpec{When: when("B", predicates.ComparisonGreaterThan, 5), ThenLit: 10, ElseLit: 0},
			Op:   predicates.ComparisonGreaterThan, Lit: 5,
		}, []int64{1, 3}},
		// (CASE WHEN A=2 THEN 100 ELSE 0 END) > 50: TRUE only where A=2 (id2,id4);
		// id3 A=NULL → A=2 UNKNOWN → ELSE 0.
		{"when_null", &Pred{
			Case: &CaseSpec{When: when("A", predicates.ComparisonEquals, 2), ThenLit: 100, ElseLit: 0},
			Op:   predicates.ComparisonGreaterThan, Lit: 50,
		}, []int64{2, 4}},
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
				t.Errorf("CASE oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
