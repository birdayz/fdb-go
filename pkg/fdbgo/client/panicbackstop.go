package client

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/internal/diag"
)

// panicBackstop is the per-goroutine recover sink for the client's long-lived /
// background goroutines (RFC-110). In Go an unrecovered panic in ANY goroutine
// aborts the whole process; libfdb_c's network thread, by contrast, wraps every
// task in Net2::run's catch(Error&)/catch(...) → logs SevError "TaskError" and
// keeps running (g_crashOnError is false in the client, flow/Error.cpp:28). A
// blown ASSERT throws internal_error (caught, logged, survived), not abort. So a
// recovered Go panic is the analog of that thrown error: we count it, log it
// (rate-limited — the storm signal is the counter, not the log volume), and the
// caller takes the layer-appropriate NON-fatal action (continue-with-backoff for
// a standing loop, fail-the-batch, fail-the-leg). Go's runtime-fatal throws
// (true OOM, stack overflow, concurrent map write) bypass recover() and still
// abort — matching C++'s deliberate hard-exit classes.
//
// mu guards the log state and consecutive is atomic, so one backstop can be shared by
// many goroutines (the database's range reads) without a lock on the clean path.
type panicBackstop struct {
	mu   sync.Mutex
	name string
	db   *database // may be nil for hand-constructed test databases

	consecutive atomic.Int64 // panics since the last successful iteration; lock-free on the clean path
	lastLog     time.Time    // last time we emitted a log line (rate limiting)
	suppressed  int          // log lines suppressed since lastLog

	// logFn overrides the diagnostics sink (default diag.Recovered). Per-instance
	// rather than a global override so tests stay t.Parallel()-safe.
	logFn func(msg string, attrs ...any)
}

const (
	// panicBackoffStart/Max bound the re-fire rate of a deterministically
	// panicking loop: the backgroundGrvUpdater Backoff (0.01→1.0s, ×2)
	// analog (NativeAPI.actor.cpp:1305-1319, DatabaseContext.h:852-865). A
	// standing loop routes a recovered panic through backoff() so a real bug
	// re-fires at ≤1/s instead of hot-spinning.
	panicBackoffStart = 10 * time.Millisecond
	panicBackoffMax   = 1 * time.Second

	// panicLogInterval rate-limits the per-loop recovered-panic log: the first
	// occurrence logs immediately, then at most once per interval carrying the
	// suppressed count. C++ TraceEvent suppression / monitorNominee's
	// .suppressFor(1.0) (MonitorLeader.actor.cpp:519-523) analog.
	panicLogInterval = time.Minute
)

// recovered handles a recovered panic value r (the result of recover(); nil when
// there was no panic). It returns true iff a panic was recovered. On a panic it
// bumps the recoveredPanics counter + the consecutive-streak high-water gauge,
// and emits a rate-limited ERROR log with the goroutine name, the value, the
// consecutive count, and the stack. The caller continues (loop) / fails the unit
// of work — it must NOT re-panic.
func (pb *panicBackstop) recovered(r any) bool {
	if r == nil {
		return false
	}
	n := pb.consecutive.Add(1)
	if pb.db != nil {
		pb.db.metrics.countRecoveredPanic(int(n))
	}
	pb.logRateLimited(r, n)
	return true
}

// run executes fn under the backstop and returns true iff fn panicked. A panic
// is recovered (counted + rate-limited-logged) and the goroutine survives; a
// clean run resets the consecutive-panic streak. The guarded loop/worker
// continues either way — the panic is never propagated. This is the production
// wrapper for a standing loop's per-iteration work; the (panicked bool) result
// lets the loop apply backoff() before its next iteration.
func (pb *panicBackstop) run(fn func()) (panicked bool) {
	defer func() {
		if pb.recovered(recover()) {
			panicked = true
		}
	}()
	fn()
	pb.reset()
	return false
}

// reset clears the consecutive-panic streak and backoff; call it after a
// successful iteration so the next panic starts fresh (immediate log, base
// backoff).
func (pb *panicBackstop) reset() {
	if pb.consecutive.Load() != 0 { // a store per clean run would bounce the shared line
		pb.consecutive.Store(0)
	}
}

// backoff returns how long the guarded standing loop should wait before its next
// iteration given the current consecutive-panic streak: 0 when healthy, else an
// exponential 10ms→1s (×2 per consecutive panic, capped) — the C++ Backoff
// analog. Bounds a deterministic re-panic to ≤1/s.
func (pb *panicBackstop) backoff() time.Duration {
	n := int(pb.consecutive.Load())
	if n == 0 {
		return 0
	}
	d := panicBackoffStart
	for i := 1; i < n && d < panicBackoffMax; i++ {
		d *= 2
	}
	if d > panicBackoffMax {
		d = panicBackoffMax
	}
	return d
}

func (pb *panicBackstop) logRateLimited(r any, consecutive int64) {
	now := time.Now()
	pb.mu.Lock()
	if consecutive > 1 && now.Sub(pb.lastLog) < panicLogInterval {
		pb.suppressed++
		pb.mu.Unlock()
		return
	}
	suppressed := pb.suppressed
	pb.lastLog, pb.suppressed = now, 0
	pb.mu.Unlock()
	log := pb.logFn
	if log == nil {
		log = diag.Recovered
	}
	log("fdbgo: recovered panic in client goroutine",
		"goroutine", pb.name,
		"err", fmt.Sprintf("%v", r),
		"consecutive", int(consecutive),
		"suppressed_since_last", suppressed,
		"stack", string(debug.Stack()),
	)
}
