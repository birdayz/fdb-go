package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func openTracedDB(t *testing.T, ctx context.Context, rate float64, sampler sdktrace.Sampler) (*Database, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithSampler(sampler))
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown tracer: %v", err)
		}
	})
	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile,
		WithTracer(tp.Tracer("test")), WithTracingSampleRate(rate), WithAPIVersion(730))
	if err != nil {
		t.Fatalf("open with tracer: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return db, sr
}

func checkEndedSpans(t *testing.T, sr *tracetest.SpanRecorder, names ...string) []sdktrace.ReadOnlySpan {
	t.Helper()
	started, ended := sr.Started(), sr.Ended()
	if len(started) != len(names) || len(ended) != len(names) {
		t.Fatalf("started=%d ended=%v, want %v", len(started), spanNames(ended), names)
	}
	want := make(map[string]int)
	for _, name := range names {
		want[name]++
	}
	seen := make(map[oteltrace.SpanID]bool)
	for _, s := range ended {
		want[s.Name()]--
		if seen[s.SpanContext().SpanID()] {
			t.Errorf("span %v ended more than once", s.SpanContext())
		}
		seen[s.SpanContext().SpanID()] = true
		if s.SpanKind() != oteltrace.SpanKindClient {
			t.Errorf("%s kind=%v, want client", s.Name(), s.SpanKind())
		}
		if !s.SpanContext().IsValid() || !s.SpanContext().IsSampled() {
			t.Errorf("invalid or unsampled exported span: %v", s.SpanContext())
		}
	}
	for name, n := range want {
		if n != 0 {
			t.Errorf("span %q count differs by %d", name, n)
		}
	}
	for _, s := range started {
		if !seen[s.SpanContext().SpanID()] {
			t.Errorf("span %v was started but not ended", s.SpanContext())
		}
	}
	return ended
}

func TestTracerExportsSpans(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	key := []byte(t.Name() + "/point")
	selector := []byte(t.Name() + "/selector")
	rangeKey := []byte(t.Name() + "/range")
	seed := openTestDB(t, ctx)
	defer seed.Close()
	if _, err := seed.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, k := range [][]byte{key, selector, rangeKey} {
			tx.Set(k, []byte("v"))
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	db, sr := openTracedDB(t, ctx, 1, sdktrace.ParentBased(sdktrace.AlwaysSample()))
	var wireSpan types.SpanContext
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// RYW getKey is a range walk; disable it to exercise each native span site.
		tx.SetReadYourWritesDisable()
		wireSpan = tx.currentSpan()
		v, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if string(v) != "v" {
			t.Errorf("point read=%q, want v", v)
		}
		k, err := tx.GetKey(ctx, selector, false, 1)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(k, selector) {
			t.Errorf("selector read=%q, want %q", k, selector)
		}
		rows, more, err := tx.GetRange(ctx, rangeKey, append(bytes.Clone(rangeKey), 0xff), 10)
		if err != nil {
			return nil, err
		}
		if more || len(rows) != 1 || !bytes.Equal(rows[0].Key, rangeKey) || string(rows[0].Value) != "v" {
			t.Errorf("range read=%v more=%v, want one seeded key", rows, more)
		}
		tx.Set(key, []byte("v2"))
		return nil, nil
	}); err != nil {
		t.Fatalf("traced transact: %v", err)
	}
	spans := checkEndedSpans(t, sr, "Transaction", "fdbgo.getValue", "fdbgo.getKey", "fdbgo.getRange")
	var root sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "Transaction" {
			root = s
		}
	}
	if root == nil {
		t.Fatal("missing transaction span")
	}
	if root.SpanContext().TraceID() != oteltrace.TraceID(wireSpan.TraceID) {
		t.Fatal("exported trace ID differs from the client wire trace ID")
	}
	if !root.Parent().Equal(otelSpanContext(wireSpan)) {
		t.Errorf("transaction parent=%v, want wire seed %v", root.Parent(), wireSpan)
	}
	for _, s := range spans {
		if s.Name() == "Transaction" {
			continue
		}
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() || s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s is not a child of the transaction", s.Name())
		}
	}
}

func TestTracerManagedHelperLifetime(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"read", "write"} {
		for _, outcome := range []string{"success", "bodyError", "cancel", "panic", "commitError"} {
			if mode == "read" && outcome == "commitError" {
				continue
			}
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				db, sr := openTracedDB(t, ctx, 1, sdktrace.AlwaysSample())
				sentinel := errors.New("application error: must remain unchanged")
				body := func(tx *Transaction) (any, error) {
					if _, err := tx.GetReadVersion(ctx); err != nil {
						return nil, err
					}
					switch outcome {
					case "bodyError":
						return nil, sentinel
					case "cancel":
						cancel()
						if mode == "read" {
							return nil, ctx.Err()
						}
					case "panic":
						panic(sentinel)
					case "commitError":
						tx.Set([]byte{0xff, 0x01}, []byte("illegal system write"))
					}
					if mode == "write" {
						tx.Set([]byte(t.Name()), []byte("v"))
					}
					return "result", nil
				}
				var result any
				var err error
				var caught any
				func() {
					defer func() { caught = recover() }()
					if mode == "read" {
						result, err = db.ReadTransact(ctx, body)
					} else {
						result, err = db.Transact(ctx, body)
					}
				}()
				switch outcome {
				case "success":
					if err != nil || result != "result" {
						t.Fatalf("result=%v error=%v", result, err)
					}
				case "bodyError":
					if err != sentinel {
						t.Fatalf("body error changed: %v", err)
					}
				case "cancel":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation changed: %v", err)
					}
				case "panic":
					if caught != sentinel {
						t.Fatalf("panic changed: %v", caught)
					}
				case "commitError":
					var fdbErr *wire.FDBError
					if !errors.As(err, &fdbErr) || fdbErr.Code != 2004 {
						t.Fatalf("commit error=%v, want key_outside_legal_range", err)
					}
				}
				if outcome != "panic" && caught != nil {
					t.Fatalf("unexpected panic: %v", caught)
				}
				checkEndedSpans(t, sr, "Transaction")
			})
		}
	}
}

func TestTracerManagedRetries(t *testing.T) {
	t.Parallel()
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "read"}[readOnly], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			db, sr := openTracedDB(t, ctx, 1, sdktrace.AlwaysSample())
			var wireSpans []types.SpanContext
			body := func(tx *Transaction) (any, error) {
				wireSpans = append(wireSpans, tx.currentSpan())
				if _, err := tx.GetReadVersion(ctx); err != nil {
					return nil, err
				}
				if len(wireSpans) < 3 {
					return nil, &wire.FDBError{Code: 1020}
				}
				if !readOnly {
					tx.Set([]byte(t.Name()), []byte("v"))
				}
				return nil, nil
			}
			var err error
			if readOnly {
				_, err = db.ReadTransact(ctx, body)
			} else {
				_, err = db.Transact(ctx, body)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(wireSpans) != 3 {
				t.Fatalf("attempts=%d, want 3", len(wireSpans))
			}
			spans := checkEndedSpans(t, sr, "Transaction", "Transaction", "Transaction")
			for i, s := range spans {
				if s.SpanContext().TraceID() != oteltrace.TraceID(wireSpans[i].TraceID) {
					t.Errorf("attempt %d lost its wire trace ID", i)
				}
				if i > 0 && wireSpans[i].TraceID == wireSpans[i-1].TraceID {
					t.Errorf("attempt %d reused prior wire trace ID", i)
				}
			}
		})
	}
}

func TestTracerWriteOnlyCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, sr := openTracedDB(t, ctx, 1, sdktrace.AlwaysSample())
	var wireSpan types.SpanContext
	key := []byte(t.Name())
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		wireSpan = tx.currentSpan()
		tx.Set(key, []byte("committed"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	spans := checkEndedSpans(t, sr, "Transaction")
	if spans[0].SpanContext().TraceID() != oteltrace.TraceID(wireSpan.TraceID) {
		t.Error("write-only commit lost its wire trace ID")
	}
	reader := openTestDB(t, ctx)
	defer reader.Close()
	if _, err := reader.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		v, err := tx.Get(ctx, key)
		if err == nil && string(v) != "committed" {
			t.Errorf("persisted value=%q, want committed", v)
		}
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
}

type observingSpanTracer struct {
	oteltrace.Tracer
	observe func()
}

func (tr observingSpanTracer) Start(ctx context.Context, name string, opts ...oteltrace.SpanStartOption) (context.Context, oteltrace.Span) {
	tr.observe()
	return tr.Tracer.Start(ctx, name, opts...)
}

func TestTracerWriteOnlyRetryAndReuse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, sr := openTracedDB(t, ctx, 1, sdktrace.AlwaysSample())
	var version int64
	if _, err := db.ReadTransact(ctx, func(reader *Transaction) (any, error) {
		var err error
		version, err = reader.GetReadVersion(ctx)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	checkEndedSpans(t, sr, "Transaction")
	sr.Reset() // the warmup establishes this database's explicit-version floor
	tx := db.CreateTransaction()
	defer tx.Cancel()
	starts := 0
	db.db.tracer = observingSpanTracer{Tracer: db.db.tracer, observe: func() {
		starts++
		if !tx.traceMu.TryLock() {
			t.Error("native commit started a span under traceMu")
		} else {
			tx.traceMu.Unlock()
		}
		if !tx.readVersionMu.TryLock() {
			t.Error("native commit started a span under readVersionMu")
		} else {
			tx.readVersionMu.Unlock()
		}
	}}
	other := openTestDB(t, ctx)
	defer other.Close()
	key := []byte(t.Name())
	// Fix the first attempt's version without starting a traced read, then
	// create a real resolver conflict after that version and before Commit.
	tx.SetReadVersion(version)
	tx.AddReadConflictKey(key)
	tx.Set(key, []byte("conflicted"))
	if _, err := other.Transact(ctx, func(writer *Transaction) (any, error) {
		writer.Set(key, []byte("other"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	wireSpans := []types.SpanContext{tx.currentSpan()}
	err := tx.Commit(ctx)
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
		t.Fatalf("first commit=%v, want not_committed", err)
	}
	anchor := tx.metricStart
	if anchor.IsZero() {
		t.Fatal("failed native commit did not start latency accounting")
	}
	if err := tx.OnError(ctx, err); err != nil {
		t.Fatal(err)
	}
	if tx.metricStart != anchor {
		t.Fatal("retry changed the total-latency anchor")
	}
	for _, value := range []string{"retried", "reused"} {
		wireSpans = append(wireSpans, tx.currentSpan())
		tx.Set(key, []byte(value))
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("%s commit: %v", value, err)
		}
	}
	spans := checkEndedSpans(t, sr, "Transaction", "Transaction", "Transaction")
	if starts != 3 {
		t.Errorf("native-start lock probes=%d, want 3", starts)
	}
	for i, s := range spans {
		if s.SpanContext().TraceID() != oteltrace.TraceID(wireSpans[i].TraceID) {
			t.Errorf("native attempt %d lost its wire trace ID", i)
		}
		if i > 0 && wireSpans[i].TraceID == wireSpans[i-1].TraceID {
			t.Errorf("native attempt %d reused its predecessor's trace ID", i)
		}
	}
	if _, err := other.ReadTransact(ctx, func(reader *Transaction) (any, error) {
		value, err := reader.Get(ctx, key)
		if err == nil && string(value) != "reused" {
			t.Errorf("persisted value=%q, want reused", value)
		}
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
}

type blockedSpanSampler struct {
	sdktrace.Sampler
	entered, resume chan struct{}
	once            sync.Once
}

func (s *blockedSpanSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	s.once.Do(func() {
		close(s.entered)
		<-s.resume
	})
	return s.Sampler.ShouldSample(p)
}

func TestTracerCancelDuringPublicReadStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sampler := &blockedSpanSampler{
		Sampler: sdktrace.AlwaysSample(), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(sampler.resume) }) }
	defer release()
	db, sr := openTracedDB(t, ctx, 1, sampler)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tx.GetReadVersion(ctx)
		done <- err
	}()
	select {
	case <-sampler.entered:
	case <-ctx.Done():
		t.Fatal("public read did not reach tracer start")
	}
	tx.Cancel()
	release()
	select {
	case err := <-done:
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != 1025 {
			t.Fatalf("read error=%v, want transaction_cancelled", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled public read did not return")
	}
	checkEndedSpans(t, sr, "Transaction")
	if tx.txSpan != nil || tx.traceCtx != nil {
		t.Error("cancelled public read installed a live transaction span")
	}
}

func TestTracerResetDuringPublicReadStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sampler := &blockedSpanSampler{
		Sampler: sdktrace.AlwaysSample(), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(sampler.resume) }) }
	db, sr := openTracedDB(t, ctx, 1, sampler)
	tx := db.CreateTransaction()
	defer func() {
		release()
		tx.Cancel()
	}()
	before := tx.currentSpan()
	readDone := make(chan error, 1)
	go func() {
		_, err := tx.GetReadVersion(ctx)
		readDone <- err
	}()
	select {
	case <-sampler.entered:
	case <-ctx.Done():
		t.Fatal("public read did not reach tracer start")
	}
	tx.readErrMu.Lock()
	inc := tx.readLife
	tx.readErrMu.Unlock()
	if inc == nil {
		t.Fatal("public read has no admitted incarnation")
	}
	resetDone := make(chan struct{})
	go func() {
		tx.Reset()
		close(resetDone)
	}()
	// Turnover cancels this incarnation before waiting for the blocked read's
	// execution lease. Observing cancellation proves retirement actually began.
	select {
	case <-inc.ctx.Done():
	case <-ctx.Done():
		t.Fatal("Reset did not retire the admitted read")
	}
	select {
	case <-resetDone:
		t.Fatal("Reset published before draining the blocked read")
	default:
	}
	release()
	select {
	case err := <-readDone:
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != 1025 {
			t.Fatalf("retired read=%v, want transaction_cancelled", err)
		}
	case <-ctx.Done():
		t.Fatal("retired read did not return")
	}
	select {
	case <-resetDone:
	case <-ctx.Done():
		t.Fatal("Reset did not finish after the read drained")
	}
	checkEndedSpans(t, sr, "Transaction")
	if tx.txSpan != nil || tx.traceCtx != nil || tx.currentSpan().TraceID == before.TraceID {
		t.Fatal("retired span contaminated the reset successor")
	}
	after := tx.currentSpan()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatalf("successor read: %v", err)
	}
	tx.Cancel()
	spans := checkEndedSpans(t, sr, "Transaction", "Transaction")
	if spans[0].SpanContext().TraceID() != oteltrace.TraceID(before.TraceID) ||
		spans[1].SpanContext().TraceID() != oteltrace.TraceID(after.TraceID) {
		t.Error("public Reset lost old or successor trace identity")
	}
}

func TestTracerManagedReturnDuringPublicReadStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sampler := &blockedSpanSampler{
		Sampler: sdktrace.AlwaysSample(), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(sampler.resume) }) }
	db, sr := openTracedDB(t, ctx, 1, sampler)
	var started *Transaction
	defer func() {
		release()
		if started != nil {
			started.Cancel()
		}
	}()
	type readResult struct {
		version int64
		err     error
	}
	readDone := make(chan readResult, 1)
	result, err := db.ReadTransact(ctx, func(tx *Transaction) (any, error) {
		started = tx
		go func() {
			version, err := tx.GetReadVersion(ctx)
			readDone <- readResult{version, err}
		}()
		select {
		case <-sampler.entered:
			return "returned", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err != nil || result != "returned" {
		t.Fatalf("managed return=%v, %v", result, err)
	}
	checkEndedSpans(t, sr) // the SDK is still blocked before constructing its span
	release()
	select {
	case read := <-readDone:
		if read.err != nil || read.version <= 0 {
			t.Fatalf("span cleanup changed the outstanding read: %+v", read)
		}
	case <-ctx.Done():
		t.Fatal("public read did not finish after managed return")
	}
	checkEndedSpans(t, sr, "Transaction")
	if started.txSpan != nil || started.traceCtx != nil {
		t.Error("late public start republished a span after managed return")
	}
}

func TestTracerPublicWaiterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sampler := &blockedSpanSampler{
		Sampler: sdktrace.AlwaysSample(), entered: make(chan struct{}), resume: make(chan struct{}),
	}
	var resumeOnce, waiterOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(sampler.resume) }) }
	allowWaiter := make(chan struct{})
	releaseWaiter := func() { waiterOnce.Do(func() { close(allowWaiter) }) }
	db, sr := openTracedDB(t, ctx, 1, sampler)
	tx := db.CreateTransaction()
	defer func() {
		releaseWaiter()
		release()
		tx.Cancel()
	}()
	producerDone := make(chan struct{})
	var producerErr error
	go func() {
		defer close(producerDone)
		_, producerErr = tx.GetReadVersion(ctx)
	}()
	select {
	case <-sampler.entered:
	case <-ctx.Done():
		t.Fatal("producer did not reach Start")
	}
	waiterEntered := make(chan struct{})
	// The producer has passed this hook. Park the contender after public
	// admission checks, so cancellation cannot bypass the context handoff.
	tx.beforeReadVersionLock = func() {
		close(waiterEntered)
		<-allowWaiter
	}
	waiterCtx, cancelWaiter := context.WithCancel(ctx)
	defer cancelWaiter()
	waiterDone := make(chan struct{})
	var waiterErr error
	go func() {
		defer close(waiterDone)
		_, waiterErr = tx.GetReadVersion(waiterCtx)
	}()
	select {
	case <-waiterEntered:
	case <-ctx.Done():
		t.Fatal("contender did not pass public admission")
	}
	cancelWaiter()
	releaseWaiter()
	select {
	case <-waiterDone:
		if !errors.Is(waiterErr, context.Canceled) {
			t.Fatalf("contender cancellation changed: %v", waiterErr)
		}
	case <-ctx.Done():
		t.Fatal("cancelled public contender waited for the producer's tracer")
	}
	checkEndedSpans(t, sr)
	release()
	select {
	case <-producerDone:
		if producerErr != nil {
			t.Fatalf("contender cancellation poisoned producer: %v", producerErr)
		}
	case <-ctx.Done():
		t.Fatal("producer did not finish")
	}
	tx.Cancel()
	checkEndedSpans(t, sr, "Transaction")
}

func TestTracerPublicStartPanicReleasesAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, sr := openTracedDB(t, ctx, 1, sdktrace.AlwaysSample())
	var first sync.Once
	sentinel := errors.New("application tracer panic")
	db.db.tracer = observingSpanTracer{Tracer: db.db.tracer, observe: func() {
		first.Do(func() { panic(sentinel) })
	}}
	tx := db.CreateTransaction()
	defer tx.Cancel()
	var caught any
	func() {
		defer func() { caught = recover() }()
		_, _ = tx.GetReadVersion(ctx)
	}()
	if caught != sentinel {
		t.Fatalf("tracer panic changed: %v", caught)
	}
	tx.readErrMu.Lock()
	inc := tx.readLife
	users := 0
	if inc != nil {
		users = inc.users
	}
	tx.readErrMu.Unlock()
	if inc == nil || users != 0 || tx.traceStarting != nil {
		t.Fatalf("panicking public Start retained admission: incarnation=%v users=%d pending=%v", inc != nil, users, tx.traceStarting != nil)
	}
	checkEndedSpans(t, sr)
	tx.Reset()
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatalf("read after tracer panic and Reset: %v", err)
	}
	tx.Cancel()
	checkEndedSpans(t, sr, "Transaction")
}

func TestTracerSampling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rate    float64
		parent  int // -1 = no injection, 0/1 = injected sampled flag
		sampler sdktrace.Sampler
		want    int
	}{
		{"wireUnsampled", 0, -1, sdktrace.AlwaysSample(), 0},
		{"providerDrops", 1, -1, sdktrace.NeverSample(), 0},
		{"sampledParent", 0, 1, sdktrace.ParentBased(sdktrace.NeverSample()), 1},
		{"unsampledParent", 1, 0, sdktrace.AlwaysSample(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			db, sr := openTracedDB(t, ctx, tc.rate, tc.sampler)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			if tc.parent >= 0 {
				var parent [spanParentSize]byte
				binary.LittleEndian.PutUint64(parent[:8], 0x0fdb00b073000000)
				binary.LittleEndian.PutUint64(parent[8:16], 0x1122334455667788)
				binary.LittleEndian.PutUint64(parent[16:24], 0x8877665544332211)
				binary.LittleEndian.PutUint64(parent[24:32], 0x1234)
				parent[32] = byte(tc.parent)
				if err := tx.SetSpanParent(parent[:]); err != nil {
					t.Fatal(err)
				}
			}
			wireSpan := tx.currentSpan()
			if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatal(err)
			}
			attempts := 1
			if tc.parent >= 0 {
				if err := tx.OnError(ctx, &wire.FDBError{Code: 1020}); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.GetReadVersion(ctx); err != nil {
					t.Fatal(err)
				}
				tx.Reset()
				if _, err := tx.GetReadVersion(ctx); err != nil {
					t.Fatal(err)
				}
				for _, value := range []string{"committed", "reused"} {
					tx.Set([]byte(t.Name()), []byte(value))
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
				attempts = 4 // original, retry, user Reset, and successful reuse
			}
			tx.Cancel()
			names := make([]string, tc.want*attempts)
			for i := range names {
				names[i] = "Transaction"
			}
			spans := checkEndedSpans(t, sr, names...)
			for _, span := range spans {
				if span.SpanContext().TraceID() != oteltrace.TraceID(wireSpan.TraceID) {
					t.Error("injected parent trace ID was lost across retirement")
				}
			}
		})
	}
}

func TestTracerDefaultNoopEmitsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithTracingSampleRate(1), WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.db.tracer != nil {
		t.Fatal("default installed an export backend")
	}
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		if _, err := tx.Get(ctx, []byte(t.Name())); err != nil {
			return nil, err
		}
		if !isSampled(tx.currentSpan()) || !spanContextValid(tx.currentSpan()) {
			t.Fatal("wire propagation changed without an exporter")
		}
		if tx.txSpan != nil || tx.traceCtx != nil || tx.traceStarting != nil {
			t.Fatal("default export constructed OTel state")
		}
		tx.Set([]byte(t.Name()), []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}
