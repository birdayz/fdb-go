package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// These tests pin the zombie-job watchdog: a GitHub-side terminal job whose
// Runner.Worker keeps running must be killed and its slot reclaimed after the
// grace period. Measured incident: GitHub marked a run failed at 05:54Z while
// the local worker kept the ONLY slot occupied until it was manually killed at
// ~10:19 — 4+ hours of a wedged CI with the supervisor reporting a busy slot.

// hangingTemplateRunner is a template whose run.sh blocks forever, modelling a
// wedged Runner.Worker.
func hangingTemplateRunner(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "actions-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// killGroupAndWait tears a launched runner's process group down and does not
// return until the group is GONE.
//
// Requesting death is not observing it: kill(2) returns once the signal is
// queued, not once the target has died and dropped its file descriptors. A
// cleanup that only signals therefore hands the rest of the run a process that
// is still holding everything it inherited. Measured on this suite: after
// signal(15); signal(9) the group was still alive on 40 of 40 launches, and the
// stragglers (run.sh and its sleep) were caught at test-binary exit still
// holding the binary's stdout pipe — which is what makes cmd/go sit out its 60s
// WaitDelay and print "Test I/O incomplete" after every test has passed.
//
// ESRCH is reachable here because the scaler's own wait() goroutine reaps the
// group leader; without that reap the leader would linger as a zombie, which
// still answers kill(pgid, 0) with nil. Measured time to ESRCH: ~1ms worst case.
func killGroupAndWait(t *testing.T, proc runnerProc) {
	t.Helper()
	proc.signal(syscall.SIGTERM)
	proc.signal(syscall.SIGKILL)
	pgid := proc.pid()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			// Two very different faults reach this line and they need opposite
			// investigations, so name which one happened rather than asserting the
			// livelier-sounding of the two. A zombie leader inherits nothing and
			// holds no descriptors: it means the reap this helper depends on stopped
			// happening, not that a process is still running with our stdout.
			t.Errorf("process group %d still present 10s after SIGKILL: %s", pgid, groupLingerReason(pgid))
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// groupLingerReason explains why a signalled process group is still present,
// distinguishing the two faults that keep kill(-pgid, 0) from returning ESRCH.
//
// The leader's state is field 3 of /proc/<pid>/stat, read after the trailing
// ')' of field 2 because comm is parenthesised and may itself contain spaces
// or a ')'.
func groupLingerReason(pgid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pgid), "stat"))
	if err != nil {
		return "the leader is gone but some group member is not; a member still running holds " +
			"every descriptor it inherited from this binary and stalls cmd/go for its full WaitDelay"
	}
	state := ""
	if i := strings.LastIndexByte(string(raw), ')'); i >= 0 {
		if f := strings.Fields(string(raw)[i+1:]); len(f) > 0 {
			state = f[0]
		}
	}
	if state == "Z" {
		return "the leader is a ZOMBIE, so nothing is holding this binary's descriptors and " +
			"cmd/go is not at risk. What broke is the reap this helper relies on: the scaler's " +
			"wait() goroutine must call Wait on the leader, and a zombie answers kill(pgid, 0) " +
			"with nil forever, so this poll can never reach ESRCH. Look at wait(), not at a survivor"
	}
	return fmt.Sprintf("the leader is in state %q — still running, holding every descriptor it "+
		"inherited from this binary and stalling cmd/go for its full WaitDelay", state)
}

// silenceChildOutput points a scaler's locally launched runners at /dev/null.
//
// Every test scaler gets this, and it is the structural half of the fix that
// killGroupAndWait is the behavioural half of. A cleanup can only protect the
// tests that remember to call it, and it cannot protect a path that leaves via
// t.Fatal before the runner is tracked; sending child output somewhere this
// process does not depend on means a straggler is a leaked process rather than
// a corrupted stream and a stalled cmd/go.
func silenceChildOutput(t *testing.T, s *Scaler) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devnull.Close() }) // exec dup'd it into the child already
	s.childOut, s.childErr = devnull, devnull
}

// newWatchdogScaler builds a local-only scaler with a fast terminal watchdog.
func newWatchdogScaler(t *testing.T, client scalerClient, pool *slotPool) *Scaler {
	t.Helper()
	cfg := &config{
		maxRunners:       1,
		minRunners:       0,
		sweepFDB:         false,
		grace:            2 * time.Second,
		jobStartTimeout:  time.Minute,
		jobTerminalGrace: 150 * time.Millisecond,
	}
	s := newScaler(discardLogger(), client, 1, cfg, pool)
	silenceChildOutput(t, s)
	s.terminalPoll = 40 * time.Millisecond
	return s
}

// launchOne launches a runner into the pool's single slot and returns it.
func launchOne(t *testing.T, s *Scaler, pool *slotPool) *runner {
	t.Helper()
	sl := pool.take()
	if sl == nil {
		t.Fatal("no free slot")
	}
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
	t.Cleanup(func() { killGroupAndWait(t, r.proc) })
	return r
}

func slotFree(pool *slotPool) func() bool {
	return func() bool {
		s := pool.take()
		if s == nil {
			return false
		}
		pool.give(s)
		return true
	}
}

// TestWatchdogKillsZombieAfterJobCompleted pins the message-session path: a
// JobCompleted arrives (the job is terminal on GitHub) but the runner process
// never exits — after the grace the watchdog kills it and the slot frees.
func TestWatchdogKillsZombieAfterJobCompleted(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// Server-side record still present: ONLY the JobCompleted message signals
	// terminality, isolating this path from the polling path.
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	r := launchOne(t, s, pool)

	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-zombie-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{
		RunnerName:     r.name,
		Result:         "failed",
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-zombie-1"},
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, func() bool { return !alive(r.proc.pid()) })
	waitFor(t, 10*time.Second, slotFree(pool))
}

// TestWatchdogKillsZombieWhenRunnerRecordGone pins the polling fallback: no
// JobCompleted message ever arrives, but GitHub has deleted the JIT runner's
// server-side record (which it does once the job is terminal). The watchdog
// must notice via GetRunnerByName and reclaim the slot.
func TestWatchdogKillsZombieWhenRunnerRecordGone(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: false}, pool) // record gone
	r := launchOne(t, s, pool)

	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-zombie-2"},
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, func() bool { return !alive(r.proc.pid()) })
	waitFor(t, 10*time.Second, slotFree(pool))
}

// TestWatchdogSparesLiveJob pins the negative: while the job is NOT terminal
// (record present, no JobCompleted), the watchdog must not touch the runner —
// long-running healthy jobs are the normal case.
func TestWatchdogSparesLiveJob(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	r := launchOne(t, s, pool)

	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-live"},
	}); err != nil {
		t.Fatal(err)
	}

	// Several grace periods and poll ticks pass; the runner must survive.
	time.Sleep(600 * time.Millisecond)
	if !alive(r.proc.pid()) {
		t.Fatal("watchdog killed a runner whose job is still live on GitHub")
	}
	if slotFree(pool)() {
		t.Fatal("slot freed while the job is still running")
	}
}

// TestWatchdogDisabledByZeroGrace pins the off switch: --job-terminal-grace=0
// must not arm the watchdog even for a terminal job.
func TestWatchdogDisabledByZeroGrace(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: false}, pool)
	s.jobTerminalGrace = 0
	r := launchOne(t, s, pool)

	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-x"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{
		RunnerName:     r.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-x"},
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(400 * time.Millisecond)
	if !alive(r.proc.pid()) {
		t.Fatal("watchdog ran despite jobTerminalGrace=0")
	}
}
