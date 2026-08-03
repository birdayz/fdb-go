package sqldriver

// The STRICT at-most-one branch of a first-or-default keeps the WHOLE-LEG
// restart: the operator is holding its first row when the out-of-band stop
// arrives, and a stop cursor carries no value, so checkpointing past that row
// would lose it. That restart cannot make progress when a page's budget cannot
// reach the first row and probe past it — every page restarts at the same
// position and the continuation never advances.
//
// That livelock is deliberate, and the driver's no-progress tripwire is its
// backstop: correct-or-loud is the right answer for a budget too small to
// answer the query at all. Nothing else pins that the backstop actually
// engages here, and it engages only because the restart continuation is
// BYTE-IDENTICAL across pages — a property of how restartAfterOutOfBand
// encodes itself and of how the flat map wraps it. Change either (the restart
// token gaining a nonce, the flat map folding a counter into its envelope) and
// this shape stops failing loudly and starts hanging, with no other test
// noticing.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestSim_StrictLegTinyBudget_FailsLoudNeverHangs(t *testing.T) {
	t.Parallel()
	key, _ := injectTickingSimFDB(t, 9317, time.Millisecond)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///strictp?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /strictp")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE strictp_tmpl "+
		"CREATE TABLE t_rd (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE uniq (id BIGINT, k BIGINT, v BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /strictp/s WITH TEMPLATE strictp_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///strictp?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	defer db.Close()

	const rows = 21
	var ins strings.Builder
	ins.WriteString("INSERT INTO t_rd VALUES ")
	var uins strings.Builder
	uins.WriteString("INSERT INTO uniq VALUES ")
	for i := 1; i <= rows; i++ {
		if i > 1 {
			ins.WriteString(",")
			uins.WriteString(",")
		}
		fmt.Fprintf(&ins, "(%d,%d,%d)", i, i%5, i%7)
		fmt.Fprintf(&uins, "(%d,%d,%d)", i, i, 100+i)
	}
	mustExecSQL(t, db, ctx, ins.String())
	mustExecSQL(t, db, ctx, uins.String())

	// Correlated STRICT scalar subquery: uniq.k is unindexed and unique, so the
	// leg finds its one match part-way and keeps scanning to prove there is no
	// second — the window where the strict branch takes its restart.
	const q = "SELECT id, (SELECT v FROM uniq AS u WHERE u.k = t_rd.id) FROM t_rd"

	for _, budget := range []int64{2, 5, 10, 20} {
		budget := budget
		t.Run(fmt.Sprintf("budget_%dms", budget), func(t *testing.T) {
			conn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
				ec.SetOptions(api.NewOptionsBuilder().
					Set(api.OptExecutionTimeLimit, budget).
					Build())
			})
			done := make(chan struct{})
			var n int
			var qerr error
			go func() {
				defer close(done)
				r, e := conn.QueryContext(ctx, q)
				if e != nil {
					qerr = e
					return
				}
				defer r.Close()
				for r.Next() {
					n++
					if n > 100000 {
						qerr = fmt.Errorf("row runaway")
						return
					}
				}
				qerr = r.Err()
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("budget=%dms: the strict leg did not terminate in 30s. The "+
					"whole-leg restart cannot advance under this budget, so the "+
					"driver's no-progress tripwire is the only thing that ends the "+
					"query — it did not fire, and a spurious error has become a hang",
					budget)
			}
			if qerr == nil {
				t.Fatalf("budget=%dms returned %d rows and no error; a budget too "+
					"small to reach the first row and probe past it cannot answer "+
					"this query, so it must fail loudly", budget, n)
			}
			if !strings.Contains(qerr.Error(), "cannot progress under the configured per-page resource limits") {
				t.Fatalf("budget=%dms failed with %v; want the no-progress tripwire — "+
					"any other failure means the restart is ending the query by some "+
					"route this test is not actually covering", budget, qerr)
			}
		})
	}
}
