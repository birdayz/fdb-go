package otel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Ports of the exporter halves of TestTxnLimitRealFDB (txn_limit_test.go),
// span exports only: the log and message write paths are streams 2B and 2C.
// The faults are injected through the tenant store's *FDBDatabase, which
// the exporter's writer and reader share (§2.1).
func TestRelExporterTxnPolicyRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	t.Run("exporter_attaches_limiter", func(t *testing.T) { t.Parallel(); testRelTxnLimit(t, cluster) })
	t.Run("exporter_calls_absorb_retryable", func(t *testing.T) { t.Parallel(); testRelCallsAbsorb(t, cluster) })
	t.Run("exporter_calls_fail_transient", func(t *testing.T) { t.Parallel(); testRelCallsFail(t, cluster) })
	t.Run("bootstrap_carries_policy", func(t *testing.T) { t.Parallel(); testRelBootstrapPolicy(t, cluster) })
}

// relTurns is a span-only conversation for the policy tests.
func relTurns() []*tracepb.ResourceSpans {
	return deltaConversation(deltaTurns, "array")
}

func testRelTxnLimit(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.MaxInFlightTransactions = 1; c.Workers, c.BatchSize = 4, 2 })
	if cap(e.txnSlots) != 1 {
		t.Fatalf("slots=%d want 1", cap(e.txnSlots))
	}
	input := relTurns()

	// While the only write slot is held, a deadline ends the wait unwritten.
	e.txnSlots <- struct{}{}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err := e.ExportResourceSpans(short, input)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("export with held slot: %v", err)
	}
	// Exports block on the held slot; queries take none.
	var stats ExportStats
	done := make(chan error, 1)
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
	if got, err := e.Trace(ctx, ftsTrace(1)); err != nil || len(got) != 0 {
		t.Fatalf("query with held slot: %d spans, %v", len(got), err)
	}
	<-e.txnSlots
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats.Spans != deltaTurns || stats.Batches < (deltaTurns+1)/2 {
		t.Fatalf("export stats %+v", stats)
	}
	if got, err := e.Trace(ctx, ftsTrace(1)); err != nil || len(got) != 1 {
		t.Fatalf("spans after export: %d, %v", len(got), err)
	}
	if len(e.txnSlots) != 0 {
		t.Fatalf("%d slots leaked", len(e.txnSlots))
	}
}

// Exporter writes and reads absorb 12 future_version retries in one attempt
// and return complete results.
func testRelCallsAbsorb(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	const k = 12
	inject := &retryInjector{Database: e.raw, code: 1009, k: k}
	e.store.db = recordlayer.NewFDBDatabaseWithTransactor(inject, e.raw)
	stats, err := e.ExportResourceSpans(ctx, relTurns())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Spans != deltaTurns {
		t.Fatalf("export stats %+v", stats)
	}
	if got, err := e.Trace(ctx, ftsTrace(1)); err != nil || len(got) != 1 {
		t.Fatalf("trace after export: %d, %v", len(got), err)
	}
	if r, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"}); err != nil || len(r.Hits) != deltaTurns {
		t.Fatalf("query after export: %+v %v", r, err)
	}
	inject.mu.Lock()
	defer inject.mu.Unlock()
	if inject.transactions == 0 || inject.minimum != k+1 {
		t.Fatalf("%d transactions, fewest executions %d; want every one to absorb %d failures", inject.transactions, inject.minimum, k)
	}
}

// A timed-out or deadline-ended exporter transaction fails the export with a
// retryable kind, not "operation", and releases its write slot.
func testRelCallsFail(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	t.Run("transaction_timeout", func(t *testing.T) {
		e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.TransactionTimeout = minExporterTxnTimeout })
		e.store.db = recordlayer.NewFDBDatabaseWithTransactor(&retryInjector{Database: e.raw, code: 1009, k: -1}, e.raw)
		_, err := e.ExportResourceSpans(ctx, relTurns())
		requireOtelKind(t, err, "transient")
		var transient *TransientError
		if code, ok := fdbErrorCode(err); !ok || code != 1031 || !errors.As(err, &transient) {
			t.Fatalf("%v, want a TransientError carrying 1031", err)
		}
	})
	t.Run("context_deadline", func(t *testing.T) {
		e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.TransactionTimeout = time.Minute })
		inject := &retryInjector{Database: e.raw, code: 1009, k: -1, waitForDeadline: true}
		e.store.db = recordlayer.NewFDBDatabaseWithTransactor(inject, e.raw)
		short, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		started := time.Now()
		_, err := e.ExportResourceSpans(short, relTurns())
		requireOtelKind(t, err, "canceled")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%v lacks context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("deadline-bounded call took %v under a 1m transaction timeout", elapsed)
		}
		inject.mu.Lock()
		bodies := inject.bodies
		inject.mu.Unlock()
		if bodies == 0 {
			t.Fatal("deadline was not exercised inside a successful live transaction body")
		}
		// The canceled operation releases its slot for a new live call.
		e.store.db = recordlayer.NewFDBDatabaseWithTransactor(e.raw, e.raw)
		if stats, err := e.ExportResourceSpans(ctx, relTurns()); err != nil || stats.Spans != deltaTurns {
			t.Fatalf("slot recovery: %+v / %v", stats, err)
		}
	})
}

// Port of TestExporterBootstrapCarriesPolicyIntoFreshResolvers: the store's
// creation (EnsureTenant) and layout check run under the exporter policy, so
// each absorbs 12 retryable failures in one attempt, and the first read
// outlives the benchmark's 1.5s transaction timeout.
func testRelBootstrapPolicy(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cfg := relTestConfig(t, cluster, func(c *ExporterConfig) { c.TransactionTimeout = time.Minute })
	var inject *delayedBootstrapTransactor
	e := startRelExporter(t, ctx, cfg, relParts{newDatabase: func(raw fdb.Database) *recordlayer.FDBDatabase {
		inject = &delayedBootstrapTransactor{retryInjector: &retryInjector{Database: raw, code: 1009, k: 12}}
		return recordlayer.NewFDBDatabaseWithTransactor(inject, raw)
	}})
	if inject == nil || !inject.delayed.Load() {
		t.Fatal("the first bootstrap transaction did not cross the benchmark timeout boundary")
	}
	inject.mu.Lock()
	transactions, minimum := inject.transactions, inject.minimum
	inject.mu.Unlock()
	// EnsureTenant and the layout publish.
	if transactions < 2 || minimum != 13 {
		t.Fatalf("bootstrap transactions=%d, minimum executions=%d; each must absorb 12 retryable failures", transactions, minimum)
	}
	if _, err := e.ExportResourceSpans(ctx, []*tracepb.ResourceSpans{deltaSpan(1)}); err != nil {
		t.Fatal(err)
	}
}

// Port of TestOpenFDBBootstrapCancellationAfterConnect: caller cancellation
// ends an active bootstrap against a silent coordinator.
func TestRelExporterBootstrapCancellationAfterConnect(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	path := filepath.Join(t.TempDir(), "blackhole.cluster")
	if err := os.WriteFile(path, []byte("otelbootstrap:cancel283@"+listener.Addr().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		cfg := DefaultExporterConfig()
		cfg.ClusterFile, cfg.Run, cfg.RecordCompression = path, "bootstrap_cancel", "zlib-1"
		e, err := newExporter(ctx, cfg, relParts{})
		if e != nil {
			_ = e.Shutdown(context.Background())
		}
		result <- err
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case err := <-result:
		t.Fatalf("bootstrap returned before connecting: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("FDB bootstrap never connected to local coordinator")
	}
	defer conn.Close()
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active bootstrap cancellation: %v", err)
		}
		if time.Since(started) > 2*time.Second {
			t.Fatal("bootstrap used its independent timeout instead of caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active FDB bootstrap ignored caller cancellation")
	}
	if _, err := newExporter(t.Context(), DefaultExporterConfig(), relParts{}); err == nil {
		t.Fatal("an empty cluster file was accepted")
	} else {
		requireOtelKind(t, err, "config")
	}
}
