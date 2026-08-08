package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file pins the two halves of one failure mode. `go test` hands the test
// binary a pipe and reads it to learn the binary has finished; a child that
// inherits that pipe and outlives the binary keeps it open, so cmd/go waits out
// its 60s WaitDelay and reports
//
//	PASS
//	*** Test I/O incomplete 1m0s after exiting.
//	exec: WaitDelay expired before I/O complete
//	FAIL	fdb.dev/tools/bazelscaleset	61.980s
//
// on a run where every test passed. Reproduced on this suite before the fix:
// run.sh and its sleep were caught at test-binary exit still holding the
// binary's stdout pipe on fd 1.
//
// The two halves are independent and both are real:
//   - startLocal must send child output where its caller says (structural: a
//     straggler can then only leak a process, never stall an unrelated stream),
//   - the test cleanup must not return until the group is actually gone
//     (behavioural: no straggler in the first place).
//
// The race itself is ~1ms wide and reproduces roughly once in eight full-suite
// runs, so these tests pin the two invariants deterministically instead.

// TestStartLocalSendsChildOutputToTheInjectedDestination pins the structural
// half: given a destination, startLocal must use it and must NOT hand the child
// this process's own stdout.
func TestStartLocalSendsChildOutputToTheInjectedDestination(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)

	// A named regular file, so the child's fd can be identified unambiguously
	// (/dev/null would be indistinguishable from "some other test's /dev/null").
	dest := filepath.Join(t.TempDir(), "child-output")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	s.childOut, s.childErr = f, f

	r := launchOne(t, s, pool)

	mine, err := os.Readlink("/proc/self/fd/1")
	if err != nil {
		t.Fatalf("reading this binary's stdout: %v", err)
	}
	for _, fd := range []struct {
		n    int
		name string
	}{{1, "stdout"}, {2, "stderr"}} {
		got, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", r.proc.pid(), fd.n))
		if err != nil {
			t.Fatalf("reading child %s: %v", fd.name, err)
		}
		if got == mine {
			t.Fatalf("startLocal gave the child this test binary's %s (%s) despite a destination "+
				"being supplied; a runner outliving the test then holds the pipe cmd/go reads to "+
				"detect that the binary finished, and the package fails with \"Test I/O incomplete\" "+
				"60s after every test passed", fd.name, got)
		}
		if got != dest {
			t.Fatalf("child %s = %q, want the injected destination %q", fd.name, got, dest)
		}
	}
}

// TestKillGroupAndWaitDoesNotReturnWhileTheGroupIsAlive pins the behavioural
// half. kill(2) returns when the signal is queued, not when the target has died
// and dropped its descriptors: measured on this suite, a cleanup that only
// signalled left the group alive on 40 of 40 launches.
func TestKillGroupAndWaitDoesNotReturnWhileTheGroupIsAlive(t *testing.T) {
	t.Parallel()

	pool, err := newSlotPool(t.TempDir(), hangingTemplateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	s := newWatchdogScaler(t, &fakeScalerClient{runnerExists: true}, pool)
	r := launchOne(t, s, pool)
	pgid := r.proc.pid()

	killGroupAndWait(t, r.proc)

	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("killGroupAndWait returned while group %d was still alive (kill(-pgid,0) = %v, "+
			"want ESRCH); a cleanup that requests death without observing it leaves a process "+
			"holding every descriptor it inherited from this binary", pgid, err)
	}
}

// TestGroupLingerReasonTellsZombieFromSurvivor pins the diagnosis killGroupAndWait
// prints when its poll times out. The two faults that reach that line need
// opposite investigations — a zombie leader holds no descriptors and means the
// reap broke, a running one means a real survivor is stalling cmd/go — so a
// message that always claimed the second would send the reader hunting a process
// that does not exist in precisely the case the helper's doc warns about.
func TestGroupLingerReasonTellsZombieFromSurvivor(t *testing.T) {
	t.Parallel()

	// A live leader in its own group, never signalled.
	live := exec.Command("sleep", "30")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	livePID := live.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-livePID, syscall.SIGKILL); _, _ = live.Process.Wait() })

	if got := groupLingerReason(livePID); !strings.Contains(got, "still running") {
		t.Fatalf("live leader %d diagnosed as %q, want it named as still running", livePID, got)
	}

	// A zombie leader: dead but deliberately not reaped, which is exactly the
	// state a broken wait() would leave behind.
	zombie := exec.Command("sleep", "30")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	zPID := zombie.Process.Pid
	t.Cleanup(func() { _, _ = zombie.Process.Wait() }) // the reap, withheld until now
	if err := syscall.Kill(-zPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !processIsZombie(zPID) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie after SIGKILL", zPID)
		}
		time.Sleep(time.Millisecond)
	}

	// The premise of the whole branch: a zombie still answers kill(pgid, 0).
	if err := syscall.Kill(zPID, 0); err != nil {
		t.Fatalf("zombie %d answered kill(pid,0) with %v, want nil; if the kernel ever stops "+
			"reporting zombies as present, killGroupAndWait's reap dependency disappears", zPID, err)
	}
	got := groupLingerReason(zPID)
	if !strings.Contains(got, "ZOMBIE") {
		t.Fatalf("zombie leader %d diagnosed as %q, want it named as a zombie", zPID, got)
	}
	if strings.Contains(got, "still running") {
		t.Fatalf("zombie leader %d diagnosed as a running survivor: %q", zPID, got)
	}
}

func processIsZombie(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(string(raw)[i+1:])
	return len(f) > 0 && f[0] == "Z"
}

// stdoutHolder is one process found holding this binary's stdout for writing.
type stdoutHolder struct {
	pid  int
	comm string
	fd   string
}

// processesHoldingOurStdout returns every OTHER process holding this binary's
// stdout open for writing.
//
// Only writable descriptors count. A pipe's two ends share one inode, so cmd/go
// — which holds the read end of exactly this pipe — matches on the link target
// and must be excluded by its open mode; /proc/<pid>/fdinfo's low two flag bits
// are O_RDONLY(0) for it and O_WRONLY(1) for an inherited stdout.
func processesHoldingOurStdout() ([]stdoutHolder, error) {
	mine, err := os.Readlink("/proc/self/fd/1")
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var found []stdoutHolder
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		if err != nil {
			continue // exited, or not ours to inspect
		}
		for _, fd := range fds {
			tgt, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err != nil || tgt != mine {
				continue
			}
			if !fdIsWritable(pid, fd.Name()) {
				continue
			}
			comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
			found = append(found, stdoutHolder{pid: pid, comm: strings.TrimSpace(string(comm)), fd: fd.Name()})
			break
		}
	}
	return found, nil
}

func fdIsWritable(pid int, fd string) bool {
	f, err := os.Open(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fd))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "flags:")
		if !ok {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimSpace(rest), 8, 64)
		if err != nil {
			return false
		}
		return flags&uint64(os.O_WRONLY|os.O_RDWR) != 0
	}
	return false
}

// TestStdoutHolderDetectorSeesAnInheritedStdout proves the at-exit detector in
// TestMain actually detects — a guard that cannot see the thing it guards
// against reports green forever. A child deliberately given this binary's
// stdout must be found, and must stop being found once it is gone.
func TestStdoutHolderDetectorSeesAnInheritedStdout(t *testing.T) {
	t.Parallel()

	// A single process, not a shell wrapping one: Wait then reaps the only holder,
	// so this test cannot itself leave a straggler for TestMain to trip over.
	cmd := exec.Command("sleep", "30")
	cmd.Stdout = os.Stdout // exactly what startLocal used to hard-wire
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })

	holders, err := processesHoldingOurStdout()
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContainsPID(holders, pid) {
		t.Fatalf("detector missed pid %d, which was handed this binary's stdout; holders=%v", pid, holders)
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		holders, err = processesHoldingOurStdout()
		if err != nil {
			t.Fatal(err)
		}
		if !slicesContainsPID(holders, pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detector still reports killed pid %d as a holder", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func slicesContainsPID(hs []stdoutHolder, pid int) bool {
	return slices.ContainsFunc(hs, func(h stdoutHolder) bool { return h.pid == pid })
}

// TestMain fails the package if any process still holds this binary's stdout
// once the tests are done — the exact condition behind "Test I/O incomplete".
//
// This is a net, not a pin: it fires only when a straggler actually survives,
// which is what the two invariant tests above exist to prevent. Its value is
// diagnostic. Without it the failure arrives 60 seconds later as a WaitDelay
// expiry that names neither the process nor the test, which is how this cost
// several people a full cycle each.
func TestMain(m *testing.M) {
	// Before m.Run, and therefore before this process forks for the first time:
	// the shared fake ssh binaries must be fully written and closed while no
	// child can be holding a duplicated write fd on them. See the comment on
	// setupFakeSSHBinaries for why this ordering is the fix for ETXTBSY.
	if err := setupFakeSSHBinaries(); err != nil {
		fmt.Fprintf(os.Stderr, "creating the shared fake ssh binaries: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(fakeSSHDir)
	if holders, err := processesHoldingOurStdout(); err == nil && len(holders) > 0 {
		for _, h := range holders {
			fmt.Fprintf(os.Stderr, "leaked child: pid %d (%s) still holds this test binary's "+
				"stdout on fd %s after all tests finished\n", h.pid, h.comm, h.fd)
		}
		fmt.Fprintf(os.Stderr, "%d process(es) outlived the tests holding this binary's stdout; "+
			"cmd/go will now block until its WaitDelay expires and report \"Test I/O incomplete\". "+
			"A launcher must not hand a long-lived child os.Stdout, and a cleanup must wait for "+
			"the process group to be gone rather than merely signalling it.\n", len(holders))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
