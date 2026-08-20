package sqldriver_test

// Query-level safety of SPARSE (filtered) indexes.
//
// A sparse index holds entries only for the records satisfying its own
// predicate. Answering a query from one is therefore sound only when the
// QUERY's predicate implies the INDEX's: otherwise the rows the index omits are
// rows the answer needed, and they are simply missing — no error, no duplicate,
// just fewer rows than the table holds.
//
// That makes the interesting cases the ones that look implied and are not.
// `keep >= 0` does not imply `keep > 0` because zero is excluded; `keep IS NOT
// NULL` does not imply it either; and a disjunction implies it only if EVERY
// arm does. Each of those is one boundary value away from a predicate that
// genuinely is implied, which is exactly where an implication check is most
// likely to be too generous.
//
// The existing sparse-index coverage is at the WIRE level — that the maintainer
// writes an entry when a record enters the predicate and removes it when the
// record leaves. This file is the other half: that the PLANNER only answers
// from those entries when they are enough.
//
// The unindexed twin is the oracle. It has no sparse index, so it always reads
// every record, and any row the indexed side is missing is a row it returns.
//
// MEASURED, AND IT CHANGES WHAT THIS FILE IS WORTH TODAY: on this fixture the
// planner does not choose a sparse index for ANY of these queries — not even
// `WHERE a = 5 AND keep > 0`, whose predicate is the index's filter verbatim.
// Every one plans as `PredicatesFilter(Scan(T))` at 900 rows. The candidate
// machinery exists (ValueIndexScanMatchCandidate carries the predicate proto and
// an opaque-filter flag, and such a candidate is documented as never COMPLETE),
// so what declines is the choice rather than the capability — the same shape as
// the OR-to-union costing observation recorded in TODO.md.
//
// So these cases are not currently exercising the implication check; they are
// pinning the ANSWERS, which is what must not move when sparse matching does
// start being chosen. That is the moment the check becomes load-bearing, and
// the cases most likely to catch it getting the implication wrong are already
// written: `keep >= 0` and `keep IS NOT NULL` both look implied and are not,
// each by exactly one value.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func mmSparseFixture(t *testing.T, ctx context.Context, dbPath, prefix string) *mmTwin {
	t.Helper()
	w := mmNewTwin(t, ctx, dbPath, prefix,
		"CREATE TABLE t (id BIGINT, a BIGINT, keep BIGINT, pad STRING, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a_sparse AS SELECT a FROM t WHERE keep > 0 "+
			"CREATE INDEX t_a_sparse_or AS SELECT a FROM t WHERE keep < -5 OR keep > 10 ")

	// For every `a` value there are rows on BOTH sides of the index predicate,
	// so any query answered from the sparse index alone is short by the rows
	// with keep <= 0 — and the boundary values 0 and NULL are represented,
	// because they are what separates `keep >= 0` and `keep IS NOT NULL` from
	// the predicate they resemble.
	rows := []string{
		"(1, 5, 1, 'p')",     // in the > 0 index
		"(2, 5, 0, 'p')",     // NOT in it: zero is excluded
		"(3, 5, -1, 'p')",    // NOT in it
		"(4, 5, NULL, 'p')",  // NOT in it: NULL fails the predicate
		"(5, 5, 20, 'p')",    // in both indexes
		"(6, 7, 3, 'p')",     // in the > 0 index
		"(7, 7, -9, 'p')",    // in the OR index only
		"(8, 7, 0, 'p')",     // in neither
		"(9, 9, 11, 'p')",    // in both
		"(10, 9, NULL, 'p')", // in neither
	}
	// Padding so an index probe is worth choosing at all.
	for i := 11; i <= 900; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d, %d, 'p')", i, 1000+i, i%3))
	}
	for start := 0; start < len(rows); start += 150 {
		end := start + 150
		if end > len(rows) {
			end = len(rows)
		}
		w.Exec("INSERT INTO t (id, a, keep, pad) VALUES " + strings.Join(rows[start:end], ", "))
	}
	return w
}

// TestFDB_SparseIndexNotUsedWhereItWouldDropRows is the core: queries whose
// predicate does NOT imply the index's filter must still see every row.
func TestFDB_SparseIndexNotUsedWhereItWouldDropRows(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmSparseFixture(t, ctx, "/testdb_sparse_safety", "spsafe")

	// No filter on keep at all: the sparse index holds 2 of the 5 rows with a=5.
	w.Want("an unfiltered equality sees every row",
		"SELECT id FROM t WHERE a = 5 ORDER BY id", []string{"1", "2", "3", "4", "5"})
	w.Want("counted", "SELECT COUNT(*) FROM t WHERE a = 5", []string{"5"})

	// keep >= 0 EXCLUDES nothing the index excludes except zero — and zero is
	// exactly what the index drops, so the implication fails on one value.
	w.Want("keep >= 0 does not imply keep > 0",
		"SELECT id FROM t WHERE a = 5 AND keep >= 0 ORDER BY id", []string{"1", "2", "5"})

	// IS NOT NULL admits 0 and negatives, so it does not imply the filter.
	w.Want("keep IS NOT NULL does not imply keep > 0",
		"SELECT id FROM t WHERE a = 5 AND keep IS NOT NULL ORDER BY id",
		[]string{"1", "2", "3", "5"})

	// A disjunction implies the filter only if BOTH arms do; this one does not.
	w.Want("a disjunction with one unimplied arm",
		"SELECT id FROM t WHERE a = 5 AND (keep > 0 OR keep = -1) ORDER BY id",
		[]string{"1", "3", "5"})

	// Negation: NOT (keep <= 0) IS keep > 0, so this one IS implied — the
	// control that the suite is not simply asserting the index is never used.
	w.Want("NOT (keep <= 0) is the filter written differently",
		"SELECT id FROM t WHERE a = 5 AND NOT (keep <= 0) ORDER BY id", []string{"1", "5"})

	// Genuinely implied predicates: the answer must be the same whether or not
	// the index is used, which is the other direction of the same invariant.
	w.Want("keep > 0 is the filter itself",
		"SELECT id FROM t WHERE a = 5 AND keep > 0 ORDER BY id", []string{"1", "5"})
	w.Want("keep > 5 implies keep > 0",
		"SELECT id FROM t WHERE a = 5 AND keep > 5 ORDER BY id", []string{"5"})
	w.Want("keep >= 1 implies keep > 0",
		"SELECT id FROM t WHERE a = 5 AND keep >= 1 ORDER BY id", []string{"1", "5"})

	// Ranges and orderings over the sparse column, where the index could supply
	// an ordering for a subset of the rows and leave the rest unsorted.
	w.Want("a range over the indexed column",
		"SELECT id FROM t WHERE a >= 5 AND a <= 9 ORDER BY a, id",
		[]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"})
	w.Want("ordered by the indexed column",
		"SELECT id FROM t WHERE a = 7 ORDER BY a, id", []string{"6", "7", "8"})

	// Aggregates, where a missing row is a wrong number rather than a short list.
	w.Want("grouped counts over the sparse column",
		"SELECT a, COUNT(*) FROM t WHERE a IN (5, 7, 9) GROUP BY a ORDER BY a",
		[]string{"5|5", "7|3", "9|2"})
	w.Want("sum of the filtered column",
		"SELECT SUM(keep) FROM t WHERE a = 5", []string{"20"})
}

// TestFDB_SparseIndexWithDisjunctivePredicate uses the OR-filtered index, whose
// implication test has to hold arm by arm rather than as a whole.
func TestFDB_SparseIndexWithDisjunctivePredicate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmSparseFixture(t, ctx, "/testdb_sparse_or", "spor")

	// The index holds keep < -5 OR keep > 10. Rows for a=7: keep 3, -9, 0 — so
	// only id 7 (keep=-9) is in it.
	w.Want("unfiltered, against a disjunctively-filtered index",
		"SELECT id FROM t WHERE a = 7 ORDER BY id", []string{"6", "7", "8"})
	w.Want("a predicate matching one arm only",
		"SELECT id FROM t WHERE a = 7 AND keep < -5 ORDER BY id", []string{"7"})
	w.Want("a predicate implied by the whole disjunction",
		"SELECT id FROM t WHERE a = 7 AND (keep < -5 OR keep > 10) ORDER BY id", []string{"7"})
	w.Want("a predicate that straddles the gap",
		"SELECT id FROM t WHERE a = 7 AND keep > -10 ORDER BY id", []string{"6", "7", "8"})
	w.Want("the excluded middle is fully visible",
		"SELECT id FROM t WHERE a IN (5, 7, 9) AND keep >= -5 AND keep <= 10 ORDER BY id",
		[]string{"1", "2", "3", "6", "8"})
}

// TestFDB_SparseIndexUnderMutation moves rows across the index predicate while
// querying. A record that leaves the predicate must stop being answerable from
// the index, and one that enters it must start — and neither may change what an
// unfiltered query returns.
func TestFDB_SparseIndexUnderMutation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmSparseFixture(t, ctx, "/testdb_sparse_mut", "spmut")

	unfiltered := "SELECT id FROM t WHERE a = 5 ORDER BY id"
	filtered := "SELECT id FROM t WHERE a = 5 AND keep > 0 ORDER BY id"

	w.Want("before", unfiltered, []string{"1", "2", "3", "4", "5"})
	w.Want("before, filtered", filtered, []string{"1", "5"})

	// A row LEAVES the index predicate.
	w.Exec("UPDATE t SET keep = -3 WHERE id = 1")
	w.Want("after a row leaves the predicate", unfiltered, []string{"1", "2", "3", "4", "5"})
	w.Want("and is gone from the filtered answer", filtered, []string{"5"})

	// A row ENTERS it.
	w.Exec("UPDATE t SET keep = 4 WHERE id = 3")
	w.Want("after a row enters the predicate", unfiltered, []string{"1", "2", "3", "4", "5"})
	w.Want("and joins the filtered answer", filtered, []string{"3", "5"})

	// A row moves from NULL into the predicate — NULL fails it, so this is an
	// entry from outside rather than a move within.
	w.Exec("UPDATE t SET keep = 9 WHERE id = 4")
	w.Want("after a NULL row enters", filtered, []string{"3", "4", "5"})

	// And back out to NULL.
	w.Exec("UPDATE t SET keep = NULL WHERE id = 4")
	w.Want("after it returns to NULL", filtered, []string{"3", "5"})
	w.Want("the unfiltered answer never moved", unfiltered, []string{"1", "2", "3", "4", "5"})

	// Deleting a row that is IN the index.
	w.Exec("DELETE FROM t WHERE id = 5")
	w.Want("after deleting an indexed row", filtered, []string{"3"})
	w.Want("unfiltered loses exactly that row", unfiltered, []string{"1", "2", "3", "4"})

	// Deleting a row that is NOT in the index must not disturb it.
	w.Exec("DELETE FROM t WHERE id = 2")
	w.Want("after deleting a non-indexed row", filtered, []string{"3"})
	w.Want("unfiltered loses exactly that row too", unfiltered, []string{"1", "3", "4"})
}
