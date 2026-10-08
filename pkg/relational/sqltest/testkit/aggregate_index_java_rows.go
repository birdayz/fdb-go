package testkit

import (
	"strings"
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
func MmAggregateIndexRowsAgree(idx, oracle []string, nAgg int) bool {
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
func (w *Twin) WantAggregateIndex(name, q string, wantIndexed, wantRecords []string) {
	w.T.Helper()
	gi, ei := QueryRowStrings(w.T, w.Ctx, w.Idx, q)
	gn, en := QueryRowStrings(w.T, w.Ctx, w.Plain, q)
	if ei != nil || en != nil {
		w.T.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", name, q, ei, en)
		return
	}
	if !EqualRows(gn, wantRecords) {
		w.T.Errorf("%s: UNINDEXED (records) answer is wrong\n  q: %s\n  got  %v\n  want %v", name, q, gn, wantRecords)
	}
	if !EqualRows(gi, wantIndexed) {
		w.T.Errorf("%s: INDEXED answer is not Java's\n  q: %s\n  got  %v\n  want %v\n  plan: %s",
			name, q, gi, wantIndexed, w.Explain(q))
	}
	if !MmAggregateIndexRowsAgree(gi, gn, MmTrailingAggregates(q)) {
		w.T.Errorf("%s: the indexed answer differs from the records' in a way Java's index does not\n"+
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
func MmTrailingAggregates(q string) int {
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
