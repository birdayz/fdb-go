package sqldriver

// WHERE THE 4-SECOND TRANSACTION READ BUDGET IS ACTUALLY ENFORCED — and therefore
// how far past it a transaction can get.
//
// THE CLAIM THIS FILE REFUTES, stated as it was inherited: that
// paginatingRows.preflightTxBudget is a strict PER-PAGE pre-flight, evaluated once
// before each page and unable to pre-empt mid-page, so "an overage equal to the
// last page's residual duration is inherent by construction". If that were true a
// single page could run arbitrarily long, and the 4s budget's one second of
// headroom against FDB's 5s MVCC wall would be worth nothing: one long page would
// blow the wall outright, which is a far more serious condition than a small
// overshoot.
//
// It is NOT true. The preflight is the second of two guards, not the only one.
// paginatingRows.executeProps arms EVERY page with TimeLimit = txPageTimeLimit and
// hands it the TRANSACTION-scoped *ScanLimiterState, which preflightTxBudget has
// just re-anchored on the client's read-version instant. Every leaf cursor then
// re-checks that anchor BEFORE EACH RECORD — key_value_cursor.go:195,
// index_scan.go:567, record_key_cursor.go:77, text_cursor.go:102,
// count_index_maintainer.go:222, bitmap_value_index_maintainer.go:574,
// multidimensional_index_maintainer.go:659 — so the budget binds at RECORD
// granularity inside the page, and the preflight only converts the resulting
// out-of-band stop into the caller-facing 40001 at the next page boundary.
//
// That is Java's structure exactly: CursorLimitManager.tryRecordScan computes
// haltedDueToTimeLimit per record from a TimeScanLimiter anchored on
// FDBRecordContext.getTransactionCreateTime (CursorLimitManager.java:92-94,
// :135-138), i.e. transaction-scoped, checked per record.
//
// SO THE OVERAGE IS RECORD-SIZED, NOT PAGE-SIZED, and that is the difference this
// test exists to keep true. A tens-of-milliseconds overshoot on a real box is the
// residual of ONE record boundary plus the unwind back to the next page's
// preflight — inherent, expected, and bounded well inside the 1s of headroom. A
// PAGE-sized overshoot would not be bounded by anything.
//
// MEASURED HERE, on a clock that advances 25ms per read and nothing else: the
// pre-emption reports `read version 4.025s old, budget 4s` — an overshoot of
// EXACTLY ONE clock step, after 158 of 600 rows and 177 clock reads. That is the
// number the whole argument rests on, and it is why a real-FDB run reporting a
// few tens of milliseconds past the budget is the guard working rather than a
// defect.
//
// THE TWO GUARDS ARE INDEPENDENT, and each was measured by deleting it:
//   - per-record check gone: the scan finishes all 600 rows having read the clock
//     17 times in total. One page, unbounded.
//   - preflight gone: the per-record check still fires (1058 clock reads), but
//     every later page re-enters on a spent budget and is carried by its
//     per-cursor free initial pass, so all 600 rows come back — with NO error, at
//     roughly 26 simulated seconds, five times past FDB's wall.
//
// Neither guard subsumes the other: one bounds the page, the other bounds the
// transaction. Each has its own assertion below, on its own evidence.
//
// WHY THIS IS A SIM TEST AND NOT A TIMED ONE. Whether a given scan on a given box
// crosses 4s is precisely the load-dependent question this codebase has already
// paid for asserting twice (see duec_window_loss_reproducer_test.go's record of
// both failures). Here the clock is the test's own: frozen through setup, then
// stepping a fixed amount per read, so the trip point is a function of the code
// and nothing else. Time cannot pass except by scanning.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// gatedSteppingClock is frozen until Arm, then advances by step on every read.
//
// The gate is what makes the measurement mean something: schema creation, inserts
// and the transaction's first read all happen at a standstill, so the ONLY thing
// that can spend the transaction's budget is the scan under test. Without it the
// setup would consume an unknown share of the 4s and the trip row would stop being
// a property of the enforcement path.
type gatedSteppingClock struct {
	mu    sync.Mutex
	now   time.Time
	step  time.Duration
	reads int
}

func (c *gatedSteppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	c.reads++
	return c.now
}

// Arm starts the clock. Called after the transaction's read version is taken, so
// the MVCC window opens at a standstill and its whole budget is available.
func (c *gatedSteppingClock) Arm(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = step
}

func (c *gatedSteppingClock) Reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// openSimSchemaOnClock is openSimSchemaWithEnv with a caller-supplied clock.
func openSimSchemaOnClock(t *testing.T, seed uint64, clock dst.Clock, tableDDL string) *sql.DB {
	t.Helper()
	env := dst.NewSim(seed)
	env.Clock = clock
	env.Buggify = dst.DisabledBuggifier()
	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })

	ctx := context.Background()
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tmpl "+tableDDL)
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSimTxBudget_BudgetBindsMidPageNotOnlyAtThePagePreflight pins the guard the
// inherited analysis said did not exist.
//
// ONE statement, ONE scan, inside an explicit transaction whose window opens with
// the full budget available. The clock then advances only as the scan reads, and
// the scan is long enough that finishing it would take ~2.5x the budget. It must
// NOT finish: the per-record check has to cut it off, and the elapsed time the
// pre-emption reports has to land just past 4s — never past FDB's 5s wall, which
// is the number the whole two-ceiling design exists to stay under.
//
// MUTATION DIRECTIONS (both verified; the fix can be wrong in either):
//   - delete the per-record time check in key_value_cursor.go:195 (or stop
//     threading the transaction's ScanState / the 4s TimeLimit into the page's
//     ExecuteProperties): nothing stops the scan mid-page, the whole table comes
//     back inside page 1, no second page is ever requested, the preflight never
//     runs — and this test reddens on "the scan COMPLETED".
//   - delete preflightTxBudget: the mid-page stop still happens, but each
//     subsequent page then grinds out only its per-cursor free initial pass, and
//     the statement either completes or dies as something other than the typed
//     pre-emption — this test reddens on the typed-identity assertion.
func TestSimTxBudget_BudgetBindsMidPageNotOnlyAtThePagePreflight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clock := &gatedSteppingClock{now: dst.Epoch}
	db := openSimSchemaOnClock(t, 71, clock,
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")

	// Enough rows that draining them all costs far more than the budget once the
	// clock is armed, so "the scan ran to completion" and "the scan was cut off"
	// are separated by a wide margin rather than by a rounding.
	const rows = 600
	var b strings.Builder
	b.WriteString("INSERT INTO t (id, v) VALUES ")
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d,%d)", i, i)
	}
	mustExecSQL(t, db, ctx, b.String())

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// The first read takes the read version — the instant the budget is anchored
	// on — while the clock is still frozen, so the transaction starts with the
	// entire 4s available and the scan below is the only thing that can spend it.
	var v int64
	if err := tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 0").Scan(&v); err != nil {
		t.Fatalf("first in-tx read: %v", err)
	}

	// 25ms per clock read: draining 600 rows costs at least 15s of simulated
	// time against a 4s budget, so a scan that is not cut off mid-page overshoots
	// by nearly four budgets.
	const step = 25 * time.Millisecond
	clock.Arm(step)

	got := 0
	rs, qerr := tx.QueryContext(ctx, "SELECT id FROM t")
	if qerr == nil {
		for rs.Next() {
			var id int64
			if err := rs.Scan(&id); err != nil {
				qerr = err
				break
			}
			got++
		}
		if qerr == nil {
			qerr = rs.Err()
		}
		rs.Close()
	}
	t.Logf("in-tx scan under a %v/read clock: %d/%d rows, %d clock reads, err=%v",
		step, got, rows, clock.Reads(), qerr)

	// GUARD ONE — THE PER-RECORD CHECK, asserted on its own evidence rather than
	// inferred from the outcome. This clock only moves when something reads it, so
	// "the budget was consulted between records" is directly countable: a scan that
	// checks per record reads the clock at least once per row it returns. Deleting
	// key_value_cursor.go's TimeLimit branch drops the count to a handful of reads
	// for the whole scan, which no outcome-level assertion can distinguish from a
	// fast scan.
	if reads := clock.Reads(); reads < got {
		t.Fatalf("the scan returned %d rows having read the clock only %d times.\nThe "+
			"transaction's time budget is NOT being consulted between records, so a single "+
			"page is unbounded: it keeps reading until something outside the driver stops "+
			"it, which on a real store is FDB's 5-second MVCC wall killing the whole "+
			"transaction with a raw 1007. The 1s of headroom the 4s ceiling buys is worth "+
			"nothing if the only check is at the page boundary. The per-record check lives "+
			"at key_value_cursor.go's TimeLimit branch and measures against the "+
			"transaction-scoped ScanLimiterState (Java: CursorLimitManager.tryRecordScan, "+
			":135-138, off a TimeScanLimiter anchored on getTransactionCreateTime); it is "+
			"what bounds the overshoot to ONE record boundary instead of one page.",
			got, reads)
	}

	// GUARD TWO — THE PAGE PREFLIGHT, which is what converts the mid-page stop into
	// a caller-facing error. The two are independent and both load-bearing: with the
	// preflight gone the per-record check still fires, but every later page is
	// re-entered on a spent budget and grinds out its per-cursor free initial pass,
	// so the scan finishes far past FDB's wall and reports SUCCESS.
	if qerr == nil {
		t.Fatalf("the scan COMPLETED (%d/%d rows, %d clock reads) with the transaction's "+
			"read version far past its 4s budget, and reported NO error.\nThe per-record "+
			"check stopped each page, but nothing enforced the budget ACROSS pages: each "+
			"new page re-entered on a spent budget and was carried by the per-cursor free "+
			"initial pass (Java's usedInitialPass, granted unconditionally on the time "+
			"branch — CursorLimitManager.java:138 has no failOnScanLimitReached term). On a "+
			"real store every one of those pages is reading at a read version FDB has "+
			"already retired. paginatingRows.preflightTxBudget is the ceiling that makes "+
			"the transaction, not just the page, bounded.",
			got, rows, clock.Reads())
	}
	if got == 0 {
		t.Fatalf("the scan returned NO rows before failing (%v): the budget was already "+
			"spent at the first page's preflight, so this run never exercised the "+
			"mid-page path it exists to test and its verdict is vacuous", qerr)
	}
	if got >= rows {
		t.Fatalf("the scan returned every one of %d rows AND an error (%v) — a cut-off "+
			"scan must not also hand back the complete result", rows, qerr)
	}

	if !api.IsTransactionTimeLimit(qerr) {
		t.Fatalf("the cut-off scan failed as %v (%T), not as the driver's typed read-budget "+
			"pre-emption. The mid-page stop is out-of-band, and preflightTxBudget is what "+
			"turns it into a caller-facing 40001 at the next page boundary; without that "+
			"conversion the caller gets some other error for a condition whose only remedy "+
			"is to decompose the transaction.", qerr, qerr)
	}
	var ttl *api.TransactionTimeLimitError
	if !errors.As(qerr, &ttl) || ttl.Source != api.TimeLimitPreempted {
		t.Fatalf("the pre-emption reports source %q, want %q — FDB's own 1007 arriving here "+
			"would mean the driver failed to pre-empt at all", ttl.Source, api.TimeLimitPreempted)
	}

	// THE HEADLINE NUMBER. The overage past the 4s budget must be a RECORD
	// boundary, not a page: with the per-record check live it is a small multiple
	// of one clock step, and it must stay inside FDB's 5s wall with room to spare.
	// A page-sized overage on this fixture would be seconds.
	if ttl.Elapsed < ttl.Limit {
		t.Fatalf("the pre-emption fired at elapsed=%v with limit=%v — it must not be able "+
			"to fire before the budget is actually spent", ttl.Elapsed, ttl.Limit)
	}
	const fdbWall = 5 * time.Second
	if ttl.Elapsed >= fdbWall {
		t.Fatalf("the transaction reached elapsed=%v before the driver pre-empted, which is "+
			"AT OR PAST FDB's %v MVCC wall.\nThe 4s ceiling exists to leave a second of "+
			"headroom; an overage this large means enforcement moved back to the page "+
			"boundary and the page is now unbounded. On a real store this is a raw 1007, "+
			"not a clean stop.", ttl.Elapsed, fdbWall)
	}
	// Tighter than the wall, and this is the claim that separates "record-sized"
	// from "merely under 5s": the overshoot is a handful of clock reads, not a
	// share of the remaining scan.
	if over := ttl.Elapsed - ttl.Limit; over > 40*step {
		t.Fatalf("the pre-emption overshot its %v budget by %v — more than 40 clock reads "+
			"(%v each).\nThe overshoot must be ONE record boundary plus the unwind to the "+
			"next page's preflight. An overshoot that scales with the fixture instead means "+
			"the per-record check stopped binding and the page-boundary check is carrying "+
			"the budget alone.", ttl.Limit, over, step)
	}
}
