package sqldriver_test

// Read-your-writes THROUGH AN INDEX, inside one transaction.
//
// A statement must see its own transaction's earlier writes whichever access
// path serves it. That is easy to get right for a base-record scan and harder
// for an index, because an index read is a second structure that the write had
// to maintain — and hardest for an AGGREGATE index, whose maintenance is an
// atomic mutation whose effect a read in the same transaction has to observe
// before it is committed.
//
// The oracle is the same indexed/unindexed twin used elsewhere, run inside
// matching transactions on both sides: whatever the unindexed schema answers
// from the records is what the indexed schema must answer from its indexes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// mmTxPair runs body against a transaction on each schema and compares whatever
// the body reads through cmp.
type mmTxPair struct {
	t   *testing.T
	ctx context.Context
	itx *sql.Tx
	ntx *sql.Tx
}

// mmTxPreempted is raised when a statement fails with 40001 — the driver
// pre-empting an explicit transaction whose MVCC window aged out. It is
// recovered by mmInTxPair, which restarts the whole body.
//
// WHY THIS IS NOT A SWALLOWED FAILURE. 40001 is retryable BY CONTRACT: it says
// the transaction lost its read version, not that the answer was wrong. These
// tests assert read-your-writes CORRECTNESS, and an explicit transaction that
// spans several round-trips is bound to FDB's 5-second wall measured in WALL
// CLOCK — so on a loaded machine the window can age out between statements with
// nothing whatsoever wrong with the engine. Observed exactly once here, under a
// full 90-target suite with four FDB containers and a JVM live: the transaction
// had been open 5.305s against a 4s budget, and the same test passes in 0.10s
// in isolation.
//
// The distinction that keeps this honest is that ONLY 40001 restarts. Every
// other error still fails the test, and a WRONG ROW is not an error at all — it
// is an assertion failure recorded on t, which no retry can clear. So this can
// convert a slow machine into a longer run, and cannot convert a wrong answer
// into a pass.
type mmTxPreempted struct{ err error }

// mmIsPreempted reports whether err is the retryable 40001.
func mmIsPreempted(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeSerializationFailure
}

// mmCheck fails the test, or signals a restart when the transaction was
// pre-empted.
func (p *mmTxPair) mmCheck(err error, format string, args ...any) {
	p.t.Helper()
	if err == nil {
		return
	}
	if mmIsPreempted(err) {
		panic(mmTxPreempted{err})
	}
	p.t.Fatalf(format, args...)
}

func (p *mmTxPair) exec(stmt string) {
	p.t.Helper()
	_, err := p.itx.ExecContext(p.ctx, stmt)
	p.mmCheck(err, "indexed schema: exec %q: %v", stmt, err)
	_, err = p.ntx.ExecContext(p.ctx, stmt)
	p.mmCheck(err, "unindexed schema: exec %q: %v", stmt, err)
}

func (p *mmTxPair) rows(tx *sql.Tx, q string) []string {
	p.t.Helper()
	rows, err := tx.QueryContext(p.ctx, q)
	p.mmCheck(err, "query %q: %v", q, err)
	defer rows.Close()
	cols, err := rows.Columns()
	p.mmCheck(err, "columns: %v", err)
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			p.mmCheck(err, "scan: %v", err)
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	p.mmCheck(rows.Err(), "rows.Err: %v", rows.Err())
	return out
}

// want asserts the SQL-correct answer on both schemas, mid-transaction.
func (p *mmTxPair) want(name, q string, expect []string) {
	p.t.Helper()
	gi := p.rows(p.itx, q)
	gn := p.rows(p.ntx, q)
	if !mmEqRows(gn, expect) {
		p.t.Errorf("%s: UNINDEXED (oracle) answer is wrong mid-transaction\n  q: %s\n  got  %v\n  want %v",
			name, q, gn, expect)
	}
	if !mmEqRows(gi, expect) {
		p.t.Errorf("%s: the INDEXED schema does not see its own transaction's writes\n"+
			"  q: %s\n  got  %v\n  want %v\n"+
			"An index read inside a transaction must reflect the writes that transaction already "+
			"made; answering from a stale index is a wrong answer no retry reproduces.",
			name, q, gi, expect)
	}
}

// mmInTxPair runs body inside a fresh transaction pair, restarting the WHOLE
// body if the driver pre-empts a transaction with 40001.
//
// Restarting the whole body rather than the failed statement is what keeps the
// semantics intact: these tests assert that a transaction sees ITS OWN earlier
// writes, so a body resumed against a fresh transaction that never made those
// writes would assert nothing. A restart replays the writes first.
//
// The attempt cap is deliberately small. This exists to absorb a loaded
// machine, not to grind through a real defect, and a test that needed many
// attempts would be reporting something worth seeing rather than something
// worth hiding — so the cap fails loudly and says how long the transaction
// survived.
func mmInTxPair(t *testing.T, ctx context.Context, w *mmTwin, body func(p *mmTxPair)) {
	t.Helper()
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		preempted := func() (preempted error) {
			p, done := mmBeginPair(t, ctx, w)
			defer done()
			defer func() {
				if r := recover(); r != nil {
					pre, ok := r.(mmTxPreempted)
					if !ok {
						panic(r) // not ours — a real panic, or t.Fatalf's runtime.Goexit
					}
					preempted = pre.err
				}
			}()
			body(p)
			return nil
		}()
		if preempted == nil {
			return
		}
		if attempt == maxAttempts {
			t.Fatalf("the transaction was pre-empted on all %d attempts: %v\n"+
				"40001 means the explicit transaction's MVCC window aged out, which a loaded "+
				"machine can cause on its own — but %d times running is long enough that "+
				"something is holding the transaction open, not merely slow",
				maxAttempts, preempted, maxAttempts)
		}
		t.Logf("attempt %d pre-empted (%v); restarting the transaction body", attempt, preempted)
	}
}

func mmBeginPair(t *testing.T, ctx context.Context, w *mmTwin) (*mmTxPair, func()) {
	t.Helper()
	itx, err := w.idx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin on the indexed schema: %v", err)
	}
	ntx, err := w.plain.BeginTx(ctx, nil)
	if err != nil {
		_ = itx.Rollback()
		t.Fatalf("begin on the unindexed schema: %v", err)
	}
	return &mmTxPair{t: t, ctx: ctx, itx: itx, ntx: ntx}, func() {
		_ = itx.Rollback()
		_ = ntx.Rollback()
	}
}

// TestFDB_ReadYourWritesThroughValueIndex covers the value-index path: rows
// written earlier in the transaction must be findable through an index that
// only this transaction has updated.
func TestFDB_ReadYourWritesThroughValueIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_ryw_value", "rywv",
		"CREATE TABLE t (id BIGINT, a BIGINT, s STRING, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_s ON t (s) ")
	w.Exec("INSERT INTO t (id, a, s) VALUES (1, 10, 'x'), (2, 20, 'y')")

	mmInTxPair(t, ctx, w, func(p *mmTxPair) {
		// An INSERT this transaction made must be visible through the index.
		p.exec("INSERT INTO t (id, a, s) VALUES (3, 30, 'z')")
		p.want("a row inserted in this transaction",
			"SELECT id FROM t WHERE a = 30 ORDER BY id", []string{"3"})
		p.want("and through the other index",
			"SELECT id FROM t WHERE s = 'z' ORDER BY id", []string{"3"})
		p.want("and in a range that spans it",
			"SELECT id FROM t WHERE a >= 10 ORDER BY a, id", []string{"1", "2", "3"})

		// An UPDATE must move the row in the index: findable at the new value and
		// NOT at the old one. Missing the second half is the classic index-stale
		// bug, because the row is still returned — just by a key it no longer has.
		p.exec("UPDATE t SET a = 40 WHERE id = 1")
		p.want("the updated row is findable at its new value",
			"SELECT id FROM t WHERE a = 40 ORDER BY id", []string{"1"})
		p.want("and is GONE from its old value",
			"SELECT id FROM t WHERE a = 10 ORDER BY id", []string{})

		// A DELETE must remove it from the index too.
		p.exec("DELETE FROM t WHERE id = 2")
		p.want("a deleted row is gone from the index",
			"SELECT id FROM t WHERE a = 20 ORDER BY id", []string{})
		p.want("and from a range scan",
			"SELECT id FROM t ORDER BY a, id", []string{"3", "1"})

		// Writing a row and then re-writing it in the same transaction: the index
		// must carry only the last state, not one entry per intermediate value.
		p.exec("INSERT INTO t (id, a, s) VALUES (4, 50, 'w')")
		p.exec("UPDATE t SET a = 60 WHERE id = 4")
		p.exec("UPDATE t SET a = 70 WHERE id = 4")
		p.want("only the last of three writes is indexed",
			"SELECT id FROM t WHERE a >= 50 ORDER BY a, id", []string{"4"})
		p.want("the intermediate values left nothing behind",
			"SELECT id FROM t WHERE a = 50 OR a = 60 ORDER BY id", []string{})

		// Insert-then-delete within the transaction leaves no trace.
		p.exec("INSERT INTO t (id, a, s) VALUES (5, 80, 'v')")
		p.exec("DELETE FROM t WHERE id = 5")
		p.want("insert-then-delete leaves nothing in the index",
			"SELECT id FROM t WHERE a = 80 ORDER BY id", []string{})
	})
}

// TestFDB_ReadYourWritesThroughAggregateIndex is the harder half. An aggregate
// index is maintained by an ATOMIC MUTATION, whose effect a read in the same
// transaction has to observe before commit — a different mechanism from a value
// index's plain key writes, and one where a stale read returns a wrong NUMBER
// rather than a missing row.
func TestFDB_ReadYourWritesThroughAggregateIndex(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_ryw_agg", "rywa",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_cnt AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_sum AS SELECT SUM(v) FROM t GROUP BY g "+
			"CREATE INDEX t_min AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max AS SELECT MAX(v) FROM t GROUP BY g ")
	w.Exec("INSERT INTO t (id, g, v) VALUES (1, 1, 5), (2, 1, 7), (3, 2, 100)")

	mmInTxPair(t, ctx, w, func(p *mmTxPair) {
		countQ := "SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g"
		sumQ := "SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g"
		minQ := "SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g"
		maxQ := "SELECT g, MAX(v) FROM t GROUP BY g ORDER BY g"

		p.want("counts before any write in this transaction", countQ, []string{"1|2", "2|1"})

		// An INSERT must move every aggregate at once.
		//
		// Each expectation below is chosen to DIFFER from the pre-write value, which
		// is what makes it able to detect a stale index read at all: an assertion
		// whose expected value equals the value before the write passes just as
		// happily against an index that never saw it. That is why the insert is
		// split in two — v=3 moves count, sum and min but sits BELOW the existing
		// maximum, so a second row above it is needed before the max arm can tell a
		// fresh read from a stale one.
		p.exec("INSERT INTO t (id, g, v) VALUES (4, 1, 3)")
		p.want("count after an uncommitted insert", countQ, []string{"1|3", "2|1"})
		p.want("sum after an uncommitted insert", sumQ, []string{"1|15", "2|100"})
		p.want("min after an uncommitted insert", minQ, []string{"1|3", "2|100"})
		p.want("max is unmoved by a value below it", maxQ, []string{"1|7", "2|100"})

		p.exec("INSERT INTO t (id, g, v) VALUES (5, 1, 42)")
		p.want("max after an uncommitted insert above it", maxQ, []string{"1|42", "2|100"})
		p.want("count after the second insert", countQ, []string{"1|4", "2|1"})
		p.want("sum after the second insert", sumQ, []string{"1|57", "2|100"})
		p.exec("DELETE FROM t WHERE id = 5")
		p.want("max falls back when the row above it goes", maxQ, []string{"1|7", "2|100"})

		// An UPDATE that changes the value updates SUM and can move both extrema.
		p.exec("UPDATE t SET v = 99 WHERE id = 4")
		p.want("sum after an uncommitted update", sumQ, []string{"1|111", "2|100"})
		p.want("min after the smallest value grew", minQ, []string{"1|5", "2|100"})
		p.want("max after the largest value grew", maxQ, []string{"1|99", "2|100"})

		// An UPDATE that moves a row BETWEEN groups has to decrement one group and
		// increment the other, which is two atomic mutations that must both be seen.
		p.exec("UPDATE t SET g = 2 WHERE id = 4")
		p.want("count after a row moves between groups", countQ, []string{"1|2", "2|2"})
		p.want("sum after a row moves between groups", sumQ, []string{"1|12", "2|199"})
		p.want("max after a row moves between groups", maxQ, []string{"1|7", "2|100"})

		// A value nulled inside the transaction leaves the row counted but removes
		// its contribution to the extrema — the repaired MIN path, uncommitted.
		p.exec("UPDATE t SET v = NULL WHERE id = 3")
		p.want("count is unchanged by a nulled value", countQ, []string{"1|2", "2|2"})
		p.want("min ignores the newly-nulled value", minQ, []string{"1|5", "2|99"})
		p.want("max ignores the newly-nulled value", maxQ, []string{"1|7", "2|99"})

		// A DELETE removes the row from every aggregate.
		p.exec("DELETE FROM t WHERE id = 1")
		p.want("count after an uncommitted delete", countQ, []string{"1|1", "2|2"})
		p.want("sum after an uncommitted delete", sumQ, []string{"1|7", "2|99"})
		p.want("min after an uncommitted delete", minQ, []string{"1|7", "2|99"})

		// Emptying a group inside the transaction removes it from the result.
		p.exec("DELETE FROM t WHERE g = 1")
		p.want("an emptied group disappears mid-transaction", countQ, []string{"2|2"})
		p.want("and from the sum", sumQ, []string{"2|99"})
	})
}

// TestFDB_ReadYourWritesCommitAndRollback checks the two exits. What a
// transaction saw must survive its COMMIT, and must leave no trace after a
// ROLLBACK — through the indexes as much as through the records.
func TestFDB_ReadYourWritesCommitAndRollback(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_ryw_exit", "rywx",
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_cnt AS SELECT COUNT(*) FROM t GROUP BY a ")
	w.Exec("INSERT INTO t (id, a) VALUES (1, 10)")

	// ---- rollback ---- (mmInTxPair's cleanup rolls both sides back)
	mmInTxPair(t, ctx, w, func(p *mmTxPair) {
		p.exec("INSERT INTO t (id, a) VALUES (2, 20)")
		p.want("visible inside the transaction",
			"SELECT id FROM t WHERE a = 20", []string{"2"})
	})

	w.Want("a rolled-back insert left nothing in the index",
		"SELECT id FROM t WHERE a = 20", []string{})
	w.Want("nor in the aggregate index",
		"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a", []string{"10|1"})

	// ---- commit ----
	itx, err := w.idx.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ntx, err := w.plain.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, tx := range []*sql.Tx{itx, ntx} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (3, 30)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := itx.Commit(); err != nil {
		t.Fatalf("commit on the indexed schema: %v", err)
	}
	if err := ntx.Commit(); err != nil {
		t.Fatalf("commit on the unindexed schema: %v", err)
	}

	w.Want("a committed insert is in the index",
		"SELECT id FROM t WHERE a = 30", []string{"3"})
	w.Want("and in the aggregate index",
		"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a", []string{"10|1", "30|1"})
	w.Want("and the rolled-back one is still absent",
		fmt.Sprintf("SELECT COUNT(*) FROM t WHERE a = %d", 20), []string{"0"})
}
