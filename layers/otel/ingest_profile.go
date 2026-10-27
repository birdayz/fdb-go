package otel

import (
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/recordlayer"
)

// IngestPhaseLatency is a bounded set of exact observations, not a quantile
// estimate. Attempt phases include failed attempts; commit includes successful
// source Runs only, following StoreTimer's existing EventCommit contract.
type IngestPhaseLatency struct {
	Count      int64 `json:"count"`
	TotalNanos int64 `json:"total_nanos"`
	MinNanos   int64 `json:"min_nanos"`
	MaxNanos   int64 `json:"max_nanos"`
}

const ingestClientMetricsScope = "whole_database_handle_including_initialization_and_ready"

// IngestClientMetrics retains cumulative, undifferenced client snapshots.
// Their scope is the whole database handle, not individual source batches:
// retries, initialization, ready publication and concurrent users can contribute.
// Client latency statistics retain their API units (seconds). Quantiles must
// never be subtracted to describe the interval between snapshots.
type IngestClientMetrics struct {
	Scope  string                       `json:"scope"`
	Before client.ClientMetricsSnapshot `json:"before"`
	After  client.ClientMetricsSnapshot `json:"after"`
}

func (d *ingestRecorder) latencyLocked(name string, elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	if d.out.PhaseLatencies == nil {
		d.out.PhaseLatencies = make(map[string]IngestPhaseLatency)
	}
	p := d.out.PhaseLatencies[name]
	n := elapsed.Nanoseconds()
	if p.Count == 0 || n < p.MinNanos {
		p.MinNanos = n
	}
	if p.Count == 0 || n > p.MaxNanos {
		p.MaxNanos = n
	}
	p.Count++
	p.TotalNanos += n
	d.out.PhaseLatencies[name] = p
}

func (d *ingestRecorder) observePhase(name string, elapsed time.Duration) {
	if d == nil {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	d.mu.Lock()
	d.out.PhaseNanos[name] += elapsed.Nanoseconds()
	d.latencyLocked(name, elapsed)
	d.mu.Unlock()
}

func (d *ingestRecorder) newRunTimer() *recordlayer.StoreTimer {
	if d == nil {
		return nil
	}
	return recordlayer.NewStoreTimer()
}

// Each Run owns its timer, so another worker's commit cannot contaminate a
// commit observation. Merge only after Run returns, when that timer is quiescent.
func (d *ingestRecorder) mergeTimer(timer *recordlayer.StoreTimer) {
	if d == nil || timer == nil {
		return
	}
	d.timer.Add(timer)
	if timer.GetCount(recordlayer.EventCommit) == 1 {
		d.mu.Lock()
		d.latencyLocked("commit", time.Duration(timer.GetTimeNanos(recordlayer.EventCommit)))
		d.mu.Unlock()
	}
}
