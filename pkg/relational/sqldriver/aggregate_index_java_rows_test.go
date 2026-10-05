package sqldriver_test

import (
	"strings"
	"testing"
)

// mmAggregateIndexRowsAgree reports whether rows read through SUM / COUNT
// aggregate indexes (idx) are the answer Java's index gives for the records the
// unindexed twin reads (oracle). Both are mmRows output: one "|"-joined string
// per row, in the read's order, the last nAgg columns aggregates.
//
// Java reads such an index alone (AggregateIndexMatchCandidate has no notion of
// group existence), so its answer differs from the records' in exactly three
// ways, and this relation admits those and nothing else:
//
//   - a group whose rows were all deleted or moved away keeps its key at the
//     atomic-add residue: an idx row the oracle does not have, every aggregate
//     0;
//   - a group with no non-NULL value has no SUM or COUNT(col) entry, so the
//     index (and an inner intersection over it) has no row where the oracle
//     has one with a NULL SUM or a 0 COUNT(col);
//   - a live group whose non-NULL values are all gone keeps its SUM at the
//     residue 0 where the oracle's SUM is NULL.
//
// Every other row must be equal, and in the same order.
func mmAggregateIndexRowsAgree(idx, oracle []string, nAgg int) bool {
	split := func(row string) (key string, aggs []string) {
		cols := strings.Split(row, "|")
		if len(cols) < nAgg {
			return row, nil
		}
		return strings.Join(cols[:len(cols)-nAgg], "|"), cols[len(cols)-nAgg:]
	}
	keys := func(rows []string) map[string]bool {
		out := make(map[string]bool, len(rows))
		for _, r := range rows {
			k, _ := split(r)
			out[k] = true
		}
		return out
	}
	oracleKeys, idxKeys := keys(oracle), keys(idx)
	var gotKept, wantKept []string
	for _, r := range idx {
		k, aggs := split(r)
		if !oracleKeys[k] && allAggregates(aggs, func(v string) bool { return v == "0" }) {
			continue // a vacated group's residue
		}
		gotKept = append(gotKept, r)
	}
	for _, r := range oracle {
		k, aggs := split(r)
		if !idxKeys[k] && anyAggregate(aggs, func(v string) bool { return v == "NULL" || v == "0" }) {
			continue // no non-NULL value, so no SUM / COUNT(col) entry
		}
		wantKept = append(wantKept, r)
	}
	if len(gotKept) != len(wantKept) {
		return false
	}
	for i := range gotKept {
		gk, ga := split(gotKept[i])
		wk, wa := split(wantKept[i])
		if gk != wk || len(ga) != len(wa) {
			return false
		}
		for j := range ga {
			if ga[j] != wa[j] && (ga[j] != "0" || wa[j] != "NULL") {
				return false
			}
		}
	}
	return true
}

// WantAggregateIndex asserts a read served by SUM / COUNT aggregate indexes:
// the indexed schema answers wantIndexed, Java's index answer, and the
// unindexed one wantRecords, and the two differ only as
// mmAggregateIndexRowsAgree allows. A read where they are equal uses Want.
func (w *mmTwin) WantAggregateIndex(name, q string, wantIndexed, wantRecords []string) {
	w.t.Helper()
	gi, ei := mmRows(w.t, w.ctx, w.idx, q)
	gn, en := mmRows(w.t, w.ctx, w.plain, q)
	if ei != nil || en != nil {
		w.t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !mmEqRows(gn, wantRecords) {
		w.t.Errorf("%s: UNINDEXED (records) answer is wrong\n  q: %s\n  got  %v\n  want %v", name, q, gn, wantRecords)
	}
	if !mmEqRows(gi, wantIndexed) {
		w.t.Errorf("%s: INDEXED answer is not Java's\n  q: %s\n  got  %v\n  want %v\n  plan: %s",
			name, q, gi, wantIndexed, w.Explain(q))
	}
	if !mmAggregateIndexRowsAgree(gi, gn, mmTrailingAggregates(q)) {
		w.t.Errorf("%s: the indexed answer differs from the records' in a way Java's index does not\n"+
			"  q: %s\n  indexed  : %v\n  unindexed: %v", name, q, gi, gn)
	}
}

func allAggregates(aggs []string, f func(string) bool) bool {
	for _, a := range aggs {
		if !f(a) {
			return false
		}
	}
	return len(aggs) > 0
}

func anyAggregate(aggs []string, f func(string) bool) bool {
	for _, a := range aggs {
		if f(a) {
			return true
		}
	}
	return false
}

// mmTrailingAggregates counts the aggregate calls in a read's select list (the
// text before its first FROM), which this file's callers project last.
func mmTrailingAggregates(q string) int {
	up := strings.ToUpper(q)
	if i := strings.Index(up, " FROM "); i >= 0 {
		up = up[:i]
	}
	n := 0
	for _, f := range []string{"SUM(", "COUNT(", "MIN(", "MAX("} {
		n += strings.Count(up, f)
	}
	return n
}

func TestAggregateIndexRowsAgree(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		idx, oracle []string
		nAgg        int
		want        bool
	}{
		{"equal", []string{"1|2", "3|4"}, []string{"1|2", "3|4"}, 1, true},
		{"vacated_residue", []string{"1|2", "2|0", "3|4"}, []string{"1|2", "3|4"}, 1, true},
		{"all_null_group_absent", []string{"1|2"}, []string{"1|2", "3|NULL"}, 1, true},
		{"sum_residue", []string{"1|0"}, []string{"1|NULL"}, 1, true},
		{"intersection_drops_all_null_sum", []string{"1|5|2"}, []string{"1|5|2", "2|NULL|3"}, 2, true},
		{"live_value_differs", []string{"1|3"}, []string{"1|2"}, 1, false},
		{"order_differs", []string{"3|4", "1|2"}, []string{"1|2", "3|4"}, 1, false},
		{"extra_nonzero_group", []string{"1|2", "2|7"}, []string{"1|2"}, 1, false},
		{"missing_live_group", []string{"1|2"}, []string{"1|2", "3|4"}, 1, false},
		{"zero_for_live_value", []string{"1|0"}, []string{"1|2"}, 1, false},
	} {
		if got := mmAggregateIndexRowsAgree(c.idx, c.oracle, c.nAgg); got != c.want {
			t.Errorf("%s: agree = %v, want %v", c.name, got, c.want)
		}
	}
	if n := mmTrailingAggregates("SELECT c.id, (SELECT SUM(v) FROM t WHERE x) FROM cust"); n != 1 {
		t.Errorf("trailing aggregates = %d, want 1", n)
	}
}
