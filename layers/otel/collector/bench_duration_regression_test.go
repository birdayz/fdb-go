package collector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunBenchmarkDurationStopsAtControlledDeadlineAndDrains(t *testing.T) {
	t.Parallel()
	cluster := benchClusterFile(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cfg := benchUnitConfig()
	cfg.ClusterFile, cfg.Run = cluster, freshCollectorRun(t)
	cfg.Conversations, cfg.TurnsPerConversation, cfg.Producers = 1, 3, 1
	cfg.QueryWorkers, cfg.QueryIterations = 1, 2
	cfg.Duration, cfg.ReportInterval, cfg.Warmup = 6*time.Second, time.Second, time.Nanosecond
	cfg.BatchMinSize = 1
	start := time.Unix(1000, 0)
	var elapsed atomic.Int64
	var began atomic.Bool
	cfg.runtime = &benchRuntime{now: func() time.Time { return start.Add(time.Duration(elapsed.Add(1))) }, beforeAdmission: func(_ context.Context, _ *benchRun, c int) error {
		if !began.Load() {
			t.Error("producer ran before ingest-start callback")
		}
		if c == 3 {
			elapsed.Store(int64(cfg.Duration))
		}
		return nil
	}}
	ctx = WithBenchmarkIngestStart(ctx, func() { began.Store(true) })
	r, err := RunBenchmark(ctx, cfg)
	if err != nil || r.Error != "" || r.Failures != 0 {
		t.Fatalf("duration run: %v report=%+v", err, r)
	}
	if !began.Load() || r.Corpus.Conversations != 4 || r.Corpus.Spans != 12 || r.Corpus.Logs != 4 {
		t.Fatalf("deadline population %+v", r.Corpus)
	}
	if s := r.Ingest.Collector; s.AcceptedRequests != 4 || s.ExportedSpans != 12 || s.FailedSpans != 0 || s.QueuedSpans != 0 {
		t.Fatalf("drain %+v", s)
	}
	if r.Ingest.Logs.Logs != 4 || r.Ingest.FailedLogCalls != 0 || r.Watermark.Watermark != 3 || r.Watermark.Tracked != 0 || r.Watermark.Anomalies != 0 {
		t.Fatalf("logs=%+v watermark=%+v", r.Ingest, r.Watermark)
	}
	var spans, logs int64
	for _, w := range r.Series {
		spans += w.ExportedSpans
		logs += w.Logs
	}
	if spans != 12 || logs != 4 || len(r.Series) == 0 || r.Series[len(r.Series)-1].Phase != benchPhaseDrain {
		t.Fatalf("windows spans=%d logs=%d series=%+v", spans, logs, r.Series)
	}
}
