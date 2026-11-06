package collector

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBenchmarkBoundaryDisclosesCustomCollectorModel(t *testing.T) {
	t.Parallel()
	cfg := DefaultCollectorBenchConfig()
	cfg.Conversations = -1
	report, err := RunBenchmark(t.Context(), cfg)
	if err == nil {
		t.Fatal("invalid fixture unexpectedly admitted database work")
	}
	for _, disclosure := range []string{"custom", "Collector-model", "not upstream OpenTelemetry Collector"} {
		if !strings.Contains(report.Boundary, disclosure) {
			t.Fatalf("missing measurement boundary %q: %s", disclosure, report.Boundary)
		}
	}
}

func TestBenchmarkDurationAdmissionUsesControlledClock(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	now := start
	r := &benchRun{cfg: CollectorBenchConfig{runtime: &benchRuntime{now: func() time.Time { return now }}}}
	deadline := start.Add(6 * time.Second)
	if c, ok := r.nextConversation(maxBenchConversations, deadline); !ok || c != 0 {
		t.Fatalf("first admission %d/%v", c, ok)
	}
	now = deadline.Add(-time.Nanosecond)
	if c, ok := r.nextConversation(maxBenchConversations, deadline); !ok || c != 1 {
		t.Fatalf("pre-deadline admission %d/%v", c, ok)
	}
	now = deadline
	if _, ok := r.nextConversation(maxBenchConversations, deadline); ok || r.claimed.Load() != 2 {
		t.Fatal("deadline admitted work or consumed a claim")
	}
	now = deadline.Add(time.Hour)
	if c, ok := r.nextConversation(3, time.Time{}); !ok || c != 2 {
		t.Fatalf("fixed-work admission depended on clock %d/%v", c, ok)
	}
	if _, ok := r.nextConversation(3, time.Time{}); ok {
		t.Fatal("fixed-work limit ignored")
	}
}

func TestBenchmarkWindowsUseControlledClockAndCounters(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	now := start
	r := &benchRun{cfg: CollectorBenchConfig{runtime: &benchRuntime{now: func() time.Time { return now }}}, start: start, phase: benchPhaseLoad, x: &TracesExporter{}, sink: &benchSink{}, logs: &benchLogWriter{}, load: newBenchLoad(nil)}
	r.track = newBenchTracker(1, &atomic.Int64{})
	now = start.Add(999 * time.Microsecond)
	r.cut(false)
	if len(r.windows) != 0 {
		t.Fatal("sub-millisecond non-final sample was not merged")
	}
	for i := int64(1); i <= 3; i++ {
		now = start.Add(time.Duration(i) * time.Second)
		r.sink.spans = 10 * i
		r.sink.messages = 20 * i
		r.sink.rawBytes = 100 * i
		r.logs.stats.Logs = i
		if i == 3 {
			r.phase = benchPhaseDrain
		}
		r.cut(true)
	}
	if len(r.windows) != 3 {
		t.Fatalf("samples %d, want 3", len(r.windows))
	}
	for i, w := range r.windows {
		if w.StartNs != int64(i)*int64(time.Second) || w.EndNs != int64(i+1)*int64(time.Second) || w.ExportedSpans != 10 || w.Messages != 20 || w.Logs != 1 {
			t.Fatalf("window %d: %+v", i, w)
		}
	}
	rates := benchRates(r.windows, time.Second)
	if rates.WholeRun.Windows != 3 || rates.WholeRun.Spans != 30 || rates.WholeRun.WallNs != int64(3*time.Second) || rates.SteadyState.Windows != 1 || rates.SteadyState.Spans != 10 || rates.SteadyState.WallNs != int64(time.Second) {
		t.Fatalf("clock-derived rates %+v", rates)
	}
}
