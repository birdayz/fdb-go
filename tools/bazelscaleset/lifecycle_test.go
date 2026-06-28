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

// TestSlotPoolPerSlotRunnerDirs pins the codex-P2 fix: at maxRunners>1 each slot gets
// its OWN cloned runner dir (distinct from the base and from each other, each with
// run.sh), so concurrent runners can't clobber each other's .runner/.credentials.
func TestSlotPoolPerSlotRunnerDirs(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	p, err := newSlotPool(t.TempDir(), base, 2)
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

// TestSlotPoolTrailingSlashBase pins codex P2 #1: a trailing slash on --runner-dir must
// still yield a SIBLING clone dir (".../actions-runner-slot0"), never a child inside the
// template (".../actions-runner/-slot0"), which would make the clone recurse into itself.
func TestSlotPoolTrailingSlashBase(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	p, err := newSlotPool(t.TempDir(), base+"/", 1) // trailing slash
	if err != nil {
		t.Fatal(err)
	}
	if want := base + "-slot0"; p.all[0].runnerDir != want {
		t.Fatalf("runnerDir = %q, want sibling %q (not a child of the template)", p.all[0].runnerDir, want)
	}
}

// TestSlotPoolResyncPropagatesTemplateChange pins codex P2 #2: cloning runs on every
// startup (not skipped when run.sh already exists), so a template update (e.g. a pinned-
// runner upgrade) propagates into an existing slot clone instead of leaving it stale.
func TestSlotPoolResyncPropagatesTemplateChange(t *testing.T) {
	t.Parallel()

	base := templateRunner(t)
	workBase := t.TempDir()
	p1, err := newSlotPool(workBase, base, 1)
	if err != nil {
		t.Fatal(err)
	}
	dst := p1.all[0].runnerDir // clone exists now (has run.sh)

	// Change the template AFTER the first clone, then rebuild the pool (supervisor restart).
	if err := os.WriteFile(filepath.Join(base, "bin-version"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSlotPool(workBase, base, 1); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "bin-version")); err != nil || string(b) != "v2" {
		t.Fatalf("re-sync did not propagate template update: read %q, err %v", b, err)
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

func TestReconcileStrayRunnersKillsGroup(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Stray runner running from THIS slot's own runner dir (so its cmdline contains
	// sl.runnerDir, which is what reconcile's per-slot guard matches).
	cmd := startStrayRunner(t, pool.all[0].runnerDir)
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })
	// Record the stray runner against the slot, as a live supervisor would.
	writeRunnerPID(discardLogger(), pool.all[0].path, pid)

	reconcileStrayRunners(discardLogger(), pool)

	// Process group must be dead, and the pid file removed.
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stray runner was not killed by reconcile")
	}
	if _, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile)); !os.IsNotExist(err) {
		t.Fatal("reconcile did not remove the pid file")
	}
}

func TestReconcileScopedToOurPidFiles(t *testing.T) {
	t.Parallel()

	runnerDir := filepath.Join(t.TempDir(), "actions-runner")
	// A runner-looking process that is NOT recorded in any slot pid file — e.g. the
	// classic runner during side-by-side migration. Reconcile must not touch it.
	other := startStrayRunner(t, runnerDir)
	otherPID := other.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-otherPID, syscall.SIGKILL); _, _ = other.Process.Wait() })

	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1) // no pid files written
	if err != nil {
		t.Fatal(err)
	}
	reconcileStrayRunners(discardLogger(), pool)

	if !alive(otherPID) {
		t.Fatal("reconcile killed an unrecorded process (not scoped to our pid files)")
	}
}

func TestReconcileSkipsReusedNonRunnerPID(t *testing.T) {
	t.Parallel()

	// A live process that does NOT look like a runner (cmdline "sleep …") models a PID
	// that was reused by something unrelated since the crash. reconcile must clear the
	// stale pid file but must NOT kill it (the cmdline guard).
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })

	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool.all[0].path, runnerPIDFile), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	reconcileStrayRunners(discardLogger(), pool)

	if _, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile)); !os.IsNotExist(err) {
		t.Fatal("reconcile did not clear the stale pid file")
	}
	if !alive(pid) {
		t.Fatal("reconcile killed a non-runner process — cmdline guard failed")
	}
}

// TestReconcileKillsGroupAfterLeaderExit pins codex's finding: a process group
// outlives its leader, so if run.sh exited but a Runner.Worker child still occupies
// the slot, reconcile must still reap the group (checking group members, not just the
// leader's cmdline).
func TestReconcileKillsGroupAfterLeaderExit(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1)
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

	writeRunnerPID(discardLogger(), pool.all[0].path, pgid)

	reconcileStrayRunners(discardLogger(), pool)

	// The whole group (the orphaned worker) must be gone.
	waitFor(t, 5*time.Second, func() bool { return syscall.Kill(-pgid, 0) != nil })
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

// TestTimeoutClientBoundsHangingPoll pins Torvalds PUSHBACK 1 (a): a half-open poll
// is bounded by --poll-timeout rather than hanging forever.
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

// TestListenerRunReturnsOnGetMessageError pins Torvalds PUSHBACK 1 (b): listener.Run
// propagates a GetMessage error out (so the process exits and systemd restarts),
// rather than retrying internally. If the Public-Preview library ever changes this,
// this test fails and we must add an in-process self-exit watchdog.
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
	pool, err := newSlotPool(t.TempDir(), templateRunner(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	sc := newScaler(discardLogger(), nil, 1, &config{maxRunners: 1, minRunners: 0}, pool)

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
