package rowdiff

// Oracle-isolation validation for the LEFT-OUTER third leg of the 3-way join
// (`l JOIN m ON l.a=m.id LEFT JOIN r ON m.c=r.c`): an (l,m) pair with no matching
// r must be NULL-extended (R.* = NULL), not dropped. The NULL-extension arm and
// its interaction with a post-join WHERE (SQL applies WHERE after the join, so an
// R-side filter drops the NULL row via three-valued UNKNOWN) are the new logic; a
// mistake would manufacture false findings against every outer-3-way query.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestOuterThreeWayOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "C", Type: ColBigint},
		},
	}
	//   ID=1 A=1 C=NULL ; ID=2 A=2 C=5 ; ID=3 A=9 C=5
	rows := []Row{
		{"ID": int64(1), "A": int64(1), "C": nil},
		{"ID": int64(2), "A": int64(2), "C": int64(5)},
		{"ID": int64(3), "A": int64(9), "C": int64(5)},
	}
	c := &Case{Table: tbl, Rows: rows}
	// l.a = m.id ; m.c = r.c ; the third leg is LEFT OUTER.
	spec := &ThreeWayJoinSpec{LMLeft: "A", LMRight: "ID", MRMid: "C", MRRight: "C", ROuter: true}

	// l JOIN m ON l.a=m.id:  l1(a=1)->m1, l2(a=2)->m2, l3(a=9)->none.
	// LEFT JOIN r ON m.c=r.c: m1.c=NULL -> no r matches -> (1,1,NULL);
	//                        m2.c=5 -> r2,r3 -> (2,2,2),(2,2,3).
	const null = int64(-1) // sentinel for a NULL-extended R.ID in the expected set

	leaf := func(qual, col string, op predicates.ComparisonType, lit int64) *BoolNode {
		return &BoolNode{Leaf: &Pred{Qual: qual, Col: col, Op: op, Lit: lit}}
	}
	type triple struct{ l, m, r int64 }
	cases := []struct {
		name  string
		where *BoolNode
		want  []triple
	}{
		// No WHERE: the NULL-extended (1,1,NULL) row survives.
		{"no_where", nil, []triple{{1, 1, null}, {2, 2, 2}, {2, 2, 3}}},
		// L-side filter keeps the NULL row (it filters the preserved L leg).
		{"lside_keeps_null", leaf("L", "ID", predicates.ComparisonEquals, 1), []triple{{1, 1, null}}},
		// R-side filter DROPS the NULL row: r.c IS NULL on the extended row is not
		// TRUE for `R.C = 5` (it's UNKNOWN), so only the matched rows survive.
		{"rside_drops_null", leaf("R", "C", predicates.ComparisonEquals, 5), []triple{{2, 2, 2}, {2, 2, 3}}},
		// IS NULL on the R leg KEEPS only the NULL-extended row.
		{"rside_isnull", &BoolNode{Leaf: &Pred{Qual: "R", Col: "C", Op: predicates.ComparisonIsNull}}, []triple{{1, 1, null}}},
	}

	norm := func(rs []Row) []triple {
		out := make([]triple, 0, len(rs))
		for _, r := range rs {
			rid := null
			if v := r["R_ID"]; v != nil {
				rid = v.(int64)
			}
			out = append(out, triple{r["L_ID"].(int64), r["M_ID"].(int64), rid})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].l != out[j].l {
				return out[i].l < out[j].l
			}
			if out[i].m != out[j].m {
				return out[i].m < out[j].m
			}
			return out[i].r < out[j].r
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
				if want[i].m != want[j].m {
					return want[i].m < want[j].m
				}
				return want[i].r < want[j].r
			})
			if !eq(got, want) {
				t.Errorf("outer-3way oracle %s: got %v, want %v\nSQL: %s",
					tc.name, got, want, c.SQL(q, []string{"L.ID", "M.ID", "R.ID"}))
			}
		})
	}
}
