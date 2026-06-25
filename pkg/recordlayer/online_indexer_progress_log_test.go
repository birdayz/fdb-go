package recordlayer

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// captureHandler is a minimal slog.Handler that records emitted entries and
// gates on a configurable level, so a test can assert both the emitted event and
// the Enabled(level) short-circuit.
type captureHandler struct {
	mu      sync.Mutex
	level   slog.Level
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func attrMap(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

// TestShouldLogBuildProgress pins the Java IndexingBase.shouldLogBuildProgress
// throttle: interval<0 never logs, interval==0 logs every range, interval>0
// throttles to once per interval, and the throttle clock advances ONLY when it
// returns true.
func TestShouldLogBuildProgress(t *testing.T) {
	t.Parallel()

	t.Run("negative interval never logs and never advances the clock", func(t *testing.T) {
		t.Parallel()
		oi := &OnlineIndexer{progressLogIntervalMillis: -1}
		if oi.shouldLogBuildProgress() {
			t.Fatal("interval<0 must return false")
		}
		if oi.timeOfLastProgressLogMillis != 0 {
			t.Fatalf("clock must not advance on false, got %d", oi.timeOfLastProgressLogMillis)
		}
	})

	t.Run("zero interval logs every range", func(t *testing.T) {
		t.Parallel()
		oi := &OnlineIndexer{progressLogIntervalMillis: 0}
		if !oi.shouldLogBuildProgress() {
			t.Fatal("first call with interval==0 must return true")
		}
		if oi.timeOfLastProgressLogMillis == 0 {
			t.Fatal("clock must advance when true")
		}
		if !oi.shouldLogBuildProgress() {
			t.Fatal("interval==0 must return true every call")
		}
	})

	t.Run("positive interval throttles until enough time elapsed", func(t *testing.T) {
		t.Parallel()
		now := time.Now().UnixMilli()
		// Just logged "now" with a 10s interval → not enough elapsed → false, clock frozen.
		oi := &OnlineIndexer{progressLogIntervalMillis: 10_000, timeOfLastProgressLogMillis: now}
		if oi.shouldLogBuildProgress() {
			t.Fatal("must throttle when interval has not elapsed")
		}
		if oi.timeOfLastProgressLogMillis != now {
			t.Fatal("clock must not advance while throttled")
		}
		// Last log was 500ms ago with a 100ms interval → enough elapsed → true.
		oi2 := &OnlineIndexer{progressLogIntervalMillis: 100, timeOfLastProgressLogMillis: now - 500}
		if !oi2.shouldLogBuildProgress() {
			t.Fatal("must log once the interval has elapsed")
		}
		if oi2.timeOfLastProgressLogMillis < now {
			t.Fatal("clock must advance to ~now after a successful log")
		}
	})
}

// TestMaybeLogBuildProgress_Emits pins the "Indexer: Built Range" INFO event and
// its key/values.
func TestMaybeLogBuildProgress_Emits(t *testing.T) {
	t.Parallel()
	h := &captureHandler{level: slog.LevelInfo}
	oi := &OnlineIndexer{
		progressLogIntervalMillis: 0, // log every range
		logger:                    slog.New(h),
		limit:                     100,
		targetIndexes:             []*Index{{Name: "idx_a"}, {Name: "idx_b"}},
	}

	oi.maybeLogBuildProgress(context.Background(), 500, 50, 7)

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Message != "Indexer: Built Range" {
		t.Fatalf("message = %q", recs[0].Message)
	}
	if recs[0].Level != slog.LevelInfo {
		t.Fatalf("level = %v, want INFO", recs[0].Level)
	}
	m := attrMap(recs[0])
	if got := m["index"]; got != "idx_a,idx_b" {
		t.Errorf("index = %v, want idx_a,idx_b", got)
	}
	if got := m["records_scanned"]; got != int64(500) {
		t.Errorf("records_scanned = %v, want 500", got)
	}
	if got := m["range_records"]; got != int64(50) {
		t.Errorf("range_records = %v, want 50", got)
	}
	if got := m["limit"]; got != int64(100) {
		t.Errorf("limit = %v, want 100", got)
	}
	if got := m["delay_ms"]; got != int64(7) {
		t.Errorf("delay_ms = %v, want 7", got)
	}
}

// TestMaybeLogBuildProgress_DisabledLevelShortCircuits pins the Java
// `LOGGER.isInfoEnabled() && shouldLogBuildProgress()` short-circuit: when INFO
// is disabled, NOTHING is emitted AND the throttle clock does not advance (so a
// later re-enabled logger isn't silently throttled by a phantom prior log).
func TestMaybeLogBuildProgress_DisabledLevelShortCircuits(t *testing.T) {
	t.Parallel()
	h := &captureHandler{level: slog.LevelWarn} // INFO disabled
	oi := &OnlineIndexer{
		progressLogIntervalMillis: 0, // would log every range if enabled
		logger:                    slog.New(h),
		targetIndexes:             []*Index{{Name: "idx_a"}},
	}

	oi.maybeLogBuildProgress(context.Background(), 1, 1, 0)

	if recs := h.snapshot(); len(recs) != 0 {
		t.Fatalf("INFO disabled must emit nothing, got %d records", len(recs))
	}
	if oi.timeOfLastProgressLogMillis != 0 {
		t.Fatal("INFO disabled must NOT advance the throttle clock (Java && short-circuit)")
	}
}
