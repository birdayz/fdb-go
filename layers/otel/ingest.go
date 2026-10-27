package otel

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// IngestDiagnostics measures source batches; overlapping phases are not
// critical-path time. ClientMetrics has its own whole-handle scope, including
// initialization and ready publication; its snapshots are not per-batch deltas.
type IngestDiagnostics struct {
	Scope                    string                                  `json:"scope"`
	SourceRuns               int64                                   `json:"source_runs"`
	SourceAttempts           int64                                   `json:"source_attempts"`
	SourceReplays            int64                                   `json:"source_replays"`
	SourceTimeouts           int64                                   `json:"source_timeouts"`
	SourceFreshRetries       int64                                   `json:"source_fresh_retries"`
	CommittedBatches         int64                                   `json:"committed_batches"`
	CommittedRows            int64                                   `json:"committed_rows"`
	ApproximateMutationBytes int64                                   `json:"approximate_mutation_bytes"`
	PhaseNanos               map[string]int64                        `json:"phase_nanos"`
	PhaseLatencies           map[string]IngestPhaseLatency           `json:"phase_latencies"`
	ClientMetrics            *IngestClientMetrics                    `json:"client_metrics,omitempty"`
	Timer                    map[string]*recordlayer.CounterSnapshot `json:"timer"`
}

type ingestRecorder struct {
	mu    sync.Mutex
	out   IngestDiagnostics
	timer *recordlayer.StoreTimer
}

func newIngestRecorder(enabled bool) *ingestRecorder {
	if !enabled {
		return nil
	}
	return &ingestRecorder{out: IngestDiagnostics{Scope: "source_batches_only", PhaseNanos: make(map[string]int64)}, timer: recordlayer.NewStoreTimer()}
}

func (d *ingestRecorder) phase(name string, started time.Time) {
	if d == nil {
		return
	}
	d.observePhase(name, time.Since(started))
}

func (d *ingestRecorder) run(attempts int64, started time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.out.SourceRuns++
	d.out.SourceAttempts += attempts
	if attempts > 1 {
		d.out.SourceReplays += attempts - 1
	}
	elapsed := time.Since(started)
	d.out.PhaseNanos["run"] += elapsed.Nanoseconds()
	d.latencyLocked("run", elapsed)
	d.mu.Unlock()
}

func (d *ingestRecorder) size(n int64) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.out.ApproximateMutationBytes += n
	d.mu.Unlock()
}

// snapshot is called only after all source workers have joined; no timer or
// phase writer remains. Do not replace the database's external AttemptObserver.
func (d *ingestRecorder) snapshot(rows, batches int64) *IngestDiagnostics {
	if d == nil {
		return nil
	}
	d.out.CommittedRows, d.out.CommittedBatches = rows, batches
	d.out.Timer = d.timer.Snapshot()
	return &d.out
}

type preparedBatch struct {
	first, next  int
	messages     []proto.Message
	logicalBytes int64
	estimate     int
}

// prepareIngestBatch runs outside retry closures. Each bounded, disjoint batch
// owns immutable messages across internal attempts and every fresh source Run.
func prepareIngestBatch(ctx context.Context, c Config, lo int) (preparedBatch, error) {
	batch := preparedBatch{first: lo, next: lo}
	if lo < 0 || lo >= c.Count {
		return batch, fail("config", "ingest batch", "invalid source offset")
	}
	for batch.next < c.Count && batch.next-lo < c.Batch {
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		s := Generate(c, batch.next)
		cost := 2048
		for _, a := range s.Attributes {
			cost += 2 * (len(a.Key) + len(a.Value) + 256)
		}
		if cost > 300000 {
			return batch, fail("admission", "ingest batch", "single span exceeds conservative 300KB transaction estimate")
		}
		if len(batch.messages) > 0 && batch.estimate+cost > 300000 {
			break
		}
		m, err := toRecord(s)
		if err != nil {
			return batch, err
		}
		wire, err := json.Marshal(s)
		if err != nil {
			return batch, err
		}
		batch.messages = append(batch.messages, m)
		batch.logicalBytes += int64(len(wire))
		batch.estimate += cost
		batch.next++
	}
	return batch, nil
}

// ingestEvent exposes lifecycle barriers to package-local regression probes.
type ingestEvent struct {
	kind     string
	batch    preparedBatch
	queued   int
	capacity int
}

func (b *FDBBackend) observeIngest(kind string, batch preparedBatch, queue chan preparedBatch) {
	if b.ingestProbe != nil {
		b.ingestProbe(ingestEvent{kind: kind, batch: batch, queued: len(queue), capacity: cap(queue)})
	}
}

func (b *FDBBackend) prepareBatch(ctx context.Context, lo int) (preparedBatch, error) {
	batch, err := prepareIngestBatch(ctx, b.cfg, lo)
	if err == nil {
		b.observeIngest("prepared", batch, nil)
	}
	return batch, err
}

func (b *FDBBackend) saveIngestBatchRun(ctx context.Context, batch preparedBatch, d *ingestRecorder) error {
	started := time.Now()
	runTimer := d.newRunTimer()
	var attempts int64
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		attempts++
		b.observeIngest("attempt", batch, nil)
		if d != nil {
			rc.SetTimer(runTimer)
		}
		phase := time.Now()
		err := b.readManifest(rc, false)
		d.phase("manifest", phase)
		if err != nil {
			return nil, err
		}
		phase = time.Now()
		store, err := b.open(rc)
		d.phase("open", phase)
		if err != nil {
			return nil, err
		}
		// Preserve the original save + guard elapsed bound, independently of
		// the optional instrumentation and the transaction's 1500ms timeout.
		saveStarted := time.Now()
		switch b.cfg.SaveMode {
		case "batch":
			_, err = store.SaveRecordBatch(batch.messages)
		case "pipelined":
			_, err = store.SaveRecordsPipelined(batch.messages, recordlayer.RecordExistenceCheckNone)
		default:
			for _, m := range batch.messages {
				if _, err = store.SaveRecord(m); err != nil {
					break
				}
			}
		}
		d.phase("save", saveStarted)
		if err != nil {
			return nil, err
		}
		phase = time.Now()
		n, err := rc.Transaction().GetApproximateSize().Get()
		d.phase("guard", phase)
		if err != nil {
			return nil, err
		}
		elapsed := time.Since(saveStarted)
		d.size(n)
		if n > 500000 || elapsed > time.Second {
			return nil, failf("budget", "ingest transaction", "transaction guard exceeded: bytes=%d elapsed=%s", n, elapsed)
		}
		return nil, nil
	})
	d.run(attempts, started)
	d.mergeTimer(runTimer)
	return err
}

func (b *FDBBackend) Ingest(ctx context.Context) (stats Statistics, retErr error) {
	if b.cfg.Diagnostics {
		before := b.raw.Metrics()
		defer func() {
			if stats.Diagnostics != nil {
				stats.Diagnostics.ClientMetrics = &IngestClientMetrics{Scope: ingestClientMetricsScope, Before: before, After: b.raw.Metrics()}
			}
		}()
	}
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureRecordTimeout(rc); err != nil {
			return nil, err
		}
		key := b.root.Sub("owner").FDBKey()
		v, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		if len(v) != 0 {
			return nil, fail("ownership", "ingest", "run already exists; choose a fresh namespace or explicitly drop owned run")
		}
		wire, _ := json.Marshal(manifest(b.cfg, "loading"))
		rc.Transaction().Set(key, wire)
		_, err = b.open(rc)
		return nil, err
	})
	if err != nil {
		return Statistics{}, err
	}

	d := newIngestRecorder(b.cfg.Diagnostics)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// At most Workers queued batches, Workers running batches, and one batch
	// being prepared. No corpus-sized job/result collection is retained.
	jobs := make(chan preparedBatch, b.cfg.Workers)
	var workers sync.WaitGroup
	var firstError sync.Once
	var firstErr error
	failWork := func(err error) {
		firstError.Do(func() {
			firstErr = err
			cancel()
		})
	}
	var progress sync.Mutex
	var rows, batches, logical int64
	for i := 0; i < b.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				if workCtx.Err() != nil {
					return
				}
				// Every Run owns its own record context, transaction, and store;
				// only the concurrency-safe database and timer are shared.
				if err := b.saveIngestBatch(workCtx, batch, d); err != nil {
					failWork(err)
					return
				}
				progress.Lock()
				rows += int64(batch.next - batch.first)
				batches++
				logical += batch.logicalBytes
				progress.Unlock()
			}
		}()
	}

produce:
	for lo := 0; lo < b.cfg.Count; {
		if err := workCtx.Err(); err != nil {
			failWork(err)
			break
		}
		started := time.Now()
		batch, err := b.prepareBatch(workCtx, lo)
		d.phase("preparation", started)
		if err != nil {
			failWork(err)
			break
		}
		select {
		case <-workCtx.Done():
			failWork(workCtx.Err())
			break produce
		case jobs <- batch:
		default:
			b.observeIngest("backpressure", batch, jobs)
			select {
			case <-workCtx.Done():
				failWork(workCtx.Err())
				break produce
			case jobs <- batch:
			}
		}
		b.observeIngest("admitted", batch, jobs)
		lo = batch.next
	}
	close(jobs)
	b.observeIngest("producer_done", preparedBatch{}, jobs)
	b.observeIngest("before_join", preparedBatch{}, jobs)
	workers.Wait()
	stats = Statistics{Rows: rows, Attributes: rows * int64(b.cfg.Attributes), LogicalPayloadBytes: logical, Diagnostics: d.snapshot(rows, batches), Details: map[string]string{
		"ingest":                 "real Record Layer saves + synchronous custom covering secondary index",
		"payload_bytes":          "fixture JSON logical bytes, not FDB serialized/storage bytes",
		"committed_batches":      strconv.FormatInt(batches, 10),
		"save_mode":              b.cfg.SaveMode,
		"workers":                strconv.Itoa(b.cfg.Workers),
		"source_run_limit":       strconv.Itoa(maxSourceRuns),
		"source_timeout_backoff": "25ms exponential, capped at 250ms; only fresh source Runs on 1031",
	}}
	if firstErr != nil {
		return stats, firstErr
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if rows != int64(b.cfg.Count) {
		return stats, fail("operation", "ingest", "incomplete source population; refusing ready publication")
	}
	_, err = b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := b.readManifest(rc, false); err != nil {
			return nil, err
		}
		wire, _ := json.Marshal(manifest(b.cfg, "ready"))
		rc.Transaction().Set(b.root.Sub("owner").FDBKey(), wire)
		return nil, nil
	})
	return stats, err
}
