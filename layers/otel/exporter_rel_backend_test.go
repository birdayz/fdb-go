package otel

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// NewExporterWithBackend: ownership from entry, one close on every failure and at
// Shutdown, the shared initializer, and no client opened behind the caller's back.

var errBackendDown = errors.New("injected backend failure")

// failingBackend fails every call and counts Close; it needs no cluster.
type failingBackend struct{ closes atomic.Int32 }

func (b *failingBackend) Transact(func(fdb.WritableTransaction) (any, error)) (any, error) {
	return nil, errBackendDown
}

func (b *failingBackend) ReadTransact(func(fdb.ReadTransaction) (any, error)) (any, error) {
	return nil, errBackendDown
}

func (b *failingBackend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	return nil, errBackendDown
}

func (b *failingBackend) LocalityGetBoundaryKeys(fdb.ExactRange, int, int64) ([]fdb.Key, error) {
	return nil, errBackendDown
}

func (b *failingBackend) Close() { b.closes.Add(1) }

// neverOpened is a cluster file path no client can parse: a constructor that opened
// its own client would fail on it.
func neverOpened(t *testing.T) string {
	return filepath.Join(t.TempDir(), "never-opened.cluster")
}

func TestExporterWithBackendOwnership(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run = neverOpened(t), "backend_ownership"
	// Invalid handles are refused without calling a method on them.
	var typedNil *failingBackend
	var nilDatabase *fdb.Database
	for name, backend := range map[string]fdb.BackendDatabase{
		"nil":             nil,
		"typed nil":       typedNil,
		"zero Database":   fdb.Database{},
		"nil *Database":   nilDatabase,
		"zero *Database":  &fdb.Database{},
		"nil wrapped ptr": (*ownedBackend)(nil),
	} {
		e, err := NewExporterWithBackend(ctx, cfg, backend)
		if e != nil {
			cleanupExporter(t, e)
			t.Fatalf("%s: exporter returned", name)
		}
		requireOtelKind(t, err, "config")
	}
	// A valid handle is owned from entry: each failure closes it exactly once.
	for name, mutate := range map[string]func(*ExporterConfig){
		"no cluster file":     func(c *ExporterConfig) { c.ClusterFile = "" },
		"bad batch size":      func(c *ExporterConfig) { c.BatchSize = 0 },
		"brotli compression":  func(c *ExporterConfig) { c.RecordCompression = "brotli" },
		"bootstrap transacts": nil,
	} {
		c := cfg
		if mutate != nil {
			mutate(&c)
		}
		backend := &failingBackend{}
		e, err := NewExporterWithBackend(ctx, c, backend)
		if e != nil || err == nil {
			cleanupExporter(t, e)
			t.Fatalf("%s: constructor succeeded", name)
		}
		if mutate != nil {
			requireOtelKind(t, err, "config")
		} else if !errors.Is(err, errBackendDown) {
			t.Fatalf("%s: %v", name, err)
		}
		if got := backend.closes.Load(); got != 1 {
			t.Fatalf("%s: backend closed %d times, want 1", name, got)
		}
	}
}

// ownedBackend hides a real pure-Go handle behind the interface, as a native client
// is: no concrete handle or client metrics reach the Exporter. It counts Close and can
// hold the next write transaction until released.
type ownedBackend struct {
	fdb.BackendDatabase
	closes  atomic.Int32
	onClose func()
	hold    atomic.Pointer[heldTransaction]
}

type heldTransaction struct{ entered, release chan struct{} }

func (b *ownedBackend) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	if h := b.hold.Swap(nil); h != nil {
		close(h.entered)
		<-h.release
	}
	return b.BackendDatabase.Transact(f)
}

func (b *ownedBackend) Close() {
	b.closes.Add(1)
	if b.onClose != nil {
		b.onClose()
	}
	b.BackendDatabase.Close()
}

// openOwned opens a pure-Go handle the constructor will own and close.
func openOwned(t *testing.T, ctx context.Context, cluster string) fdb.Database {
	t.Helper()
	raw, err := openRelDatabase(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// backendConfig is a fresh run whose cluster file the constructor must not open.
func backendConfig(t *testing.T, cluster string, mutate func(*ExporterConfig)) ExporterConfig {
	cfg := relTestConfig(t, cluster, mutate)
	cfg.ClusterFile = neverOpened(t)
	return cfg
}

// cleanupExporter shuts a constructed exporter down when the test ends, so a failed
// assertion leaks no owned handle; Shutdown is idempotent after an explicit one.
func cleanupExporter(t *testing.T, e *Exporter) {
	if e == nil {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
}

func shutdownWithin(t *testing.T, e *Exporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExporterWithBackendRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	t.Run("interface_backend_matches_default_rows_and_indexes", func(t *testing.T) {
		t.Parallel()
		testBackendParity(t, cluster)
	})
	t.Run("concrete_backend_keeps_its_handle_and_metrics", func(t *testing.T) {
		t.Parallel()
		testBackendConcrete(t, cluster)
	})
	t.Run("layout_and_hook_failures_close_once", func(t *testing.T) {
		t.Parallel()
		testBackendSetupFailures(t, cluster)
	})
	t.Run("shutdown_waits_for_calls_and_hooks_then_closes_once", func(t *testing.T) {
		t.Parallel()
		testBackendShutdown(t, cluster)
	})
}

// tenantKVs is a store's complete key/value map relative to its subspace, without
// the header (a per-store record); an independent handle reads it.
func tenantKVs(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, cfg ExporterConfig) map[string][]byte {
	t.Helper()
	path, schema := relTenantPaths(cfg.Run, cfg.Tenant)
	ts, err := otelsql.OpenTenantStoreCompiled(ctx, db, path, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	begin, end := ts.Subspace.FDBRangeKeys()
	out, err := db.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatal(err)
	}
	prefix := len(ts.Subspace.Bytes())
	header := ts.Subspace.Pack(tuple.Tuple{int64(recordlayer.StoreInfoKey)})
	kvs := map[string][]byte{}
	sawHeader := false
	for _, kv := range out.([]fdb.KeyValue) {
		if bytes.Equal(kv.Key, header) {
			sawHeader = true
			continue
		}
		kvs[string(kv.Key[prefix:])] = bytes.Clone(kv.Value)
	}
	if !sawHeader {
		t.Fatal("store header is absent")
	}
	return kvs
}

func testBackendParity(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	spans := []*tracepb.ResourceSpans{deltaSpan(1), deltaSpan(2), deltaSpan(3)}
	direct := openRelExporter(t, ctx, cluster, nil)
	if _, err := direct.ExportResourceSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	owned := &ownedBackend{BackendDatabase: openOwned(t, ctx, cluster)}
	cfg := backendConfig(t, cluster, nil)
	e, err := NewExporterWithBackend(ctx, cfg, owned)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExporter(t, e)
	if _, err := e.ExportResourceSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	reader := rdDB(t, ctx, cluster)
	want, got := tenantKVs(t, ctx, reader, direct.Config()), tenantKVs(t, ctx, reader, cfg)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("backend store has %d keys, default store %d; they must be identical", len(got), len(want))
	}
	// Default construction keeps measuring its pure client; the interface path has no
	// client metrics to report, an empty snapshot rather than measured zeros.
	if reflect.DeepEqual(direct.ClientMetrics(), client.ClientMetricsSnapshot{}) {
		t.Fatal("default exporter lost its pure-client metrics")
	}
	if e.raw.IsValid() || !reflect.DeepEqual(e.ClientMetrics(), client.ClientMetricsSnapshot{}) {
		t.Fatal("an interface backend must leave no concrete handle or metrics")
	}
	if owned.closes.Load() != 0 {
		t.Fatal("successful construction closed the backend")
	}
	shutdownWithin(t, e)
	if got := owned.closes.Load(); got != 1 {
		t.Fatalf("backend closed %d times, want 1", got)
	}
}

func testBackendConcrete(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// validBackend accepts the handle by value and by pointer; both stay concrete.
	for _, byPointer := range []bool{false, true} {
		raw := openOwned(t, ctx, cluster)
		var backend fdb.BackendDatabase = raw
		if byPointer {
			backend = &raw
		}
		e, err := NewExporterWithBackend(ctx, backendConfig(t, cluster, nil), backend)
		if err != nil {
			t.Fatal(err)
		}
		cleanupExporter(t, e)
		if e.raw != raw {
			t.Fatalf("pointer %v: an actual fdb.Database must stay the Exporter's concrete handle", byPointer)
		}
		if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1)}); err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(e.ClientMetrics(), client.ClientMetricsSnapshot{}) {
			t.Fatalf("pointer %v: a concrete backend's client metrics are lost", byPointer)
		}
		shutdownWithin(t, e)
	}
}

func testBackendSetupFailures(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// A store published with another layout refuses this exporter's: an ownership error.
	deltas := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	cfg := deltas.Config()
	cfg.EnableMessageDeltas = false
	cfg.ClusterFile = neverOpened(t)
	owned := &ownedBackend{BackendDatabase: openOwned(t, ctx, cluster)}
	if e, err := NewExporterWithBackend(ctx, cfg, owned); e != nil || err == nil {
		cleanupExporter(t, e)
		t.Fatal("a mismatched store layout was accepted")
	} else {
		requireOtelKind(t, err, "ownership")
	}
	if got := owned.closes.Load(); got != 1 {
		t.Fatalf("layout failure closed the backend %d times, want 1", got)
	}
	// The message hooks fail after the store is ready.
	hookErr := errors.New("injected hook failure")
	owned = &ownedBackend{BackendDatabase: openOwned(t, ctx, cluster)}
	failing := relParts{messages: func(*tenantStore, ExporterConfig) (relMessageHooks, error) { return nil, hookErr }}
	cfg = backendConfig(t, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	if e, err := newExporterWithBackend(ctx, cfg, owned, failing); e != nil || !errors.Is(err, hookErr) {
		cleanupExporter(t, e)
		t.Fatalf("hook failure: %v", err)
	}
	if got := owned.closes.Load(); got != 1 {
		t.Fatalf("hook failure closed the backend %d times, want 1", got)
	}
}

func testBackendShutdown(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	hooks := &reMessages{}
	var hooksClosedFirst atomic.Bool
	owned := &ownedBackend{BackendDatabase: openOwned(t, ctx, cluster)}
	owned.onClose = func() {
		hooks.mu.Lock()
		defer hooks.mu.Unlock()
		hooksClosedFirst.Store(hooks.closed)
	}
	cfg := backendConfig(t, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	e, err := newExporterWithBackend(ctx, cfg, owned, reMessageParts(hooks))
	if err != nil {
		t.Fatal(err)
	}
	cleanupExporter(t, e)
	held := &heldTransaction{entered: make(chan struct{}), release: make(chan struct{})}
	// Deferred release runs before the cleanup Shutdown, so a failed assertion cannot
	// leave that Shutdown waiting on the held transaction.
	release := sync.OnceFunc(func() { close(held.release) })
	defer release()
	owned.hold.Store(held)
	exported := make(chan error, 1)
	go func() {
		_, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1)})
		exported <- err
	}()
	select {
	case <-held.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the export never reached its write transaction")
	}
	// A Shutdown whose deadline ends first returns canceled and closes nothing yet.
	expired, stop := context.WithTimeout(ctx, time.Millisecond)
	defer stop()
	requireOtelKind(t, e.Shutdown(expired), "canceled")
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(2)}); err == nil {
		t.Fatal("new work was admitted after Shutdown began")
	} else {
		requireOtelKind(t, err, "operation")
	}
	if owned.closes.Load() != 0 {
		t.Fatal("the backend was closed under an active call")
	}
	// Concurrent Shutdowns rejoin the same drain and see one close after the work.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.Shutdown(ctx)
		}()
	}
	release()
	select {
	case err := <-exported:
		if err != nil {
			t.Fatalf("the active export failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the released export never returned")
	}
	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent Shutdowns never returned")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("shutdown %d: %v", i, err)
		}
	}
	if got := owned.closes.Load(); got != 1 {
		t.Fatalf("backend closed %d times, want 1", got)
	}
	if !hooksClosedFirst.Load() {
		t.Fatal("the backend closed before the message hooks")
	}
	if err := e.Shutdown(ctx); err != nil || owned.closes.Load() != 1 {
		t.Fatalf("a later Shutdown: %v, closes %d", err, owned.closes.Load())
	}
}
