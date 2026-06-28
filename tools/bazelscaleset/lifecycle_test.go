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
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stray runner: %v", err)
	}
	return cmd
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestReconcileStrayRunnersKillsGroup(t *testing.T) {
	t.Parallel()

	runnerDir := filepath.Join(t.TempDir(), "actions-runner")
	cmd := startStrayRunner(t, runnerDir)
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })

	pool, err := newSlotPool(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Record the stray runner against the slot, as a live supervisor would.
	writeRunnerPID(discardLogger(), pool.all[0].path, pid)

	reconcileStrayRunners(discardLogger(), pool, runnerDir)

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

	pool, err := newSlotPool(t.TempDir(), 1) // no pid files written
	if err != nil {
		t.Fatal(err)
	}
	reconcileStrayRunners(discardLogger(), pool, runnerDir)

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

	pool, err := newSlotPool(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pool.all[0].path, runnerPIDFile), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	reconcileStrayRunners(discardLogger(), pool, "/home/runner/actions-runner")

	if _, err := os.Stat(filepath.Join(pool.all[0].path, runnerPIDFile)); !os.IsNotExist(err) {
		t.Fatal("reconcile did not clear the stale pid file")
	}
	if !alive(pid) {
		t.Fatal("reconcile killed a non-runner process — cmdline guard failed")
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
	pool, err := newSlotPool(t.TempDir(), 1)
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
