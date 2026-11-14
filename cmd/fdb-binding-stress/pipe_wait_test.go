package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type pipeFixtureIdentity struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	PGID int    `json:"pgid"`
	SID  int    `json:"sid"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

// The holder is a direct, unreaped child of this Go process. Its PID cannot be
// recycled before cleanup, and no process-group signal is needed, even on failure.
func checkOwnedPipeHolder(pid int) error {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return fmt.Errorf("invalid process stat: %q", data)
	}
	fields := strings.Fields(string(data)[end+2:])
	if len(fields) < 4 {
		return fmt.Errorf("short process stat: %q", data)
	}
	if fields[0] == "Z" || fields[0] == "X" || fields[1] != strconv.Itoa(os.Getpid()) || fields[2] != strconv.Itoa(pid) || fields[3] != strconv.Itoa(pid) {
		return fmt.Errorf("holder must be live Go-owned session/group leader: %q", data)
	}
	return nil
}

func TestTesterWaitDelayBoundsEscapedPipeHolderAfterNormalExit(t *testing.T) {
	t.Parallel()
	// Bazel's t.TempDir paths can exceed the AF_UNIX pathname limit.
	root, err := os.MkdirTemp("/tmp", "fdb-pipe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: filepath.Join(root, "fd"), Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	const testerProgram = `import array, json, os, socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET)
s.settimeout(20)
s.connect(sys.argv[1])
st = os.fstat(1)
identity = dict(pid=os.getpid(), ppid=os.getppid(), pgid=os.getpgrp(), sid=os.getsid(0), dev=st.st_dev, ino=st.st_ino)
s.sendmsg([json.dumps(identity).encode()], [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array('i', [1]))])
if s.recv(16) != b'ready':
    raise RuntimeError('holder readiness was not acknowledged')
s.close()
print('parent exits zero without cancelling escaped child', flush=True)
`
	// Background never cancels: the production group-killing Cancel callback is
	// installed, but this fixture exercises normal parent exit, not cancellation.
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "python3", "-c", testerProgram, listener.Addr().String())
	process := configureTesterProcess(cmd)
	// Do not replace WaitDelay: exercise the production five-second assignment.
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	var testerStarted bool
	var waitErr error
	waitDone := make(chan struct{})
	var conn *net.UnixConn
	var stdout *os.File
	var holder *exec.Cmd
	var holderStarted bool
	var readyRead, readyWrite *os.File
	t.Cleanup(func() {
		// Close every Go-owned copy before waiting. Otherwise this fixture could
		// itself keep the tester's stdout pipe open after killing the holder.
		if conn != nil {
			_ = conn.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		if readyRead != nil {
			_ = readyRead.Close()
		}
		if readyWrite != nil {
			_ = readyWrite.Close()
		}
		if holderStarted {
			if err := checkOwnedPipeHolder(holder.Process.Pid); err != nil {
				t.Errorf("holder identity before cleanup: %v", err)
			}
			// This is our direct child and has not been waited on. Even if the
			// identity assertion fails, an individual-PID kill is safe and needed.
			if err := holder.Process.Kill(); err != nil {
				t.Errorf("kill owned holder: %v", err)
			}
			holderWait := make(chan error, 1)
			go func() { holderWait <- holder.Wait() }()
			select {
			case err := <-holderWait:
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Errorf("holder was not killed and reaped: %v", err)
				} else if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || exit.Pid() != holder.Process.Pid {
					t.Errorf("unexpected holder reap status: %v", exit)
				}
			case <-time.After(5 * time.Second):
				t.Error("owned holder was SIGKILLed but was not reaped within cleanup bound")
			}
		}
		if testerStarted {
			// Never invoke Cancel/killTesterGroup after Wait: the original group
			// ID may have been recycled. Only signal our Go-owned process handle.
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill tester during cleanup: %v", err)
			}
			select {
			case <-waitDone:
			case <-time.After(5 * time.Second):
				t.Error("tester Wait did not join after holder cleanup")
			}
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	testerStarted = true
	go func() { waitErr = process.Wait(); close(waitDone) }()

	conn, err = listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data, control := make([]byte, 4096), make([]byte, syscall.CmsgSpace(4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(data, control)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := syscall.ParseSocketControlMessage(control[:oobn])
	if err != nil {
		t.Fatal(err)
	}
	var fds []int
	for _, message := range messages {
		if message.Header.Level != syscall.SOL_SOCKET || message.Header.Type != syscall.SCM_RIGHTS {
			t.Errorf("unexpected ancillary message: %+v", message.Header)
			continue
		}
		received, err := syscall.ParseUnixRights(&message)
		if err != nil {
			t.Fatal(err)
		}
		fds = append(fds, received...)
	}
	if len(fds) != 1 || flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 {
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
		t.Fatalf("want exactly one untruncated stdout descriptor: fds=%v flags=%d", fds, flags)
	}
	stdout = os.NewFile(uintptr(fds[0]), "tester-stdout")
	var parent pipeFixtureIdentity
	if err := json.Unmarshal(data[:n], &parent); err != nil {
		t.Fatal(err)
	}
	info, err := stdout.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeNamedPipe == 0 || uint64(stat.Dev) != parent.Dev || stat.Ino != parent.Ino || parent.PID != cmd.Process.Pid || parent.PGID != parent.PID || parent.PPID != os.Getpid() {
		t.Fatalf("not the actual tester stdout pipe: parent=%+v info=%v", parent, info)
	}

	readyRead, readyWrite, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	const holderProgram = `import json, os, signal
st = os.fstat(1)
print('escaped child retains output pipe', flush=True)
identity = dict(pid=os.getpid(), ppid=os.getppid(), pgid=os.getpgrp(), sid=os.getsid(0), dev=st.st_dev, ino=st.st_ino)
os.write(3, (json.dumps(identity) + '\n').encode())
os.close(3)
while True:
    signal.pause()
`
	// Go, not the normally exiting tester, owns and later reaps this child.
	// Its fd 1 is the SCM_RIGHTS duplicate of the tester's actual output pipe.
	holder = exec.Command("python3", "-c", holderProgram)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	holder.Stdout, holder.Stderr = stdout, os.Stderr
	holder.ExtraFiles = []*os.File{readyWrite}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	holderStarted = true
	if err := readyWrite.Close(); err != nil {
		t.Fatal(err)
	}
	readyWrite = nil
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	stdout = nil
	if err := readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var child pipeFixtureIdentity
	if err := json.NewDecoder(io.LimitReader(readyRead, 4096)).Decode(&child); err != nil {
		t.Fatal(err)
	}
	if child.PID != holder.Process.Pid || child.PPID != os.Getpid() || child.PGID != child.PID || child.SID != child.PID || child.PGID == parent.PGID || child.SID == parent.SID || child.Dev != parent.Dev || child.Ino != parent.Ino {
		t.Fatalf("holder is not independent escaped owner of actual tester pipe: parent=%+v child=%+v", parent, child)
	}
	if err := checkOwnedPipeHolder(child.PID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
		if !errors.Is(waitErr, exec.ErrWaitDelay) || ctx.Err() != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() || cmd.WaitDelay != 5*time.Second {
			t.Fatalf("normal exit pipe wait=%v ctx=%v process=%v configuredDelay=%v output=%s", waitErr, ctx.Err(), cmd.ProcessState, cmd.WaitDelay, output.String())
		}
	case <-time.After(15 * time.Second):
		// Cleanup kills and reaps the directly owned holder, also releasing a
		// removeWaitDelay mutant's blocked pipe reader before joining Wait.
		t.Fatal("normal tester exit hung behind live escaped holder; production WaitDelay ineffective")
	}
	if err := checkOwnedPipeHolder(child.PID); err != nil {
		t.Fatalf("Wait was not released while escaped holder remained alive: %v", err)
	}
	if !strings.Contains(output.String(), "parent exits zero") || !strings.Contains(output.String(), "escaped child retains output pipe") {
		t.Fatalf("fixture output=%s", output.String())
	}
}
