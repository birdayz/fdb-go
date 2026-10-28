package otel

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

func TestIngestPhaseLatencyMoments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		samples []time.Duration
		want    IngestPhaseLatency
	}{
		{"zero", []time.Duration{0}, IngestPhaseLatency{Count: 1}},
		{"zero_first", []time.Duration{0, 7, 3}, IngestPhaseLatency{Count: 3, TotalNanos: 10, MinNanos: 0, MaxNanos: 7}},
		{"zero_later", []time.Duration{9, 0, 5}, IngestPhaseLatency{Count: 3, TotalNanos: 14, MinNanos: 0, MaxNanos: 9}},
		{"positive", []time.Duration{7, 3, 11}, IngestPhaseLatency{Count: 3, TotalNanos: 21, MinNanos: 3, MaxNanos: 11}},
		{"negative_clamped", []time.Duration{4, -7, 2}, IngestPhaseLatency{Count: 3, TotalNanos: 6, MinNanos: 0, MaxNanos: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newIngestRecorder(true)
			for _, sample := range tc.samples {
				d.observePhase("save", sample)
			}
			d.observePhase("manifest", 23*time.Nanosecond)
			got := d.snapshot(0, 0)
			if got.PhaseLatencies["save"] != tc.want || got.PhaseNanos["save"] != tc.want.TotalNanos {
				t.Fatalf("phase samples=%v moments=%+v legacy=%d want=%+v", tc.samples, got.PhaseLatencies["save"], got.PhaseNanos["save"], tc.want)
			}
			if len(got.PhaseLatencies) != 2 || got.PhaseLatencies["manifest"] != (IngestPhaseLatency{Count: 1, TotalNanos: 23, MinNanos: 23, MaxNanos: 23}) || got.PhaseNanos["manifest"] != 23 {
				t.Fatalf("phase observations contaminated another name: %+v", got)
			}
		})
	}
}

func TestIngestProfilingNilAndNoSamples(t *testing.T) {
	t.Parallel()
	d := newIngestRecorder(false)
	if d != nil || d.newRunTimer() != nil {
		t.Fatal("disabled diagnostics allocated a recorder or Run timer")
	}
	d.observePhase("save", 0)
	d.phase("save", time.Now())
	d.run(3, time.Now())
	d.mergeTimer(nil)
	d.mergeTimer(recordlayer.NewStoreTimer())
	if d.snapshot(1, 1) != nil {
		t.Fatal("nil recorder published diagnostics")
	}
	enabled := newIngestRecorder(true)
	enabled.mergeTimer(nil)
	enabled.mergeTimer(recordlayer.NewStoreTimer())
	got := enabled.snapshot(0, 0)
	if got == nil || len(got.PhaseLatencies) != 0 || len(got.PhaseNanos) != 0 || len(got.Timer) != 0 || got.ClientMetrics != nil {
		t.Fatalf("no-sample recorder fabricated profiling data: %+v", got)
	}
}

func TestIngestPhaseAndRunObserveSameLegacyDurations(t *testing.T) {
	t.Parallel()
	d := newIngestRecorder(true)
	d.observePhase("open", 0)
	d.phase("open", time.Now().Add(-time.Millisecond))
	var runTotal, runMin, runMax int64
	for i, attempts := range []int64{0, 1, 3} {
		previous := d.out.PhaseNanos["run"]
		d.run(attempts, time.Now().Add(-time.Duration(i+1)*time.Millisecond))
		sample := d.out.PhaseNanos["run"] - previous
		if sample <= 0 {
			t.Fatalf("Run observation is not positive: %d", sample)
		}
		runTotal += sample
		if i == 0 || sample < runMin {
			runMin = sample
		}
		if i == 0 || sample > runMax {
			runMax = sample
		}
	}
	got := d.snapshot(0, 0)
	open := got.PhaseLatencies["open"]
	if open.Count != 2 || open.MinNanos != 0 || open.MaxNanos <= 0 || open.TotalNanos != got.PhaseNanos["open"] || open.TotalNanos != open.MaxNanos {
		t.Fatalf("phase(start) missed moments or changed legacy duration: %+v legacy=%d", open, got.PhaseNanos["open"])
	}
	want := IngestPhaseLatency{Count: 3, TotalNanos: runTotal, MinNanos: runMin, MaxNanos: runMax}
	if got.PhaseLatencies["run"] != want || got.PhaseNanos["run"] != runTotal || got.SourceRuns != 3 || got.SourceAttempts != 4 || got.SourceReplays != 2 {
		t.Fatalf("Run moments/counters changed their population: %+v wantMoments=%+v", got, want)
	}
}

func TestIngestMergePrivateRunTimers(t *testing.T) {
	t.Parallel()
	d := newIngestRecorder(true)
	first, second := d.newRunTimer(), d.newRunTimer()
	if first == nil || second == nil || first == second || first == d.timer || second == d.timer {
		t.Fatal("source Runs did not receive independent private timers")
	}
	d.observePhase("manifest", 5*time.Nanosecond)
	first.Record(recordlayer.EventSaveRecord, 7)
	first.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 32)
	d.mergeTimer(first)
	if _, exists := d.out.PhaseLatencies["commit"]; exists {
		t.Fatal("timer without EventCommit fabricated a commit sample")
	}
	for i, nanos := range []int64{0, 17, 9} {
		timer := second
		if i != 0 {
			timer = d.newRunTimer()
		}
		timer.Record(recordlayer.EventCommit, nanos)
		if i == 0 {
			timer.IncrementBy(recordlayer.CountSaveRecordKeyBytes, 24)
		}
		d.mergeTimer(timer)
	}
	wantCommit := IngestPhaseLatency{Count: 3, TotalNanos: 26, MinNanos: 0, MaxNanos: 17}
	if got := d.out.PhaseLatencies["commit"]; got != wantCommit {
		t.Fatalf("per-Run commit moments=%+v want=%+v", got, wantCommit)
	}
	// A non-singleton timer is not a valid one-Run commit sample. Its native
	// counters must still merge without inventing a duration/minimum/maximum.
	multiple := d.newRunTimer()
	multiple.Record(recordlayer.EventCommit, 100)
	multiple.Record(recordlayer.EventCommit, 200)
	multiple.IncrementBy(recordlayer.CountReads, 3)
	d.mergeTimer(multiple)
	got := d.snapshot(0, 0)
	if got.PhaseLatencies["commit"] != wantCommit || len(got.PhaseLatencies) != 2 {
		t.Fatalf("non-singleton/noncommit native events contaminated moments: %v", got.PhaseLatencies)
	}
	if _, exists := got.PhaseNanos["commit"]; exists {
		t.Fatalf("commit was incorrectly added to legacy overlapping phase totals: %v", got.PhaseNanos)
	}
	if len(got.PhaseNanos) != 1 || got.PhaseNanos["manifest"] != 5 {
		t.Fatalf("timer merge changed legacy phase samples: %v", got.PhaseNanos)
	}
	commit := got.Timer[recordlayer.EventCommit.Name]
	if commit == nil || commit.Event != recordlayer.EventCommit || commit.Count != 5 || commit.CumulativeValue != 326 {
		t.Fatalf("native commit counter merge=%+v", commit)
	}
	size := got.Timer[recordlayer.CountSaveRecordKeyBytes.Name]
	if size == nil || size.Event.Kind != recordlayer.KindSize || size.Count != 56 || size.CumulativeValue != 0 {
		t.Fatalf("native size units were changed during merge: %+v", size)
	}
	save := got.Timer[recordlayer.EventSaveRecord.Name]
	if save == nil || save.Event.Kind != recordlayer.KindTimed || save.Count != 1 || save.CumulativeValue != 7 {
		t.Fatalf("native timed event was lost/converted: %+v", save)
	}
	reads := got.Timer[recordlayer.CountReads.Name]
	if reads == nil || reads.Event.Kind != recordlayer.KindCount || reads.Count != 3 || reads.CumulativeValue != 0 {
		t.Fatalf("native count units were changed during merge: %+v", reads)
	}
}

func TestIngestProfilingJSONKeepsUndifferencedClientSnapshots(t *testing.T) {
	t.Parallel()
	phase := IngestPhaseLatency{Count: 3, TotalNanos: 10, MinNanos: 0, MaxNanos: 7}
	wire, err := json.Marshal(phase)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"count":3,"total_nanos":10,"min_nanos":0,"max_nanos":7}` {
		t.Fatalf("phase moments JSON changed names/zero semantics: %s", wire)
	}
	before := client.ClientMetricsSnapshot{
		TransactionsCommitStarted: 101, TransactionsCommitCompleted: 103, TransactionReadVersionsCompleted: 107,
		GRVLatency:  client.LatencyStats{Count: 11, Sum: 1.125, Mean: 0.125, Median: 0.0625, P90: 0.5, P99: 0.875, Min: 0.001, Max: 1},
		ReadLatency: client.LatencyStats{Count: 17, P99: 7.75},
	}
	after := client.ClientMetricsSnapshot{
		TransactionsCommitStarted: 211, TransactionsCommitCompleted: 223, TransactionReadVersionsCompleted: 227,
		GRVLatency:  client.LatencyStats{Count: 1201, Sum: 2.25, Mean: 0.0625, Median: 0.03125, P90: 0.0625, P99: 0.125, Min: 0.0001, Max: 1},
		ReadLatency: client.LatencyStats{Count: 29, P99: 5.25},
	}
	stats := Statistics{Diagnostics: &IngestDiagnostics{Scope: "source_batches_only", PhaseLatencies: map[string]IngestPhaseLatency{"save": phase}, ClientMetrics: &IngestClientMetrics{Scope: "whole_database_handle_including_initialization_and_ready", Before: before, After: after}}}
	wire, err = json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Statistics
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Diagnostics == nil || decoded.Diagnostics.Scope != "source_batches_only" || decoded.Diagnostics.PhaseLatencies["save"] != phase || decoded.Diagnostics.ClientMetrics == nil {
		t.Fatalf("profiling JSON omitted scoped observations: %s", wire)
	}
	metrics := decoded.Diagnostics.ClientMetrics
	if metrics.Scope != "whole_database_handle_including_initialization_and_ready" || !reflect.DeepEqual(metrics.Before, before) || !reflect.DeepEqual(metrics.After, after) {
		t.Fatalf("client snapshots were differenced/narrowed: %+v", metrics)
	}
	if metrics.Before.GRVLatency.P99 != 0.875 || metrics.After.GRVLatency.P99 != 0.125 || metrics.Before.ReadLatency.P99 != 7.75 || metrics.After.ReadLatency.P99 != 5.25 {
		t.Fatalf("client P99 snapshots became invalid quantile deltas: %+v", metrics)
	}
	var envelope struct {
		Diagnostics struct {
			ClientMetrics map[string]json.RawMessage `json:"client_metrics"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Diagnostics.ClientMetrics) != 3 || envelope.Diagnostics.ClientMetrics["scope"] == nil || envelope.Diagnostics.ClientMetrics["before"] == nil || envelope.Diagnostics.ClientMetrics["after"] == nil {
		t.Fatalf("client snapshot JSON does not expose scope/before/after only: %s", wire)
	}
	wire, err = json.Marshal(Statistics{})
	if err != nil {
		t.Fatal(err)
	}
	var disabled map[string]json.RawMessage
	if err := json.Unmarshal(wire, &disabled); err != nil {
		t.Fatal(err)
	}
	if _, exists := disabled["diagnostics"]; exists {
		t.Fatalf("disabled diagnostics serialized empty profiling snapshots: %s", wire)
	}
}

type ingestProfileTransactor struct {
	fdb.Database
	entries atomic.Int64
}

func (d *ingestProfileTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *ingestProfileTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { d.entries.Add(1); return fn(tx) })
}

func testOtelIngestProfiling(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	t.Run("overlapping_workers", func(t *testing.T) { testOtelIngestProfilingOverlap(t, ctx, cluster, order) })
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Diagnostics, c.Order = 3, 1, 4, 1, 1, enabled, order
			c.SaveMode = "pipelined"
			b := openOtelFixture(t, ctx, cluster, c, false)
			// Seed one prior read outside the measured interval: before must
			// retain real nonzero lifetime counters/quantiles, not a zero delta.
			_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
				_, err := tx.Get(b.root.Sub("profile_probe").FDBKey()).Get()
				return nil, err
			})
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &ingestProfileTransactor{Database: b.raw}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			defer func() { b.db = original }()
			externalTimer := recordlayer.NewStoreTimer()
			b.db.SetTimer(externalTimer)
			var observerMu sync.Mutex
			var calls []recordlayer.AttemptCall
			b.db.SetAttemptObserver(func(call recordlayer.AttemptCall, _ error) {
				observerMu.Lock()
				calls = append(calls, call)
				observerMu.Unlock()
			})
			before := b.raw.Metrics()
			stats, err := b.Ingest(ctx)
			after := b.raw.Metrics()
			b.db.SetAttemptObserver(nil)
			if err != nil || stats.Rows != 3 || stats.Attributes != 12 {
				t.Fatalf("profiled ingestion population: stats=%+v error=%v", stats, err)
			}
			observerMu.Lock()
			observed := append([]recordlayer.AttemptCall(nil), calls...)
			observerMu.Unlock()
			if len(observed) < 5 || wrapped.entries.Load() < 5 || b.db.Timer() != externalTimer {
				t.Fatalf("profiling replaced caller timer/observer or missed init/ready: observed=%d entries=%d timerSame=%t", len(observed), wrapped.entries.Load(), b.db.Timer() == externalTimer)
			}
			// This fixture has one serial worker, no GRV-cache opt-in and no
			// concurrent handle users. Snapshot reads must not start extra GRVs.
			grvs := after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted
			if grvs < 5 || grvs > wrapped.entries.Load() {
				t.Fatalf("client snapshots caused extra/missing read-version transactions: GRV completions=%d actual body entries=%d", grvs, wrapped.entries.Load())
			}
			if !enabled {
				if stats.Diagnostics != nil || externalTimer.GetCount(recordlayer.EventCommit) != 5 {
					t.Fatalf("disabled profiling attached diagnostics/private source timers: diagnostics=%+v caller commits=%d", stats.Diagnostics, externalTimer.GetCount(recordlayer.EventCommit))
				}
			} else {
				d := stats.Diagnostics
				if d == nil || d.ClientMetrics == nil || d.Scope != "source_batches_only" || d.CommittedRows != 3 || d.CommittedBatches != 3 {
					t.Fatalf("enabled profile omitted scoped population: %+v", d)
				}
				if externalTimer.GetCount(recordlayer.EventCommit) != 2 {
					t.Fatalf("source Run timers contaminated caller's initialization/ready timer: count=%d", externalTimer.GetCount(recordlayer.EventCommit))
				}
				for _, phase := range []string{"preparation", "manifest", "open", "save", "guard", "run", "commit"} {
					moment, exists := d.PhaseLatencies[phase]
					if !exists || moment.Count < 1 || moment.TotalNanos <= 0 || moment.MinNanos <= 0 || moment.MaxNanos < moment.MinNanos || moment.TotalNanos < moment.MaxNanos {
						t.Fatalf("phase %s lacks positive bounded moments: %+v", phase, moment)
					}
					if phase != "commit" && moment.TotalNanos != d.PhaseNanos[phase] {
						t.Fatalf("phase %s moments changed legacy total: moment=%+v legacy=%d", phase, moment, d.PhaseNanos[phase])
					}
				}
				if d.PhaseLatencies["preparation"].Count != 3 || d.PhaseLatencies["run"].Count != d.SourceRuns || d.PhaseLatencies["manifest"].Count != d.SourceAttempts {
					t.Fatalf("profiling phase sample populations: %+v", d)
				}
				commit := d.Timer[recordlayer.EventCommit.Name]
				moment := d.PhaseLatencies["commit"]
				if commit == nil || commit.Event.Kind != recordlayer.KindTimed || commit.Count != 3 || moment.Count != commit.Count || moment.TotalNanos != commit.CumulativeValue {
					t.Fatalf("commit moments do not match existing successful-Run timer: moments=%+v timer=%+v", moment, commit)
				}
				if _, exists := d.PhaseNanos["commit"]; exists {
					t.Fatalf("commit leaked into legacy phase totals: %v", d.PhaseNanos)
				}
				metrics := d.ClientMetrics
				if metrics.Scope != "whole_database_handle_including_initialization_and_ready" || metrics.Before.TransactionsCommitCompleted != before.TransactionsCommitCompleted || metrics.After.TransactionsCommitCompleted != after.TransactionsCommitCompleted || metrics.After.TransactionsCommitCompleted-metrics.Before.TransactionsCommitCompleted < 5 {
					t.Fatalf("client snapshots exclude initialization/ready or became deltas: profile=%+v outsideBefore=%+v outsideAfter=%+v", metrics, before, after)
				}
				if metrics.Before.GRVLatency.Count != before.GRVLatency.Count || metrics.After.GRVLatency.Count != after.GRVLatency.Count || metrics.Before.GRVLatency.Count < 1 || metrics.After.GRVLatency.Count <= metrics.Before.GRVLatency.Count || metrics.Before.GRVLatency.P99 != before.GRVLatency.P99 || metrics.After.GRVLatency.P99 != after.GRVLatency.P99 || metrics.After.GRVLatency.P99 <= 0 {
					t.Fatalf("client GRV samples/P99 were omitted, fabricated or differenced: profile=%+v before=%+v after=%+v", metrics, before.GRVLatency, after.GRVLatency)
				}
			}
			b.db = original
			if audited, err := b.Audit(ctx); err != nil || audited.Rows != 3 {
				t.Fatalf("profiled source audit: %+v %v", audited, err)
			}
		})
	}
}

func testOtelIngestProfilingOverlap(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Diagnostics, c.Order = 2, 1, 4, 1, 2, true, order
	c.SaveMode = "pipelined"
	b := openOtelFixture(t, ctx, cluster, c, false)
	runCtx, cancel := context.WithCancel(ctx)
	attemptRelease, returnRelease := make(chan struct{}), make(chan struct{})
	releaseAttempts := sync.OnceFunc(func() { close(attemptRelease) })
	releaseReturns := sync.OnceFunc(func() { close(returnRelease) })
	bothEntered, bothReturned, probeFailed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, returnedOnce sync.Once
	var mu sync.Mutex
	var probeError error
	firstTimers := map[int]*recordlayer.StoreTimer{}
	timerOwners := map[*recordlayer.StoreTimer]int{}
	returned := map[*recordlayer.StoreTimer]bool{}
	successfulBatches := map[int]bool{}
	type runSample struct {
		batch        int
		timer        *recordlayer.StoreTimer
		count, nanos int64
	}
	var samples []runSample
	var attempted int64
	var heldAttempts, heldReturns atomic.Int64
	failLocked := func(format string, args ...any) {
		if probeError == nil {
			probeError = fmt.Errorf(format, args...)
			close(probeFailed)
		}
	}
	originalProbe := b.ingestProbe
	b.ingestProbe = func(event ingestEvent) {
		if event.kind != "attempt" && event.kind != "run_returned" {
			return
		}
		mu.Lock()
		if event.batch.first < 0 || event.batch.first > 1 || event.batch.next != event.batch.first+1 || len(event.batch.messages) != 1 {
			failLocked("invalid overlap fixture batch: first=%d next=%d messages=%d", event.batch.first, event.batch.next, len(event.batch.messages))
		}
		if event.timer == nil {
			failLocked("%s did not expose an actual private Run timer", event.kind)
		}
		waitAttempt, waitReturn := false, false
		if event.kind == "attempt" {
			attempted++
			if returned[event.timer] {
				failLocked("new source Run reused returned timer %p", event.timer)
			}
			if owner, exists := timerOwners[event.timer]; exists && owner != event.batch.first {
				failLocked("two overlapping batches shared timer %p: owners=%d/%d", event.timer, owner, event.batch.first)
			} else {
				timerOwners[event.timer] = event.batch.first
			}
			if _, exists := firstTimers[event.batch.first]; !exists {
				firstTimers[event.batch.first] = event.timer
				heldAttempts.Add(1)
				waitAttempt = true
				if len(firstTimers) == 2 {
					enteredOnce.Do(func() { close(bothEntered) })
				}
			}
		} else {
			if owner, exists := timerOwners[event.timer]; !exists || owner != event.batch.first {
				failLocked("returned Run changed its actual timer identity: batch=%d timer=%p owner=%d present=%t", event.batch.first, event.timer, owner, exists)
			}
			if returned[event.timer] {
				failLocked("timer %p returned from more than one source Run", event.timer)
			}
			returned[event.timer] = true
			if event.timer != nil {
				count, nanos := event.timer.GetCount(recordlayer.EventCommit), event.timer.GetTimeNanos(recordlayer.EventCommit)
				if count < 0 || count > 1 || nanos < 0 || count == 0 && nanos != 0 {
					failLocked("non-private commit sample before merge: batch=%d timer=%p count=%d nanos=%d", event.batch.first, event.timer, count, nanos)
				}
				samples = append(samples, runSample{batch: event.batch.first, timer: event.timer, count: count, nanos: nanos})
				if count == 1 {
					if successfulBatches[event.batch.first] {
						failLocked("batch %d produced more than one successful Run", event.batch.first)
					}
					successfulBatches[event.batch.first] = true
					heldReturns.Add(1)
					waitReturn = true
					if len(successfulBatches) == 2 {
						returnedOnce.Do(func() { close(bothReturned) })
					}
				}
			}
		}
		mu.Unlock()
		if waitAttempt {
			defer heldAttempts.Add(-1)
			select {
			case <-attemptRelease:
			case <-runCtx.Done():
			}
		}
		if waitReturn {
			defer heldReturns.Add(-1)
			select {
			case <-returnRelease:
			case <-runCtx.Done():
			}
		}
	}
	externalTimer := recordlayer.NewStoreTimer()
	originalTimer := b.db.Timer()
	b.db.SetTimer(externalTimer)
	var observerMu sync.Mutex
	var calls []recordlayer.AttemptCall
	b.db.SetAttemptObserver(func(call recordlayer.AttemptCall, _ error) {
		observerMu.Lock()
		calls = append(calls, call)
		observerMu.Unlock()
	})
	type outcome struct {
		stats Statistics
		err   error
	}
	result, done := make(chan outcome, 1), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseAttempts()
		releaseReturns()
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		select {
		case <-done:
			b.ingestProbe = originalProbe
			b.db.SetAttemptObserver(nil)
			b.db.SetTimer(originalTimer)
		case <-cleanup.Done():
			t.Error("overlap Ingest failed to join after releasing all profiling barriers")
		}
	})
	go func() { defer close(done); stats, err := b.Ingest(runCtx); result <- outcome{stats: stats, err: err} }()
	await := func(signal <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-signal:
		case <-probeFailed:
			mu.Lock()
			err := probeError
			mu.Unlock()
			t.Fatalf("overlap probe before %s: %v", name, err)
		case <-done:
			out := <-result
			t.Fatalf("Ingest returned before positive %s handshake: stats=%+v error=%v", name, out.stats, out.err)
		case <-runCtx.Done():
			t.Fatalf("waiting for overlap %s: %v", name, runCtx.Err())
		}
	}
	await(bothEntered, "two simultaneous source attempts")
	mu.Lock()
	first, second, earlyReturns, earlyError := firstTimers[0], firstTimers[1], len(samples), probeError
	mu.Unlock()
	if first == nil || second == nil || first == second || heldAttempts.Load() != 2 || earlyReturns != 0 || earlyError != nil {
		t.Fatalf("source Runs did not overlap with separate actual timers: timers=%p/%p held=%d earlyReturns=%d error=%v", first, second, heldAttempts.Load(), earlyReturns, earlyError)
	}
	releaseAttempts()
	await(bothReturned, "two completed source Runs paused before merge")
	if heldReturns.Load() != 2 {
		t.Fatalf("source return barrier did not hold both merges: %d", heldReturns.Load())
	}
	select {
	case <-done:
		t.Fatal("Ingest returned while both source Run merges were held")
	default:
	}
	releaseReturns()
	select {
	case <-done:
	case <-runCtx.Done():
		t.Fatalf("overlap Ingest failed after releasing merges: %v", runCtx.Err())
	}
	out := <-result
	if out.err != nil || out.stats.Rows != 2 || out.stats.Attributes != 8 {
		t.Fatalf("overlapping profiled ingestion failed: stats=%+v error=%v", out.stats, out.err)
	}
	mu.Lock()
	observedSamples, actualTimers, actualReturns, actualAttempts, finalError := append([]runSample(nil), samples...), len(timerOwners), len(returned), attempted, probeError
	mu.Unlock()
	if finalError != nil || heldAttempts.Load() != 0 || heldReturns.Load() != 0 {
		t.Fatalf("overlap timers/barriers inconsistent after join: error=%v attemptHeld=%d returnHeld=%d", finalError, heldAttempts.Load(), heldReturns.Load())
	}
	d := out.stats.Diagnostics
	if d == nil || d.CommittedRows != 2 || d.CommittedBatches != 2 || int64(actualTimers) != d.SourceRuns || int64(actualReturns) != d.SourceRuns || int64(len(observedSamples)) != d.SourceRuns || actualAttempts != d.SourceAttempts {
		t.Fatalf("actual private Run identities/populations differ from one joined recorder: timers=%d returns=%d samples=%d attempts=%d diagnostics=%+v", actualTimers, actualReturns, len(observedSamples), actualAttempts, d)
	}
	var expected IngestPhaseLatency
	for _, sample := range observedSamples {
		if sample.count == 0 {
			continue
		}
		if sample.nanos <= 0 {
			t.Fatalf("real successful Run commit sample is not positive: %+v", sample)
		}
		if expected.Count == 0 || sample.nanos < expected.MinNanos {
			expected.MinNanos = sample.nanos
		}
		if expected.Count == 0 || sample.nanos > expected.MaxNanos {
			expected.MaxNanos = sample.nanos
		}
		expected.Count++
		expected.TotalNanos += sample.nanos
	}
	if expected.Count != 2 || d.PhaseLatencies["commit"] != expected {
		t.Fatalf("overlapping merge lost/cross-assigned commit moments: got=%+v exact premerge samples=%+v want=%+v", d.PhaseLatencies["commit"], observedSamples, expected)
	}
	commit := d.Timer[recordlayer.EventCommit.Name]
	if commit == nil || commit.Event != recordlayer.EventCommit || commit.Count != expected.Count || commit.CumulativeValue != expected.TotalNanos {
		t.Fatalf("joined native aggregate disagrees with actual private Run samples: native=%+v samples=%+v", commit, expected)
	}
	if _, exists := d.PhaseNanos["commit"]; exists {
		t.Fatalf("overlap commit samples changed legacy phase totals: %v", d.PhaseNanos)
	}
	observerMu.Lock()
	observedCalls := append([]recordlayer.AttemptCall(nil), calls...)
	observerMu.Unlock()
	ids := map[uint64]bool{}
	for _, call := range observedCalls {
		if call.CallID == 0 || call.Owner != "run" || call.Route != recordlayer.RouteAttempt {
			t.Fatalf("overlap replaced external observer identity: %+v", observedCalls)
		}
		ids[call.CallID] = true
	}
	if int64(len(ids)) != d.SourceRuns+2 || b.db.Timer() != externalTimer || externalTimer.GetCount(recordlayer.EventCommit) != 2 {
		t.Fatalf("overlap profiling stole caller observer/timer population: RunIDs=%d sourceRuns=%d external commits=%d timerSame=%t", len(ids), d.SourceRuns, externalTimer.GetCount(recordlayer.EventCommit), b.db.Timer() == externalTimer)
	}
	if audited, err := b.Audit(ctx); err != nil || audited.Rows != 2 {
		t.Fatalf("overlapping profiled sources failed final audit: %+v %v", audited, err)
	}
}
