package sqldriver_test

// An IN list that repeats a value.
//
// `WHERE a IN (10, 10)` is the same shape as the OR-to-union defect this branch
// fixed, reached by a different syntax: an index-backed IN plans as a probe PER
// LIST ELEMENT, so a repeated element probes the same key twice and produces the
// same record twice unless something collapses it. The list is small and the
// duplicate is obvious to a reader, which is exactly why it is easy to leave
// untested — and it is: across the yamsql corpus and this package there are 104
// two-element numeric IN lists and not one of them repeats a value.
//
//	grep -rhoE "IN \([0-9]+, ?[0-9]+\)" <corpus> | sort -u | awk -F'[(,) ]+' '$2==$3'
//
// The DML forms matter more than the SELECT: a row produced twice by a SELECT is
// a visibly duplicated row, while a row updated twice by `SET v = v + 1` is a
// silently wrong NUMBER that looks like a plausible value.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mmInListFixture pads the probed table so an index probe beats a full scan and
// the IN actually plans as one probe per element — on a handful of rows the
// planner picks a scan with a residual filter and no probe is repeated at all.
func mmInListFixture(t *testing.T, ctx context.Context, dbPath, prefix string) *mmTwin {
	t.Helper()
	w := mmNewTwin(t, ctx, dbPath, prefix,
		"CREATE TABLE t (id BIGINT, a BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) ")
	var rows []string
	// Rows 1..3 carry the probed values; the rest are padding with values no
	// probe below mentions.
	rows = append(rows, "(1, 10, 100)", "(2, 20, 200)", "(3, 30, 300)")
	for i := 4; i <= 1200; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d, %d)", i, 1000+i, i))
	}
	for start := 0; start < len(rows); start += 150 {
		end := start + 150
		if end > len(rows) {
			end = len(rows)
		}
		w.Exec("INSERT INTO t (id, a, v) VALUES " + strings.Join(rows[start:end], ", "))
	}
	return w
}

func TestFDB_InListRepeatedValueSelect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmInListFixture(t, ctx, "/testdb_inlist_dup", "inld")

	w.Want("a value repeated twice",
		"SELECT id FROM t WHERE a IN (10, 10) ORDER BY id", []string{"1"})
	w.Want("a value repeated three times",
		"SELECT id FROM t WHERE a IN (10, 10, 10) ORDER BY id", []string{"1"})
	w.Want("a repeat separated by another value",
		"SELECT id FROM t WHERE a IN (10, 20, 10) ORDER BY id", []string{"1", "2"})
	w.Want("every element repeated",
		"SELECT id FROM t WHERE a IN (10, 20, 10, 20, 30, 30) ORDER BY id",
		[]string{"1", "2", "3"})

	// COUNT is where a surviving duplicate stops being visible as a row and
	// starts being a wrong number.
	w.Want("counted",
		"SELECT COUNT(*) FROM t WHERE a IN (10, 10)", []string{"1"})
	w.Want("summed, where a duplicate doubles the total",
		"SELECT SUM(v) FROM t WHERE a IN (10, 10, 20)", []string{"300"})

	// A repeated value that matches NOTHING must still match nothing.
	w.Want("a repeated value with no rows",
		"SELECT id FROM t WHERE a IN (99999, 99999) ORDER BY id", []string{})

	// A NULL ELEMENT IS REJECTED, on both schemas, and that is pinned rather
	// than worked around. NULL in an IN list is where three-valued logic bites
	// hardest — `x IN (1, NULL)` is UNKNOWN rather than FALSE for x=2, and
	// `x NOT IN (1, NULL)` is UNKNOWN for EVERY row, so it selects nothing — and
	// the reason this suite carries no such expectations is that the list cannot
	// contain a NULL at all. If that restriction lifts, those semantics arrive
	// with it and the cases have to be written; the failure message says so.
	for _, q := range []string{
		"SELECT id FROM t WHERE a IN (10, NULL, 10) ORDER BY id",
		"SELECT COUNT(*) FROM t WHERE a NOT IN (10, NULL)",
		"SELECT id FROM t WHERE a IN (NULL) ORDER BY id",
	} {
		_, ei := mmRows(t, ctx, w.idx, q)
		_, en := mmRows(t, ctx, w.plain, q)
		if ei == nil || en == nil {
			t.Errorf("a NULL element in an IN list is now ACCEPTED (indexed err %v, unindexed err %v). "+
				"Three-valued IN semantics are now reachable and need pinning: `x IN (v, NULL)` is "+
				"UNKNOWN rather than FALSE for a non-matching x, and `x NOT IN (v, NULL)` is UNKNOWN "+
				"for EVERY row and so selects nothing.\n  q: %s", ei, en, q)
		}
	}

	// The negated form with a repeat, which is legal because no element is NULL.
	w.Want("NOT IN with a repeated value",
		"SELECT COUNT(*) FROM t WHERE a NOT IN (10, 10) AND a < 100", []string{"2"})
}

// TestFDB_InListRepeatedValueDML is the sharper half. A row visited twice by an
// UPDATE that reads its own column lands on a value nothing in the statement
// asked for, and a DELETE that visits a row twice must not fail the second time.
func TestFDB_InListRepeatedValueDML(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmInListFixture(t, ctx, "/testdb_inlist_dup_dml", "inldd")

	// v = v + 1 applied through a list that names the row's value twice. If the
	// row is visited once per element, v ends at 102 rather than 101.
	w.Exec("UPDATE t SET v = v + 1 WHERE a IN (10, 10)")
	w.Want("an increment through a repeated IN value applies once",
		"SELECT id, v FROM t WHERE id = 1", []string{"1|101"})

	w.Exec("UPDATE t SET v = v + 1 WHERE a IN (10, 10, 10, 10)")
	w.Want("four repeats still apply once",
		"SELECT id, v FROM t WHERE id = 1", []string{"1|102"})

	// Two distinct rows, each named twice.
	w.Exec("UPDATE t SET v = v + 5 WHERE a IN (20, 30, 20, 30)")
	w.Want("two rows, each named twice", "SELECT id, v FROM t WHERE id IN (2, 3) ORDER BY id",
		[]string{"2|205", "3|305"})

	// A DELETE naming the same row twice must remove it and not error.
	w.Exec("DELETE FROM t WHERE a IN (10, 10)")
	w.Want("a repeated DELETE target is removed once",
		"SELECT COUNT(*) FROM t WHERE a = 10", []string{"0"})
	w.Want("and nothing else went with it",
		"SELECT COUNT(*) FROM t WHERE a IN (20, 30)", []string{"2"})

	// Deleting an already-absent value through a repeated list is a no-op.
	w.Exec("DELETE FROM t WHERE a IN (10, 10)")
	w.Want("deleting an absent repeated value is a no-op",
		"SELECT COUNT(*) FROM t", []string{"1199"})
}
