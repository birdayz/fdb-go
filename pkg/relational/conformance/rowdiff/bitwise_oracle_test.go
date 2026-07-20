package rowdiff

// Oracle-isolation validation for the bitwise LHS leaf `(A <op> B) <cmp> lit`.
// The oracle folds the bitwise op through the engine's ScalarFunctionValue; this
// pins that path against independently hand-computed results (binary arithmetic)
// and the NULL-propagation (a NULL operand → NULL LHS → UNKNOWN, row dropped).

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestBitwiseOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint},
		},
	}
	//   ID=1 A=6(110) B=3(011): A&B=2 A|B=7 A^B=5
	//   ID=2 A=12(1100) B=10(1010): A&B=8 A|B=14 A^B=6
	//   ID=3 A=5 B=NULL: all NULL → UNKNOWN
	rows := []Row{
		{"ID": int64(1), "A": int64(6), "B": int64(3)},
		{"ID": int64(2), "A": int64(12), "B": int64(10)},
		{"ID": int64(3), "A": int64(5), "B": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	bit := func(op string, cmp predicates.ComparisonType, lit int64) *Pred {
		return &Pred{Col: "A", Bitwise: true, BitOp: op, BitCol2: "B", Op: cmp, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		{"and_eq", bit("BITAND", predicates.ComparisonEquals, 2), []int64{1}},      // A&B: 2,8,NULL
		{"or_eq", bit("BITOR", predicates.ComparisonEquals, 7), []int64{1}},        // A|B: 7,14,NULL
		{"xor_eq", bit("BITXOR", predicates.ComparisonEquals, 6), []int64{2}},      // A^B: 5,6,NULL
		{"and_gt", bit("BITAND", predicates.ComparisonGreaterThan, 5), []int64{2}}, // A&B > 5
		// NULL operand: (A&B) = anything is UNKNOWN for id=3 — never matched.
		{"null_dropped", bit("BITAND", predicates.ComparisonEquals, 5), nil}, // no row has A&B=5
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
				t.Errorf("bitwise oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
