package fdb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/internal/fdbwire"
)

type stubTracer struct{ oteltrace.Tracer }

func TestOpenConfigValidationAndDefaults(t *testing.T) {
	t.Parallel()

	t.Run("defaults_and_options", func(t *testing.T) {
		t.Parallel()
		var cur atomic.Int32
		var gotCtx context.Context
		var gotCluster string
		var gotOpts []Option
		db, err := openWithConfig(
			context.Background(),
			Config{
				ClusterFile:           "/tmp/fdb.cluster",
				TLSConfig:             &tls.Config{MinVersion: tls.VersionTLS12},
				Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
				Tracer:                stubTracer{},
				DialFunc:              func(context.Context, string, string) (net.Conn, error) { return nil, nil },
				TransactionTimeout:    1500 * time.Millisecond,
				TransactionRetryLimit: 5,
				Options:               []Option{WithAdaptiveLinger(time.Millisecond)},
			},
			&cur,
			func(ctx context.Context, cluster string, opts ...Option) (Database, error) {
				gotCtx = ctx
				gotCluster = cluster
				gotOpts = opts
				return Database{d: &internalDB{ctx: context.Background()}}, nil
			},
		)
		if err != nil {
			t.Fatalf("openWithConfig: %v", err)
		}
		if cur.Load() != 730 {
			t.Fatalf("APIVersion = %d, want 730", cur.Load())
		}
		if gotCtx == nil || gotCluster != "/tmp/fdb.cluster" {
			t.Fatalf("got ctx=%v cluster=%q", gotCtx, gotCluster)
		}
		if len(gotOpts) != 5 {
			t.Fatalf("got %d options, want 5", len(gotOpts))
		}
		defs := db.d.defaults()
		if defs.timeout != 1500 || !defs.hasRetryLimit || defs.retryLimit != 5 {
			t.Fatalf("defaults = %+v, want timeout=1500 retryLimit=5", defs)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("reuse_existing_api_version", func(t *testing.T) {
		t.Parallel()
		var cur atomic.Int32
		cur.Store(720)
		db, err := openWithConfig(
			context.Background(),
			Config{APIVersion: 0},
			&cur,
			func(context.Context, string, ...Option) (Database, error) {
				return Database{d: &internalDB{ctx: context.Background()}}, nil
			},
		)
		if err != nil {
			t.Fatalf("openWithConfig with APIVersion 0 and preset 720: %v", err)
		}
		if cur.Load() != 720 {
			t.Fatalf("APIVersion = %d, want 720", cur.Load())
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("api_version_conflict_and_invalid", func(t *testing.T) {
		t.Parallel()
		var cur atomic.Int32
		cur.Store(720)
		called := false
		_, err := openWithConfig(
			context.Background(),
			Config{APIVersion: 730},
			&cur,
			func(context.Context, string, ...Option) (Database, error) {
				called = true
				return Database{}, nil
			},
		)
		if code, ok := ErrorCode(err); !ok || code != 2201 {
			t.Fatalf("conflict err = %v, want 2201", err)
		}
		if called {
			t.Fatal("opener called despite APIVersion conflict")
		}

		var unset atomic.Int32
		_, err = openWithConfig(
			context.Background(),
			Config{APIVersion: 999},
			&unset,
			func(context.Context, string, ...Option) (Database, error) { return Database{}, nil },
		)
		if code, ok := ErrorCode(err); !ok || code != 2203 {
			t.Fatalf("unsupported err = %v, want 2203", err)
		}
	})

	t.Run("invalid_timeout_and_retry_limit", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			cfg  Config
		}{
			{name: "negative_timeout", cfg: Config{TransactionTimeout: -time.Millisecond}},
			{name: "overflow_timeout", cfg: Config{TransactionTimeout: time.Duration(math.MaxInt32+1) * time.Millisecond}},
			{name: "invalid_retry_limit", cfg: Config{TransactionRetryLimit: -2}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var cur atomic.Int32
				_, err := openWithConfig(
					context.Background(),
					tc.cfg,
					&cur,
					func(context.Context, string, ...Option) (Database, error) { return Database{}, nil },
				)
				if code, ok := ErrorCode(err); !ok || code != 2006 {
					t.Fatalf("%s: err = %v, want 2006", tc.name, err)
				}
				if cur.Load() != 0 {
					t.Fatalf("invalid config mutated APIVersion to %d", cur.Load())
				}
			})
		}
	})

	t.Run("sub_ms_timeout_and_nil_ctx", func(t *testing.T) {
		t.Parallel()
		var cur atomic.Int32
		var nilCtx context.Context
		var gotCtx context.Context
		db, err := openWithConfig(
			nilCtx,
			Config{TransactionTimeout: 200 * time.Microsecond, TransactionRetryLimit: -1},
			&cur,
			func(ctx context.Context, _ string, _ ...Option) (Database, error) {
				gotCtx = ctx
				return Database{d: &internalDB{ctx: context.Background()}}, nil
			},
		)
		if err != nil {
			t.Fatalf("openWithConfig: %v", err)
		}
		if gotCtx == nil {
			t.Fatal("nil ctx was not normalized")
		}
		defs := db.d.defaults()
		if defs.timeout != 1 || !defs.hasRetryLimit || defs.retryLimit != -1 {
			t.Fatalf("defaults = %+v, want timeout=1 retryLimit=-1", defs)
		}
	})
}

func TestDatabaseCloseSatisfiesIOCloser(t *testing.T) {
	t.Parallel()
	var closer io.Closer = Database{}
	if err := closer.Close(); err != nil {
		t.Fatalf("zero Database.Close() = %v, want nil", err)
	}
}

type mockPlainTransactor struct {
	writeCalled bool
	readCalled  bool
	err         error
}

func (m *mockPlainTransactor) Transact(fn func(WritableTransaction) (any, error)) (any, error) {
	m.writeCalled = true
	if m.err != nil {
		return nil, m.err
	}
	return fn(nil)
}

func (m *mockPlainTransactor) ReadTransact(fn func(ReadTransaction) (any, error)) (any, error) {
	m.readCalled = true
	if m.err != nil {
		return nil, m.err
	}
	return fn(nil)
}

type mockCtxTransactor struct {
	mockPlainTransactor
	gotWriteCtx context.Context
	gotReadCtx  context.Context
}

func (m *mockCtxTransactor) TransactCtx(ctx context.Context, fn func(WritableTransaction) (any, error)) (any, error) {
	m.gotWriteCtx = ctx
	return fn(nil)
}

func (m *mockCtxTransactor) ReadTransactCtx(ctx context.Context, fn func(ReadTransaction) (any, error)) (any, error) {
	m.gotReadCtx = ctx
	return fn(nil)
}

func TestGenericTransactAndReadTransact(t *testing.T) {
	t.Parallel()

	t.Run("ctx_transactor_dispatch", func(t *testing.T) {
		t.Parallel()
		m := &mockCtxTransactor{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		w, err := Transact(ctx, m, func(WritableTransaction) (int, error) {
			return 42, nil
		})
		if err != nil || w != 42 || m.gotWriteCtx != ctx {
			t.Fatalf("Transact = (%d, %v), ctx=%v", w, err, m.gotWriteCtx)
		}

		r, err := ReadTransact(ctx, m, func(ReadTransaction) (string, error) {
			return "ok", nil
		})
		if err != nil || r != "ok" || m.gotReadCtx != ctx {
			t.Fatalf("ReadTransact = (%q, %v), ctx=%v", r, err, m.gotReadCtx)
		}
	})

	t.Run("plain_transactor_fallback_and_cancelled_ctx", func(t *testing.T) {
		t.Parallel()
		m := &mockPlainTransactor{}
		got, err := Transact(context.Background(), m, func(WritableTransaction) ([]byte, error) {
			return []byte("hello"), nil
		})
		if err != nil || string(got) != "hello" || !m.writeCalled {
			t.Fatalf("Transact fallback = (%q, %v), called=%v", got, err, m.writeCalled)
		}

		gotRead, err := ReadTransact(context.Background(), m, func(ReadTransaction) (any, error) {
			return nil, nil
		})
		if err != nil || gotRead != nil || !m.readCalled {
			t.Fatalf("ReadTransact nil any = (%v, %v), called=%v", gotRead, err, m.readCalled)
		}

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		m2 := &mockPlainTransactor{}
		if _, err := Transact(cancelled, m2, func(WritableTransaction) (int, error) { return 1, nil }); !errors.Is(err, context.Canceled) || m2.writeCalled {
			t.Fatalf("cancelled Transact = %v, called=%v", err, m2.writeCalled)
		}
		if _, err := ReadTransact(cancelled, m2, func(ReadTransaction) (int, error) { return 1, nil }); !errors.Is(err, context.Canceled) || m2.readCalled {
			t.Fatalf("cancelled ReadTransact = %v, called=%v", err, m2.readCalled)
		}
	})

	t.Run("nil_guards_and_error_propagation", func(t *testing.T) {
		t.Parallel()
		if _, err := Transact[int](context.Background(), nil, func(WritableTransaction) (int, error) { return 0, nil }); err == nil {
			t.Fatal("Transact with nil db succeeded")
		}
		if _, err := ReadTransact[int](context.Background(), nil, func(ReadTransaction) (int, error) { return 0, nil }); err == nil {
			t.Fatal("ReadTransact with nil db succeeded")
		}
		m := &mockPlainTransactor{}
		if _, err := Transact[int](context.Background(), m, nil); err == nil {
			t.Fatal("Transact with nil fn succeeded")
		}
		if _, err := ReadTransact[int](context.Background(), m, nil); err == nil {
			t.Fatal("ReadTransact with nil fn succeeded")
		}
		wantErr := Error{Code: 1020}
		if _, err := Transact(context.Background(), m, func(WritableTransaction) (int, error) { return 99, wantErr }); !errors.Is(err, wantErr) {
			t.Fatalf("Transact err = %v, want %v", err, wantErr)
		}
	})
}

type mockRawFuture struct {
	ready     atomic.Bool
	cancelled atomic.Bool
	blocked   atomic.Int32
}

func (m *mockRawFuture) BlockUntilReady() {
	m.blocked.Add(1)
	m.ready.Store(true)
}

func (m *mockRawFuture) IsReady() bool {
	return m.ready.Load()
}

func (m *mockRawFuture) Cancel() {
	if m.ready.Load() {
		return
	}
	m.cancelled.Store(true)
	m.ready.Store(true)
}

func TestGenericFuturesZeroGoroutines(t *testing.T) {
	rf := ReadyFuture("ready", nil)
	if !rf.IsReady() || rf.MustGet() != "ready" {
		t.Fatalf("ReadyFuture = (%v, %q)", rf.IsReady(), rf.MustGet())
	}
	rf.Cancel()

	raw := &mockRawFuture{}
	var decodeCalls atomic.Int32
	f := FutureOf(raw, func() (int, error) {
		decodeCalls.Add(1)
		return 21, nil
	})
	mapped := MapFuture(f, func(v int) (int, error) { return v * 2, nil })
	if f.IsReady() || mapped.IsReady() {
		t.Fatal("futures should not be ready before raw")
	}
	if got, err := mapped.Get(); err != nil || got != 42 {
		t.Fatalf("mapped.Get() = (%d, %v), want (42, nil)", got, err)
	}
	if got, err := mapped.Get(); err != nil || got != 42 || decodeCalls.Load() != 1 || !mapped.IsReady() {
		t.Fatalf("cached Get = (%d, %v), decodeCalls=%d, ready=%v", got, err, decodeCalls.Load(), mapped.IsReady())
	}

	var mapCalled atomic.Bool
	errFut := MapFuture(ReadyFuture(0, Error{Code: 1020}), func(int) (string, error) { mapCalled.Store(true); return "x", nil })
	if _, err := errFut.Get(); err == nil || mapCalled.Load() {
		t.Fatalf("errFut = (%v, mapCalled=%v), want error and no mapFn call", err, mapCalled.Load())
	}

	// Cancel before ready produces 1101 on both FutureOf and MapFuture.
	rawCancel := &mockRawFuture{}
	var cancelDecodeRan atomic.Bool
	cf := FutureOf(rawCancel, func() (int, error) {
		cancelDecodeRan.Store(true)
		return 1, nil
	})
	cm := MapFuture(cf, func(v int) (int, error) { return v + 1, nil })
	cm.Cancel()
	if !cm.IsReady() {
		t.Fatal("cancelled MapFuture must report IsReady() == true")
	}
	if _, err := cm.Get(); err == nil || cancelDecodeRan.Load() {
		t.Fatalf("cancelled Get = %v, cancelDecodeRan=%v", err, cancelDecodeRan.Load())
	} else if code, ok := ErrorCode(err); !ok || code != 1101 {
		t.Fatalf("cancelled code = %d, want 1101", code)
	}

	// Cancel after ready preserves value.
	rawReady := &mockRawFuture{}
	rawReady.ready.Store(true)
	readyWrapped := FutureOf(rawReady, func() (string, error) { return "kept", nil })
	readyWrapped.Cancel()
	if got, err := readyWrapped.Get(); err != nil || got != "kept" {
		t.Fatalf("late Cancel overwrote ready value: (%q, %v)", got, err)
	}

	// MustGet panics on error.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("MustGet did not panic on error")
			}
		}()
		_ = ReadyFuture(0, errors.New("boom")).MustGet()
	}()

	// Verify FutureOf and MapFuture do not spawn per-future goroutines by checking
	// that goroutine count does not scale with N=200 pending/completed cycles.
	const n = 200
	before := runtime.NumGoroutine()
	futures := make([]Future[int], n)
	for i := range futures {
		base := FutureOf(&mockRawFuture{}, func() (int, error) { return i, nil })
		futures[i] = MapFuture(base, func(v int) (int, error) { return v + 1, nil })
	}
	during := runtime.NumGoroutine()
	if delta := during - before; delta >= n/4 {
		t.Fatalf("goroutines grew by %d across %d pending FutureOf/MapFuture constructions (before=%d, during=%d)", delta, n, before, during)
	}
	for i, fut := range futures {
		if i%2 == 0 {
			got, err := fut.Get()
			if err != nil || got != i+1 {
				t.Fatalf("fut[%d].Get() = (%d, %v), want (%d, nil)", i, got, err, i+1)
			}
		} else {
			fut.Cancel()
		}
	}
	after := runtime.NumGoroutine()
	if delta := after - before; delta >= n/4 {
		t.Fatalf("goroutines grew by %d after %d FutureOf/MapFuture cycles (before=%d, after=%d)", delta, n, before, after)
	}
}

func TestGenericFuturesPanickingCallbacks(t *testing.T) {
	t.Parallel()

	t.Run("panicking_decode", func(t *testing.T) {
		t.Parallel()
		raw := &mockRawFuture{}
		var calls atomic.Int32
		f := FutureOf(raw, func() (int, error) {
			calls.Add(1)
			panic("decode boom")
		})

		var panicked any
		func() {
			defer func() { panicked = recover() }()
			_, _ = f.Get()
		}()
		if panicked != "decode boom" {
			t.Fatalf("first Get() panic = %v, want %q", panicked, "decode boom")
		}
		if !f.IsReady() {
			t.Fatal("FutureOf after panicking decode must report IsReady() == true")
		}
		f.Cancel()

		for i := range 2 {
			got, err := f.Get()
			if got != 0 || err == nil {
				t.Fatalf("subsequent Get() #%d = (%d, %v), want (0, non-nil error)", i+1, got, err)
			}
			if !errors.Is(err, errFutureCallbackPanicked) {
				t.Fatalf("subsequent Get() #%d err = %v, want %v", i+1, err, errFutureCallbackPanicked)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("decode calls = %d, want 1", calls.Load())
		}
	})

	t.Run("panicking_mapFn", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		m := MapFuture(ReadyFuture(7, nil), func(int) (string, error) {
			calls.Add(1)
			panic("map boom")
		})

		var panicked any
		func() {
			defer func() { panicked = recover() }()
			_, _ = m.Get()
		}()
		if panicked != "map boom" {
			t.Fatalf("first Get() panic = %v, want %q", panicked, "map boom")
		}
		if !m.IsReady() {
			t.Fatal("MapFuture after panicking mapFn must report IsReady() == true")
		}
		m.Cancel()

		for i := range 2 {
			got, err := m.Get()
			if got != "" || err == nil {
				t.Fatalf("subsequent Get() #%d = (%q, %v), want empty string and non-nil error", i+1, got, err)
			}
			if !errors.Is(err, errFutureCallbackPanicked) {
				t.Fatalf("subsequent Get() #%d err = %v, want %v", i+1, err, errFutureCallbackPanicked)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("mapFn calls = %d, want 1", calls.Load())
		}
	})

	t.Run("panicking_inner_future_propagates_to_MapFuture", func(t *testing.T) {
		t.Parallel()
		inner := FutureOf(&mockRawFuture{}, func() (int, error) {
			panic("inner decode boom")
		})
		var mapCalled atomic.Bool
		m := MapFuture(inner, func(v int) (int, error) {
			mapCalled.Store(true)
			return v * 2, nil
		})

		var panicked any
		func() {
			defer func() { panicked = recover() }()
			_, _ = m.Get()
		}()
		if panicked != "inner decode boom" {
			t.Fatalf("first MapFuture.Get() panic = %v, want %q", panicked, "inner decode boom")
		}

		got, err := m.Get()
		if got != 0 || !errors.Is(err, errFutureCallbackPanicked) || mapCalled.Load() {
			t.Fatalf("second MapFuture.Get() = (%d, %v, mapCalled=%v), want (0, errFutureCallbackPanicked, false)", got, err, mapCalled.Load())
		}
	})
}

func TestTxConfigAndApplyTxConfig(t *testing.T) {
	t.Parallel()

	tr, inner := newBareOptionsTx()
	cfg := TxConfig{
		Timeout:               2500 * time.Millisecond,
		RetryLimit:            4,
		Priority:              PriorityBatch,
		Access:                AccessSystemKeys,
		Lock:                  LockAware,
		GRVCache:              GRVCacheUse,
		CausalReadRisky:       true,
		DisableReadYourWrites: true,
		DisableWriteConflicts: true,
		DebugID:               "req-123",
		LogTransaction:        true,
	}
	if err := ApplyTxConfig(tr, cfg); err != nil {
		t.Fatalf("ApplyTxConfig: %v", err)
	}
	if !inner.LockAware() {
		t.Fatal("LockAware not applied")
	}
	if !inner.CausalReadRisky() {
		t.Fatal("CausalReadRisky not applied alongside GRVCacheUse")
	}

	// Verify ApplyTxConfig works on a ReadTransaction (e.g. Snapshot / ReadTransact).
	tr2, inner2 := newBareOptionsTx()
	var rtx ReadTransaction = tr2.Snapshot()
	if err := ApplyTxConfig(rtx, TxConfig{
		Timeout:         500 * time.Microsecond,
		RetryLimit:      -1,
		Priority:        PrioritySystemImmediate,
		Access:          AccessReadSystemKeys,
		Lock:            LockReadAware,
		GRVCache:        GRVCacheUse,
		CausalReadRisky: true,
	}); err != nil {
		t.Fatalf("ApplyTxConfig on ReadTransaction: %v", err)
	}
	if !inner2.ReadLockAware() {
		t.Fatal("LockReadAware not applied")
	}
	if !inner2.CausalReadRisky() {
		t.Fatal("CausalReadRisky not applied on ReadTransaction")
	}

	tr3, _ := newBareOptionsTx()
	if err := ApplyTxConfig(tr3, TxConfig{GRVCache: GRVCacheSkip}); err != nil {
		t.Fatalf("ApplyTxConfig GRVCacheSkip: %v", err)
	}

	if err := ApplyTxConfig(nil, TxConfig{}); err == nil {
		t.Fatal("ApplyTxConfig(nil) succeeded")
	}

	for _, bad := range []TxConfig{
		{Timeout: -time.Millisecond},
		{Timeout: time.Duration(math.MaxInt32+1) * time.Millisecond},
		{RetryLimit: -2},
		{Priority: Priority(99)},
		{Access: AccessMode(99)},
		{Lock: LockMode(99)},
		{GRVCache: GRVCacheMode(99)},
	} {
		trBad, _ := newBareOptionsTx()
		err := ApplyTxConfig(trBad, bad)
		if code, ok := ErrorCode(err); !ok || code != 2006 {
			t.Fatalf("ApplyTxConfig(%+v) = %v, want 2006", bad, err)
		}
	}

	// Verify String() formatting on enum values.
	if PriorityBatch.String() != "batch" || AccessSystemKeys.String() != "access_system_keys" ||
		LockAware.String() != "lock_aware" || LockReadAware.String() != "read_lock_aware" ||
		GRVCacheUse.String() != "use_grv_cache" || GRVCacheSkip.String() != "skip_grv_cache" {
		t.Fatal("unexpected enum String() output")
	}
}

func TestErrorPredicatesUnified(t *testing.T) {
	t.Parallel()

	var nilPtr *Error
	var nilWirePtr *wire.FDBError
	valErr := Error{Code: 1020}
	var ptrErr error = &Error{Code: 1021}
	wireConflict := &wire.FDBError{Code: 1020}
	wireMaybeComm := &wire.FDBError{Code: 1021}
	wrappedVal := fmt.Errorf("wrap: %w", valErr)
	wrappedPtr := fmt.Errorf("wrap: %w", ptrErr)
	wrappedWire := fmt.Errorf("wrap: %w", wireMaybeComm)
	joined := errors.Join(errors.New("other"), wrappedPtr)

	for _, tc := range []struct {
		err           error
		wantCode      int
		wantOK        bool
		wantRetryable bool
		wantMaybeComm bool
	}{
		{nilPtr, 0, false, false, false},
		{nilWirePtr, 0, false, false, false},
		{valErr, 1020, true, true, false},
		{ptrErr, 1021, true, true, true},
		{wireConflict, 1020, true, true, false},
		{wireMaybeComm, 1021, true, true, true},
		{wrappedVal, 1020, true, true, false},
		{wrappedPtr, 1021, true, true, true},
		{wrappedWire, 1021, true, true, true},
		{joined, 1021, true, true, true},
		{Error{Code: 1039}, 1039, true, true, true},
		{Error{Code: 1030}, 1030, true, false, false},
		{Error{Code: 1031}, 1031, true, false, false},
		{Error{Code: 2101}, 2101, true, false, false},
		{errors.New("plain"), 0, false, false, false},
		{nil, 0, false, false, false},
	} {
		gotCode, gotOK := ErrorCode(tc.err)
		if gotCode != tc.wantCode || gotOK != tc.wantOK {
			t.Errorf("ErrorCode(%v) = (%d, %v), want (%d, %v)", tc.err, gotCode, gotOK, tc.wantCode, tc.wantOK)
		}
		if got := IsRetryableError(tc.err); got != tc.wantRetryable {
			t.Errorf("IsRetryableError(%v) = %v, want %v", tc.err, got, tc.wantRetryable)
		}
		if got := IsMaybeCommitted(tc.err); got != tc.wantMaybeComm {
			t.Errorf("IsMaybeCommitted(%v) = %v, want %v", tc.err, got, tc.wantMaybeComm)
		}
	}

	if !(Error{Code: 1021}).IsMaybeCommitted() || !(Error{Code: 1039}).IsMaybeCommitted() {
		t.Fatal("Error.IsMaybeCommitted must be true for 1021 and 1039")
	}
	if (Error{Code: 1020}).IsMaybeCommitted() || (Error{Code: 1030}).IsMaybeCommitted() {
		t.Fatal("Error.IsMaybeCommitted must be false for 1020 and 1030")
	}
}
