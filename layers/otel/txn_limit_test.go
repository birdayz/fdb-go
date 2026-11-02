package otel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func TestTxnPolicyContext(t *testing.T) {
	t.Parallel()
	if got := txnTimeout(context.Background()); got != benchTxnTimeout || isExporterTxn(context.Background()) {
		t.Fatalf("default timeout %v exporter=%v, want the 1.5s benchmark bound", got, isExporterTxn(context.Background()))
	}
	ctx := withExporterTxns(withTxnLimiter(t.Context(), make(chan struct{}, 1)), 3*time.Second)
	if got := txnTimeout(ctx); got != 3*time.Second || !isExporterTxn(ctx) {
		t.Fatalf("exporter timeout %v exporter=%v", got, isExporterTxn(ctx))
	}
	if _, ok := ctx.Value(txnLimiterKey{}).(chan struct{}); !ok {
		t.Fatal("exporter policy dropped the limiter")
	}
}

func TestExporterTxnError(t *testing.T) {
	t.Parallel()
	live := t.Context()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	admission := fail("admission", "q", "bad span")
	budget := wrap("budget", "q", fdb.Error{Code: 1031})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		kind string // "" means err is returned unchanged
		code int    // FDB code the result must still carry, 0 for none
	}{
		{"admission kept", live, admission, "", 0},
		{"corruption kept", live, fail("corruption", "q", "bad row"), "", 0},
		{"ownership kept", live, fail("ownership", "q", "not ours"), "", 0},
		{"budget kept", live, budget, "", 1031},
		{"canceled kept", live, wrap("canceled", "q", context.Canceled), "", 0},
		{"timeout", live, fdb.Error{Code: 1031}, "transient", 1031},
		{"timeout pointer", live, &fdb.Error{Code: 1031}, "transient", 1031},
		{"wrapped timeout", live, fmt.Errorf("commit: %w", fdb.Error{Code: 1031}), "transient", 1031},
		{"transaction_too_old", live, fdb.Error{Code: 1007}, "transient", 1007},
		{"not_committed", live, fdb.Error{Code: 1020}, "transient", 1020},
		{"commit_unknown_result", live, fdb.Error{Code: 1021}, "transient", 1021},
		{"transaction_cancelled", live, fdb.Error{Code: 1025}, "transient", 1025},
		{"process_behind", live, fdb.Error{Code: 1037}, "transient", 1037},
		{"proxy_tag_throttled", live, fdb.Error{Code: 1223}, "transient", 1223},
		{"record layer retriable", live, &recordlayer.RecordCoreRetriableTransactionError{Message: "retry"}, "transient", 0},
		{"platform_error", live, fdb.Error{Code: 1500}, "operation", 1500},
		{"transaction_too_large", live, fdb.Error{Code: 2101}, "operation", 2101},
		{"key_too_large", live, fdb.Error{Code: 2102}, "operation", 2102},
		{"internal_error", live, fdb.Error{Code: 4100}, "operation", 4100},
		{"plain error", live, errors.New("boom"), "operation", 0},
		{"deadline error", live, context.DeadlineExceeded, "canceled", 0},
		{"canceled caller, FDB error", canceled, fdb.Error{Code: 1025}, "canceled", 1025},
		{"canceled caller, plain error", canceled, errors.New("boom"), "canceled", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := exporterTxnError(tc.ctx, "op", tc.err)
			if tc.kind == "" {
				if got != tc.err {
					t.Fatalf("domain error changed: %v -> %v", tc.err, got)
				}
			} else {
				requireOtelKind(t, got, tc.kind)
				if !errors.Is(got, tc.err) {
					t.Fatalf("classification lost the cause: %v", got)
				}
			}
			if code, ok := fdbErrorCode(got); tc.code != 0 && (!ok || code != tc.code) {
				t.Fatalf("code %d, %v; want %d", code, ok, tc.code)
			}
			if tc.ctx == canceled && !errors.Is(got, context.Canceled) {
				t.Fatalf("canceled caller missing from %v", got)
			}
		})
	}
	var transient *TransientError
	if !errors.As(exporterTxnError(live, "op", fdb.Error{Code: 1031}), &transient) {
		t.Fatal("timeout is not a TransientError")
	}
	if exporterTxnError(live, "op", nil) != nil {
		t.Fatal("nil error classified")
	}
}

func TestWithTxnLimiterNil(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if withTxnLimiter(ctx, nil) != ctx {
		t.Fatal("nil slots attached a limiter")
	}
}

// One FDB testcontainer serves every subtest; each uses a fresh namespace.
func TestTxnLimitRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	fixture := func(t *testing.T) (context.Context, *recordlayer.FDBDatabase, subspace.Subspace) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		t.Cleanup(cancel)
		b := openOtelFixture(t, ctx, cluster, DefaultConfig(), false)
		return ctx, b.db, b.root.Sub("txn_limit")
	}
	t.Run("limit_bounds_concurrency", func(t *testing.T) { t.Parallel(); testTxnLimitBound(t, fixture) })
	t.Run("no_limiter_no_limit", func(t *testing.T) { t.Parallel(); testTxnLimitAbsent(t, fixture) })
	t.Run("canceled_waiter", func(t *testing.T) { t.Parallel(); testTxnLimitCanceled(t, fixture) })
	t.Run("bench_errors_unclassified", func(t *testing.T) { t.Parallel(); testTxnBenchErrors(t, fixture) })
	t.Run("exporter_absorbs_retryable", func(t *testing.T) { t.Parallel(); testExporterTxnAbsorbs(t, fixture) })
	t.Run("bench_retry_limit", func(t *testing.T) { t.Parallel(); testBenchTxnRetryLimit(t, fixture) })
	t.Run("exporter_timeout_bounds", func(t *testing.T) { t.Parallel(); testExporterTxnTimeoutBound(t, fixture) })
	t.Run("exporter_deadline_bounds", func(t *testing.T) { t.Parallel(); testExporterTxnDeadlineBound(t, fixture) })
	t.Run("exporter_attaches_limiter", func(t *testing.T) { t.Parallel(); testExporterTxnLimit(t, cluster) })
	t.Run("exporter_calls_absorb_retryable", func(t *testing.T) { t.Parallel(); testExporterCallsAbsorb(t, cluster) })
	t.Run("exporter_calls_fail_transient", func(t *testing.T) { t.Parallel(); testExporterCallsFail(t, cluster) })
}

type txnFixture func(*testing.T) (context.Context, *recordlayer.FDBDatabase, subspace.Subspace)

// peakTracker records the most concurrent fn bodies; await blocks a body
// until the peak reaches n or the deadline passes.
type peakTracker struct{ cur, peak, calls atomic.Int64 }

func (p *peakTracker) enter() {
	p.calls.Add(1)
	n := p.cur.Add(1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			return
		}
	}
}

func (p *peakTracker) await(n int64, d time.Duration) {
	for deadline := time.Now().Add(d); p.peak.Load() < n && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
}

func runConcurrentSlots(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, n int, body func()) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := runTxnSlot(ctx, db, "limit test", func(*recordlayer.FDBRecordContext) (any, error) {
				body()
				return nil, nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func testTxnLimitBound(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	const limit, callers = 3, 24
	slots := make(chan struct{}, limit)
	var p peakTracker
	runConcurrentSlots(t, withTxnLimiter(ctx, slots), db, callers, func() {
		p.enter()
		// Hold the slot until the limit is reached, so the peak is not vacuous.
		p.await(limit, 10*time.Second)
		time.Sleep(5 * time.Millisecond)
		p.cur.Add(-1)
	})
	if p.peak.Load() != limit || p.calls.Load() != callers || len(slots) != 0 {
		t.Fatalf("peak=%d calls=%d held=%d, want peak %d over %d calls and no held slot", p.peak.Load(), p.calls.Load(), len(slots), limit, callers)
	}
}

func testTxnLimitAbsent(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	const callers = 12
	var p peakTracker
	// Every body waits for all callers; a limit would leave the peak short.
	runConcurrentSlots(t, ctx, db, callers, func() {
		p.enter()
		p.await(callers, 10*time.Second)
		p.cur.Add(-1)
	})
	if p.peak.Load() != callers || p.calls.Load() != callers {
		t.Fatalf("peak=%d calls=%d, want %d unlimited", p.peak.Load(), p.calls.Load(), callers)
	}
}

func testTxnLimitCanceled(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	slots := make(chan struct{}, 1)
	slots <- struct{}{} // another transaction holds the only slot
	ran := false
	fn := func(*recordlayer.FDBRecordContext) (any, error) { ran = true; return nil, nil }

	waitCtx, cancel := context.WithCancel(withTxnLimiter(ctx, slots))
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := runTxnSlot(waitCtx, db, "limit test", fn)
	requireOtelKind(t, err, "canceled")
	if !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("waiter: err=%v ran=%t", err, ran)
	}

	// A canceled context does not take a free slot.
	<-slots
	_, err = runTxnSlot(waitCtx, db, "limit test", fn)
	requireOtelKind(t, err, "canceled")
	if ran || len(slots) != 0 {
		t.Fatalf("canceled caller ran=%t held=%d", ran, len(slots))
	}
}

func testExporterTxnLimit(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, func(c *ExporterConfig) { c.MaxInFlightTransactions = 1; c.Workers = 4 })
	if cap(e.txnSlots) != 1 {
		t.Fatalf("slots=%d want 1", cap(e.txnSlots))
	}
	logs := []LogRecord{
		{EventID: "limit-1", Timestamp: ftsBase, TraceID: ftsTrace(1), SpanID: ftsSpan(1), Attributes: []*commonpb.KeyValue{exKV("phase", exStr("one"))}},
		{EventID: "limit-2", Timestamp: ftsBase + 1, TraceID: ftsTrace(1), SpanID: ftsSpan(1)},
	}
	input := deltaConversation(deltaTurns, "array")

	// While the only write slot is held, a deadline ends the wait unwritten.
	e.txnSlots <- struct{}{}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err := e.ExportLogs(short, logs)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("log export with held slot: %v", err)
	}
	// Exports block on the held slot; queries take none.
	var stats ExportStats
	var logStats LogExportStats
	done := make(chan error, 2)
	go func() {
		var err error
		logStats, err = e.ExportLogs(ctx, logs)
		done <- err
	}()
	go func() {
		var err error
		stats, err = e.ExportResourceSpans(ctx, input)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("export returned while the only write slot was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 0 {
		t.Fatalf("query with held slot: %d logs, %v", len(got), err)
	}
	for _, name := range []string{messageBlobSubspace, msgMetaSubspace} {
		if got := deltaBlobRows(t, ctx, e.backend.db, e.backend.root.Sub(name)); len(got) != 0 {
			t.Fatalf("%d %s rows written while the slot was held", len(got), name)
		}
	}
	<-e.txnSlots

	// With one slot, concurrent exports over every write path complete.
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	n := deltaTurns
	if stats.Spans != int64(n) || stats.MessageFTS.IndexedBlobs != 2*n+3 || stats.MessageFTS.SpanRefs != n*(n+1)+3*n {
		t.Fatalf("export stats %+v", stats)
	}
	if logStats.Logs != 2 {
		t.Fatalf("log export %+v", logStats)
	}
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 2 {
		t.Fatalf("logs after export: %d, %v", len(got), err)
	}
	hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase})
	if len(hits) == 0 {
		t.Fatal("no full-text hits after limited export")
	}
	if len(e.txnSlots) != 0 {
		t.Fatalf("%d slots leaked", len(e.txnSlots))
	}
}

// attemptRecorder records every Record Layer attempt-loop execution of db.
func attemptRecorder(db *recordlayer.FDBDatabase) func() []error {
	var mu sync.Mutex
	var runs []error
	db.SetAttemptObserver(func(_ recordlayer.AttemptCall, err error) {
		mu.Lock()
		runs = append(runs, err)
		mu.Unlock()
	})
	return func() []error {
		mu.Lock()
		defer mu.Unlock()
		return append([]error(nil), runs...)
	}
}

// A benchmark caller gets Run's error unchanged.
func testTxnBenchErrors(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	for _, want := range []error{fdb.Error{Code: 2101}, errors.New("boom")} {
		_, err := runTxnSlot(ctx, db, "bench", func(*recordlayer.FDBRecordContext) (any, error) { return nil, want })
		var domain *OtelError
		if !errors.Is(err, want) || errors.As(err, &domain) {
			t.Fatalf("benchmark error classified: %v -> %v", want, err)
		}
	}
}

// An Exporter transaction has no retry limit: FDB's OnError loop absorbs a
// retryable code injected more often than the benchmark's limit of 10, in one
// Record Layer attempt and one write slot, and only the last execution commits.
func testExporterTxnAbsorbs(t *testing.T, fixture txnFixture) {
	ctx, db, root := fixture(t)
	runs := attemptRecorder(db)
	for _, tc := range []struct{ code, k int }{{1007, 15}, {1009, 15}, {1020, 4}, {1037, 4}} {
		key := root.Sub("absorb", int64(tc.code)).FDBKey()
		slots := make(chan struct{}, 1)
		before := len(runs())
		calls := 0
		_, err := runTxnSlot(withTxnLimiter(withExporterTxns(ctx, time.Minute), slots), db, "absorb", func(rc *recordlayer.FDBRecordContext) (any, error) {
			if err := configureStoreContext(rc); err != nil {
				return nil, err
			}
			calls++
			if len(slots) != 1 {
				return nil, errors.New("execution ran without its slot")
			}
			rc.Transaction().Set(key, []byte(strconv.Itoa(calls)))
			if calls <= tc.k {
				return nil, fdb.Error{Code: tc.code}
			}
			return nil, nil
		})
		got := runs()[before:]
		if err != nil || calls != tc.k+1 || len(got) != 1 || got[0] != nil || len(slots) != 0 {
			t.Fatalf("code %d: err=%v calls=%d attempts=%v held=%d; want %d calls in one attempt", tc.code, err, calls, got, len(slots), tc.k+1)
		}
		var value []byte
		if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
			var err error
			value, err = rc.Transaction().Get(key).Get()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if string(value) != strconv.Itoa(tc.k+1) {
			t.Fatalf("code %d: committed %q, want only execution %d", tc.code, value, tc.k+1)
		}
	}
}

// The benchmark keeps its retry limit of 10: OnError gives up after 11
// executions and the Record Layer runs a second transaction.
func testBenchTxnRetryLimit(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	runs := attemptRecorder(db)
	calls := 0
	_, err := runTxnSlot(ctx, db, "bench", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		calls++
		if calls <= 15 {
			return nil, fdb.Error{Code: 1009}
		}
		return nil, nil
	})
	got := runs()
	if err != nil || calls != 16 || len(got) != 2 || got[1] != nil {
		t.Fatalf("err=%v calls=%d attempts=%v; want 16 calls in two attempts", err, calls, got)
	}
	if code, ok := fdbErrorCode(got[0]); !ok || code != 1009 {
		t.Fatalf("first attempt ended with %v, want the retry-limited 1009", got[0])
	}
}

// The transaction timeout ends an Exporter transaction whose retryable error
// never clears, as a transient 1031 from one Record Layer attempt.
func testExporterTxnTimeoutBound(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	runs := attemptRecorder(db)
	const timeout = 500 * time.Millisecond
	calls := 0
	started := time.Now()
	_, err := runTxnSlot(withExporterTxns(ctx, timeout), db, "timeout test", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		calls++
		return nil, fdb.Error{Code: 1009}
	})
	elapsed := time.Since(started)
	requireOtelKind(t, err, "transient")
	if code, ok := fdbErrorCode(err); !ok || code != 1031 {
		t.Fatalf("timeout error %v, want 1031", err)
	}
	if elapsed < timeout || elapsed > timeout+5*time.Second {
		t.Fatalf("ended after %v, want the %v transaction timeout", elapsed, timeout)
	}
	if got := runs(); calls <= benchTxnRetryLimit+1 || len(got) != 1 {
		t.Fatalf("calls=%d attempts=%v; want more than %d calls in one attempt", calls, got, benchTxnRetryLimit+1)
	}
}

// The caller's deadline ends an Exporter transaction before a longer
// transaction timeout, as a canceled error.
func testExporterTxnDeadlineBound(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	runs := attemptRecorder(db)
	const deadline = 500 * time.Millisecond
	short, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	calls := 0
	started := time.Now()
	_, err := runTxnSlot(withExporterTxns(short, time.Minute), db, "deadline test", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		calls++
		return nil, fdb.Error{Code: 1009}
	})
	elapsed := time.Since(started)
	requireOtelKind(t, err, "canceled")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error %v lacks context.DeadlineExceeded", err)
	}
	if elapsed > deadline+5*time.Second {
		t.Fatalf("ended after %v, want the %v deadline", elapsed, deadline)
	}
	if got := runs(); calls <= benchTxnRetryLimit+1 || len(got) != 1 {
		t.Fatalf("calls=%d attempts=%v; want more than %d calls in one attempt", calls, got, benchTxnRetryLimit+1)
	}
}

// retryInjector fails the first k executions of every transaction (k < 0:
// every execution) with code after the body succeeded, as a commit would.
type retryInjector struct {
	fdb.Database
	code, k int

	mu           sync.Mutex
	transactions int
	minimum      int // fewest executions of a transaction that succeeded
}

func (r *retryInjector) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return r.TransactCtx(context.Background(), fn)
}

func (r *retryInjector) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	executions := 0
	result, err := r.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		executions++
		result, err := fn(tx)
		if err == nil && (r.k < 0 || executions <= r.k) {
			return nil, fdb.Error{Code: r.code}
		}
		return result, err
	})
	if err == nil {
		r.mu.Lock()
		if r.transactions == 0 || executions < r.minimum {
			r.minimum = executions
		}
		r.transactions++
		r.mu.Unlock()
	}
	return result, err
}

func txnTestLogs() []LogRecord {
	return []LogRecord{
		{EventID: "policy-1", Timestamp: ftsBase, TraceID: ftsTrace(1), SpanID: ftsSpan(1), Attributes: []*commonpb.KeyValue{exKV("phase", exStr("one"))}},
		{EventID: "policy-2", Timestamp: ftsBase + 1, TraceID: ftsTrace(1), SpanID: ftsSpan(1)},
	}
}

// Every Exporter transaction, write and read, absorbs 12 injected
// future_version errors, more than the benchmark's retry limit, and the calls
// succeed with complete results.
func testExporterCallsAbsorb(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openFTSExporter(t, ctx, cluster, nil)
	const k = 12
	inject := &retryInjector{Database: e.backend.raw, code: 1009, k: k}
	e.backend.db = recordlayer.NewFDBDatabaseWithTransactor(inject, e.backend.raw)
	stats, err := e.ExportResourceSpans(ctx, deltaConversation(deltaTurns, "array"))
	if err != nil {
		t.Fatal(err)
	}
	n := deltaTurns
	if stats.Spans != int64(n) || stats.MessageFTS.IndexedBlobs != 2*n+3 || stats.MessageFTS.SpanRefs != n*(n+1)+3*n {
		t.Fatalf("export stats %+v", stats)
	}
	if logStats, err := e.ExportLogs(ctx, txnTestLogs()); err != nil || logStats.Logs != 2 {
		t.Fatalf("log export %+v, %v", logStats, err)
	}
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 2 {
		t.Fatalf("logs after export: %d, %v", len(got), err)
	}
	if hits, _ := ftsDrain(t, ctx, e, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase}); len(hits) == 0 {
		t.Fatal("no full-text hits")
	}
	inject.mu.Lock()
	defer inject.mu.Unlock()
	if inject.transactions == 0 || inject.minimum != k+1 {
		t.Fatalf("%d transactions, fewest executions %d; want every one to absorb %d failures", inject.transactions, inject.minimum, k)
	}
}

// A timed-out or deadline-ended Exporter transaction fails ExportResourceSpans
// and ExportLogs with a retryable kind, not "operation".
func testExporterCallsFail(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	input := deltaConversation(deltaTurns, "array")
	exporter := func(timeout time.Duration) *Exporter {
		e := openTestExporter(t, ctx, cluster, func(c *ExporterConfig) { c.TransactionTimeout = timeout })
		e.backend.db = recordlayer.NewFDBDatabaseWithTransactor(&retryInjector{Database: e.backend.raw, code: 1009, k: -1}, e.backend.raw)
		return e
	}
	calls := func(e *Exporter, ctx context.Context) map[string]error {
		_, spanErr := e.ExportResourceSpans(ctx, input)
		_, logErr := e.ExportLogs(ctx, txnTestLogs())
		return map[string]error{"spans": spanErr, "logs": logErr}
	}

	t.Run("transaction_timeout", func(t *testing.T) {
		e := exporter(minExporterTxnTimeout)
		for name, err := range calls(e, ctx) {
			requireOtelKind(t, err, "transient")
			var transient *TransientError
			if code, ok := fdbErrorCode(err); !ok || code != 1031 || !errors.As(err, &transient) {
				t.Fatalf("%s: %v, want a TransientError carrying 1031", name, err)
			}
		}
	})
	t.Run("context_deadline", func(t *testing.T) {
		e := exporter(time.Minute)
		short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		started := time.Now()
		for name, err := range calls(e, short) {
			requireOtelKind(t, err, "canceled")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: %v lacks context.DeadlineExceeded", name, err)
			}
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("deadline-bounded calls took %v under a 1m transaction timeout", elapsed)
		}
	})
}
