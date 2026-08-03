package sqldriver

// The per-page TIME budget is a resumable page boundary by design — it exists
// so a page ends before FDB's 5s transaction wall, and the SQL layer resumes
// from the continuation. Underneath a correlated NOT EXISTS it was instead
// escaping as a terminal query error:
//
//	54F01: leaf cursor scan limit exceeded: scan limit reached: time limit exceeded
//
// so a slow-but-healthy cluster failed a legitimate query. NOT EXISTS lowers to
// a nested loop whose inner leg is a RecordQueryFirstOrDefaultPlan; when that
// leg's leaf halted on the budget before the operator could decide what to
// flow, the resumable stop was converted into a *recordlayer.ScanLimitReachedError.
//
// This test drives the whole SQL stack over SimFDB with a LOGICAL clock that
// ticks a fixed amount per read, so the time limit fires after an exact number
// of clock reads. The page boundaries are then a property of the query, not of
// how loaded the machine is — a wall-clock version of this test either goes
// vacuous on a fast box (the budget never trips) or trips the driver's
// no-progress tripwire on a slow one.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/simfdb"
)

// tickClock advances a fixed delta on every read. It is the smallest clock that
// makes a TIME budget deterministic: the budget converts to an exact number of
// clock reads, and every leaf cursor's limit check is one read.
type tickClock struct {
	mu   sync.Mutex
	now  time.Time
	tick time.Duration
}

func (c *tickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.tick)
	return c.now
}

// txCountingBackend counts write transactions. A paginating SELECT opens exactly
// one per page, so the delta across a query is the page count — the instrument
// that keeps a "this query paginated" test from silently going vacuous when the
// budget stops being tight enough to end a page.
type txCountingBackend struct {
	fdb.BackendDatabase
	n atomic.Int64
}

func (b *txCountingBackend) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	b.n.Add(1)
	return b.BackendDatabase.Transact(f)
}

func (b *txCountingBackend) count() int64 { return b.n.Load() }

// injectTickingSimFDB is injectSimFDB with the simulation's wall clock replaced
// by a ticking logical clock and its backend wrapped in a transaction counter.
func injectTickingSimFDB(t *testing.T, seed uint64, tick time.Duration) (string, *txCountingBackend) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	env.Clock = &tickClock{now: dst.Epoch, tick: tick}
	backend := &txCountingBackend{BackendDatabase: simfdb.New(env)}
	simDB := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	return key, backend
}

// pinSimConn pins one *sql.Conn and configures its embedded connection, so the
// options under test apply to the connection that actually runs the statements.
func pinSimConn(t *testing.T, db *sql.DB, configure func(*embedded.EmbeddedConnection)) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("driver conn is %T, want *embedded.EmbeddedConnection", driverConn)
		}
		configure(ec)
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	return conn
}

func drainIDBSim(ctx context.Context, conn *sql.Conn, sqlText string) ([]string, error) {
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

// TestSim_NotExistsTinyTimeBudget_MakesProgress is the probe that decides
// between the two possible resume positions for an out-of-band stop taken before
// the inner leg yielded anything.
//
// Restarting the whole leg is only viable while a page's budget can hold one
// entire inner leg. Below that it livelocks: every page re-scans the same prefix,
// the continuation never advances, and the driver's no-progress tripwire ends the
// query with a 54F01 — the same class of spurious failure on a slow cluster that
// this whole change exists to remove, just with a different message. Checkpointing
// on the inner's own position makes progress per INNER ROW instead, so the budget
// can be arbitrarily small and the query still finishes.
//
// The budgets below are deliberately far under one inner leg. Each must return
// the identical answer, and the whole thing must terminate promptly — a livelock
// shows up here as either the tripwire error or a test that runs away.
func TestSim_NotExistsTinyTimeBudget_MakesProgress(t *testing.T) {
	t.Parallel()
	const tick = time.Millisecond

	key, _ := injectTickingSimFDB(t, 832, tick)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///tiny?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /tiny")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tiny_tmpl "+
		"CREATE TABLE t_rd (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /tiny/s WITH TEMPLATE tiny_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///tiny?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	defer db.Close()

	// Small enough that a per-inner-row page rate still finishes quickly, large
	// enough that one inner leg cannot fit in any of the budgets below.
	const rows = 21
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
	mustExecSQL(t, db, ctx, ins.String())

	const q = "SELECT id, b FROM t_rd WHERE NOT EXISTS " +
		"(SELECT 1 FROM t_rd AS r WHERE r.b = t_rd.b AND r.a < 3)"

	oracleConn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	want, err := drainIDBSim(ctx, oracleConn, q)
	if err != nil {
		t.Fatalf("unbudgeted NOT EXISTS query failed: %v", err)
	}
	if len(want) == 0 || len(want) == rows {
		t.Fatalf("oracle returned %d/%d rows — the fixture no longer exercises a "+
			"partially-satisfied NOT EXISTS", len(want), rows)
	}

	for _, budgetMillis := range []int64{5, 10, 20, 40} {
		t.Run(fmt.Sprintf("%dms", budgetMillis), func(t *testing.T) {
			conn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionTimeLimit, budgetMillis).
					Build())
			})
			start := time.Now()
			got, err := drainIDBSim(ctx, conn, q)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("a %dms per-page budget must still complete the query by "+
					"checkpointing the inner leg; got error after %d rows: %v",
					budgetMillis, len(got), err)
			}
			if elapsed > time.Second {
				t.Fatalf("a %dms per-page budget took %v — pages are not advancing "+
					"the continuation fast enough to be called progress",
					budgetMillis, elapsed)
			}
			if len(got) != len(want) {
				t.Fatalf("%dms budget returned %d rows, oracle returned %d\n got: %v\nwant: %v",
					budgetMillis, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%dms budget row %d = %q, want %q", budgetMillis, i, got[i], want[i])
				}
			}
		})
	}
}

// TestSim_NotExistsTimeLimit_PaginatesNotErrors runs the reported query shape
// under a time budget that is guaranteed to end pages mid-query, and requires
// the answer to be identical to the same query with no per-page budget.
//
// Both directions of the defect are covered by that oracle comparison:
// converting the stop into an error fails on the error, and reading the
// truncated inner leg as an EMPTY one — which is what Java's
// RecordQueryFirstOrDefaultPlan.java:100-106 does, answering NOT EXISTS = true
// from a partial scan (FoundationDB/fdb-record-layer#3220) — fails on the
// extra rows.
func TestSim_NotExistsTimeLimit_PaginatesNotErrors(t *testing.T) {
	t.Parallel()
	// 1ms per clock read against a 500ms budget: a page ends after exactly 500
	// clock reads. One outer row costs far fewer than that, so every page makes
	// progress (the driver's no-progress tripwire never fires), while the whole
	// 60-row query costs several times a page, so pages genuinely end mid-query.
	const tick = time.Millisecond
	const budgetMillis = 500

	key, backend := injectTickingSimFDB(t, 831, tick)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///netl?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /netl")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE netl_tmpl "+
		"CREATE TABLE t_rd (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /netl/s WITH TEMPLATE netl_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///netl?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	defer db.Close()

	// b repeats in seven groups; only the b=0 and b=1 groups hold a row with
	// a < 3, so NOT EXISTS is true for five of seven — a partial answer, so a
	// wrong-direction regression (all rows / no rows) cannot pass by accident.
	const rows = 60
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
	mustExecSQL(t, db, ctx, ins.String())

	// The corpus reproducer's predicate (scenario fc_0000000831_q2_p2): the
	// inner leg is an unindexed correlated self-scan, so the leaf that halts on
	// the budget is the one underneath the first-or-default.
	const q = "SELECT id, b FROM t_rd WHERE NOT EXISTS " +
		"(SELECT 1 FROM t_rd AS r WHERE r.b = t_rd.b AND r.a < 3)"

	oracleConn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	oracleStart := backend.count()
	want, err := drainIDBSim(ctx, oracleConn, q)
	if err != nil {
		t.Fatalf("unbudgeted NOT EXISTS query failed: %v", err)
	}
	oraclePages := backend.count() - oracleStart
	if len(want) == 0 || len(want) == rows {
		t.Fatalf("oracle returned %d/%d rows — the fixture no longer exercises a "+
			"partially-satisfied NOT EXISTS", len(want), rows)
	}

	budgetConn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionTimeLimit, int64(budgetMillis)).
			Build())
	})
	budgetStart := backend.count()
	got, err := drainIDBSim(ctx, budgetConn, q)
	if err != nil {
		t.Fatalf("a correlated NOT EXISTS under a %dms per-page time budget must paginate "+
			"the out-of-band time-limit stop and still complete; got error after %d rows: %v",
			budgetMillis, len(got), err)
	}
	budgetPages := backend.count() - budgetStart

	// NON-VACUITY. Everything above is satisfied by a run that never reached the
	// budget at all, which is exactly how this test would rot into proving
	// nothing if the fixture got cheaper or the budget got looser. One
	// transaction per page makes "did it actually paginate" measurable.
	if budgetPages < 2 {
		t.Fatalf("the budgeted run used %d transaction(s) — it never ended a page, so "+
			"this test never exercised the out-of-band stop it claims to pin",
			budgetPages)
	}
	if budgetPages <= oraclePages {
		t.Fatalf("the budgeted run used %d pages and the unbudgeted run %d — the %dms "+
			"budget is not tightening the page boundaries, so the two runs are the "+
			"same experiment", budgetPages, oraclePages, budgetMillis)
	}

	if len(got) != len(want) {
		t.Fatalf("time-budgeted NOT EXISTS returned %d rows, oracle returned %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (full got: %v)", i, got[i], want[i], got)
		}
	}
}
