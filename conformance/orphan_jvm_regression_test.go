// The process-group liveness probe below walks /proc directly, so this test is
// Linux-only. That costs no coverage: the leak it pins (orphaned JVMs on a
// SIGKILLed parent) only ever bites the Linux CI runner, and the tether under
// test is exercised on every platform by the ordinary suite (each server now
// runs with the stdin watchdog).
//go:build bazelrunfiles && linux

package conformance_test

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestJavaServerExitsOnParentDeath pins the fix for the CI-runner OOM incident:
// conformance runs killed abruptly (bazel timeout ⇒ SIGKILL, so no Go cleanup
// code — not even CloseAllJavaServers — runs) left the -Xmx2g conformance JVMs
// orphaned. Accumulated orphans exhausted swap on the runner box, the kernel
// OOM-killer fired, and systemd's default OOMPolicy=stop tore down the whole
// runner unit mid-job.
//
// The fix is a parent-death tether: startJavaServer hands the server a pipe as
// stdin and holds the write end; conformance_server.java halts on stdin EOF.
// The one thing the kernel guarantees on parent death — however it died — is
// closing its fds. This test performs exactly that kernel action (closing the
// parent-side write end, without running any of the invoker's cleanup) and
// asserts the whole server process tree exits on its own within a bounded
// window.
//
// The server is probed via its process GROUP (Setpgid at spawn): the bazel
// java_binary wrapper script and the real JVM are both members. Zombies are
// ignored — a reaped-but-unwaited wrapper holds no memory; live JVMs are the
// leak.
func TestJavaServerExitsOnParentDeath(t *testing.T) {
	t.Parallel()

	inv, err := NewIsolatedJavaInvoker()
	if err != nil {
		t.Fatalf("failed to start Java conformance server: %v", err)
	}
	// Backstop only (idempotent): on test FAILURE this kills the group so the
	// leak being asserted on can't outlive the test. On success it is a no-op
	// beyond reaping the wrapper zombie.
	defer func() { _ = inv.Close() }()

	pgid := inv.serverCmd.Process.Pid // Setpgid: true ⇒ pgid == leader pid
	if n := liveProcessGroupMembers(t, pgid); n == 0 {
		t.Fatalf("no live process in group %d right after startup", pgid)
	}

	// Simulate abrupt parent death. When this test process is SIGKILLed the
	// kernel closes its fds — nothing more. Closing the tether's write end is
	// that exact event, minus everything Close() would normally also do (HTTP
	// /shutdown, group SIGKILL): the server must now exit BY ITSELF.
	// The deferred Close() above will close stdinW a second time; that double
	// close is deliberate (kernel action here, normal backstop there) and its
	// error is discarded.
	if err := inv.stdinW.Close(); err != nil {
		t.Fatalf("failed to close parent-side stdin handle: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if liveProcessGroupMembers(t, pgid) == 0 {
			return // whole tree exited on its own — no orphan
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Java server process group %d still has live members 30s after the parent-side stdin handle closed — this is the orphan-JVM leak that OOM-killed the CI runner", pgid)
}

// liveProcessGroupMembers counts non-zombie processes in the given process
// group by scanning /proc/<pid>/stat (field 3 = state, field 5 = pgrp; the
// comm field is parenthesized and may contain spaces, so parse after the last
// ')'). Zombies don't count: they hold no memory and are reaped by whoever
// waits on them; syscall.Kill(-pgid, 0) alone can't make this distinction.
func liveProcessGroupMembers(t *testing.T, pgid int) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("failed to read /proc: %v", err)
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid dir
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // process vanished between ReadDir and ReadFile
		}
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[i+2:])
		if len(fields) < 3 {
			continue
		}
		state := fields[0]
		pg, err := strconv.Atoi(fields[2])
		if err != nil || pg != pgid {
			continue
		}
		if state != "Z" {
			n++
		}
	}
	return n
}
