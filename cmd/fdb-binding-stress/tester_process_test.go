package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTesterGroupKillRejectsReapedProcessWithoutCallingSignal(t *testing.T) {
	t.Parallel()
	cmd := &exec.Cmd{Process: &os.Process{Pid: 12345}, ProcessState: &os.ProcessState{}}
	calls := 0
	err := killTesterGroupWithKill(cmd, func(int, syscall.Signal) error {
		calls++
		return nil
	})
	if !errors.Is(err, os.ErrProcessDone) || calls != 0 {
		t.Fatalf("reaped group signal attempt: err=%v calls=%d", err, calls)
	}
}

func TestTesterGroupSignalPrecedesReapAndStopsAfterReap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", "pass")
	process := configureTesterProcess(cmd)
	var calls atomic.Int32
	process.kill = func(pid int, signal syscall.Signal) error {
		calls.Add(1)
		if pid != -cmd.Process.Pid || signal != syscall.SIGKILL {
			t.Errorf("group signal=%d,%v", pid, signal)
		}
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
			t.Errorf("leader %s was reaped before group signal: %v", strconv.Itoa(cmd.Process.Pid), err)
		}
		if info.Signo != int32(syscall.SIGCHLD) {
			t.Errorf("group signal did not observe exited unreaped leader: %+v", info)
		}
		return nil
	}
	if err := process.Run(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("signal calls=%d process=%v", calls.Load(), cmd.ProcessState)
	}
	if err := process.cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("post-reap cancellation=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("post-reap cancellation sent another group signal")
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("Wait did not reap owned leader: %v", err)
	}
}
