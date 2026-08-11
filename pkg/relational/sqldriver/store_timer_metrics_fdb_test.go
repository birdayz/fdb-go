package sqldriver_test

// End-to-end proof that record-layer instrumentation is reachable from the SQL
// layer: arm a StoreTimer through the driver, run known SQL, and read the exact
// counts back out of the Prometheus exposition.
//
// This is the gap the whole change exists to close. The driver opens the
// record-layer database itself and caches it privately, so before
// EnableStoreTimer there was no way for a database/sql-only deployment to reach
// an FDBRecordContext and install a timer on it — record loads, index scans and
// commit timings were unobservable no matter what the operator did.
//
// Every test here registers its OWN record-layer database under a unique
// cluster-file key. That is load-bearing, not tidiness, in both directions: the
// timer's scope is one per cluster-file key, so tests sharing the package's
// clusterFilePath would share a timer and every "exactly N" assertion below would
// instead count whatever the rest of this parallel package happened to be doing —
// and, worse, arming a timer on the shared database would wrap every scan cursor
// in the whole suite. The client CONNECTION is shared (see
// sharedStoreTimerBackend); only the record-layer wrapper is per test.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/dst"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/internal/fdbclient"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/rlmetrics"
	"fdb.dev/pkg/relational/sqldriver"
)

// storeTimerBackendOnce guards the ONE raw FDB handle every test in this file shares.
//
// Each test still needs its own *recordlayer.FDBDatabase — that is what carries a
// per-test StoreTimer, and the isolation is the point — but a record-layer database
// is a thin wrapper, so all of them can sit on one client connection. Opening a
// connection per test instead put three extra clients into a test binary that
// already runs hundreds of FDB tests in parallel against one containerised cluster,
// and the pressure lands on the 5-second MVCC window that the explicit-transaction
// tests in this package are bounded by.
var (
	storeTimerBackendOnce sync.Once
	storeTimerBackendDB   fdb.BackendDatabase
	storeTimerBackendErr  error
)

func sharedStoreTimerBackend(t *testing.T) fdb.BackendDatabase {
	t.Helper()
	storeTimerBackendOnce.Do(func() {
		storeTimerBackendDB, storeTimerBackendErr = fdbclient.Open(clusterFilePath)
	})
	if storeTimerBackendErr != nil {
		t.Fatalf("open FDB: %v", storeTimerBackendErr)
	}
	return storeTimerBackendDB
}

// armTimer registers a dedicated record-layer database under key and returns the
// timer the driver installs on it. armFirst chooses which of the two orderings
// between EnableStoreTimer and the database appearing in the driver's cache is
// exercised — both must end with an instrumented database, and the two paths that
// make that true are different code (EnableStoreTimer's own cache lookup, versus
// applyStoreTimer at registration).
// clock, when supplied, becomes the record layer's elapsed-time source for this
// database. It exists so the explicit-transaction test can force the
// whole-transaction budget pre-emption through the SAME registered backend the
// timer is bound to — the timer and the clock have to live on one database, or
// the scrape would describe a different store from the one that was pre-empted.
func armTimer(t *testing.T, key string, armFirst bool, clock ...dst.Clock) *recordlayer.StoreTimer {
	t.Helper()

	var timer *recordlayer.StoreTimer
	if armFirst {
		timer = sqldriver.EnableStoreTimer(key)
	}

	db := recordlayer.NewFDBDatabaseWithBackend(sharedStoreTimerBackend(t))
	if len(clock) == 1 {
		db.SetEnv(&dst.Env{Clock: clock[0]})
	}
	db.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	t.Cleanup(sqldriver.RegisterBackend(key, db))

	if !armFirst {
		timer = sqldriver.EnableStoreTimer(key)
	}
	if db.Timer() != timer {
		t.Fatalf("the driver's database did not receive the armed timer (armFirst=%v); "+
			"a SQL-only deployment would silently scrape zeros", armFirst)
	}
	return timer
}

// scrape renders the timer through the exporter and parses the sample lines.
func scrape(t *testing.T, timer *recordlayer.StoreTimer) map[string]float64 {
	t.Helper()

	var buf bytes.Buffer
	if err := rlmetrics.WriteText(&buf, timer.Snapshot()); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("malformed sample line %q", line)
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			t.Fatalf("sample %q has a non-numeric value: %v", line, err)
		}
		out[f[0]] = v
	}
	return out
}

// TestFDB_StoreTimerExporter_CountsRealSQLWork is the fixture-math test: after a
// reset, N single-row INSERTs must show up as exactly N saved records on the
// scrape.
//
// The reset is what makes the arithmetic exact. Catalog and schema DDL writes
// records through the same FDBRecordStore.SaveRecord path as user data, so the
// counters are non-zero before the workload even starts; resetting after setup
// draws the line at the point the fixture becomes known.
func TestFDB_StoreTimerExporter_CountsRealSQLWork(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const key = "storetimer-metrics-counts"
	timer := armTimer(t, key, true)

	dsn := fmt.Sprintf("fdbsql:///testdb_sttimer?cluster_file=%s", key)
	setup, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { setup.Close() })

	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sttimer")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sttimer CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sttimer/s WITH TEMPLATE sttimer")

	db, err := sql.Open("fdbsql", dsn+"&schema=s")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// One warm-up write, so schema resolution and store-header state are settled
	// and the reset below starts from a steady state rather than mid-warm-up.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (0, 0)")

	before := scrape(t, timer)
	if before["fdb_recordlayer_save_record_seconds_count"] == 0 {
		t.Fatal("no saves recorded before the reset — the timer is not reaching the write path at all, " +
			"which would make the post-reset assertions below pass for the wrong reason")
	}

	timer.Reset()
	if got := scrape(t, timer); len(got) != 0 {
		t.Fatalf("Reset left %d samples behind: %v", len(got), got)
	}

	const rows = 7
	for i := 1; i <= rows; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i*10))
	}

	got := scrape(t, timer)

	// Exactly `rows` records saved: one per INSERT, no more. The table has no
	// secondary index, so nothing else writes a record on this path.
	if v := got["fdb_recordlayer_save_record_seconds_count"]; v != rows {
		t.Errorf("fdb_recordlayer_save_record_seconds_count = %v, want %d (one per INSERT)", v, rows)
	}
	// The count counterpart of the same operation, incremented at the same site.
	if v := got["fdb_recordlayer_save_record_key_total"]; v != rows {
		t.Errorf("fdb_recordlayer_save_record_key_total = %v, want %d", v, rows)
	}
	// TWO commits per autocommit statement, not one. Each page of the autocommit
	// page loop runs in its own transaction via FDBDatabase.Run, and a statement
	// takes a second page to learn it is exhausted — so a single-row INSERT costs
	// two commits. That is pre-existing execution behaviour; what is new is that
	// it is now measurable, and it is exactly the kind of thing a commit counter
	// exists to reveal.
	//
	// This assertion is pinned to the observed number deliberately. If the page
	// loop is ever changed to avoid the trailing probe transaction this test goes
	// red with the ratio in the message, which is the correct outcome: it is a
	// halving of the commit load per statement, and it should be noticed rather
	// than absorbed.
	if v := got["fdb_recordlayer_commit_seconds_count"]; v != 2*rows {
		t.Errorf("fdb_recordlayer_commit_seconds_count = %v, want %d (%d autocommit statements × 2 "+
			"transactions each: one execution page plus the page that finds the statement exhausted)",
			v, 2*rows, rows)
	}
	// Durations are sums over real work: positive, and small enough that a
	// unit mix-up (nanoseconds rendered as seconds) would be caught.
	sum := got["fdb_recordlayer_save_record_seconds_sum"]
	if sum <= 0 || sum > 60 {
		t.Errorf("fdb_recordlayer_save_record_seconds_sum = %v, want a plausible duration in SECONDS "+
			"(a nanosecond value rendered here would be enormous)", sum)
	}
	// Size counters carry bytes, and a saved record's key is never zero-length.
	if v := got["fdb_recordlayer_save_record_key_bytes_total"]; v <= 0 {
		t.Errorf("fdb_recordlayer_save_record_key_bytes_total = %v, want > 0", v)
	}

	// Reads must be visible too, and must not be inflated by the writes above.
	// The warm-up query before the reset is deliberate: the FIRST statement on a
	// freshly-checked-out pool connection also initialises this session's catalog
	// view, and that is one-time work whose counters belong to neither the read
	// nor the write being measured.
	var warm int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&warm); err != nil {
		t.Fatalf("warm-up SELECT COUNT(*): %v", err)
	}
	timer.Reset()
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("SELECT COUNT(*): %v", err)
	}
	if n != rows+1 {
		t.Fatalf("COUNT(*) = %d, want %d", n, rows+1)
	}
	after := scrape(t, timer)
	if v := after["fdb_recordlayer_save_record_seconds_count"]; v != 0 {
		t.Errorf("a read-only statement recorded %v saves; samples: %v", v, after)
	}

	// The scan event counts ONE PER RECORD PRODUCED, plus one for the terminal
	// probe that reports exhaustion — so a full scan of rows+1 records reads
	// rows+2. This is the assertion that would have caught the original defect:
	// the event used to be recorded once, around CURSOR CONSTRUCTION, which for a
	// lazy cursor timed an allocation and reported a single occurrence no matter
	// how many records the scan returned. Java instruments the cursor's onNext
	// instead (StoreTimer.java:894-931, wrapped at FDBRecordStore.java:1441), and
	// this count is what that difference looks like from the outside.
	if v := after["fdb_recordlayer_scan_records_seconds_count"]; v != rows+2 {
		t.Errorf("fdb_recordlayer_scan_records_seconds_count = %v, want %d (%d records + the "+
			"terminal exhaustion probe). A value of 1 means the event is timing cursor "+
			"construction again rather than each record produced; samples: %v",
			v, rows+2, rows+1, after)
	}
}

// TestFDB_StoreTimerExporter_IndexScansAreCounted pins the index-scan event across
// the two entry points that reach an index, because they are separate methods and
// only one of them used to be instrumented.
//
// The dimension that was unprobed: `ScanIndex` (plain BY_VALUE) was instrumented
// while `ScanIndexByType` — which the query executor uses for aggregate/group and
// vector scans — returned the maintainer's cursor raw. A test that only exercised a
// BY_VALUE predicate would have passed with every aggregate-index scan in the engine
// uncounted, which is the same shape of gap as the record scan on the outer wrapper.
func TestFDB_StoreTimerExporter_IndexScansAreCounted(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const key = "storetimer-metrics-index"
	timer := armTimer(t, key, true)

	dsn := fmt.Sprintf("fdbsql:///testdb_sttimer_idx?cluster_file=%s", key)
	setup, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { setup.Close() })

	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sttimer_idx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sttimeridx "+
			"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v AS SELECT v, id FROM t ORDER BY v, id")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sttimer_idx/s WITH TEMPLATE sttimeridx")

	db, err := sql.Open("fdbsql", dsn+"&schema=s")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Five rows, all sharing v = 100, so an equality probe on the index returns a
	// known number of entries.
	const matching = 5
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, v) VALUES (1, 100), (2, 100), (3, 100), (4, 100), (5, 100), (6, 999)")

	// Warm up so per-connection catalog initialisation is not in the measurement.
	var warm int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE v = 100").Scan(&warm); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if warm != matching {
		t.Fatalf("warm-up COUNT = %d, want %d", warm, matching)
	}

	timer.Reset()
	rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE v = 100")
	if err != nil {
		t.Fatalf("indexed SELECT: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterating: %v", err)
	}
	rows.Close()
	if n != matching {
		t.Fatalf("returned %d rows, want %d", n, matching)
	}

	got := scrape(t, timer)
	// Per index ENTRY, like the record scan: an equality probe over 5 entries costs
	// at least 5 timed OnNext calls. A lower bound rather than an exact count
	// because the plan may also probe the index for its bounds; a value of 1 would
	// mean the event is back to timing cursor construction, and 0 would mean the
	// entry point this query uses is not instrumented at all.
	if v := got["fdb_recordlayer_scan_index_seconds_count"]; v < matching {
		t.Errorf("fdb_recordlayer_scan_index_seconds_count = %v, want >= %d (one per index entry "+
			"produced). 0 means this scan entry point is uninstrumented; 1 means the event is "+
			"timing cursor construction rather than each entry; samples: %v", v, matching, got)
	}
	// A covering-index query must not be charged a full record scan.
	if v := got["fdb_recordlayer_save_record_seconds_count"]; v != 0 {
		t.Errorf("an indexed read-only query recorded %v saves; samples: %v", v, got)
	}
}

// TestFDB_StoreTimerExporter_ArmingAfterTheBackendStillInstruments covers the
// other ordering. EnableStoreTimer must work whether it runs before or after the
// database exists, because an operator arming metrics at startup and one arming
// them from an admin endpoint after traffic has begun are both realistic, and
// the second is the one where a silent no-op looks exactly like "no traffic".
func TestFDB_StoreTimerExporter_ArmingAfterTheBackendStillInstruments(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const key = "storetimer-metrics-late-arm"
	timer := armTimer(t, key, false)

	dsn := fmt.Sprintf("fdbsql:///testdb_sttimer_late?cluster_file=%s", key)
	setup, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { setup.Close() })

	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sttimer_late")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sttimerlate CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sttimer_late/s WITH TEMPLATE sttimerlate")

	db, err := sql.Open("fdbsql", dsn+"&schema=s")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")
	timer.Reset()

	const rows = 3
	for i := 2; i < 2+rows; i++ {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", i))
	}

	if v := scrape(t, timer)["fdb_recordlayer_save_record_seconds_count"]; v != rows {
		t.Errorf("fdb_recordlayer_save_record_seconds_count = %v, want %d", v, rows)
	}
}

// TestFDB_StoreTimerExporter_ExplicitTransactionIsInstrumented pins the path that
// does NOT go through FDBDatabase.Run: an explicit BeginTx→COMMIT builds its
// context directly, and that construction site historically dropped per-database
// state (the DST env) on the floor precisely because it enumerated the fields by
// hand. An uninstrumented explicit transaction would be the same bug wearing a
// different field name, and it is invisible — the metric just reads lower.
func TestFDB_StoreTimerExporter_ExplicitTransactionIsInstrumented(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const key = "storetimer-metrics-explicit-tx"
	// One-shot spike on the SAME database the timer is bound to.
	clk := newLateClock(30 * time.Second)
	timer := armTimer(t, key, true, clk)

	dsn := fmt.Sprintf("fdbsql:///testdb_sttimer_tx?cluster_file=%s", key)
	setup, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { setup.Close() })

	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sttimer_tx")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sttimertx CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sttimer_tx/s WITH TEMPLATE sttimertx")

	db, err := sql.Open("fdbsql", dsn+"&schema=s")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (1)")
	timer.Reset()

	// Four in-transaction statements on one read version, so this transaction
	// carries the budget assumption like any other — and it is the one case where
	// retrying naively would corrupt the measurement rather than merely repeat
	// it. A pre-empted attempt still charged its own INSERTs to the exporter, so
	// the counters below would read 8 saves after one retry and the exact
	// assertion would have quietly become an inequality nobody wrote.
	//
	// BeforeAttempt re-zeroes the timer at the start of EVERY attempt, so the
	// scrape describes exactly the attempt that succeeded. That keeps `want 4`
	// and `want 1 commit` meaning what they say.
	const rows = 4
	var attemptsRun int
	opts := spikeOnce(clk, &attemptsRun)
	opts.BeforeAttempt = func(i int) {
		attemptsRun = i
		timer.Reset()
	}
	retryTx(t, db, opts, func(a txAttempt) error {
		for i := 10; i < 10+rows; i++ {
			if _, err := a.tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO t (id) VALUES (%d)", i)); err != nil {
				return err
			}
		}
		return a.tx.Commit()
	})
	mustHaveRetried(t, attemptsRun)

	got := scrape(t, timer)
	if v := got["fdb_recordlayer_save_record_seconds_count"]; v != rows {
		t.Errorf("fdb_recordlayer_save_record_seconds_count = %v, want %d "+
			"(explicit BeginTx path must be instrumented too)", v, rows)
	}
	// One transaction, therefore one commit — the count that distinguishes an
	// explicit transaction from the same statements in autocommit.
	if v := got["fdb_recordlayer_commit_seconds_count"]; v != 1 {
		t.Errorf("fdb_recordlayer_commit_seconds_count = %v, want 1 for a single explicit transaction", v)
	}
}
