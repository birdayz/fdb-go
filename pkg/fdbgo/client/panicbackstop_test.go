package client

import (
	"testing"
	"time"
)

// logCapture records the rate-limited recovered-panic log lines a backstop emits.
type logCapture struct {
	calls []map[string]any
}

func (c *logCapture) fn(msg string, attrs ...any) {
	m := map[string]any{"_msg": msg}
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok {
			m[k] = attrs[i+1]
		}
	}
	c.calls = append(c.calls, m)
}

// TestPanicBackstop_RecoverCountReset is the core RFC-110 contract: run() recovers
// a panic (the goroutine survives), counts it, and on a clean run resets the
// consecutive streak — the analog of libfdb_c Net2::run catching a thrown
// internal_error and continuing.
func TestPanicBackstop_RecoverCountReset(t *testing.T) {
	t.Parallel()
	var db database
	cap := &logCapture{}
	pb := &panicBackstop{name: "test", db: &db, logFn: cap.fn}

	// A panic is recovered and reported (run returns true, never propagates).
	panicked := pb.run(func() { panic("boom") })
	if !panicked {
		t.Fatal("run should report the recovered panic")
	}
	if got := db.metrics.Snapshot().RecoveredPanics; got != 1 {
		t.Fatalf("RecoveredPanics = %d, want 1", got)
	}
	if pb.consecutive != 1 {
		t.Fatalf("consecutive = %d, want 1", pb.consecutive)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(cap.calls))
	}
	if cap.calls[0]["goroutine"] != "test" || cap.calls[0]["consecutive"] != 1 {
		t.Fatalf("log attrs wrong: %v", cap.calls[0])
	}

	// A clean run resets the consecutive streak (run returns false).
	if pb.run(func() {}) {
		t.Fatal("clean run must not report a panic")
	}
	if pb.consecutive != 0 {
		t.Fatalf("consecutive after clean run = %d, want 0", pb.consecutive)
	}
	// Total stays monotonic; the high-water mark is preserved across the reset.
	snap := db.metrics.Snapshot()
	if snap.RecoveredPanics != 1 || snap.RecoveredPanicsConsecutiveMax != 1 {
		t.Fatalf("after reset: total=%d max=%d, want 1/1", snap.RecoveredPanics, snap.RecoveredPanicsConsecutiveMax)
	}
}

// TestPanicBackstop_NilDB: a hand-constructed backstop with no database must not
// nil-panic when it records (the loops always pass db, but the helper must be safe).
func TestPanicBackstop_NilDB(t *testing.T) {
	t.Parallel()
	pb := &panicBackstop{name: "test", logFn: func(string, ...any) {}}
	if !pb.run(func() { panic("x") }) {
		t.Fatal("run should recover with a nil db")
	}
}

// TestPanicBackstop_Backoff: a deterministic re-panic must be bounded to ≤1/s —
// backoff() grows 10ms→1s (×2 per consecutive) and is 0 when healthy. This is the
// C++ backgroundGrvUpdater Backoff that prevents a hot loop.
func TestPanicBackstop_Backoff(t *testing.T) {
	t.Parallel()
	var db database
	pb := &panicBackstop{name: "test", db: &db, logFn: func(string, ...any) {}}

	if pb.backoff() != 0 {
		t.Fatalf("healthy backoff = %v, want 0", pb.backoff())
	}

	want := []time.Duration{
		panicBackoffStart,      // consecutive 1: 10ms
		panicBackoffStart * 2,  // 2: 20ms
		panicBackoffStart * 4,  // 3: 40ms
		panicBackoffStart * 8,  // 4: 80ms
		panicBackoffStart * 16, // 5: 160ms
	}
	for i, w := range want {
		pb.run(func() { panic("boom") })
		if got := pb.backoff(); got != w {
			t.Fatalf("after %d consecutive panics: backoff = %v, want %v", i+1, got, w)
		}
	}
	// Keep panicking until the cap; backoff must never exceed panicBackoffMax.
	for i := 0; i < 20; i++ {
		pb.run(func() { panic("boom") })
	}
	if got := pb.backoff(); got != panicBackoffMax {
		t.Fatalf("capped backoff = %v, want %v", got, panicBackoffMax)
	}

	// A clean run resets backoff to 0.
	pb.run(func() {})
	if pb.backoff() != 0 {
		t.Fatalf("backoff after recovery = %v, want 0", pb.backoff())
	}
}

// TestPanicBackstop_LogRateLimited: the per-occurrence log is rate-limited (first
// immediate, then suppressed within the interval) so a deterministically broken
// loop can't drown the log — the storm signal is the counter, not the log volume.
func TestPanicBackstop_LogRateLimited(t *testing.T) {
	t.Parallel()
	var db database
	cap := &logCapture{}
	pb := &panicBackstop{name: "test", db: &db, logFn: cap.fn}

	// First panic logs immediately (consecutive==1).
	pb.run(func() { panic("boom") })
	// Many more panics in quick succession are suppressed (no clean run resets
	// the streak, so consecutive>1 and lastLog is recent).
	for i := 0; i < 50; i++ {
		pb.run(func() { panic("boom") })
	}
	if len(cap.calls) != 1 {
		t.Fatalf("rate-limited log emitted %d lines, want 1", len(cap.calls))
	}
	// All 51 panics counted even though only 1 was logged.
	if got := db.metrics.Snapshot().RecoveredPanics; got != 51 {
		t.Fatalf("RecoveredPanics = %d, want 51", got)
	}
	// The high-water consecutive gauge reflects the streak — the alertable signal.
	if got := db.metrics.Snapshot().RecoveredPanicsConsecutiveMax; got != 51 {
		t.Fatalf("RecoveredPanicsConsecutiveMax = %d, want 51", got)
	}

	// Simulating interval elapse: force lastLog into the past, next panic logs
	// again carrying the suppressed count.
	pb.lastLog = pb.lastLog.Add(-2 * panicLogInterval)
	pb.run(func() { panic("boom") })
	if len(cap.calls) != 2 {
		t.Fatalf("after interval, log lines = %d, want 2", len(cap.calls))
	}
	if cap.calls[1]["suppressed_since_last"] != 50 {
		t.Fatalf("suppressed_since_last = %v, want 50", cap.calls[1]["suppressed_since_last"])
	}
}

// TestPanicBackstop_ConsecutiveMaxNotMasked: the high-water gauge must not be
// masked when a DIFFERENT healthy loop resets its own streak — two backstops
// share one Database's metrics; a stuck loop's streak must stay visible.
func TestPanicBackstop_ConsecutiveMaxNotMasked(t *testing.T) {
	t.Parallel()
	var db database
	stuck := &panicBackstop{name: "stuck", db: &db, logFn: func(string, ...any) {}}
	healthy := &panicBackstop{name: "healthy", db: &db, logFn: func(string, ...any) {}}

	for i := 0; i < 5; i++ {
		stuck.run(func() { panic("boom") }) // streak climbs to 5
	}
	healthy.run(func() {}) // a healthy loop's clean iteration (resets only its own streak)

	if got := db.metrics.Snapshot().RecoveredPanicsConsecutiveMax; got != 5 {
		t.Fatalf("RecoveredPanicsConsecutiveMax = %d, want 5 (healthy loop must not mask the stuck one)", got)
	}
}
