package main

import (
	"context"
	"os"
	"path/filepath"
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
	t.Cleanup(func() { r.proc.signal(15); r.proc.signal(9) })
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
