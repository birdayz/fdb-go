package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCmdlineMatchesRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cmdline   string
		runnerDir string
		want      bool
	}{
		{"runner dir substring", "/bin/sh\x00/home/runner/actions-runner/run.sh", "/home/runner/actions-runner", true},
		{"Runner.Listener", "/home/x/bin/Runner.Listener\x00run", "/other/dir", true},
		{"Runner.Worker", "dotnet\x00Runner.Worker.dll", "/other/dir", true},
		{"unrelated process", "/usr/bin/sleep\x00300", "/home/runner/actions-runner", false},
		{"empty", "", "/home/runner/actions-runner", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cmdlineMatchesRunner([]byte(tt.cmdline), tt.runnerDir); got != tt.want {
				t.Fatalf("cmdlineMatchesRunner(%q, %q) = %v, want %v", tt.cmdline, tt.runnerDir, got, tt.want)
			}
		})
	}
}

func TestWriteHeartbeat(t *testing.T) {
	t.Parallel()

	// Empty path is a no-op and must not panic or create anything.
	writeHeartbeat("")

	path := filepath.Join(t.TempDir(), "hb")
	before := time.Now().Unix()
	writeHeartbeat(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("heartbeat not written: %v", err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("heartbeat not a timestamp: %q", data)
	}
	if ts < before {
		t.Fatalf("heartbeat %d older than call time %d", ts, before)
	}
	// No leftover temp file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp heartbeat file left behind")
	}
}

// startStrayRunner launches a process whose /proc cmdline contains runnerDir (so
// reconcile's cmdline guard matches it), in its own process group, and returns the
// command. The script loops without exec so the sh process keeps the script path in
// its cmdline.
func startStrayRunner(t *testing.T, runnerDir string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(runnerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(runnerDir, "run.sh")
	if err := os.WriteFile(script, []byte("while true; do sleep 1; done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Run via `/bin/sh <script>` (script read as data) rather than exec'ing the script
	// directly — the latter races with concurrent forks in parallel tests (ETXTBSY).
	cmd := exec.Command("/bin/sh", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stray runner: %v", err)
	}
	return cmd
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestSlotPoolPerSlotRunnerDirs pins the review-P2 fix: at maxRunners>1 each slot gets
// its OWN cloned runner dir (distinct from the base and from each other, each with
// run.sh), so concurrent runners can't clobber each other's .runner/.credentials.
func TestSlotPoolPerSlotRunnerDirs(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	p, err := newSlotPool(t.TempDir(), base, 2, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range p.all {
		if s.runnerDir == base {
			t.Fatalf("slot %d shares the base runner dir %q", s.index, base)
		}
		if seen[s.runnerDir] {
			t.Fatalf("slot %d has a duplicate runner dir %q", s.index, s.runnerDir)
		}
		seen[s.runnerDir] = true
		if _, err := os.Stat(filepath.Join(s.runnerDir, "run.sh")); err != nil {
			t.Fatalf("slot %d runner dir not cloned (no run.sh): %v", s.index, err)
		}
	}
}

// TestSlotPoolTrailingSlashBase pins review P2 #1: a trailing slash on --runner-dir must
// still yield a SIBLING clone dir (".../actions-runner-slot0"), never a child inside the
// template (".../actions-runner/-slot0"), which would make the clone recurse into itself.
func TestSlotPoolTrailingSlashBase(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	p, err := newSlotPool(t.TempDir(), base+"/", 1, remoteSlotConfig{}) // trailing slash
	if err != nil {
		t.Fatal(err)
	}
	if want := base + "-slot0"; p.all[0].runnerDir != want {
		t.Fatalf("runnerDir = %q, want sibling %q (not a child of the template)", p.all[0].runnerDir, want)
	}
}

// TestSlotPoolResyncPropagatesTemplateChange pins review P2 #2: cloning runs on every
// startup (not skipped when run.sh already exists), so a template update (e.g. a pinned-
// runner upgrade) propagates into an existing slot clone instead of leaving it stale.
func TestSlotPoolResyncPropagatesTemplateChange(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	workBase := t.TempDir()
	p1, err := newSlotPool(workBase, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dst := p1.all[0].runnerDir // clone exists now (has run.sh)

	// Change the template AFTER the first clone, then rebuild the pool (supervisor restart).
	if err := os.WriteFile(filepath.Join(base, "bin-version"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSlotPool(workBase, base, 1, remoteSlotConfig{}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "bin-version")); err != nil || string(b) != "v2" {
		t.Fatalf("re-sync did not propagate template update: read %q, err %v", b, err)
	}
}

// TestCloneRunnerDirSymlinkedSource pins review P2: a symlinked --runner-dir must still
// produce a populated clone (run.sh present) — the walk must resolve the symlink root.
func TestCloneRunnerDirSymlinkedSource(t *testing.T) {
	t.Parallel()

	real := templateRunner(t)
	link := filepath.Join(t.TempDir(), "runner-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if err := cloneRunnerDir(link, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "run.sh")); err != nil {
		t.Fatalf("clone from symlinked source missing run.sh: %v", err)
	}
}

// TestCopyFileReplacesContentAndMode pins review P3 + the umask fix: re-copying onto an
// existing file applies the new content AND the exact mode. The 0o775 group-write bit is
// dropped by the common umask 022 unless copyFile chmods explicitly, so this catches both
// the stale-perms-on-resync bug and the umask masking.
func TestCopyFileReplacesContentAndMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("new"), 0o775); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil { // pre-existing, different mode
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o775); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new" {
		t.Fatalf("content = %q, err %v; want \"new\"", b, err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o775 {
		t.Fatalf("mode = %v, err %v; want 0775 (umask must not mask it)", info.Mode().Perm(), err)
	}
}

// templateRunner creates a minimal template actions/runner dir (just run.sh) that
// newSlotPool clones per-slot.
func templateRunner(t *testing.T) string {
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

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// newAdoptScaler builds a local-only scaler pointed at the given work base and
// runner template, with a fast adoption liveness poll.
func newAdoptScaler(t *testing.T, client scalerClient, pool *slotPool, workBase, runnerBase string) *Scaler {
	t.Helper()
	cfg := &config{
		maxRunners:       pool.size(),
		minRunners:       0,
		grace:            2 * time.Second,
		jobStartTimeout:  time.Minute,
		jobTerminalGrace: time.Minute,
		workBase:         workBase,
		runnerDir:        runnerBase,
	}
	s := newScaler(discardLogger(), client, 1, cfg, pool)
	silenceChildOutput(t, s)
	s.adoptPoll = 30 * time.Millisecond
	return s
}

// TestAdoptLocalLiveRunner pins the restart-adopts-live-runner contract: a
// runner the previous incarnation recorded (pid + name) and left running must
// be ADOPTED at startup — still alive, tracked under its recorded name, its
// slot occupied — and job messages must re-attach to it by name. When it exits,
// the normal lifecycle frees the slot and removes the pidfile. Before adoption
// existed, this exact scenario was a kill: every supervisor restart canceled
// the in-flight CI job.
func TestAdoptLocalLiveRunner(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stray := startStrayRunner(t, pool.all[0].runnerDir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	const name = "bazelscaleset-s0-99-1"
	writeRunnerPID(discardLogger(), pool.all[0].path, pid, name)

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()

	if !alive(pid) {
		t.Fatal("adoption killed the live runner — the restart-kills-jobs bug")
	}
	if s.count() != 1 {
		t.Fatalf("running = %d, want 1 adopted runner", s.count())
	}
	if got := pool.take(); got != nil {
		t.Fatalf("slot %d handed out while its adopted runner is alive", got.index)
	}
	// Job messages re-attach by the recorded name.
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-adopted"},
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	busy := s.running[name] != nil && s.running[name].busy
	s.mu.Unlock()
	if !busy {
		t.Fatal("JobStarted did not re-attach to the adopted runner by name")
	}
	// The runner exits; the adopted lifecycle frees the slot and removes the pidfile.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_, _ = stray.Process.Wait()
	waitFor(t, 10*time.Second, slotFree(pool))
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile))
		return os.IsNotExist(err)
	})
}

// TestAdoptedRunnerWatchdogReclaims pins that adoption arms the terminal
// watchdog: an adopted runner whose job is terminal on the GitHub side (its
// JIT record is gone) must still be killed after the grace and its slot
// reclaimed — a restart must not turn a zombie into an untouchable squatter.
func TestAdoptedRunnerWatchdogReclaims(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stray := startStrayRunner(t, pool.all[0].runnerDir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	writeRunnerPID(discardLogger(), pool.all[0].path, pid, "bazelscaleset-s0-99-2")

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: false}, pool, wb, base) // record gone
	s.jobTerminalGrace = 150 * time.Millisecond
	s.terminalPoll = 40 * time.Millisecond
	s.adoptOrReapStrayRunners()

	// The stray is a direct child of this test: it stays a zombie (which still
	// answers kill(pid, 0)) until reaped, so death is observed via Wait.
	done := make(chan struct{})
	go func() { _, _ = stray.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal watchdog did not reclaim the adopted zombie runner")
	}
	waitFor(t, 10*time.Second, slotFree(pool))
}

// TestAdoptKillsLegacyNamelessPidfile pins the transition path: a pidfile in
// the pre-adoption bare-pid format has no runner name, so the runner cannot be
// tracked, watched, or matched to job messages — it gets the old
// kill-reconcile, and the record is cleared.
func TestAdoptKillsLegacyNamelessPidfile(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := startStrayRunner(t, pool.all[0].runnerDir)
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })
	// Legacy format: bare pid, no name line.
	if err := os.WriteFile(filepath.Join(pool.all[0].path, runnerPIDFile), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()

	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy nameless stray runner was not killed")
	}
	if _, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile)); !os.IsNotExist(err) {
		t.Fatal("legacy pid file was not removed")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (nameless runner cannot be adopted)", s.count())
	}
}

func TestReconcileScopedToOurPidFiles(t *testing.T) {
	t.Parallel()

	runnerDir := filepath.Join(t.TempDir(), "actions-runner")
	// A runner-looking process that is NOT recorded in any slot pid file — e.g. the
	// classic runner during side-by-side migration. Startup must not touch it.
	other := startStrayRunner(t, runnerDir)
	otherPID := other.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-otherPID, syscall.SIGKILL); _, _ = other.Process.Wait() })

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{}) // slot dirs, no pid files written
	if err != nil {
		t.Fatal(err)
	}
	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()

	if !alive(otherPID) {
		t.Fatal("startup killed an unrecorded process (not scoped to our pid files)")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (nothing recorded, nothing adopted)", s.count())
	}
}

func TestReconcileSkipsReusedNonRunnerPID(t *testing.T) {
	t.Parallel()

	// A live process that does NOT look like a runner (cmdline "sleep …") models a PID
	// that was reused by something unrelated since the previous incarnation. Startup
	// must clear the stale pid file but must NOT kill or adopt it (the cmdline guard).
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerPID(discardLogger(), pool.all[0].path, pid, "bazelscaleset-s0-99-3")

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()

	if _, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile)); !os.IsNotExist(err) {
		t.Fatal("startup did not clear the stale pid file")
	}
	if !alive(pid) {
		t.Fatal("startup killed a non-runner process — cmdline guard failed")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (a reused non-runner pid must not be adopted)", s.count())
	}
}

// TestAdoptGroupAfterLeaderExit pins member-based liveness for adoption: a
// process group outlives its leader, so if run.sh exited but a Runner.Worker
// child still occupies the slot, the group counts as ALIVE and is adopted —
// the worker keeps running and the slot stays occupied until the group is gone.
func TestAdoptGroupAfterLeaderExit(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	pool, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	runnerDir := pool.all[0].runnerDir // this slot's own runner dir
	worker := filepath.Join(runnerDir, "Runner.Worker")
	if err := os.WriteFile(worker, []byte("while true; do sleep 1; done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The leader backgrounds the worker (via `sh`, read as data — avoids ETXTBSY) into
	// its own process group, then exits.
	leaderScript := filepath.Join(runnerDir, "run.sh")
	if err := os.WriteFile(leaderScript, []byte("sh \"$1\" &\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	leader := exec.Command("/bin/sh", leaderScript, worker)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if err := leader.Wait(); err != nil { // leader exits immediately, worker lives on
		t.Fatalf("leader should exit cleanly: %v", err)
	}
	// Worker is now an orphan whose process group is still the (dead) leader's pid.
	waitFor(t, 3*time.Second, func() bool { return groupHasRunnerMember(pgid, runnerDir) })

	writeRunnerPID(discardLogger(), pool.all[0].path, pgid, "bazelscaleset-s0-99-4")

	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool, wb, base)
	s.adoptOrReapStrayRunners()

	// The surviving worker was adopted, not killed.
	if !groupHasRunnerMember(pgid, runnerDir) {
		t.Fatal("adoption killed the orphaned worker instead of adopting its group")
	}
	if s.count() != 1 {
		t.Fatalf("running = %d, want 1", s.count())
	}
	if got := pool.take(); got != nil {
		t.Fatalf("slot %d handed out while the adopted worker is alive", got.index)
	}
	// Once the group is gone, the slot frees.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	waitFor(t, 10*time.Second, slotFree(pool))
}

// TestAdoptLeavesOutOfPoolRunnerRunning pins the downgrade path: a restart with
// a LOWER --max-runners must not kill a live runner recorded in a now-out-of-
// pool higher slot. It cannot occupy a pool slot, so it is left to finish its
// one job untracked (it deregisters on its own), and its pidfile is kept so a
// later restart reaps the record once the runner is gone.
func TestAdoptLeavesOutOfPoolRunnerRunning(t *testing.T) {
	t.Parallel()

	wb, base := t.TempDir(), templateRunner(t)
	// Prior maxRunners=2 run: slot-0 + slot-1 (+ their runner clones) exist on disk.
	pool2, err := newSlotPool(wb, base, 2, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stray := startStrayRunner(t, pool2.all[1].runnerDir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	writeRunnerPID(discardLogger(), pool2.all[1].path, pid, "bazelscaleset-s1-99-5")

	// Supervisor restarts with maxRunners=1 (downgrade): the new pool has only slot-0.
	pool1, err := newSlotPool(wb, base, 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newAdoptScaler(t, &fakeScalerClient{runnerExists: true}, pool1, wb, base)
	s.adoptOrReapStrayRunners()

	if !alive(pid) {
		t.Fatal("out-of-pool live runner was killed — its in-flight job would have been canceled")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (out-of-pool runner is untracked)", s.count())
	}
	if _, err := os.Stat(filepath.Join(pool2.all[1].path, runnerPIDFile)); err != nil {
		t.Fatal("out-of-pool runner's pidfile must be kept for a later restart to reap")
	}
	// slot-0 stays free: the out-of-pool runner occupies no pool capacity.
	if got := pool1.take(); got == nil || got.index != 0 {
		t.Fatalf("slot 0 should be free, take = %v", got)
	}
}

// TestPollTimeoutExitLeavesRunners pins the incident fix: a supervisor exit NOT
// driven by SIGTERM/SIGINT (poll timeout, listener error — the exit-for-restart
// paths) must leave every runner running, busy or idle, with its pidfile in
// place for the next incarnation to adopt. The measured incident: a quiet-poll
// timeout exit killed the in-flight race-lane job with "The runner has received
// a shutdown signal".
func TestPollTimeoutExitLeavesRunners(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 2, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	s.maxRunners = 2

	busyR := launchOne(t, s, pool)
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName:     busyR.name,
		JobMessageBase: scaleset.JobMessageBase{JobID: "job-inflight"},
	}); err != nil {
		t.Fatal(err)
	}
	// Second runner: launched, no job yet. launchOne returns an arbitrary
	// tracked runner, so pick the one that is not busyR explicitly.
	launchOne(t, s, pool)
	var idleR *runner
	s.mu.Lock()
	for _, r := range s.running {
		if r.name != busyR.name {
			idleR = r
		}
	}
	s.mu.Unlock()
	if idleR == nil {
		t.Fatal("second (idle) runner not tracked")
	}

	finishRunners(context.Background(), s) // ctx not canceled: exit-for-restart

	if !alive(busyR.proc.pid()) {
		t.Fatal("exit-for-restart killed the BUSY runner — the in-flight job died")
	}
	if !alive(idleR.proc.pid()) {
		t.Fatal("exit-for-restart killed the idle runner — warm capacity lost, not adoptable")
	}
	for _, r := range []*runner{busyR, idleR} {
		if _, err := os.Stat(filepath.Join(r.slot.path, runnerPIDFile)); err != nil {
			t.Fatalf("runner %s pidfile missing after detach: adoption impossible", r.name)
		}
	}
}

// TestGenuineStopStillKillsRunners pins the other side: SIGTERM/SIGINT (a
// canceled run context — systemctl stop) still gets the full TERM/grace/KILL
// escalation; the operator asked for a quiet box.
func TestGenuineStopStillKillsRunners(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	r := launchOne(t, s, pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal arrived
	finishRunners(ctx, s)

	if alive(r.proc.pid()) {
		t.Fatal("genuine stop left the runner running")
	}
}

// TestStartLocalRetriesETXTBSY pins the fork/exec race cure: a write fd held
// open on run.sh at exec time (in production: another goroutine's fork
// duplicating clone-resync's write fd for the microseconds before its exec)
// makes Start fail with "text file busy". startLocal must retry past a
// transient hold instead of failing the launch — before the retry, this exact
// shape flaked the suite under -count=8.
func TestStartLocalRetriesETXTBSY(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	sl := pool.take()
	if sl == nil {
		t.Fatal("no free slot")
	}

	// Hold run.sh open for writing: any live write fd makes exec ETXTBSY.
	f, err := os.OpenFile(filepath.Join(sl.runnerDir, "run.sh"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond) // well inside the 2s retry budget
		_ = f.Close()
		close(released)
	}()

	proc, err := s.startLocal(sl, "jit-test")
	if err != nil {
		t.Fatalf("startLocal did not retry past a transient ETXTBSY: %v", err)
	}
	<-released
	proc.signal(syscall.SIGKILL)
	_ = proc.wait()
}

func TestParseRunnerPIDFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in       string
		wantPID  int
		wantName string
	}{
		{"1234\nbazelscaleset-s0-1-2\n", 1234, "bazelscaleset-s0-1-2"},
		{"1234", 1234, ""},   // legacy bare pid
		{"1234\n", 1234, ""}, // legacy with newline
		{"garbage", 0, ""},
		{"1\nname\n", 0, ""}, // pid <= 1 rejected
		{"", 0, ""},
	}
	for _, tt := range tests {
		pid, name := parseRunnerPIDFile([]byte(tt.in))
		if pid != tt.wantPID || name != tt.wantName {
			t.Fatalf("parseRunnerPIDFile(%q) = (%d, %q), want (%d, %q)", tt.in, pid, name, tt.wantPID, tt.wantName)
		}
	}
}

// fakeClient implements listener.Client for fault-injection tests.
type fakeClient struct {
	getMessage func(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error)
	session    scaleset.RunnerScaleSetSession
}

func (f *fakeClient) GetMessage(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	return f.getMessage(ctx, last, capacity)
}
func (f *fakeClient) DeleteMessage(context.Context, int) error                    { return nil }
func (f *fakeClient) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) { return ids, nil }
func (f *fakeClient) Session() scaleset.RunnerScaleSetSession                     { return f.session }

// TestTimeoutClientBoundsHangingPoll pins the half-open-poll case: a poll
// that never receives must be bounded by --poll-timeout rather than
// hanging forever.
func TestTimeoutClientBoundsHangingPoll(t *testing.T) {
	t.Parallel()

	hang := &fakeClient{getMessage: func(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
		<-ctx.Done() // never returns on its own
		return nil, ctx.Err()
	}}
	tc := &timeoutClient{inner: hang, pollTimeout: 100 * time.Millisecond}

	start := time.Now()
	if _, err := tc.GetMessage(context.Background(), 0, 1); err == nil {
		t.Fatal("expected a timeout error from a hanging poll")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("poll was not bounded by pollTimeout: took %s", elapsed)
	}
}

// TestListenerRunReturnsOnGetMessageError pins the propagate-not-retry
// contract: listener.Run propagates a GetMessage error out (so the
// process exits and systemd restarts), rather than retrying internally.
// If the Public-Preview library ever changes this, this test fails and
// we must add an in-process self-exit watchdog.
func TestListenerRunReturnsOnGetMessageError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom-from-getmessage")
	fc := &fakeClient{
		session: scaleset.RunnerScaleSetSession{
			SessionID:  uuid.New(),
			Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
		},
		getMessage: func(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
			return nil, wantErr
		},
	}
	lis, err := listener.New(fc, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatalf("listener.New: %v", err)
	}
	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sc := newScaler(discardLogger(), nil, 1, &config{maxRunners: 1, minRunners: 0}, pool)
	silenceChildOutput(t, sc)

	done := make(chan error, 1)
	go func() { done <- lis.Run(context.Background(), sc) }()
	select {
	case got := <-done:
		if got == nil || !strings.Contains(got.Error(), "boom-from-getmessage") {
			t.Fatalf("listener.Run should propagate the GetMessage error, got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener.Run did not return on a GetMessage error (does the library now retry internally?)")
	}
}
