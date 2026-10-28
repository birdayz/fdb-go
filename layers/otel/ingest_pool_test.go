package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

type otelPoolProbe struct {
	mu              sync.Mutex
	prepared        map[int]preparedBatch
	attempts        map[int]int
	admitted        []int
	backpressure    ingestEvent
	hasBackpressure bool
	producerDone    bool
	beforeJoin      bool
	err             error
	changed         chan struct{}
	join            chan struct{}
}

func newOtelPoolProbe() *otelPoolProbe {
	return &otelPoolProbe{prepared: map[int]preparedBatch{}, attempts: map[int]int{}, changed: make(chan struct{}, 1), join: make(chan struct{})}
}

func (p *otelPoolProbe) notify() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *otelPoolProbe) observe(event ingestEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bad := func(format string, args ...any) {
		if p.err == nil {
			p.err = fmt.Errorf(format, args...)
		}
	}
	if event.queued < 0 || event.queued > event.capacity {
		bad("invalid actual queue depth/capacity %d/%d", event.queued, event.capacity)
	}
	switch event.kind {
	case "prepared":
		if _, duplicate := p.prepared[event.batch.first]; duplicate {
			bad("batch %d prepared more than once", event.batch.first)
		}
		p.prepared[event.batch.first] = event.batch
	case "attempt":
		prepared, exists := p.prepared[event.batch.first]
		if !exists || prepared.next != event.batch.next || len(prepared.messages) != len(event.batch.messages) || prepared.logicalBytes != event.batch.logicalBytes || prepared.estimate != event.batch.estimate {
			bad("attempt did not use its prepared batch: first=%d", event.batch.first)
		} else if len(prepared.messages) > 0 && &prepared.messages[0] != &event.batch.messages[0] {
			bad("attempt replaced prepared message storage: first=%d", event.batch.first)
		}
		p.attempts[event.batch.first]++
	case "preparation_started", "run_returned":
	case "admitted":
		p.admitted = append(p.admitted, event.batch.first)
	case "backpressure":
		p.backpressure, p.hasBackpressure = event, true
	case "producer_done":
		p.producerDone = true
	case "before_join":
		if !p.producerDone {
			bad("join boundary preceded producer close")
		}
		if p.beforeJoin {
			bad("duplicate join boundary")
		} else {
			close(p.join)
		}
		p.beforeJoin = true
	default:
		bad("unknown ingest lifecycle event %q", event.kind)
	}
	p.notify()
}

type otelPoolResult struct {
	stats                       Statistics
	err                         error
	premature                   bool
	metricsBefore, metricsAfter client.ClientMetricsSnapshot
}

type otelPoolGateTransactor struct {
	fdb.Database
	parent                 context.Context
	release                <-chan struct{}
	probe                  *otelPoolProbe
	mu                     sync.Mutex
	active, maximum, calls int
}

func (d *otelPoolGateTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *otelPoolGateTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ctx == d.parent {
		return d.Database.TransactCtx(ctx, fn)
	}
	// Source Runs use the derived work context. Hold them before creating a
	// transaction, so this pool barrier cannot age the transaction/save guard.
	d.mu.Lock()
	d.active++
	d.calls++
	if d.active > d.maximum {
		d.maximum = d.active
	}
	d.mu.Unlock()
	d.probe.notify()
	defer func() { d.mu.Lock(); d.active--; d.mu.Unlock(); d.probe.notify() }()
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.Database.TransactCtx(ctx, fn)
}

func awaitOtelPoolSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", name, ctx.Err())
	case <-time.After(10 * time.Second):
		t.Fatalf("no positive %s handshake", name)
	}
}

func awaitOtelPoolBound(t *testing.T, ctx context.Context, p *otelPoolProbe, workers int, active func() int) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		p.mu.Lock()
		prepared, admitted, event, hasBackpressure, err := len(p.prepared), len(p.admitted), p.backpressure, p.hasBackpressure, p.err
		p.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if active() > workers {
			t.Fatalf("actual source concurrency exceeded %d workers", workers)
		}
		if active() == workers && hasBackpressure && event.batch.first == 2*workers && prepared == 2*workers+1 && admitted == 2*workers && event.queued == workers && event.capacity == workers {
			return
		}
		select {
		case <-p.changed:
		case <-ctx.Done():
			t.Fatalf("pool never reached bounded backpressure: prepared=%d admitted=%d event=%+v: %v", prepared, admitted, event, ctx.Err())
		case <-timer.C:
			t.Fatalf("pool never reached bounded backpressure: prepared=%d admitted=%d event=%+v active=%d", prepared, admitted, event, active())
		}
	}
}

func testOtelIngestPoolBounds(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, workers := range []int{1, 2, 8} {
		t.Run("workers_"+strconv.Itoa(workers), func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Order = 2*workers+3, 1, 4, 1, workers, order
			b := openOtelFixture(t, ctx, cluster, c, false)
			runCtx, cancel := context.WithCancel(ctx)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			p := newOtelPoolProbe()
			gate := &otelPoolGateTransactor{Database: b.raw, parent: runCtx, release: release, probe: p}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(gate, b.raw)
			b.ingestProbe = p.observe
			result := make(chan otelPoolResult, 1)
			done := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				unblock()
				select {
				case <-done:
					b.db, b.ingestProbe = original, nil
				case <-time.After(10 * time.Second):
					t.Error("source pool did not leave released cleanup gates")
				}
			})
			go func() {
				defer close(done)
				stats, err := b.Ingest(runCtx)
				result <- otelPoolResult{stats: stats, err: err}
			}()
			active := func() int { gate.mu.Lock(); defer gate.mu.Unlock(); return gate.active }
			awaitOtelPoolBound(t, ctx, p, workers, active)
			gate.mu.Lock()
			actualActive, maximum, calls := gate.active, gate.maximum, gate.calls
			gate.mu.Unlock()
			p.mu.Lock()
			prepared, admitted, event := len(p.prepared), len(p.admitted), p.backpressure
			attempts := len(p.attempts)
			p.mu.Unlock()
			if actualActive != workers || maximum != workers || calls != workers || prepared != 2*workers+1 || admitted != 2*workers || event.queued != workers || event.capacity != workers || attempts != 0 {
				t.Fatalf("actual bounded population active=%d max=%d calls=%d prepared=%d admitted=%d queue=%d/%d attempts=%d", actualActive, maximum, calls, prepared, admitted, event.queued, event.capacity, attempts)
			}
			unblock()
			awaitOtelPoolSignal(t, ctx, done, "completed source pool")
			out := <-result
			if out.err != nil || out.stats.Rows != int64(c.Count) || out.stats.Attributes != int64(c.Count*c.Attributes) {
				t.Fatalf("released pool: stats=%+v error=%v", out.stats, out.err)
			}
			if out.stats.Diagnostics != nil {
				t.Fatalf("disabled pool diagnostics fabricated profile fields: %+v", out.stats.Diagnostics)
			}
			gate.mu.Lock()
			actualActive, maximum = gate.active, gate.maximum
			gate.mu.Unlock()
			p.mu.Lock()
			if p.err != nil || len(p.prepared) != c.Count || len(p.admitted) != c.Count || len(p.attempts) != c.Count || !p.producerDone || !p.beforeJoin {
				t.Errorf("pool lifecycle counts prepared=%d admitted=%d attempted=%d producerDone=%t beforeJoin=%t error=%v", len(p.prepared), len(p.admitted), len(p.attempts), p.producerDone, p.beforeJoin, p.err)
			}
			for ordinal := 0; ordinal < c.Count; ordinal++ {
				batch, exists := p.prepared[ordinal]
				if !exists || batch.first != ordinal || batch.next != ordinal+1 || len(batch.messages) != 1 || ordinal >= len(p.admitted) || p.admitted[ordinal] != ordinal || p.attempts[ordinal] < 1 {
					t.Errorf("noncontiguous/disjoint source batch %d: %+v attempts=%d", ordinal, batch, p.attempts[ordinal])
					continue
				}
				got, err := decodeRecord(batch.messages[0])
				if err != nil || !equalSpan(got, Generate(b.cfg, ordinal)) {
					t.Errorf("prepared source %d mutated: %+v %v", ordinal, got, err)
				}
			}
			p.mu.Unlock()
			if actualActive != 0 || maximum != workers {
				t.Fatalf("pool exit active=%d max=%d, want 0/%d", actualActive, maximum, workers)
			}
			b.db, b.ingestProbe = original, nil
			stats, err := b.Audit(ctx)
			if err != nil || stats.Rows != int64(c.Count) || stats.Attributes != int64(c.Count*c.Attributes) {
				t.Fatalf("bounded pool full audit: %+v %v", stats, err)
			}
			sources := make([]Span, c.Count)
			for i := range sources {
				sources[i] = Generate(b.cfg, i)
			}
			assertOtelCommittedPostings(t, ctx, b, sources)
			if err := AuditQueries(ctx, b, b.cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type otelPoolGuardFuture struct {
	fdb.FutureInt64
	once sync.Once
	get  func() (int64, error)
	n    int64
	err  error
}

func (f *otelPoolGuardFuture) Get() (int64, error) {
	f.once.Do(func() { f.n, f.err = f.get() })
	return f.n, f.err
}

type otelPoolGuardTransaction struct {
	fdb.WritableTransaction
	once   sync.Once
	future fdb.FutureInt64
	get    func() (int64, error)
}

func (tx *otelPoolGuardTransaction) GetApproximateSize() fdb.FutureInt64 {
	tx.once.Do(func() {
		tx.future = &otelPoolGuardFuture{FutureInt64: tx.WritableTransaction.GetApproximateSize(), get: tx.get}
	})
	return tx.future
}

type otelPoolFailureTransactor struct {
	fdb.Database
	parent                                     context.Context
	cause                                      error
	probe                                      *otelPoolProbe
	failRelease, heldRelease                   <-chan struct{}
	failEntered, heldEntered, canceled, exited chan struct{}
	failOnce, heldOnce, cancelOnce, exitOnce   sync.Once
	calls                                      atomic.Int64
	active                                     atomic.Int64
}

func (d *otelPoolFailureTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *otelPoolFailureTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ctx == d.parent {
		return d.Database.TransactCtx(ctx, fn)
	}
	role := d.calls.Add(1)
	d.active.Add(1)
	d.probe.notify()
	defer func() {
		d.active.Add(-1)
		if role == 2 {
			d.exitOnce.Do(func() { close(d.exited) })
		}
		d.probe.notify()
	}()
	if role > 2 {
		return nil, fmt.Errorf("unexpected source attempt %d after controlled worker failure", role)
	}
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		get := func() (int64, error) {
			if role == 1 {
				d.failOnce.Do(func() { close(d.failEntered) })
				<-d.failRelease
				return 0, d.cause
			}
			d.heldOnce.Do(func() { close(d.heldEntered) })
			<-ctx.Done()
			d.cancelOnce.Do(func() { close(d.canceled) })
			// Cancellation is observed positively, but cannot release this
			// worker. Only the independent test-owned barrier can do that.
			<-d.heldRelease
			return 0, ctx.Err()
		}
		return fn(&otelPoolGuardTransaction{WritableTransaction: tx, get: get})
	})
}

func assertOtelPoolRollback(t *testing.T, ctx context.Context, b *FDBBackend) {
	t.Helper()
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		wire, err := tx.Get(b.root.Sub("owner").FDBKey()).Get()
		if err != nil {
			return nil, err
		}
		var owner Manifest
		if err := json.Unmarshal(wire, &owner); err != nil {
			return nil, err
		}
		if owner != manifest(b.cfg, "loading") {
			return nil, fmt.Errorf("worker error published owner %+v", owner)
		}
		spaces := []subspace.Subspace{b.store.Sub(recordlayer.RecordKey), otelSecondary(b)}
		for _, name := range []string{spanIndexName, timeIndexName} {
			index := b.md.GetIndex(name)
			if index == nil {
				return nil, fmt.Errorf("rollback fixture missing maintained index %s", name)
			}
			spaces = append(spaces, b.store.Sub(recordlayer.IndexKey, index.SubspaceTupleKey()))
		}
		for _, space := range spaces {
			rows, err := tx.GetRange(space, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) != 0 {
				return nil, fmt.Errorf("failed source transaction left keys under %x", space.Bytes())
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testOtelWorkerErrorAndJoin(t *testing.T, ctx context.Context, cluster, order, mode string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Diagnostics = 16, 1, 4, 1, 2, true
	c.Order, c.SaveMode = order, mode
	b := openOtelFixture(t, ctx, cluster, c, false)
	seedOtelProfileClientMetrics(t, ctx, b)
	runCtx, cancel := context.WithCancel(ctx)
	failRelease, heldRelease := make(chan struct{}), make(chan struct{})
	releaseFailure := sync.OnceFunc(func() { close(failRelease) })
	releaseHeld := sync.OnceFunc(func() { close(heldRelease) })
	p := newOtelPoolProbe()
	cause := errors.New("controlled non-context source guard failure")
	gate := &otelPoolFailureTransactor{Database: b.raw, parent: runCtx, cause: cause, probe: p, failRelease: failRelease, heldRelease: heldRelease, failEntered: make(chan struct{}), heldEntered: make(chan struct{}), canceled: make(chan struct{}), exited: make(chan struct{})}
	original := b.db
	b.db = recordlayer.NewFDBDatabaseWithTransactor(gate, b.raw)
	b.ingestProbe = p.observe
	result := make(chan otelPoolResult, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseFailure()
		releaseHeld()
		select {
		case <-done:
			b.db, b.ingestProbe = original, nil
		case <-time.After(10 * time.Second):
			t.Error("failed pool did not leave released cleanup barriers")
		}
	})
	go func() {
		defer close(done)
		before := b.raw.Metrics()
		stats, err := b.Ingest(runCtx)
		after := b.raw.Metrics()
		premature := false
		select {
		case <-gate.exited:
		default:
			premature = true
		}
		result <- otelPoolResult{stats: stats, err: err, premature: premature, metricsBefore: before, metricsAfter: after}
	}()
	awaitOtelPoolSignal(t, ctx, gate.failEntered, "failing source guard entered")
	awaitOtelPoolSignal(t, ctx, gate.heldEntered, "sibling source guard entered")
	awaitOtelPoolBound(t, ctx, p, 2, func() int { return int(gate.active.Load()) })
	if ctx.Err() != nil || runCtx.Err() != nil {
		t.Fatalf("parent canceled before worker fault: parent=%v ingest=%v", ctx.Err(), runCtx.Err())
	}
	releaseFailure()
	awaitOtelPoolSignal(t, ctx, gate.canceled, "sibling observed internal cancellation")
	awaitOtelPoolSignal(t, ctx, p.join, "producer closed and reached join boundary")
	if ctx.Err() != nil || runCtx.Err() != nil {
		t.Fatalf("worker error canceled live parent: parent=%v ingest=%v", ctx.Err(), runCtx.Err())
	}
	p.mu.Lock()
	prepared, admitted, producerDone, beforeJoin, probeErr := len(p.prepared), len(p.admitted), p.producerDone, p.beforeJoin, p.err
	p.mu.Unlock()
	if prepared != 5 || admitted != 4 || !producerDone || !beforeJoin || probeErr != nil || gate.calls.Load() != 2 {
		t.Fatalf("worker failure did not stop bounded producer: prepared=%d admitted=%d producerDone=%t beforeJoin=%t calls=%d probe=%v", prepared, admitted, producerDone, beforeJoin, gate.calls.Load(), probeErr)
	}
	select {
	case <-gate.exited:
		t.Fatal("held sibling exited without independent release")
	default:
	}
	// This check is anchored at a positive join boundary with a known live,
	// canceled-but-held worker. It is not a quiet database-write snapshot.
	select {
	case out := <-result:
		t.Fatalf("Ingest returned while sibling guard remained held: %+v", out)
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		t.Fatalf("parent canceled while checking held join: %v", ctx.Err())
	}
	assertOtelPoolRollback(t, ctx, b)
	releaseHeld()
	awaitOtelPoolSignal(t, ctx, gate.exited, "held sibling source transactor exited")
	awaitOtelPoolSignal(t, ctx, done, "Ingest returned after worker exit")
	out := <-result
	assertOtelProfileClientMetrics(t, out.stats, out.metricsBefore, out.metricsAfter)
	if out.premature || !errors.Is(out.err, cause) || errors.Is(out.err, context.Canceled) || ctx.Err() != nil || runCtx.Err() != nil {
		t.Fatalf("worker join/cause preservation: premature=%t error=%v parent=%v ingest=%v", out.premature, out.err, ctx.Err(), runCtx.Err())
	}
	if out.stats.Rows != 0 || out.stats.Attributes != 0 || out.stats.LogicalPayloadBytes != 0 || out.stats.Details["committed_batches"] != "0" {
		t.Fatalf("failed uncommitted batches reported progress: %+v", out.stats)
	}
	if d := out.stats.Diagnostics; d == nil || d.SourceRuns != 2 || d.SourceAttempts != 2 || d.SourceReplays != 0 || d.CommittedRows != 0 || d.CommittedBatches != 0 {
		t.Fatalf("worker failure diagnostics population: %+v", d)
	}
	p.mu.Lock()
	finalPrepared, finalAdmitted, finalProbeErr := len(p.prepared), len(p.admitted), p.err
	var actualAttempts int64
	for _, count := range p.attempts {
		actualAttempts += int64(count)
	}
	p.mu.Unlock()
	if finalPrepared != prepared || finalAdmitted != admitted || finalProbeErr != nil || gate.active.Load() != 0 || gate.calls.Load() != 2 {
		t.Fatalf("post-error producer/worker activity: prepared=%d/%d admitted=%d/%d active=%d calls=%d probe=%v", finalPrepared, prepared, finalAdmitted, admitted, gate.active.Load(), gate.calls.Load(), finalProbeErr)
	}
	assertOtelProfileMoments(t, out.stats, map[string]int64{
		"preparation": int64(finalPrepared), "manifest": actualAttempts,
		"open": actualAttempts, "save": actualAttempts, "guard": actualAttempts,
		"run": gate.calls.Load(), "timeout_backoff": 0, "commit": 0,
	})
	b.db, b.ingestProbe = original, nil
	assertOtelPoolRollback(t, ctx, b)
}
