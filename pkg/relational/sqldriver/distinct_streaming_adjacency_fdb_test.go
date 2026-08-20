package sqldriver_test

// The adjacency precondition of the streaming DISTINCT.
//
// A distinct has two executors. The hash one keeps every key it has seen; the
// STREAMING one keeps only the PREVIOUS row's key and drops a row when it
// repeats — which is correct only if equal rows are ADJACENT, i.e. only if the
// inner is ordered by EVERY column of the dedup key. Order by a proper subset
// and two equal rows can sit either side of a third, where an adjacent-only
// dedup emits both.
//
// The mode is deliberately invisible in EXPLAIN (it produces identical rows when
// the precondition holds, so surfacing it would only make plan assertions
// brittle), which means it cannot be asserted directly — only the ROWS can. The
// fixtures below are built so a wrongly-streamed distinct returns a duplicate
// rather than the same answer more slowly: in each one the repeated pair is
// separated by a row that differs, so adjacency genuinely fails.
//
// The unindexed twin is the oracle: with no index there is no ordered access
// path to tempt the planner into streaming, so its DISTINCT is always the hash.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_StreamingDistinctRequiresFullKeyOrdering(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_streamdist", "sd",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_b ON t (b) ")

	// Ordered by `a` alone (and then by primary key, which is the id), the rows
	// of group a=1 come out (1,1), (1,2), (1,1): the repeated pair is SPLIT by a
	// row that differs. A dedup keyed on (a, b) that only compares neighbours
	// emits (1,1) twice.
	w.Exec("INSERT INTO t (id, a, b) VALUES " +
		"(1, 1, 1), (2, 1, 2), (3, 1, 1), " +
		"(4, 2, 5), (5, 2, 5), (6, 2, 6), (7, 2, 5)")

	w.Want("DISTINCT on two columns, ordered by only the first",
		"SELECT DISTINCT a, b FROM t ORDER BY a",
		[]string{"1|1", "1|2", "2|5", "2|6"})
	w.Want("the same, ordered fully",
		"SELECT DISTINCT a, b FROM t ORDER BY a, b",
		[]string{"1|1", "1|2", "2|5", "2|6"})
	w.Want("counted, so a survivor cannot hide in a long list",
		"SELECT COUNT(*) FROM (SELECT DISTINCT a, b FROM t) AS s",
		[]string{"4"})

	// The mirror: the dedup key is a SUBSET of the ordering columns rather than
	// a superset. Ordering by `b` scatters equal `a` values, so a dedup on `a`
	// alone must not stream over it.
	w.Want("DISTINCT on one column, ordered by a different one",
		"SELECT DISTINCT a FROM t ORDER BY a",
		[]string{"1", "2"})
	w.Want("DISTINCT projecting a, ordered by b",
		"SELECT COUNT(*) FROM (SELECT DISTINCT a FROM t) AS s",
		[]string{"2"})

	// DESCENDING order still makes equal rows adjacent, so this one SHOULD be
	// streamable — and must therefore still be right, which is the other half of
	// the claim: the guard must not be so conservative that it never fires, but
	// this test can only see the rows.
	w.Want("DISTINCT under a descending full ordering",
		"SELECT DISTINCT a, b FROM t ORDER BY a DESC, b DESC",
		[]string{"2|6", "2|5", "1|2", "1|1"})

	// A dedup over a column with NULLs, ordered by another: NULLs cluster at one
	// end under the ordering that includes them and scatter under one that does
	// not.
	w.Exec("INSERT INTO t (id, a, b) VALUES (8, NULL, 1), (9, 3, 3), (10, NULL, 2)")
	w.Want("NULLs in the dedup key, ordered by another column",
		"SELECT DISTINCT a FROM t ORDER BY a",
		[]string{"NULL", "1", "2", "3"})
	w.Want("and counted",
		"SELECT COUNT(*) FROM (SELECT DISTINCT a FROM t) AS s",
		[]string{"4"})
}

// TestFDB_StreamingDistinctAtScale repeats the split-pair shape over enough rows
// that a single surviving duplicate is a count difference rather than something
// a reader has to spot in a list, and across enough distinct groups that the
// adjacency failure recurs rather than depending on one unlucky triple.
func TestFDB_StreamingDistinctAtScale(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_streamdist_scale", "sds",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) ")

	// 100 groups; within each, the pattern (x,0) (x,1) (x,0) repeats the first
	// pair with a different row between them. Under an ordering on `a` alone
	// every group is a fresh chance for an adjacent-only dedup to leak.
	var rows []string
	id := 1
	for g := 0; g < 100; g++ {
		rows = append(rows, fmt.Sprintf("(%d, %d, 0)", id, g))
		id++
		rows = append(rows, fmt.Sprintf("(%d, %d, 1)", id, g))
		id++
		rows = append(rows, fmt.Sprintf("(%d, %d, 0)", id, g))
		id++
	}
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		w.Exec("INSERT INTO t (id, a, b) VALUES " + strings.Join(rows[start:end], ", "))
	}

	// 100 groups x 2 distinct pairs = 200. A leak of one per group gives 300.
	w.Want("200 distinct pairs over 300 rows",
		"SELECT COUNT(*) FROM (SELECT DISTINCT a, b FROM t) AS s",
		[]string{"200"})
	w.Want("ordered by the first column only",
		"SELECT COUNT(*) FROM (SELECT DISTINCT a, b FROM t ORDER BY a) AS s",
		[]string{"200"})
	w.Want("the first few pairs are right",
		"SELECT DISTINCT a, b FROM t ORDER BY a, b LIMIT 6",
		[]string{"0|0", "0|1", "1|0", "1|1", "2|0", "2|1"})
	w.Want("and a page from the middle",
		"SELECT DISTINCT a, b FROM t ORDER BY a, b LIMIT 4 OFFSET 100",
		[]string{"50|0", "50|1", "51|0", "51|1"})
}
