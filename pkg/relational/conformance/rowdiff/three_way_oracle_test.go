package rowdiff

// Oracle-isolation validation for the 3-way self-join. The triple nested loop
// and the L/M/R row keying are new; a mistake would manufacture false findings
// against every 3-way query. Each case is hand-computed over a 3-row table with
// the chain L.A = M.B AND M.A = R.B.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestThreeWayOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=2 B=3
	//   ID=2 A=3 B=1
	//   ID=3 A=1 B=2
	// Chain L.A=M.B, M.A=R.B → the unique closed cycle:
	//   (L1,M3,R2), (L2,M1,R3), (L3,M2,R1)
	rows := []Row{
		{"ID": int64(1), "A": int64(2), "B": int64(3)},
		{"ID": int64(2), "A": int64(3), "B": int64(1)},
		{"ID": int64(3), "A": int64(1), "B": int64(2)},
	}
	c := &Case{Table: tbl, Rows: rows}
	spec := &ThreeWayJoinSpec{LMLeft: "A", LMRight: "B", MRMid: "A", MRRight: "B"}

	leaf := func(qual, col string, op predicates.ComparisonType, lit int64) *BoolNode {
		return &BoolNode{Leaf: &Pred{Qual: qual, Col: col, Op: op, Lit: lit}}
	}

	type triple struct{ l, m, r int64 }
	cases := []struct {
		name  string
		where *BoolNode
		want  []triple
	}{
		{"no_where", nil, []triple{{1, 3, 2}, {2, 1, 3}, {3, 2, 1}}},
		// L.ID = 1 → only the (1,3,2) tuple.
		{"lside", leaf("L", "ID", predicates.ComparisonEquals, 1), []triple{{1, 3, 2}}},
		// R.B > 1: R.B is r2=1, r3=2, r1=3 for the three tuples → keep the ones
		// whose R is id 3 or 1.
		{"rside", leaf("R", "B", predicates.ComparisonGreaterThan, 1), []triple{{2, 1, 3}, {3, 2, 1}}},
		// M.A = 1: M.A is m3=1, m1=2, m2=3 → only the tuple whose M is id 3.
		{"mside", leaf("M", "A", predicates.ComparisonEquals, 1), []triple{{1, 3, 2}}},
	}

	norm := func(rs []Row) []triple {
		out := make([]triple, 0, len(rs))
		for _, r := range rs {
			out = append(out, triple{r["L_ID"].(int64), r["M_ID"].(int64), r["R_ID"].(int64)})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].l != out[j].l {
				return out[i].l < out[j].l
			}
			return out[i].m < out[j].m
		})
		return out
	}
	eq := func(a, b []triple) bool {
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

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{ThreeWay: spec, Where: tc.where}
			out, err := OracleRows(c, q, []string{"L.ID", "M.ID", "R.ID"})
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			got := norm(out)
			want := append([]triple(nil), tc.want...)
			sort.Slice(want, func(i, j int) bool {
				if want[i].l != want[j].l {
					return want[i].l < want[j].l
				}
				return want[i].m < want[j].m
			})
			if !eq(got, want) {
				t.Errorf("3-way oracle %s: got %v, want %v\nSQL: %s",
					tc.name, got, want, c.SQL(q, []string{"L.ID", "M.ID", "R.ID"}))
			}
		})
	}
}
