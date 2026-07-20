package rowdiff

// Oracle-isolation validation for LEFT OUTER JOIN. The join oracle now
// NULL-extends unmatched left rows; a mistake in either the match tracking or
// the WHERE-vs-ON handling would manufacture false findings. Each case is
// hand-computed. The axis most worth pinning: an unmatched left row survives an
// L-side WHERE filter but is dropped by an R-side one (the R column is NULL, so
// the predicate is UNKNOWN) — SQL's WHERE-after-LEFT-JOIN semantics.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestLeftJoinOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{{Name: "A", Type: ColBigint}},
	}
	//   ID=1 A=2    → l.A=2 matches r.ID=2
	//   ID=2 A=3    → l.A=3 matches r.ID=3
	//   ID=3 A=99   → l.A=99 matches nothing → NULL-extended
	rows := []Row{
		{"ID": int64(1), "A": int64(2)},
		{"ID": int64(2), "A": int64(3)},
		{"ID": int64(3), "A": int64(99)},
	}
	c := &Case{Table: tbl, Rows: rows}
	// LEFT JOIN r ON l.A = r.ID.
	spec := &JoinSpec{LeftCol: "A", RightCol: "ID", Inner: true, LeftOuter: true}

	leaf := func(qual, col string, op predicates.ComparisonType, lit any) *BoolNode {
		return &BoolNode{Leaf: &Pred{Qual: qual, Col: col, Op: op, Lit: lit}}
	}

	type pair struct{ l, r int64 } // r = -1 encodes a NULL R.ID
	cases := []struct {
		name  string
		where *BoolNode
		want  []pair
	}{
		// No WHERE: every left row appears; l=3 NULL-extended. (INNER would
		// drop l=3 → this is the LEFT-specific behavior.)
		{"no_where", nil, []pair{{1, 2}, {2, 3}, {3, -1}}},
		// R.ID IS NOT NULL drops the NULL-extended row → inner-like.
		{"rid_not_null", leaf("R", "ID", predicates.ComparisonIsNotNull, nil), []pair{{1, 2}, {2, 3}}},
		// L.A > 2: l=1 (A=2) dropped; l=2 kept; l=3 NULL-extended SURVIVES the
		// L-side filter (its L.A=99 is real).
		{"lside_filter", leaf("L", "A", predicates.ComparisonGreaterThan, int64(2)), []pair{{2, 3}, {3, -1}}},
		// R.A > 5: l=1 matched R.A=3 (3>5 no) dropped; l=2 matched R.A=99 kept;
		// l=3 NULL-extended R.A NULL → UNKNOWN → dropped.
		{"rside_filter", leaf("R", "A", predicates.ComparisonGreaterThan, int64(5)), []pair{{2, 3}}},
	}

	norm := func(rs []Row) []pair {
		out := make([]pair, 0, len(rs))
		for _, r := range rs {
			p := pair{l: r["L_ID"].(int64), r: -1}
			if v, ok := r["R_ID"].(int64); ok {
				p.r = v
			}
			out = append(out, p)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].l != out[j].l {
				return out[i].l < out[j].l
			}
			return out[i].r < out[j].r
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
			out, err := OracleRows(c, q, []string{"L.ID", "R.ID"})
			if err != nil {
				t.Fatalf("OracleRows: %v", err)
			}
			got := norm(out)
			want := append([]pair(nil), tc.want...)
			sort.Slice(want, func(i, j int) bool {
				if want[i].l != want[j].l {
					return want[i].l < want[j].l
				}
				return want[i].r < want[j].r
			})
			if !eq(got, want) {
				t.Errorf("LEFT JOIN oracle %s: got %v, want %v\nSQL: %s",
					tc.name, got, want, c.SQL(q, []string{"L.ID", "R.ID"}))
			}
		})
	}
}
