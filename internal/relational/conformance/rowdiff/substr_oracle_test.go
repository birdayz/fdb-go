package rowdiff

// Oracle-isolation validation for SUBSTR's 1-based, bound-clamped semantics —
// the risk axis for this function. substrVal must match the engine (verified by
// probe): out-of-range start → "", length past the end truncated, empty string
// and NULL handled. Each case is hand-computed.

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestSubstrOracle_HandComputed(t *testing.T) {
	tbl := TableDef{Name: "t", Cols: []ColumnDef{{Name: "S", Type: ColString}}}
	//   ID=1 "alpha" ; ID=2 "beta" ; ID=3 "" ; ID=4 NULL
	rows := []Row{
		{"ID": int64(1), "S": "alpha"},
		{"ID": int64(2), "S": "beta"},
		{"ID": int64(3), "S": ""},
		{"ID": int64(4), "S": nil},
	}
	c := &Case{Table: tbl, Rows: rows}

	sub := func(start, length int64, op predicates.ComparisonType, lit string) *Pred {
		return &Pred{StrFn: &StrFnSpec{Fn: StrFnSubstr, Col: "S", Start: start, Length: length}, Op: op, Lit: lit}
	}

	cases := []struct {
		name string
		p    *Pred
		want []int64
	}{
		// SUBSTR(s,1,2)='al': alpha→"al"; beta→"be"; ""→""; NULL UNKNOWN.
		{"prefix2", sub(1, 2, predicates.ComparisonEquals, "al"), []int64{1}},
		// SUBSTR(s,3,10)='pha': alpha→"pha" (length past end truncated).
		{"mid_clamped", sub(3, 10, predicates.ComparisonEquals, "pha"), []int64{1}},
		// SUBSTR(s,10,2)='': start beyond end → "" for every non-NULL row.
		{"start_beyond", sub(10, 2, predicates.ComparisonEquals, ""), []int64{1, 2, 3}},
		// SUBSTR(s,1,0)='': zero length → "" for every non-NULL row.
		{"zero_len", sub(1, 0, predicates.ComparisonEquals, ""), []int64{1, 2, 3}},
		// SUBSTR(s,1,4)='beta': only beta (alpha→"alph").
		{"whole_beta", sub(1, 4, predicates.ComparisonEquals, "beta"), []int64{2}},
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
				t.Errorf("SUBSTR oracle %s: got IDs %v, want %v\nSQL: %s",
					tc.name, got, tc.want, c.SQL(q, []string{"ID"}))
			}
		})
	}
}
