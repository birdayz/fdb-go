package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// These tests pin the "a launch that produced no job is a FAILED launch"
// contract. Measured incident: four remote pool boxes had an unreadable
// ancestor on the runner work path (/mnt/ci-data at mode 0711, so the runner
// user could traverse but not enumerate it), and the stock actions/runner
// refused to start with "Fail to create and validate runner's work directory"
// — exiting 0 about twelve seconds after launch, before ever contacting the
// job queue. The supervisor could not tell that from a runner that had served
// its job: both were `runner exited err=<nil>` at INFO, and both handed the
// slot straight back. The four slots relaunched into the same failure roughly
// every fifty seconds for hours (505 "runner started" against 8 "job started"
// in one two-hour window, every one of those 8 on the single healthy slot)
// while the supervisor kept advertising five slots of capacity. Real jobs
// queued behind the one slot that worked.
//
// The fix is not to detect that particular filesystem fault: it is that a
// runner which exits without ever reporting JobStarted must bench its slot on
// a growing backoff, exactly as a launch that failed at exec does, and say so
// at WARN.

// exitingTemplateRunner is a template whose run.sh exits 0 immediately without
// ever taking a job — the shape a runner has when it refuses to start.
func exitingTemplateRunner(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "actions-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newNoJobScaler builds a local-only scaler with the watchdogs disabled, so
// what these tests observe is purely the wait/exit path.
func newNoJobScaler(t *testing.T, pool *slotPool) *Scaler {
	t.Helper()
	cfg := &config{
		maxRunners:       pool.size(),
		minRunners:       0,
		sweepFDB:         false,
		grace:            2 * time.Second,
		jobStartTimeout:  0, // no job-start watchdog: the runner exits on its own
		jobTerminalGrace: 0,
	}
	return newScaler(discardLogger(), &fakeScalerClient{}, 1, cfg, pool)
}

// slotReturnedBenched reports whether the given slot has been returned to the
// pool by wait() AND benched there. It is the post-fix form of "the slot
// freed" for a runner that never took a job: the slot is back in the pool (so
// wait() ran to completion) but take() will skip it until its backoff expires.
func slotReturnedBenched(pool *slotPool, index int) func() bool {
	return func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		inFree := false
		for _, s := range pool.free {
			if s.index == index {
				inFree = true
				break
			}
		}
		return inFree && pool.noJobExits[index] > 0 && time.Now().Before(pool.unhealthyUntil[index])
	}
}

// TestNoJobExitBenchesSlot is the core regression: a runner that exits without
// ever reporting JobStarted must NOT make its slot immediately available
// again. Before the fix the slot went straight back via pool.give and the very
// next HandleDesiredRunnerCount relaunched into the identical failure — the
// hot loop that starved the queue.
func TestNoJobExitBenchesSlot(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), exitingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newNoJobScaler(t, pool)

	sl := pool.take()
	if sl == nil {
		t.Fatal("no free slot")
	}
	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The runner exits on its own within moments; wait for wait() to reap it.
	waitFor(t, 10*time.Second, func() bool { return s.count() == 0 })

	// The slot is back in the pool but must be BENCHED: take() skips it until
	// the backoff expires. noJobBackoff(1) is a minute, far longer than this
	// test runs, so a non-nil slot here means the bench never happened.
	if got := pool.take(); got != nil {
		t.Fatalf("slot %d handed out immediately after a runner exited without taking a job; "+
			"it must be benched for noJobBackoff(1)=%s, else the supervisor relaunches into the "+
			"same failure every poll (the churn that starved the queue)", got.index, noJobBackoff(1))
	}

	pool.mu.Lock()
	n := pool.noJobExits[sl.index]
	pool.mu.Unlock()
	if n != 1 {
		t.Fatalf("consecutive no-job exits for slot %d = %d, want 1", sl.index, n)
	}
}

// TestServedJobExitKeepsSlotHealthy is the opposite direction, and the one
// that makes the fix safe: a runner that DID report JobStarted has proven the
// slot works, so its exit must return the slot immediately and clear any
// accumulated backoff. A fix that benched every exit would reduce a healthy
// box to one job per backoff period.
func TestServedJobExitKeepsSlotHealthy(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newNoJobScaler(t, pool)

	sl := pool.take()
	if sl == nil {
		t.Fatal("no free slot")
	}
	// Pre-load failure state, so this test also proves a recovered slot's
	// backoff is actually cleared rather than merely not incremented.
	pool.mu.Lock()
	pool.noJobExits[sl.index] = 3
	pool.mu.Unlock()

	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("launch: %v", err)
	}
	var r *runner
	s.mu.Lock()
	for _, rr := range s.running {
		r = rr
	}
	s.mu.Unlock()
	if r == nil {
		t.Fatal("no running runner tracked")
	}

	// The job starts, then the runner exits the way a finished JIT runner does.
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"},
	}); err != nil {
		t.Fatal(err)
	}
	r.proc.signal(syscall.SIGKILL)
	waitFor(t, 10*time.Second, func() bool { return s.count() == 0 })

	waitFor(t, 5*time.Second, slotFree(pool))

	pool.mu.Lock()
	n, benched := pool.noJobExits[sl.index], pool.unhealthyUntil[sl.index]
	pool.mu.Unlock()
	if n != 0 {
		t.Fatalf("no-job counter for slot %d = %d after it served a job; want 0 (a slot that "+
			"works must not keep an escalated backoff forever)", sl.index, n)
	}
	if !benched.IsZero() {
		t.Fatalf("slot %d still benched until %s after serving a job", sl.index, benched)
	}
}

// TestAdoptedRunnerExitDoesNotBenchSlot pins the restart case. An adopted
// runner's JobStarted was consumed by the incarnation that launched it, so its
// busy flag is false for the whole job. Reading that as "produced no job"
// would bench a perfectly healthy slot every time the supervisor restarted
// under load — turning the adoption fix into a capacity leak.
func TestAdoptedRunnerExitDoesNotBenchSlot(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stray := startStrayRunner(t, pool.all[0].runnerDir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	const name = "bazelscaleset-s0-99-7"
	writeRunnerPID(discardLogger(), pool.all[0].path, pid, name)

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()
	if s.count() != 1 {
		t.Fatalf("running = %d, want 1 adopted runner", s.count())
	}

	// It finishes its (invisible-to-us) job and exits. busy is false throughout.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_, _ = stray.Process.Wait()
	waitFor(t, 10*time.Second, func() bool { return s.count() == 0 })

	waitFor(t, 5*time.Second, slotFree(pool))

	pool.mu.Lock()
	n := pool.noJobExits[pool.all[0].index]
	pool.mu.Unlock()
	if n != 0 {
		t.Fatalf("adopted runner's exit counted as a no-job exit (n=%d); its JobStarted belonged "+
			"to the previous incarnation, so busy==false proves nothing", n)
	}
}

// TestNoJobBackoffEscalatesAndCaps pins the backoff schedule itself. The first
// bench must outlast one idle long-poll (~50s) or the slot is handed back on
// the very next HandleDesiredRunnerCount and the churn continues undiminished.
func TestNoJobBackoffEscalatesAndCaps(t *testing.T) {
	t.Parallel()

	if noJobBackoff(1) <= 50*time.Second {
		t.Fatalf("noJobBackoff(1) = %s; must exceed the ~50s idle long-poll, else a benched slot "+
			"is retaken on the next poll and nothing is throttled", noJobBackoff(1))
	}
	prev := time.Duration(0)
	for n := 1; n <= 12; n++ {
		d := noJobBackoff(n)
		if d < prev {
			t.Fatalf("noJobBackoff(%d) = %s went backwards from %s", n, d, prev)
		}
		if d > noJobBackoffCap {
			t.Fatalf("noJobBackoff(%d) = %s exceeds the cap %s", n, d, noJobBackoffCap)
		}
		prev = d
	}
	if noJobBackoff(12) != noJobBackoffCap {
		t.Fatalf("noJobBackoff(12) = %s, want the cap %s", noJobBackoff(12), noJobBackoffCap)
	}
	// Out-of-range n must not produce a zero (an unthrottled) bench.
	if noJobBackoff(0) != noJobBackoffBase {
		t.Fatalf("noJobBackoff(0) = %s, want %s", noJobBackoff(0), noJobBackoffBase)
	}
}

// TestMarkNoJobExitEscalatesPerSlot pins that the bench grows per slot and
// that a healthy slot's state is independent of a broken one's.
func TestMarkNoJobExitEscalatesPerSlot(t *testing.T) {
	t.Parallel()

	p, err := newSlotPool(t.TempDir(), templateRunner(t), 2, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	broken, healthy := p.all[0], p.all[1]

	for want := 1; want <= 3; want++ {
		got, d := p.markNoJobExit(broken)
		if got != want {
			t.Fatalf("consecutive = %d, want %d", got, want)
		}
		if d != noJobBackoff(want) {
			t.Fatalf("backoff for n=%d = %s, want %s", want, d, noJobBackoff(want))
		}
	}
	// The broken slot is benched; take must hand out only the healthy one.
	for range 3 {
		s := p.take()
		if s == nil {
			t.Fatal("take returned nil while a healthy slot exists")
		}
		if s.index == broken.index {
			t.Fatalf("benched slot %d handed out", s.index)
		}
		p.give(s)
	}
	// Recovery clears everything.
	p.markHealthy(broken)
	p.mu.Lock()
	n, until := p.noJobExits[broken.index], p.unhealthyUntil[broken.index]
	p.mu.Unlock()
	if n != 0 || !until.IsZero() {
		t.Fatalf("markHealthy left state: n=%d until=%s", n, until)
	}
	_ = healthy
}
