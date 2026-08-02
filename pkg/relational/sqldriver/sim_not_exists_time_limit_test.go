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
	"testing"
	"time"

	"fdb.dev/pkg/dst"
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

// injectTickingSimFDB is injectSimFDB with the simulation's wall clock replaced
// by a ticking logical clock.
func injectTickingSimFDB(t *testing.T, seed uint64, tick time.Duration) string {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	env.Clock = &tickClock{now: dst.Epoch, tick: tick}
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	return key
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

	key := injectTickingSimFDB(t, 831, tick)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///netl?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /netl")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE netl_tmpl "+
		"CREATE TABLE t_rd (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))")
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
	want, err := drainIDBSim(ctx, oracleConn, q)
	if err != nil {
		t.Fatalf("unbudgeted NOT EXISTS query failed: %v", err)
	}
	if len(want) == 0 || len(want) == rows {
		t.Fatalf("oracle returned %d/%d rows — the fixture no longer exercises a "+
			"partially-satisfied NOT EXISTS", len(want), rows)
	}

	budgetConn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionTimeLimit, int64(budgetMillis)).
			Build())
	})
	got, err := drainIDBSim(ctx, budgetConn, q)
	if err != nil {
		t.Fatalf("a correlated NOT EXISTS under a %dms per-page time budget must paginate "+
			"the out-of-band time-limit stop and still complete; got error after %d rows: %v",
			budgetMillis, len(got), err)
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
