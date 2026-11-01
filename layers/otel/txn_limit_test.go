package otel

import (
	"context"
	"errors"
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

func TestExporterTxnTimeout(t *testing.T) {
	t.Parallel()
	if got := txnTimeout(context.Background()); got != benchTxnTimeout || isExporterTxn(context.Background()) {
		t.Fatalf("default timeout %v exporter=%v, want the 1.5s benchmark bound", got, isExporterTxn(context.Background()))
	}
	ctx := withExporterTxns(withTxnLimiter(t.Context(), make(chan struct{}, 1)))
	if got := txnTimeout(ctx); got != exporterTxnTimeout || !isExporterTxn(ctx) {
		t.Fatalf("exporter timeout %v exporter=%v", got, isExporterTxn(ctx))
	}
	// Queries run under the exporter timeout even when the caller's ctx has none.
	_, err := retryQuery(t.Context(), "q", func(qctx context.Context) (int, error) {
		if txnTimeout(qctx) != exporterTxnTimeout {
			return 0, errors.New("query ran without the exporter timeout")
		}
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRetryQuery(t *testing.T) {
	t.Parallel()
	timeout := fdb.Error{Code: 1031}
	t.Run("succeeds after timeouts", func(t *testing.T) {
		t.Parallel()
		calls := 0
		got, err := retryQuery(t.Context(), "q", func(ctx context.Context) (int, error) {
			calls++
			if calls <= maxQueryRetries {
				return 0, timeout
			}
			return 7, nil
		})
		if err != nil || got != 7 || calls != maxQueryRetries+1 {
			t.Fatalf("got %d, %v after %d calls", got, err, calls)
		}
	})
	t.Run("exhaustion wraps the timeout", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := retryQuery(t.Context(), "q", func(ctx context.Context) (int, error) { calls++; return 0, &timeout })
		if calls != maxQueryRetries+1 {
			t.Fatalf("calls=%d want %d", calls, maxQueryRetries+1)
		}
		requireOtelKind(t, err, "operation")
		var code *fdb.Error
		if !errors.As(err, &code) || code.Code != 1031 || !isSourceTimeout(err) {
			t.Fatalf("exhaustion lost the 1031 cause: %v", err)
		}
	})
	t.Run("transient read errors are retried", func(t *testing.T) {
		t.Parallel()
		for _, code := range []int{1007, 1009, 1037, 1038, 1051, 1078, 1213, 1223} {
			calls := 0
			got, err := retryQuery(t.Context(), "q", func(ctx context.Context) (int, error) {
				calls++
				if calls == 1 {
					return 0, fdb.Error{Code: code}
				}
				return 7, nil
			})
			if err != nil || got != 7 || calls != 2 {
				t.Fatalf("code %d: got %d, %v after %d calls", code, got, err, calls)
			}
		}
	})
	t.Run("other errors are returned once", func(t *testing.T) {
		t.Parallel()
		for _, want := range []error{fdb.Error{Code: 1020}, wrap("budget", "q", timeout), errors.New("boom")} {
			calls := 0
			_, err := retryQuery(t.Context(), "q", func(ctx context.Context) (int, error) { calls++; return 0, want })
			if calls != 1 || err != want {
				t.Fatalf("%v: calls=%d err=%v", want, calls, err)
			}
		}
	})
	t.Run("canceled during backoff", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		_, err := retryQuery(ctx, "q", func(ctx context.Context) (int, error) { calls++; cancel(); return 0, timeout })
		if calls != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		requireOtelKind(t, err, "canceled")
	})
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
	t.Run("fresh_run_on_1031", func(t *testing.T) { t.Parallel(); testTxnFreshRetry(t, fixture) })
	t.Run("slot_free_during_backoff", func(t *testing.T) { t.Parallel(); testTxnBackoffReleasesSlot(t, fixture) })
	t.Run("exporter_attaches_limiter", func(t *testing.T) { t.Parallel(); testExporterTxnLimit(t, cluster) })
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
	if err := runIdempotentTxn(waitCtx, db, "limit test", fn); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("idempotent waiter: err=%v ran=%t", err, ran)
	}

	// A canceled context does not take a free slot.
	<-slots
	_, err = runTxnSlot(waitCtx, db, "limit test", fn)
	requireOtelKind(t, err, "canceled")
	if ran || len(slots) != 0 {
		t.Fatalf("canceled caller ran=%t held=%d", ran, len(slots))
	}
}

func testTxnFreshRetry(t *testing.T, fixture txnFixture) {
	ctx, db, root := fixture(t)
	timeout := fdb.Error{Code: 1031}
	rows := func(t *testing.T, space subspace.Subspace) []fdb.KeyValue {
		t.Helper()
		return deltaBlobRows(t, ctx, db, space)
	}
	for _, k := range []int{1, 3} {
		t.Run("timeouts_"+strconv.Itoa(k), func(t *testing.T) {
			space := root.Sub("fresh", int64(k))
			key, value := space.Sub("doc").FDBKey(), []byte("v")
			var calls, writes int
			slots := make(chan struct{}, 1)
			err := runIdempotentTxn(withTxnLimiter(ctx, slots), db, "retry test", func(rc *recordlayer.FDBRecordContext) (any, error) {
				calls++
				if len(slots) != 1 {
					return nil, errors.New("attempt ran without its slot")
				}
				current, err := rc.Transaction().Get(key).Get()
				if err != nil {
					return nil, err
				}
				// Idempotent set: write only if the expected value is absent.
				if string(current) != string(value) {
					rc.Transaction().Set(key, value)
					writes++
				}
				if calls == 1 {
					// The first timed-out attempt is maybe-committed: land it.
					if err := deltaRun(t, ctx, db, func(rc *recordlayer.FDBRecordContext) error {
						rc.Transaction().Set(key, value)
						return nil
					}); err != nil {
						return nil, err
					}
				}
				if calls <= k {
					return nil, timeout
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Only the first attempt saw the key absent; fresh Runs reread it.
			if calls != k+1 || writes != 1 || len(slots) != 0 {
				t.Fatalf("calls=%d writes=%d held=%d, want %d calls and 1 write", calls, writes, len(slots), k+1)
			}
			if got := rows(t, space); len(got) != 1 || string(got[0].Value) != "v" {
				t.Fatalf("rows %v", got)
			}
		})
	}

	t.Run("other_errors_not_retried", func(t *testing.T) {
		for _, want := range []error{wrap("budget", "retry test", timeout), fail("corruption", "retry test", "bad row"), errors.New("boom")} {
			calls := 0
			err := runIdempotentTxn(ctx, db, "retry test", func(*recordlayer.FDBRecordContext) (any, error) { calls++; return nil, want })
			if calls != 1 || !errors.Is(err, want) {
				t.Fatalf("%v: calls=%d err=%v", want, calls, err)
			}
			var got, expected *OtelError
			if errors.As(want, &expected) && (!errors.As(err, &got) || got.Kind != expected.Kind) {
				t.Fatalf("error kind changed: %v -> %v", want, err)
			}
		}
	})

	t.Run("exhaustion", func(t *testing.T) {
		calls := 0
		err := runIdempotentTxn(ctx, db, "retry test", func(*recordlayer.FDBRecordContext) (any, error) { calls++; return nil, timeout })
		if calls != maxSourceRuns {
			t.Fatalf("calls=%d want %d", calls, maxSourceRuns)
		}
		requireOtelKind(t, err, "operation")
		var code fdb.Error
		if !errors.As(err, &code) || code.Code != 1031 || !isSourceTimeout(err) {
			t.Fatalf("exhaustion lost the 1031 cause: %v", err)
		}
	})
}

func testTxnBackoffReleasesSlot(t *testing.T, fixture txnFixture) {
	ctx, db, _ := fixture(t)
	slots := make(chan struct{}, 1)
	lctx := withTxnLimiter(ctx, slots)
	var calls atomic.Int64
	seen := make(chan int64, 1)
	other := make(chan error, 1)
	err := runIdempotentTxn(lctx, db, "backoff test", func(*recordlayer.FDBRecordContext) (any, error) {
		if calls.Add(1) > 1 {
			return nil, nil
		}
		// A second caller queues on the held slot before this attempt times out.
		go func() {
			_, err := runTxnSlot(lctx, db, "backoff test", func(*recordlayer.FDBRecordContext) (any, error) {
				seen <- calls.Load()
				return nil, nil
			})
			other <- err
		}()
		time.Sleep(200 * time.Millisecond)
		return nil, fdb.Error{Code: 1031}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	// The queued caller ran between the attempts, not after the retry finished.
	if got := <-seen; got != 1 || calls.Load() != 2 {
		t.Fatalf("queued caller saw %d attempts, total %d; want it between attempts 1 and 2", got, calls.Load())
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
