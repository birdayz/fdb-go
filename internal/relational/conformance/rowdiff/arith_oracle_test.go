package rowdiff

// Oracle-isolation validation for arithmetic-in-predicate leaves
// `(col - col2) <cmp> lit`. The oracle evaluates the subtraction through the
// engine's own ArithmeticValue (baked constants), but the NULL-propagation
// wiring around it is harness code and must be proven independently: a NULL in
// either operand makes the LHS NULL, so the comparison is UNKNOWN and the row
// drops. Each case is hand-computed.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestArithOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=10  B=3   → A-B=7
	//   ID=2 A=5   B=8   → A-B=-3
	//   ID=3 A=nil B=8   → A-B = NULL (A is NULL)
	rows := []Row{
		{"ID": int64(1), "A": int64(10), "B": int64(3)},
		{"ID": int64(2), "A": int64(5), "B": int64(8)},
		{"ID": int64(3), "A": nil, "B": int64(8)},
	}
	c := &Case{Table: tbl, Rows: rows}

	arith := func(col, col2 string, op predicates.ComparisonType, lit int64) *Pred {
		return &Pred{Col: col, HasArith: true, ArithOp: values.OpSub, ArithCol2: col2, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// (A-B) > 0: id1 7>0 yes, id2 -3>0 no, id3 NULL→UNKNOWN. → {1}.
		{"a_minus_b_gt0", arith("A", "B", predicates.ComparisonGreaterThan, 0), []int64{1}},
		// (A-B) < 0: id2 only.
		{"a_minus_b_lt0", arith("A", "B", predicates.ComparisonLessThan, 0), []int64{2}},
		// (A-B) = -3: id2 only.
		{"a_minus_b_eq_neg3", arith("A", "B", predicates.ComparisonEquals, -3), []int64{2}},
		// (B-A) > 0: id1 3-10=-7 no, id2 8-5=3 yes, id3 8-NULL=NULL UNKNOWN. → {2}.
		// (operand order matters — distinct from A-B.)
		{"b_minus_a_gt0", arith("B", "A", predicates.ComparisonGreaterThan, 0), []int64{2}},
		// (A-A) = 0: id1,id2 → 0=0 yes; id3 NULL-NULL=NULL → UNKNOWN. → {1,2}.
		{"a_minus_a_eq0", arith("A", "A", predicates.ComparisonEquals, 0), []int64{1, 2}},
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
				t.Errorf("arith oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
