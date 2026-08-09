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
// THE NUMBER THE WHOLE ARGUMENT RESTS ON is the overshoot past the 4s budget,
// and it is DERIVED rather than recited. On a clock that advances only when
// something reads it, the reported elapsed is
//
//	ceil(limit/step)*step + step
//
// — the first read at or past the budget, which is the soonest the per-record
// check can possibly see it spent, plus the one further read that stamps the
// figure onto the error. TWO reads of granularity, whatever the step. The test
// computes that and asserts it; see the final assertion below.
//
// Stating it as a millisecond count instead is how this went wrong before: the
// header recited a clock-read count that no longer matched what the run printed,
// and an "exactly one clock step" reading of the overshoot that is true only
// because 4s happens to be a whole multiple of 25ms — at a 30ms step the same
// mechanism reports 4.05s, which is 1.67 steps. Nothing here should state a
// number the test does not compute.
//
// This is also why a real-FDB run reporting a few tens of milliseconds past the
// budget is the guard working rather than a defect.
//
// THE TWO GUARDS ARE INDEPENDENT, and neither subsumes the other: one bounds the
// page, the other bounds the transaction. Each has its own assertion below, on
// its own evidence, and each was established by deleting the guard and watching
// its own assertion — not the other's — go red.
//
// Those two deletions are edits to PRODUCTION files, so no test can re-derive
// their receipts and none are quoted here. To re-run either, exactly:
//   - per-record check: delete the TimeLimit branch in
//     pkg/recordlayer/cursors/key_value_cursor.go (the one consulting
//     ScanLimiterState), then run this test. GUARD ONE fails — the scan drains
//     the whole table inside page one, so no second page is requested and the
//     preflight never runs.
//   - preflight: delete paginatingRows.preflightTxBudget's budget check, then run
//     this test. GUARD ONE still passes (the mid-page stop still happens); GUARD
//     TWO fails, because each later page re-enters on a spent budget, is carried
//     by its per-cursor free initial pass, and the statement completes with no
//     error far past FDB's wall.
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
	mu   sync.Mutex
	now  time.Time
	step time.Duration
	// reads counts EVERY read since construction, including the schema
	// creation, the inserts and the transaction's first read — all of which
	// happen before Arm and none of which advance the clock.
	reads int
	// armReads is the value of reads at the moment Arm was called, so a count
	// scoped to the armed window is available. Without it the only available
	// count is cumulative, and a guard of the form "reads must be at least the
	// number of rows returned" is then satisfied partly by setup reads that
	// could not possibly have been budget checks — it fails OPEN, growing
	// easier to pass the more setup the fixture does.
	armReads int
}

func (c *gatedSteppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	c.reads++
	return c.now
}

// Arm starts the clock. Called after the transaction's read version is taken, so
// the MVCC window opens at a standstill and its whole budget is available. It
// also anchors the read counter, because only reads taken after this point can
// be the budget checks the test is counting.
func (c *gatedSteppingClock) Arm(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = step
	c.armReads = c.reads
}

// Reads is the cumulative count since construction. Report it; do not gate on
// it — see ReadsSinceArm.
func (c *gatedSteppingClock) Reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// ReadsSinceArm counts only the reads taken while the clock was running, which
// are the only ones that can have spent the transaction's budget. This is the
// count any guard must use: the cumulative one is inflated by setup and makes a
// lower-bound guard easier to satisfy the more setup there is.
func (c *gatedSteppingClock) ReadsSinceArm() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads - c.armReads
}

// preemptionOf returns the driver's typed read-budget pre-emption carried by
// err, plus a description of err for a failure message.
//
// The description is built WITHOUT reading any field of the typed error, and
// that separation is the point: errors.As leaves its target nil when it returns
// false, so a diagnostic that formats Source off that target dereferences nil on
// exactly the path reached when something else has already gone wrong —
// replacing the message that would have explained the failure with a panic.
// Callers get a nil pointer and a usable string, never one that depends on the
// other.
func preemptionOf(err error) (*api.TransactionTimeLimitError, string) {
	var ttl *api.TransactionTimeLimitError
	if errors.As(err, &ttl) {
		return ttl, fmt.Sprintf("%v (%T)", err, err)
	}
	return nil, fmt.Sprintf("%v (%T)", err, err)
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
	t.Logf("in-tx scan under a %v/read clock: %d/%d rows, %d clock reads since arm (%d cumulative), err=%v",
		step, got, rows, clock.ReadsSinceArm(), clock.Reads(), qerr)

	// GUARD ONE — THE PER-RECORD CHECK, asserted on its own evidence rather than
	// inferred from the outcome. This clock only moves when something reads it, so
	// "the budget was consulted between records" is directly countable: a scan that
	// checks per record reads the clock at least once per row it returns. Deleting
	// key_value_cursor.go's TimeLimit branch drops the count to a handful of reads
	// for the whole scan, which no outcome-level assertion can distinguish from a
	// fast scan.
	//
	// THE DIRECTION OF THE ALARM IS "TOO FEW", and the count is scoped to the
	// ARMED window for that reason. Schema creation, 600 inserts and the
	// transaction's first read all read this clock before Arm, and none of them
	// can be a per-record budget check on the scan under test. Counting them would
	// make this lower bound easier to clear the more setup the fixture does — a
	// fail-open guard, which on a fixture with more setup rows than scanned rows
	// would pass with the per-record check deleted outright.
	// Vacuity: the scoping only matters if this fixture actually reads the clock
	// before arming it. If it stops doing so the two counts coincide and the
	// guard below is no longer being asked the question it was corrected to ask.
	if clock.Reads() == clock.ReadsSinceArm() {
		t.Fatalf("no clock read happened before the arm (%d cumulative, %d since arm), so "+
			"scoping the guard to the armed window makes no difference on this fixture and "+
			"a pass here says nothing about it. Setup — schema, %d inserts, the transaction's "+
			"first read — is supposed to read this clock while it is frozen.",
			clock.Reads(), clock.ReadsSinceArm(), rows)
	}
	if reads := clock.ReadsSinceArm(); reads < got {
		t.Fatalf("the scan returned %d rows having read the clock only %d times SINCE THE "+
			"CLOCK WAS ARMED.\nThe "+
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
	// TWO SEPARATE QUESTIONS, TWO SEPARATE MESSAGES. "Is it the typed error at
	// all" cannot be answered in the same breath as "which source does it
	// report", because the answer to the second is only available when the first
	// is yes — see preemptionOf.
	ttl, describe := preemptionOf(qerr)
	if ttl == nil {
		t.Fatalf("the cut-off scan failed as %s, which is not the driver's typed "+
			"read-budget pre-emption at all, so no source can be read off it.", describe)
	}
	if ttl.Source != api.TimeLimitPreempted {
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
	// THE HEADLINE NUMBER, DERIVED RATHER THAN RECITED.
	//
	// On this clock time moves only when something reads it, so the reported
	// elapsed is fully determined by the enforcement path and can be computed
	// from first principles instead of transcribed from a run:
	//
	//	firstFire = the first read at or past the budget = ceil(limit/step)*step
	//	reported  = firstFire + step
	//
	// — two reads' worth of granularity in total. The first term is the per-record
	// check itself: it cannot see the budget spent any sooner than the read on
	// which it is spent. The second is the read that stamps Elapsed onto the
	// error, taken after the stop has propagated back out.
	//
	// So the overshoot is TWO CLOCK READS AT MOST, whatever the step — that is
	// what "record-sized, not page-sized" means, and it is why it is asserted as a
	// formula rather than as a millisecond count. A hardcoded count would be a
	// property of this fixture's step; this is a property of the mechanism. (It
	// was checked at a second step to make sure: at 30ms the run reports 4.05s,
	// which is what this formula predicts and is NOT one step past the budget.)
	//
	// If this stops holding, do not widen it. A larger multiple means more time
	// passed between the check that should have stopped the scan and the stop
	// taking effect, i.e. enforcement drifting back towards the page boundary.
	firstFire := ((ttl.Limit + step - 1) / step) * step
	if want := firstFire + step; ttl.Elapsed != want {
		t.Fatalf("the pre-emption reports elapsed=%v (%.2f clock reads past its %v budget); "+
			"the enforcement path admits exactly %v.\nThe per-record check fires on the first "+
			"read at or past the budget (%v here) and one further read stamps the error, so "+
			"the overshoot is two reads of granularity and nothing else. A larger figure means "+
			"the per-record check stopped binding and the page-boundary check is carrying the "+
			"budget alone — an overshoot that scales with the fixture rather than with the "+
			"clock.", ttl.Elapsed, float64(ttl.Elapsed-ttl.Limit)/float64(step), ttl.Limit,
			want, firstFire)
	}
}

// TestSimTxBudget_ClockReadCountIsScopedToTheArmedWindow pins the correction to
// GUARD ONE's counting, on the axis that made it fail OPEN.
//
// GUARD ONE is a LOWER bound — "the clock was read at least once per row
// returned" — so any read added to its count makes it EASIER to satisfy. The
// cumulative counter includes every read taken while the clock was frozen:
// schema creation, the inserts, the transaction's first read. Those cannot be
// per-record budget checks on the scan under test, and there are enough of them
// that on a fixture with more setup reads than scanned rows the guard clears
// with the per-record check deleted outright. The direction of the alarm is
// TOO FEW post-arm reads; setup must not be able to supply them.
//
// The numbers below are the shape that breaks, not a transcript of a run: many
// frozen reads, then a scan that returns many rows having consulted the budget
// almost never.
func TestSimTxBudget_ClockReadCountIsScopedToTheArmedWindow(t *testing.T) {
	t.Parallel()
	const (
		setupReads  = 200 // reads taken while the clock is frozen
		armedReads  = 2   // what a scan with no per-record check manages
		rowsScanned = 158 // rows it nonetheless returned
	)
	c := &gatedSteppingClock{now: dst.Epoch}
	for i := 0; i < setupReads; i++ {
		c.Now()
	}
	if elapsed := c.Now().Sub(dst.Epoch); elapsed != 0 {
		t.Fatalf("the frozen clock advanced by %v over %d reads; the gate is what makes the "+
			"armed window the only thing that can spend the budget", elapsed, setupReads)
	}
	c.Arm(25 * time.Millisecond)
	for i := 0; i < armedReads; i++ {
		c.Now()
	}

	// The premise: on this input the cumulative count CLEARS the lower bound. If
	// it ever stops doing so the test is no longer exercising the fail-open shape.
	if c.Reads() < rowsScanned {
		t.Fatalf("cumulative reads %d < %d rows, so the uncorrected guard would already have "+
			"failed on this input and this test no longer demonstrates the fail-open",
			c.Reads(), rowsScanned)
	}
	// The correction: the scoped count must NOT clear it.
	if c.ReadsSinceArm() >= rowsScanned {
		t.Fatalf("reads since arm = %d, which clears the >= %d rows lower bound even though "+
			"only %d reads happened while the clock was running. Setup reads are leaking into "+
			"the count, so GUARD ONE is satisfied by work that cannot possibly have been a "+
			"per-record budget check — it fails OPEN, and gets easier to pass the more setup "+
			"the fixture does.", c.ReadsSinceArm(), rowsScanned, armedReads)
	}
	if got := c.ReadsSinceArm(); got != armedReads {
		t.Fatalf("reads since arm = %d, want exactly the %d taken after Arm", got, armedReads)
	}
	if got := c.Reads(); got != setupReads+1+armedReads {
		t.Fatalf("cumulative reads = %d, want %d — Reads must stay CUMULATIVE; it is what "+
			"the log line reports, and losing the pre-arm reads from it would hide that "+
			"setup is happening at all", got, setupReads+1+armedReads)
	}
}

// TestSimTxBudget_PreemptionDiagnosticSurvivesAForeignError pins the DIAGNOSTIC
// path, which is the one reached only when something has already gone wrong and
// is therefore the one never exercised by a green run.
//
// errors.As leaves its target nil when it returns false. A diagnostic written as
// `if !errors.As(err, &ttl) || ttl.Source != want { t.Fatalf(..., ttl.Source) }`
// therefore panics on precisely the failure it exists to explain: the reader
// gets a nil-pointer stack trace instead of "the scan failed as <this> instead".
// preemptionOf makes that shape unwritable by returning the description
// separately, and this test drives the branch a passing budget test never can.
func TestSimTxBudget_PreemptionDiagnosticSurvivesAForeignError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string // substring the description must carry
	}{
		{"plain_error", errors.New("connection reset"), "connection reset"},
		{"wrapped_foreign_error", fmt.Errorf("page 2: %w", errors.New("boom")), "page 2: boom"},
		{"nil_error", nil, "<nil>"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ttl, describe := preemptionOf(tc.err)
			if ttl != nil {
				t.Fatalf("preemptionOf(%v) reported a typed pre-emption (%+v); this input "+
					"carries none, so the branch this test exists to drive was not taken",
					tc.err, ttl)
			}
			if !strings.Contains(describe, tc.want) {
				t.Fatalf("the description of a non-pre-emption error is %q, which does not "+
					"name the error (%q). This string is the ENTIRE diagnostic on this path: "+
					"the typed error's fields are unavailable, so anything the message does "+
					"not say here is lost.", describe, tc.want)
			}
		})
	}

	// And the typed case still yields its pointer, so the guard above cannot be
	// satisfied by a helper that simply never reports a pre-emption.
	typed := &api.TransactionTimeLimitError{
		Source: api.TimeLimitPreempted, Elapsed: 4025 * time.Millisecond, Limit: 4 * time.Second,
	}
	ttl, _ := preemptionOf(fmt.Errorf("query: %w", typed))
	if ttl == nil {
		t.Fatalf("preemptionOf did not find a wrapped *TransactionTimeLimitError; with this "+
			"broken every assertion above holds vacuously and the budget test's typed-identity "+
			"check can never fail either. want %+v", typed)
	}
	if ttl.Source != api.TimeLimitPreempted || ttl.Elapsed != typed.Elapsed {
		t.Fatalf("preemptionOf returned %+v, want the wrapped %+v", ttl, typed)
	}
}
