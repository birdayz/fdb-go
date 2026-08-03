package sqldriver_test

// Regression coverage for an out-of-band per-page resource stop underneath a
// correlated NOT EXISTS. Out-of-band stops (scanned records, scanned bytes,
// and — the reported defect — the per-page TIME budget) are resumable page
// boundaries by design: the time budget exists so a page ends before FDB's 5s
// transaction wall. A slow-but-healthy cluster must therefore page, never fail
// a legitimate query.
//
// The shape that broke: NOT EXISTS lowers to a nested loop whose inner leg is a
// RecordQueryFirstOrDefaultPlan. When that leg's leaf cursor halted out-of-band
// before the operator could decide what to flow, executeFirstOrDefault turned
// the resumable stop into a *recordlayer.ScanLimitReachedError, which the SQL
// layer surfaced as the terminal
//
//	54F01: leaf cursor scan limit exceeded: scan limit reached: time limit exceeded
//
// Nothing was exhausted: the outer row had not been emitted, so the enclosing
// flat map still held a perfectly good resume point.
//
// Both directions of the defect are covered, because every assertion compares
// against an unbudgeted oracle run of the same query: converting the stop into
// an error fails on the error, and reading the truncated inner leg as an EMPTY
// one (Java's RecordQueryFirstOrDefaultPlan.java:100-106 / issue 3220, which
// answers NOT EXISTS = true from a partial scan) fails on the extra rows.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// notExistsQuery is the corpus reproducer's predicate (scenario
// fc_0000000831_q2_p2): a correlated NOT EXISTS whose inner leg is an
// unindexed self-scan, so the inner leg is the leaf that halts on the budget.
const notExistsQuery = "SELECT id, b FROM t_rd WHERE NOT EXISTS " +
	"(SELECT 1 FROM t_rd AS r WHERE r.b = t_rd.b AND r.a < 3)"

// drainIDB runs sqlText and returns every row rendered as "id|b".
func drainIDB(ctx context.Context, conn *sql.Conn, sqlText string) ([]string, error) {
	r, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var id, b int64
		if err := r.Scan(&id, &b); err != nil {
			return out, err
		}
		out = append(out, fmt.Sprintf("%d|%d", id, b))
		if len(out) > 10000 {
			return out, fmt.Errorf("row runaway")
		}
	}
	return out, r.Err()
}

// seedNotExistsFixture fills t_rd with rows whose b values repeat in seven
// groups, only two of which contain a row with a < 3. NOT EXISTS is therefore
// true for five of the seven groups — a non-empty, non-total answer, so a
// wrong-direction regression (all rows / no rows) cannot pass by accident.
func seedNotExistsFixture(t *testing.T, ctx context.Context, db *sql.DB, rows int) {
	t.Helper()
	var ins strings.Builder
	ins.WriteString("INSERT INTO t_rd VALUES ")
	for i := 1; i <= rows; i++ {
		if i > 1 {
			ins.WriteString(",")
		}
		b := i % 7
		a := 5
		if b < 2 {
			a = 1
		}
		fmt.Fprintf(&ins, "(%d,%d,%d)", i, a, b)
	}
	if _, err := db.ExecContext(ctx, ins.String()); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestFDB_NotExistsOutOfBandStop_PaginatesNotErrors pins that a correlated
// NOT EXISTS whose inner leg is cut off by a per-page resource limit still
// returns EXACTLY the rows the same query returns with no per-page limit.
//
// The budget here is counted in SCANNED RECORDS, not milliseconds: the defect
// is identical for every out-of-band reason (they share one conversion site),
// and a record budget makes the page boundaries a property of the data rather
// than of how loaded the machine is. The TIME reason — the one actually
// reported — is pinned deterministically over SimFDB's logical clock in
// TestSim_NotExistsTimeLimit_PaginatesNotErrors.
func TestFDB_NotExistsOutOfBandStop_PaginatesNotErrors(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_notexists_oob", "neoob",
		"CREATE TABLE t_rd (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE uniq (id BIGINT, k BIGINT, v BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 40
	seedNotExistsFixture(t, ctx, db, rows)

	// uniq.k is UNINDEXED and unique, so a correlated lookup on it is a full
	// scan that finds its one match part-way through and then keeps scanning to
	// prove there is no second — which is exactly the window the strict arm
	// below needs the per-page budget to close in.
	var uins strings.Builder
	uins.WriteString("INSERT INTO uniq VALUES ")
	for i := 1; i <= rows; i++ {
		if i > 1 {
			uins.WriteString(",")
		}
		fmt.Fprintf(&uins, "(%d,%d,%d)", i, i, 100+i)
	}
	if _, err := db.ExecContext(ctx, uins.String()); err != nil {
		t.Fatalf("seed uniq: %v", err)
	}

	// Oracle: no per-page budget beyond the 4s transaction ceiling.
	plainConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	want, err := drainIDB(ctx, plainConn, notExistsQuery)
	if err != nil {
		t.Fatalf("unbudgeted NOT EXISTS query failed: %v", err)
	}
	if len(want) == 0 || len(want) == rows {
		t.Fatalf("oracle returned %d/%d rows — the fixture no longer exercises a "+
			"partially-satisfied NOT EXISTS", len(want), rows)
	}

	assertSame := func(t *testing.T, got []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("budgeted NOT EXISTS returned %d rows, oracle returned %d\n got: %v\nwant: %v",
				len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d = %q, want %q (full got: %v)", i, got[i], want[i], got)
			}
		}
	}

	// ScannedRows is the CLOCK-FREE arm: the budget is counted in records, so
	// the number of pages and the page boundaries are identical on every
	// machine. One inner leg costs `rows` scans, so a budget of a few inner
	// legs guarantees both that pages end mid-query and that each page makes
	// progress.
	t.Run("ScannedRowsLimit", func(t *testing.T) {
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, int64(3*rows)).
				Build())
		})
		got, err := drainIDB(ctx, conn, notExistsQuery)
		if err != nil {
			t.Fatalf("a correlated NOT EXISTS under a %d-row per-page scan budget must "+
				"paginate the out-of-band stop and still complete; got error after %d rows: %v",
				3*rows, len(got), err)
		}
		assertSame(t, got)
	})

	// The STRICT arm: a correlated scalar subquery goes down the same operator
	// but through its at-most-one-row probe, which reads one row PAST the first
	// to prove the cardinality. That probe meets the out-of-band stop on its own
	// return path, so it is an independently-breakable direction of the fix and
	// gets its own coverage — a page boundary there must also page, not error,
	// and must not be mistaken for "there was no second row" (which would let a
	// genuine cardinality violation through).
	t.Run("ScannedRowsLimitStrictScalarSubquery", func(t *testing.T) {
		const scalarQuery = "SELECT id, (SELECT u.v FROM uniq AS u WHERE u.k = t_rd.id) FROM t_rd"
		wantScalar, err := drainIDB(ctx, plainConn, scalarQuery)
		if err != nil {
			t.Fatalf("unbudgeted correlated scalar subquery failed: %v", err)
		}
		if len(wantScalar) != rows {
			t.Fatalf("oracle returned %d rows, want %d — the scalar subquery fixture "+
				"no longer produces one value per outer row", len(wantScalar), rows)
		}
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, int64(3*rows)).
				Build())
		})
		got, err := drainIDB(ctx, conn, scalarQuery)
		if err != nil {
			t.Fatalf("a correlated scalar subquery under a per-page scan budget must "+
				"paginate the out-of-band stop and still complete; got error after %d rows: %v",
				len(got), err)
		}
		if len(got) != len(wantScalar) {
			t.Fatalf("scalar subquery: got %d rows, want %d", len(got), len(wantScalar))
		}
		for i := range wantScalar {
			if got[i] != wantScalar[i] {
				t.Fatalf("scalar row %d = %q, want %q", i, got[i], wantScalar[i])
			}
		}
	})

	// The corpus scenario carries an ORDER BY over the same predicate; the
	// sort sits ABOVE the flat map, so it must not change the outcome.
	t.Run("ScannedRowsLimitOrdered", func(t *testing.T) {
		const ordered = notExistsQuery + " ORDER BY b DESC, id"
		wantOrdered, err := drainIDB(ctx, plainConn, ordered)
		if err != nil {
			t.Fatalf("unbudgeted ordered NOT EXISTS failed: %v", err)
		}
		conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, int64(3*rows)).
				Build())
		})
		got, err := drainIDB(ctx, conn, ordered)
		if err != nil {
			t.Fatalf("ordered NOT EXISTS under a per-page scan budget must paginate; "+
				"got error after %d rows: %v", len(got), err)
		}
		if len(got) != len(wantOrdered) {
			t.Fatalf("ordered: got %d rows, want %d\n got: %v\nwant: %v",
				len(got), len(wantOrdered), got, wantOrdered)
		}
		for i := range wantOrdered {
			if got[i] != wantOrdered[i] {
				t.Fatalf("ordered row %d = %q, want %q", i, got[i], wantOrdered[i])
			}
		}
	})
}
