package sqldriver_test

// DML whose own writes could re-enter its own scan — the Halloween problem.
//
// `UPDATE t SET a = a + 1 WHERE a > 5` with an index on `a` is the pure form: the
// scan walks the index in ascending order, each update moves the row FORWARD in
// that same index, and a scan that observes its own writes meets the row again
// and updates it again. The row does not end at a+1; it runs to the end of the
// range. On an unindexed table the scan is over the primary key, which the update
// does not touch, so the same statement is well-behaved — which is exactly why
// the indexed/unindexed twin is the right oracle and why an unindexed-only test
// would report all-clear.
//
// The sweeps drive predicate-driven UPDATEs, but always to a CONSTANT value. A
// constant cannot move a row forward past the scan cursor, so none of them can
// reach this. Every case here sets a column FROM ITSELF.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_SelfReferencingUpdateDoesNotChase(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_halloween", "hw",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) ")
	w.Exec("INSERT INTO t (id, a, b) VALUES (1, 1, 10), (2, 2, 20), (3, 3, 30), (4, 4, 40), (5, 5, 50)")

	all := "SELECT id, a, b FROM t ORDER BY id"

	// Each row must be incremented EXACTLY ONCE. A scan that meets its own
	// writes increments the matching rows repeatedly and lands them all at the
	// top of the range.
	w.Exec("UPDATE t SET a = a + 1 WHERE a >= 3")
	w.Want("increment along the indexed column", all,
		[]string{"1|1|10", "2|2|20", "3|4|30", "4|5|40", "5|6|50"})

	// The same in the other direction: decrementing moves rows BACKWARD, behind
	// a forward cursor, so the failure here is the mirror image — a row that
	// should be visited is stepped over.
	w.Exec("UPDATE t SET a = a - 1 WHERE a <= 4")
	w.Want("decrement along the indexed column", all,
		[]string{"1|0|10", "2|1|20", "3|3|30", "4|5|40", "5|6|50"})

	// A predicate that stays TRUE after the write is the unbounded form: if the
	// scan re-admits its own writes there is no fixed point at all, so a run
	// that terminates with each row moved once is the whole assertion.
	w.Exec("UPDATE t SET a = a + 10 WHERE a >= 0")
	w.Want("a predicate that survives its own update", all,
		[]string{"1|10|10", "2|11|20", "3|13|30", "4|15|40", "5|16|50"})

	// Multiplication grows the value faster than the cursor advances, which
	// reaches the end of the range in fewer steps if the row is re-visited.
	w.Exec("UPDATE t SET a = a * 2 WHERE a > 12")
	w.Want("multiply along the indexed column", all,
		[]string{"1|10|10", "2|11|20", "3|26|30", "4|30|40", "5|32|50"})

	// The composite index (a, b) is maintained by the same write; updating the
	// SECOND component moves the row within a group rather than between groups.
	w.Exec("UPDATE t SET b = b + 1 WHERE b >= 30")
	w.Want("increment the trailing component of a composite index", all,
		[]string{"1|10|10", "2|11|20", "3|26|31", "4|30|41", "5|32|51"})

	// Both components at once.
	w.Exec("UPDATE t SET a = a + 1, b = b + 1 WHERE a >= 26")
	w.Want("increment both components", all,
		[]string{"1|10|10", "2|11|20", "3|27|32", "4|31|42", "5|33|52"})
}

// TestFDB_SelfReferencingDeleteTerminates is the DELETE twin. A delete whose
// predicate reads the column its own deletions vacate cannot loop, but it can
// STEP OVER rows if the cursor is invalidated by the removal — the same
// mechanism, showing as rows that survive a statement that should have removed
// them.
func TestFDB_SelfReferencingDeleteTerminates(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_halloween_del", "hwd",
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) ")

	var rows []string
	for i := 1; i <= 60; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d)", i, i%10))
	}
	w.Exec("INSERT INTO t (id, a) VALUES " + strings.Join(rows, ", "))

	// Deleting a whole indexed group: every row with a=3 must go, in one pass.
	w.Exec("DELETE FROM t WHERE a = 3")
	w.Want("a whole indexed group is removed", "SELECT COUNT(*) FROM t WHERE a = 3", []string{"0"})
	w.Want("and nothing else is", "SELECT COUNT(*) FROM t", []string{"54"})

	// A range delete spanning several adjacent groups.
	w.Exec("DELETE FROM t WHERE a >= 7")
	w.Want("a range delete leaves no stragglers", "SELECT COUNT(*) FROM t WHERE a >= 7", []string{"0"})
	w.Want("and removes exactly the range", "SELECT COUNT(*) FROM t", []string{"36"})
	w.Want("the survivors are intact",
		"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a",
		[]string{"0|6", "1|6", "2|6", "4|6", "5|6", "6|6"})
}

// TestFDB_InsertFromSelectOverTheSameTable is the third shape: a statement whose
// SOURCE is the table it writes to. If the reading side observes the rows the
// writing side is appending, the statement does not terminate at the original
// row count — it feeds on its own output.
func TestFDB_InsertFromSelectOverTheSameTable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_selfref_insert_select", "isel",
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE u (id BIGINT, a BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX u_a ON u (a) ")
	w.Exec("INSERT INTO t (id, a) VALUES (1, 1), (2, 2), (3, 3)")

	// Copy between tables first — the well-behaved control.
	//
	// A FAILURE here is a failure, not a reason to skip. The self-referencing
	// assertions below are the point of this file and they all run through
	// INSERT ... SELECT, so a skip on this line would take the entire file
	// offline while reporting green — the shape a reader scans as "covered".
	// If the statement ever stops being supported that is a regression worth a
	// red build, and if it is ever supported only on one schema the next line
	// catches that separately.
	if _, err := w.idx.ExecContext(ctx, "INSERT INTO u SELECT id, a FROM t"); err != nil {
		t.Fatalf("INSERT ... SELECT failed on the indexed schema: %v\n"+
			"  Every assertion in this file runs through this statement, so it cannot be "+
			"skipped past — if the support was deliberately withdrawn, pin the exact rejection "+
			"here instead of stepping around it", err)
	}
	if _, err := w.plain.ExecContext(ctx, "INSERT INTO u SELECT id, a FROM t"); err != nil {
		t.Fatalf("the indexed schema accepted INSERT ... SELECT and the unindexed one did not: %v", err)
	}
	w.Want("a copy between tables", "SELECT id, a FROM u ORDER BY id",
		[]string{"1|1", "2|2", "3|3"})

	// Now the self-referencing form. Whatever it does — succeed with exactly
	// three new rows, or be rejected — it must do the same on both schemas and
	// must not consume its own output.
	_, ei := w.idx.ExecContext(ctx, "INSERT INTO t SELECT id + 100, a FROM t")
	_, en := w.plain.ExecContext(ctx, "INSERT INTO t SELECT id + 100, a FROM t")
	if (ei == nil) != (en == nil) {
		t.Fatalf("self-referencing INSERT ... SELECT differs by index presence\n  indexed: %v\n  unindexed: %v",
			ei, en)
	}
	if ei == nil {
		w.Want("a self-referencing insert reads only the pre-existing rows",
			"SELECT id, a FROM t ORDER BY id",
			[]string{"1|1", "2|2", "3|3", "101|1", "102|2", "103|3"})
	} else {
		t.Logf("self-referencing INSERT ... SELECT is rejected on both schemas: %v", ei)
	}
}
