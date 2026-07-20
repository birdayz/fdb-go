package rowdiff

// Oracle-isolation validation for correlated [NOT] EXISTS. The oracle is the
// differential's ground truth, so it must be proven correct WITHOUT the engine
// (a circular check — engine vs an engine-derived oracle — proves nothing). Each
// case below is hand-computed: the correlation is over the SAME table under
// alias r, so a non-NULL correlation value always self-matches (r == outer row),
// a NULL correlation value matches nothing (r.c = NULL is UNKNOWN), and the
// optional inner predicate filters which correlated rows count.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestExistsOracle_HandComputed(t *testing.T) {
	tbl := TableDef{
		Name: "t",
		Cols: []ColumnDef{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint, NotNull: true},
		},
	}
	// A: correlation column (nullable). B: inner-filter column.
	//   ID=1 A=5 B=3
	//   ID=2 A=5 B=8
	//   ID=3 A=7 B=3
	//   ID=4 A=nil B=1   (NULL correlation → matches nothing)
	rows := []Row{
		{"ID": int64(1), "A": int64(5), "B": int64(3)},
		{"ID": int64(2), "A": int64(5), "B": int64(8)},
		{"ID": int64(3), "A": int64(7), "B": int64(3)},
		{"ID": int64(4), "A": nil, "B": int64(1)},
	}
	c := &Case{Table: tbl, Rows: rows}

	lt := func(col string, lit int64) *Pred {
		return &Pred{Col: col, Op: predicates.ComparisonLessThan, Lit: lit}
	}
	gt := func(col string, lit int64) *Pred {
		return &Pred{Col: col, Op: predicates.ComparisonGreaterThan, Lit: lit}
	}

	cases := []struct {
		name string
		e    *ExistsSpec
		want []int64
	}{
		// Bare correlation: non-NULL A self-matches; NULL A (ID=4) does not.
		{"exists_bare", &ExistsSpec{CorrCol: "A"}, []int64{1, 2, 3}},
		{"not_exists_bare", &ExistsSpec{CorrCol: "A", Negated: true}, []int64{4}},
		// A=5 group has a B=3 (<5) → ID 1,2 hold. A=7 group has B=3 (<5) → ID 3
		// holds. A=nil → no match. All non-NULL rows hold.
		{"exists_innerlt", &ExistsSpec{CorrCol: "A", Inner: lt("B", 5)}, []int64{1, 2, 3}},
		// A=5 group has a B=8 (>5) → ID 1,2 hold. A=7 group max B is 3 (not >5)
		// → ID 3 fails. A=nil → fails.
		{"exists_innergt", &ExistsSpec{CorrCol: "A", Inner: gt("B", 5)}, []int64{1, 2}},
		// NOT of the above: everything the inner-gt EXISTS dropped.
		{"not_exists_innergt", &ExistsSpec{CorrCol: "A", Inner: gt("B", 5), Negated: true}, []int64{3, 4}},
		// No row has B<0, so no correlated row ever passes the inner → EXISTS
		// empty, NOT EXISTS keeps every row (including the NULL-A one).
		{"exists_impossible", &ExistsSpec{CorrCol: "A", Inner: lt("B", 0)}, []int64{}},
		{"not_exists_impossible", &ExistsSpec{CorrCol: "A", Inner: lt("B", 0), Negated: true}, []int64{1, 2, 3, 4}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := Query{Exists: tc.e}
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
				t.Errorf("EXISTS oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}

func equalInt64(a, b []int64) bool {
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
