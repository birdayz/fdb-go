package rowdiff

// Oracle-isolation validation for RIGHT OUTER JOIN — the mirror of the LEFT
// case: every RIGHT row is preserved, unmatched LEFT is NULL-extended. The
// WHERE-vs-ON subtlety mirrors too: an L-side filter drops the NULL-extended
// rows (l.col is NULL → predicate UNKNOWN), while an R-side filter keeps/drops
// on real R values (R is the preserved side). Each case is hand-computed.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestRightJoinOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	//   ID=1 A=2  B=10
	//   ID=2 A=3  B=2
	//   ID=3 A=99 B=3
	// l RIGHT JOIN r ON l.A = r.B, matching L.A to R.B:
	//   r1(B=10) ← no L (A∈{2,3,99}) → NULL-L
	//   r2(B=2)  ← l1(A=2)
	//   r3(B=3)  ← l2(A=3)
	rows := []Row{
		{"ID": int64(1), "A": int64(2), "B": int64(10)},
		{"ID": int64(2), "A": int64(3), "B": int64(2)},
		{"ID": int64(3), "A": int64(99), "B": int64(3)},
	}
	c := &Case{Table: tbl, Rows: rows}
	spec := &JoinSpec{LeftCol: "A", RightCol: "B", Inner: true, RightOuter: true}

	leaf := func(qual, col string, op predicates.ComparisonType, lit any) *BoolNode {
		return &BoolNode{Leaf: &Pred{Qual: qual, Col: col, Op: op, Lit: lit}}
	}

	type pair struct{ r, l int64 } // l = -1 encodes a NULL L.ID
	cases := []struct {
		name  string
		where *BoolNode
		want  []pair
	}{
		// No WHERE: every R row appears; r1 has a NULL L. (INNER would drop r1.)
		{"no_where", nil, []pair{{1, -1}, {2, 1}, {3, 2}}},
		// L.ID IS NOT NULL drops the NULL-extended row → inner-like.
		{"lid_not_null", leaf("L", "ID", predicates.ComparisonIsNotNull, nil), []pair{{2, 1}, {3, 2}}},
		// R.B > 5: R is preserved, so this keeps on real R.B — only r1 (B=10),
		// which is the NULL-L row → it SURVIVES the R-side filter.
		{"rside_filter", leaf("R", "B", predicates.ComparisonGreaterThan, int64(5)), []pair{{1, -1}}},
		// L.A = 3: r3's L (id2) has A=3 → kept; r2's L (id1) A=2 → dropped;
		// r1's L is NULL → UNKNOWN → dropped.
		{"lside_filter", leaf("L", "A", predicates.ComparisonEquals, int64(3)), []pair{{3, 2}}},
	}

	norm := func(rs []Row) []pair {
		out := make([]pair, 0, len(rs))
		for _, r := range rs {
			p := pair{r: r["R_ID"].(int64), l: -1}
			if v, ok := r["L_ID"].(int64); ok {
				p.l = v
			}
			out = append(out, p)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].r != out[j].r {
				return out[i].r < out[j].r
			}
			return out[i].l < out[j].l
		})
		return out
	}
	eq := func(a, b []pair) bool {
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
			q := Query{Join: spec, Where: tc.where}
			out, err := OracleRows(c, q, []string{"R.ID", "L.ID"})
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			got := norm(out)
			want := append([]pair(nil), tc.want...)
			sort.Slice(want, func(i, j int) bool {
				if want[i].r != want[j].r {
					return want[i].r < want[j].r
				}
				return want[i].l < want[j].l
			})
			if !eq(got, want) {
				t.Errorf("RIGHT JOIN oracle %s: got %v, want %v\nSQL: %s",
					tc.name, got, want, c.SQL(q, []string{"R.ID", "L.ID"}))
			}
		})
	}
}
