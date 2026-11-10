package otel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The exporter's transaction policy on every write path: span batches, log
// batches and message persistence take a write slot, absorb retryable
// failures in one attempt, and fail transient on the transaction timeout or
// canceled on the caller's deadline. The faults are injected through the
// tenant store's *FDBDatabase, which every writer and reader shares.
func TestRelExporterTxnPolicyRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	t.Run("exporter_attaches_limiter", func(t *testing.T) { t.Parallel(); testRelTxnLimit(t, cluster) })
	t.Run("exporter_calls_absorb_retryable", func(t *testing.T) { t.Parallel(); testRelCallsAbsorb(t, cluster) })
	t.Run("exporter_calls_fail_transient", func(t *testing.T) { t.Parallel(); testRelCallsFail(t, cluster) })
	t.Run("bootstrap_carries_policy", func(t *testing.T) { t.Parallel(); testRelBootstrapPolicy(t, cluster) })
}

// relTurns is a GenAI conversation, one span and trace per turn: with
// messages on, its export persists 2n+3 messages before the spans.
func relTurns() []*tracepb.ResourceSpans {
	return deltaConversation(deltaTurns, "array")
}

// relFullText enables message full text.
func relFullText(c *ExporterConfig) { c.EnableMessageFullText = true }

func txnTestLogs() []LogRecord {
	return []LogRecord{
		{EventID: "policy-1", Timestamp: ftsBase, TraceID: ftsTrace(1), SpanID: ftsSpan(1), Attributes: []*commonpb.KeyValue{exKV("phase", exStr("one"))}},
		{EventID: "policy-2", Timestamp: ftsBase + 1, TraceID: ftsTrace(1), SpanID: ftsSpan(1)},
	}
}

// relIndexEntries counts index's entries in e's store.
func relIndexEntries(t *testing.T, ctx context.Context, e *Exporter, index string) int {
	t.Helper()
	n := 0
	if err := e.store.read(e.txnContext(ctx), "test", func(store *recordlayer.FDBRecordStore) error {
		n = 0
		w := &queryWork{maxCandidates: 1 << 30, maxBytes: 1 << 40}
		return scanIndex(ctx, store, "test", index, recordlayer.TupleRangeAll, false, w, func(*recordlayer.IndexEntry) (bool, error) {
			n++
			return false, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireRelTurnsStored checks relTurns' export stats and that its spans,
// messages and full text are readable.
func requireRelTurnsStored(t *testing.T, ctx context.Context, e *Exporter, stats ExportStats) {
	t.Helper()
	n := int64(deltaTurns)
	if stats.Spans != n || stats.Messages.Written != 2*n+3 || stats.Messages.References != n*(n+1)+3*n {
		t.Fatalf("export stats %+v", stats)
	}
	if got, err := e.Trace(ctx, ftsTrace(1)); err != nil || len(got) != 1 {
		t.Fatalf("trace after export: %d, %v", len(got), err)
	}
	if got := relIndexEntries(t, ctx, e, messagesHashIndex); got != 2*deltaTurns+3 {
		t.Fatalf("%d messages stored", got)
	}
	page, err := e.QueryMessageFullText(ctx, MessageFullTextQuery{Query: "user turn 3", Mode: MessageMatchPhrase})
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("full text after export: %+v, %v", page, err)
	}
}

func testRelTxnLimit(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) {
		relFullText(c)
		c.MaxInFlightTransactions = 1
		c.Workers, c.BatchSize = 4, 2
	})
	if cap(e.txnSlots) != 1 {
		t.Fatalf("slots=%d want 1", cap(e.txnSlots))
	}
	input := relTurns()

	// While the only write slot is held, a deadline ends each write path's
	// wait unwritten.
	e.txnSlots <- struct{}{}
	for name, export := range map[string]func(context.Context) error{
		"spans": func(ctx context.Context) error { _, err := e.ExportResourceSpans(ctx, input); return err },
		"logs":  func(ctx context.Context) error { _, err := e.ExportLogs(ctx, txnTestLogs()); return err },
	} {
		short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		err := export(short)
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s export with held slot: %v", name, err)
		}
	}
	// Exports block on the held slot, before any message is persisted;
	// queries take none.
	var stats ExportStats
	var logStats LogExportStats
	done := make(chan error, 2)
	go func() {
		var err error
		stats, err = e.ExportResourceSpans(ctx, input)
		done <- err
	}()
	go func() {
		var err error
		logStats, err = e.ExportLogs(ctx, txnTestLogs())
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
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 0 {
		t.Fatalf("log query with held slot: %d logs, %v", len(got), err)
	}
	if got := relIndexEntries(t, ctx, e, messagesHashIndex); got != 0 {
		t.Fatalf("%d messages persisted while the slot was held", got)
	}
	<-e.txnSlots

	// With one slot, concurrent exports over every write path complete.
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if stats.Batches < (deltaTurns+1)/2 {
		t.Fatalf("export stats %+v", stats)
	}
	requireRelTurnsStored(t, ctx, e, stats)
	if logStats.Logs != 2 {
		t.Fatalf("log export %+v", logStats)
	}
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 2 {
		t.Fatalf("logs after export: %d, %v", len(got), err)
	}
	if len(e.txnSlots) != 0 {
		t.Fatalf("%d slots leaked", len(e.txnSlots))
	}
}

// Exporter writes (spans, messages, logs) and reads absorb 12 future_version
// retries in one attempt and return complete results.
func testRelCallsAbsorb(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, relFullText)
	const k = 12
	inject := &retryInjector{Database: e.raw, code: 1009, k: k}
	e.store.db = recordlayer.NewFDBDatabaseWithTransactor(inject, e.raw)
	traceEnabled := os.Getenv("OTEL_TEST_RETRY_TRACE") == "1"
	if os.Getenv("OTEL_TEST_RETRY_DIAGNOSTICS") == "1" || traceEnabled {
		diagnostics := &retryDiagnostics{started: time.Now(), traceRegions: traceEnabled}
		inject.diagnostics = diagnostics
		e.store.db.SetAttemptObserver(func(call recordlayer.AttemptCall, err error) {
			diagnostics.attempt(time.Now(), call, err)
		})
		defer func() {
			inject.mu.Lock()
			transactions, minimum, bodies := inject.transactions, inject.minimum, inject.bodies
			inject.mu.Unlock()
			t.Logf("retry diagnostics fixture turns=%d k=%d configured_timeout=%v failed=%t successful_transactions=%d minimum_executions=%d successful_bodies=%d",
				deltaTurns, k, e.cfg.TransactionTimeout, t.Failed(), transactions, minimum, bodies)
			diagnostics.log(t.Logf)
		}()
		if traceEnabled {
			out := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
			if out == "" {
				t.Fatal("OTEL_TEST_RETRY_TRACE=1 requires TEST_UNDECLARED_OUTPUTS_DIR")
			}
			output, err := os.CreateTemp(out, "otel-retry-*.trace")
			if err != nil {
				t.Fatalf("create retry trace artifact in %s: %v", out, err)
			}
			if err := trace.Start(output); err != nil {
				t.Fatalf("start retry trace artifact=%s: %v (close error: %v)", output.Name(), err, output.Close())
			}
			defer func() {
				trace.Stop()
				info, statErr := output.Stat()
				closeErr := output.Close()
				size := int64(-1)
				if statErr != nil {
					t.Errorf("stat retry trace artifact=%s: %v", output.Name(), statErr)
				} else {
					size = info.Size()
					if size == 0 {
						t.Errorf("empty retry trace artifact=%s", output.Name())
					}
				}
				if closeErr != nil {
					t.Errorf("close retry trace artifact=%s: %v", output.Name(), closeErr)
				}
				t.Logf("retry trace finished artifact=%s bytes=%d failed=%t", output.Name(), size, t.Failed())
			}()
			t.Logf("retry trace started artifact=%s (process-wide; original turns=%d k=%d configured_timeout=%v)", output.Name(), deltaTurns, k, e.cfg.TransactionTimeout)
		}
	}
	stats, err := e.ExportResourceSpans(ctx, relTurns())
	if err != nil {
		t.Fatal(err)
	}
	requireRelTurnsStored(t, ctx, e, stats)
	if r, err := e.Query(ctx, Attribute{"span", "gen_ai.operation.name", "string", "chat"}); err != nil || len(r.Hits) != deltaTurns {
		t.Fatalf("query after export: %+v %v", r, err)
	}
	if logStats, err := e.ExportLogs(ctx, txnTestLogs()); err != nil || logStats.Logs != 2 {
		t.Fatalf("log export %+v, %v", logStats, err)
	}
	if got, err := e.QueryLogsByTrace(ctx, ftsTrace(1), ""); err != nil || len(got) != 2 {
		t.Fatalf("logs after export: %d, %v", len(got), err)
	}
	inject.mu.Lock()
	defer inject.mu.Unlock()
	if inject.transactions == 0 || inject.minimum != k+1 {
		t.Fatalf("%d transactions, fewest executions %d; want every one to absorb %d failures", inject.transactions, inject.minimum, k)
	}
}

// A timed-out or deadline-ended exporter transaction fails span export,
// message persistence and log export with a retryable kind, not
// "operation", and releases its write slot.
func testRelCallsFail(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	// Each write path, alone: spans without messages, messages (persisted
	// before their spans), logs.
	paths := []struct {
		name     string
		messages bool
		call     func(*Exporter, context.Context) error
	}{
		{"spans", false, func(e *Exporter, ctx context.Context) error {
			_, err := e.ExportResourceSpans(ctx, relTurns())
			return err
		}},
		{"messages", true, func(e *Exporter, ctx context.Context) error {
			_, err := e.ExportResourceSpans(ctx, relTurns())
			return err
		}},
		{"logs", false, func(e *Exporter, ctx context.Context) error { _, err := e.ExportLogs(ctx, txnTestLogs()); return err }},
	}
	// requirePath checks that a message-path failure came from a message
	// transaction, not from the spans after it.
	requirePath := func(t *testing.T, messages bool, err error) {
		t.Helper()
		var oe *OtelError
		if !errors.As(err, &oe) || messages != strings.HasPrefix(oe.Op, "message ") {
			t.Fatalf("failing operation of %v, want a message transaction: %t", err, messages)
		}
	}
	open := func(t *testing.T, messages bool, timeout time.Duration) *Exporter {
		return openRelExporter(t, ctx, cluster, func(c *ExporterConfig) {
			c.EnableMessageFullText = messages
			c.TransactionTimeout = timeout
		})
	}
	for _, p := range paths {
		t.Run(p.name+"/transaction_timeout", func(t *testing.T) {
			e := open(t, p.messages, minExporterTxnTimeout)
			e.store.db = recordlayer.NewFDBDatabaseWithTransactor(&retryInjector{Database: e.raw, code: 1009, k: -1}, e.raw)
			err := p.call(e, ctx)
			requireOtelKind(t, err, "transient")
			requirePath(t, p.messages, err)
			var transient *TransientError
			if code, ok := fdbErrorCode(err); !ok || code != 1031 || !errors.As(err, &transient) {
				t.Fatalf("%v, want a TransientError carrying 1031", err)
			}
		})
		t.Run(p.name+"/context_deadline", func(t *testing.T) {
			e := open(t, p.messages, time.Minute)
			inject := &retryInjector{Database: e.raw, code: 1009, k: -1, waitForDeadline: true}
			e.store.db = recordlayer.NewFDBDatabaseWithTransactor(inject, e.raw)
			short, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			started := time.Now()
			err := p.call(e, short)
			requireOtelKind(t, err, "canceled")
			requirePath(t, p.messages, err)
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
			if err := p.call(e, ctx); err != nil {
				t.Fatalf("slot recovery: %v", err)
			}
		})
	}
}

// testRelBootstrapPolicy: the store's creation (EnsureTenant) and layout
// check run under the exporter policy, so each absorbs 12 retryable failures
// in one attempt, and the first read outlives the 1.5s default transaction
// timeout (benchTxnTimeout).
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
		t.Fatal("the first bootstrap transaction did not cross the default timeout boundary")
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

// TestRelExporterBootstrapCancellationAfterConnect: caller cancellation ends
// an active bootstrap against a silent coordinator.
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

// canceledAfterFirstErr is a canceled context whose first Err() reports it
// live: cancellation arriving between openRelDatabase's two checks.
type canceledAfterFirstErr struct {
	context.Context
	calls int
}

func (c *canceledAfterFirstErr) Err() error {
	if c.calls++; c.calls == 1 {
		return nil
	}
	return c.Context.Err()
}

// TestOpenRelDatabaseGuards: openRelDatabase refuses a canceled caller
// before and after parsing the cluster file, an empty path, and an absent,
// empty or malformed cluster file, without dialing (Validate is bypassed).
func TestOpenRelDatabaseGuards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := write("valid.cluster", "otelguards:guards283@127.0.0.1:1\n")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	requireOp := func(err error, kind, op string) {
		t.Helper()
		requireOtelKind(t, err, kind)
		var typed *OtelError
		if !errors.As(err, &typed) || typed.Op != op {
			t.Fatalf("want %s error from %q: %v", kind, op, err)
		}
	}
	// Before parsing, a canceled caller wins over an unreadable cluster file;
	// after parsing, it is refused before the dial.
	for name, open := range map[string]func() (fdb.Database, error){
		"canceled_before_parse": func() (fdb.Database, error) { return openRelDatabase(canceled, "not-a-cluster-file") },
		"canceled_after_parse": func() (fdb.Database, error) {
			return openRelDatabase(&canceledAfterFirstErr{Context: canceled}, valid)
		},
	} {
		_, err := open()
		requireOp(err, "canceled", "open FDB")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: cause is not context.Canceled: %v", name, err)
		}
	}
	_, err := openRelDatabase(t.Context(), "")
	requireOp(err, "config", "open FDB")
	for _, path := range []string{
		filepath.Join(dir, "absent.cluster"),
		write("empty.cluster", ""),
		write("comments.cluster", "# no connection string\n\n"),
		write("malformed.cluster", "not a connection string\n"),
	} {
		_, err := openRelDatabase(t.Context(), path)
		var config *ConfigError
		if !errors.As(err, &config) {
			t.Fatalf("cluster file %s: want *ConfigError, got %T: %v", filepath.Base(path), err, err)
		}
		requireOp(err, "config", "cluster file")
	}
}
