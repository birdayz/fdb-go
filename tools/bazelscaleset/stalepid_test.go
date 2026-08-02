package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A REMOTE runner's pidfile outlives the runner: wait() unlinks only local
// pidfiles, because a remote one has no tracking incarnation left to clean it.
// So every launch after the first starts with the PREVIOUS runner's pidfile
// sitting in the slot's work dir, non-empty. The launch wrapper used to wait
// with `[ -s pidfile ]`, which that corpse satisfies immediately, so the wait
// never waited and `head` raced the new session's truncate:
//
//   - head first  -> the launch reports the previous runner's DEAD pid, the
//     supervisor's liveness probe finds nothing runner-like within ~15s, and it
//     logs a clean "runner exited err=<nil>" and frees the slot while the real
//     runner is alive and untracked.
//   - head inside the truncate window -> empty file, exit 65 ("runner session
//     wrote no pidfile").
//
// Measured: 32 exit-65s in ten hours across the pool, 17 of them on the box with
// a 150GB root disk at 48% while the other three sat at 91-94%. More than half
// the failures on the LEAST pressured box is what ruled out disk pressure.

// staleRemoteSlot builds a remote slot whose work dir already holds a pidfile
// from a previous runner, with a pid that is guaranteed dead.
func staleRemoteSlot(t *testing.T, runnerDir, workBase string) (*slot, int) {
	t.Helper()
	sl := &slot{index: 1, host: "runner@10.0.0.9", path: workBase + "/slot-0", runnerDir: runnerDir}
	if err := os.MkdirAll(sl.path, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pid that is reaped and gone: start a process, wait for it, reuse its id.
	dead := startSleeper(t)
	_ = syscall.Kill(-dead, syscall.SIGKILL)
	stale := fmt.Sprintf("%d\nbazelscaleset-s1-OLD-1\n", dead)
	if err := os.WriteFile(remotePIDFilePath(sl), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	return sl, dead
}

// TestRemoteLaunchIgnoresStalePidfile pins the fix: a launch must report the pid
// of the session it just started, never the leftover pid of the previous one.
//
// The launch is repeated because the defect is a race the OLD code loses almost
// every time but not provably every time: the parent's first stat beats the
// child's fork/exec/exec/write by orders of magnitude. Repeating turns "almost
// always wrong" into a reliable RED without making the passing path flaky --
// the fixed code must get all of them right.
func TestRemoteLaunchIgnoresStalePidfile(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSH(t)
	rdir := remoteRunnerDir(t, "sleep 30")
	workBase := filepath.Join(t.TempDir(), "rwork")
	conn := sshConn{bin: sshBin, keyFile: "/test/remote_id", host: "runner@10.0.0.9", connectTimeout: 2 * 1e9}
	cfg := remoteProcConfig{pollInterval: 30 * 1e6, probeTimeout: 5 * 1e9, unreachableLimit: 3}

	const attempts = 15
	for i := range attempts {
		sl, dead := staleRemoteSlot(t, rdir, workBase)
		name := fmt.Sprintf("bazelscaleset-s1-NEW-%d", i)

		proc, err := launchRemote(context.Background(), conn, sl, name, "jit-blob", cfg, discardLogger())
		if err != nil {
			t.Fatalf("attempt %d: launch failed: %v (a stale pidfile must never make a launch fail; "+
				"an empty read of the truncate window is the exit-65 this fix removes)", i, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-proc.pid(), syscall.SIGKILL) })

		if proc.pid() == dead {
			t.Fatalf("attempt %d: launch reported the PREVIOUS runner's dead pid %d. The supervisor "+
				"would track a corpse, declare it exited within ~15s and free the slot while the real "+
				"runner keeps running untracked on the box", i, dead)
		}
		// The reported pid must be the live session that was just started.
		if err := syscall.Kill(proc.pid(), 0); err != nil {
			t.Fatalf("attempt %d: reported pid %d is not a live process: %v", i, proc.pid(), err)
		}

		// And the published pidfile must describe THIS runner, not the old one.
		data, err := os.ReadFile(remotePIDFilePath(sl))
		if err != nil {
			t.Fatalf("attempt %d: pidfile unreadable: %v", i, err)
		}
		gotPID, gotName := parseRunnerPIDFile(data)
		if gotPID != proc.pid() || gotName != name {
			t.Fatalf("attempt %d: pidfile = (%d, %q), want (%d, %q)", i, gotPID, gotName, proc.pid(), name)
		}
		_ = syscall.Kill(-proc.pid(), syscall.SIGKILL)
		_ = os.RemoveAll(sl.path)
	}
}

// TestRemoteLaunchScriptPublishesAtomically pins the second half of the fix.
// Even with the stale file removed, a plain `> pidfile` leaves a window in which
// the file exists but is empty, and a launch that reads it there fails with
// exit 65. Publishing through a temp file and an atomic rename means a non-empty
// pidfile always means "the session published its pid", so the wait loop's
// condition is sound rather than merely usually true.
func TestRemoteLaunchScriptPublishesAtomically(t *testing.T) {
	t.Parallel()

	sl := &slot{index: 1, host: "runner@10.0.0.9", path: "/w/slot-0", runnerDir: "/r"}
	script := remoteLaunchScript(sl, "bazelscaleset-s1-1-1")
	pidfile := remotePIDFilePath(sl)

	// The stale file is cleared before the session starts, so the wait loop
	// cannot be satisfied by a previous runner's corpse.
	if !strings.Contains(script, "rm -f "+shQuote(pidfile)) {
		t.Fatalf("launch script does not clear a stale pidfile before waiting for the new one:\n%s", script)
	}
	// The session writes elsewhere and renames into place -- never truncating
	// the path the launcher polls.
	if !strings.Contains(script, "mv "+shQuote(pidfile+".new")+" "+shQuote(pidfile)) {
		t.Fatalf("launch script does not publish the pidfile atomically:\n%s", script)
	}
	if strings.Contains(script, "} > "+shQuote(pidfile)) {
		t.Fatalf("launch script still truncates the polled pidfile directly, reopening the "+
			"empty-read window that surfaces as exit 65:\n%s", script)
	}
}
