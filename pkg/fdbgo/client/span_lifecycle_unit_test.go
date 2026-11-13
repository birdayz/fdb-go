package client

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/pkg/fdbgo/wire"
)

type traceObserver struct {
	onStart func()
	onEnd   func()
}

func (p traceObserver) OnStart(context.Context, sdktrace.ReadWriteSpan) {
	if p.onStart != nil {
		p.onStart()
	}
}

func (p traceObserver) OnEnd(sdktrace.ReadOnlySpan) {
	if p.onEnd != nil {
		p.onEnd()
	}
}
func (traceObserver) Shutdown(context.Context) error   { return nil }
func (traceObserver) ForceFlush(context.Context) error { return nil }

func traceUnitDB(t *testing.T, processors ...sdktrace.SpanProcessor) *Database {
	t.Helper()
	opts := make([]sdktrace.TracerProviderOption, len(processors))
	for i, p := range processors {
		opts[i] = sdktrace.WithSpanProcessor(p)
	}
	tp := sdktrace.NewTracerProvider(opts...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Errorf("shutdown tracer: %v", err)
		}
	})
	return &Database{db: &database{tracer: tp.Tracer("test"), tracingSampleRate: 1}}
}

func assertTraceBalance(t *testing.T, sr *tracetest.SpanRecorder, want int) {
	t.Helper()
	started, ended := sr.Started(), sr.Ended()
	if len(started) != want || len(ended) != want {
		t.Fatalf("spans started=%d ended=%d, want %d of each", len(started), len(ended), want)
	}
	counts := make(map[string]int)
	for _, s := range ended {
		counts[s.SpanContext().SpanID().String()]++
	}
	for _, s := range started {
		if n := counts[s.SpanContext().SpanID().String()]; n != 1 {
			t.Errorf("span %v ended %d times, want once", s.SpanContext(), n)
		}
	}
}

func TestTracerLifecycleCallbacksOutsideLocks(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"reset", "retry", "cancel", "postCommitReset", "publishCommit"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			var tx *Transaction
			sr := tracetest.NewSpanRecorder()
			observe := func() {
				if !tx.traceMu.TryLock() {
					t.Error("tracer called under traceMu")
					return
				}
				tx.traceMu.Unlock()
				if state := tx.readVersionState; state != nil {
					if !state.readVersionMu.TryLock() {
						t.Error("tracer called under readVersionMu")
						return
					}
					state.readVersionMu.Unlock()
				}
				if !tx.resetMu.TryLock() {
					t.Error("tracer called during turnover")
					return
				}
				tx.resetMu.Unlock()
				// Probe publication after the locks; no concurrent turnover runs here.
				tx.ReadVersionInstant()
			}
			db := traceUnitDB(t, sr, traceObserver{onStart: observe, onEnd: observe})
			tx = db.CreateTransaction()
			tx.readVersion, tx.hasReadVersion = 123, true
			if _, _, err := tx.cachedOrFetchReadVersion(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "reset":
				tx.Reset()
			case "retry":
				lease := tx.enterState()
				inc := lease.inc
				lease.release()
				if err := tx.resetExpected(false, inc, lease, nil); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				tx.Cancel()
			case "postCommitReset":
				tx.postCommitReset()
			case "publishCommit":
				lease := tx.enterState()
				inc := lease.inc
				lease.release()
				tx.publishCommit(inc, lease, commitOutcome{version: 456})
			}
			assertTraceBalance(t, sr, 1)
		})
	}
}

func TestTracerRetryKeepsMetricsButRestartsSpan(t *testing.T) {
	t.Parallel()
	for _, nativeCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "writeOnlyCommit"}[nativeCommit], func(t *testing.T) {
			t.Parallel()
			sr := tracetest.NewSpanRecorder()
			tx := traceUnitDB(t, sr).CreateTransaction()
			defer tx.Cancel()
			start := time.Now().Add(-time.Second)
			tx.metricStart = start
			for attempt := 0; attempt < 2; attempt++ {
				if nativeCommit {
					tx.captureNativeReadState(&commitInput{span: tx.currentSpan()})
				} else {
					tx.readVersion, tx.hasReadVersion = 123, true
					if _, _, err := tx.cachedOrFetchReadVersion(context.Background(), 0); err != nil {
						t.Fatal(err)
					}
				}
				if tx.metricStart != start {
					t.Fatal("retry changed whole-transaction latency anchor")
				}
				tx.reset(false)
			}
			assertTraceBalance(t, sr, 2)
			spans := sr.Ended()
			if spans[0].SpanContext().TraceID() == spans[1].SpanContext().TraceID() {
				t.Error("retry reused the unparented wire trace ID")
			}
		})
	}
}

func TestTracerManagedCommitLazyReuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		parent int
		spans  int
	}{
		{"unparented", -1, 2},
		{"sampledParent", 1, 2},
		{"unsampledParent", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sr := tracetest.NewSpanRecorder()
			db := traceUnitDB(t, sr)
			db.db.timeouts = newTimeoutQueue()
			var tx *Transaction
			var before [16]byte
			_, err := db.Transact(ctx, func(attempt *Transaction) (any, error) {
				tx = attempt
				tx.SetTimeout(60000)
				tx.SetReadVersion(123)
				tx.ensureTxSpan(ctx)
				before = tx.currentSpan().TraceID
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			assertTraceBalance(t, sr, 1)
			if !tx.spanNeedsRegen.Load() || tx.spanContext.Load() != nil {
				t.Fatal("managed commit did not leave lazy span regeneration")
			}
			tx.readErrMu.Lock()
			released := !tx.transactScoped && tx.designEntry == nil && !tx.timeoutArmed
			tx.readErrMu.Unlock()
			if !released {
				t.Fatal("managed helper did not release its timeout while ending its span")
			}

			parentTrace := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
			// Parent injection must reopen the lazy successor before publishing its context.
			done := make(chan error, 1)
			go func() {
				if tc.parent >= 0 {
					var parent [spanParentSize]byte
					binary.LittleEndian.PutUint64(parent[:8], 0x0fdb00b073000000)
					copy(parent[8:24], parentTrace[:])
					binary.LittleEndian.PutUint64(parent[24:32], 0x1234)
					parent[32] = byte(tc.parent)
					if err := tx.SetSpanParent(parent[:]); err != nil {
						done <- err
						return
					}
				}
				tx.ensureTxSpan(ctx)
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("lazy span regeneration deadlocked")
			}
			defer tx.Cancel()
			after := tx.currentSpan()
			if after.TraceID == before || !spanContextValid(after) || tx.spanNeedsRegen.Load() {
				t.Fatal("escaped handle did not acquire a fresh wire span")
			}
			if tc.parent >= 0 && (after.TraceID != parentTrace || after.Flags != uint8(tc.parent)) {
				t.Fatalf("successor context=%v, want injected trace %x flags %d", after, parentTrace, tc.parent)
			}
			tx.Cancel()
			assertTraceBalance(t, sr, tc.spans)
			if tc.parent == 1 && sr.Ended()[1].SpanContext().TraceID() != oteltrace.TraceID(parentTrace) {
				t.Fatal("successor span did not use the injected trace")
			}
		})
	}
}

type traceWaitContext struct {
	context.Context
	waiting chan<- struct{}
}

func (ctx traceWaitContext) Done() <-chan struct{} {
	select {
	case ctx.waiting <- struct{}{}:
	default:
	}
	return ctx.Context.Done()
}

func TestTracerConcurrentStartAndRetirement(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"concurrent", "cancelWaiter", "cancel", "reset", "helperReturn"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			entered, resume := make(chan struct{}), make(chan struct{})
			var first, resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			sr := tracetest.NewSpanRecorder()
			db := traceUnitDB(t, sr, traceObserver{onStart: func() {
				first.Do(func() {
					close(entered)
					<-resume
				})
			}})
			tx := db.CreateTransaction()
			defer tx.Cancel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			defer func() {
				release()
				wg.Wait()
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				tx.ensureTxSpan(ctx)
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("tracer start did not arrive")
			}
			switch action {
			case "concurrent":
				waiting := make(chan struct{}, 8)
				waitCtx := traceWaitContext{Context: ctx, waiting: waiting}
				for range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						tx.ensureTxSpan(waitCtx)
					}()
				}
				for range 8 {
					select {
					case <-waiting:
					case <-ctx.Done():
						t.Fatal("concurrent start did not wait for the in-flight start")
					}
				}
			case "cancelWaiter":
				waiterCtx, cancelWaiter := context.WithCancel(ctx)
				defer cancelWaiter()
				waiting, done := make(chan struct{}, 1), make(chan struct{})
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer close(done)
					tx.ensureTxSpan(traceWaitContext{Context: waiterCtx, waiting: waiting})
				}()
				select {
				case <-waiting:
				case <-ctx.Done():
					t.Fatal("contender did not wait for Start")
				}
				cancelWaiter()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("cancelled contender did not return while Start was blocked")
				}
			case "cancel":
				tx.Cancel()
			case "reset":
				tx.Reset()
			case "helperReturn":
				tx.endTxSpan()
			}
			release()
			wg.Wait()
			if action == "concurrent" || action == "cancelWaiter" {
				tx.endTxSpan()
			}
			assertTraceBalance(t, sr, 1)
			if tx.txSpan != nil || tx.traceCtx != nil {
				t.Fatal("retired start installed a live transaction span")
			}
			if action == "reset" {
				tx.ensureTxSpan(ctx)
				tx.endTxSpan()
				assertTraceBalance(t, sr, 2)
			} else {
				tx.ensureTxSpan(ctx)
				assertTraceBalance(t, sr, 1)
			}
		})
	}
}

type panickingSpanTracer struct {
	oteltrace.Tracer
	entered, resume chan struct{}
	once            sync.Once
}

func (tr *panickingSpanTracer) Start(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span) {
	tr.once.Do(func() {
		close(tr.entered)
		<-tr.resume
		panic("tracer failed")
	})
	return tr.Tracer.Start(ctx, name, opts...)
}

func TestTracerStartPanicReleasesWaiters(t *testing.T) {
	t.Parallel()
	sr := tracetest.NewSpanRecorder()
	db := traceUnitDB(t, sr)
	tr := &panickingSpanTracer{Tracer: db.db.tracer, entered: make(chan struct{}), resume: make(chan struct{})}
	db.db.tracer = tr
	tx := db.CreateTransaction()
	defer tx.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(tr.resume) }) }
	defer release()
	caught := make(chan any, 1)
	go func() {
		defer func() { caught <- recover() }()
		tx.ensureTxSpan(ctx)
	}()
	select {
	case <-tr.entered:
	case <-ctx.Done():
		t.Fatal("tracer did not enter Start")
	}
	waiting := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tx.ensureTxSpan(traceWaitContext{Context: ctx, waiting: waiting})
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("contender did not wait for Start")
	}
	release()
	select {
	case value := <-caught:
		if value != "tracer failed" {
			t.Fatalf("tracer panic changed: %v", value)
		}
	case <-ctx.Done():
		t.Fatal("panicking Start did not return")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("panicking Start stranded its waiter")
	}
	tx.endTxSpan()
	assertTraceBalance(t, sr, 1)
}

func TestTracerDisabledDoesNotAllocate(t *testing.T) {
	// AllocsPerRun temporarily changes GOMAXPROCS; keep this test exclusive.
	db := &Database{db: &database{tracingSampleRate: 1}}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	if !isSampled(tx.currentSpan()) || !spanContextValid(tx.currentSpan()) {
		t.Fatal("positive control: wire tracing must remain sampled and valid without an exporter")
	}
	if n := testing.AllocsPerRun(100, func() {
		tx.ensureTxSpan(context.Background())
		endSpan(tx.startOpSpan("fdbgo.getValue"))
		tx.endTxSpan()
	}); n != 0 {
		t.Fatalf("disabled export allocated %g times per lifecycle probe, want 0", n)
	}
	if tx.txSpan != nil || tx.traceCtx != nil || tx.traceStarting != nil {
		t.Fatal("disabled export constructed trace state")
	}
}

func TestSpanParentInvalidLengthIsFDBError(t *testing.T) {
	t.Parallel()
	tx := (&Database{db: &database{}}).CreateTransaction()
	defer tx.Cancel()
	before := tx.currentSpan()
	for _, n := range []int{0, 25, 32, 34} {
		err := tx.SetSpanParent(make([]byte, n))
		if e, ok := err.(*wire.FDBError); !ok || e.Code != 2006 {
			t.Errorf("SetSpanParent(%d bytes) = %v, want invalid_option_value (2006)", n, err)
		}
		if tx.currentSpan() != before {
			t.Error("invalid SPAN_PARENT changed the wire context")
		}
	}
}
