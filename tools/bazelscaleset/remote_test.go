package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// fakeSSH writes a fake "ssh" binary that records every invocation's argv (one
// line per call) and then EXECUTES the final argument (the remote command)
// locally with /bin/sh — simulating a pool box whose filesystem is this
// machine's. Remote runner/work dirs in these tests are therefore local temp
// dirs, and the real wrapper/liveness/kill/reconcile scripts run for real.
func fakeSSH(t *testing.T) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "ssh")
	// One line per invocation (newlines in the remote script flattened), so
	// assertions can match host + script content of a single call.
	script := `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> ` + shQuote(argvLog) + `
printf '\n' >> ` + shQuote(argvLog) + `
for a in "$@"; do last=$a; done
exec /bin/sh -c "$last"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

// fakeSSHFailing simulates an unreachable host: every invocation exits 255
// (ssh's transport-failure code) without running anything.
func fakeSSHFailing(t *testing.T) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "ssh")
	script := `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> ` + shQuote(argvLog) + `
printf '\n' >> ` + shQuote(argvLog) + `
exit 255
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

// remoteRunnerDir creates a "pool box" actions/runner dir whose run.sh runs
// the given body.
func remoteRunnerDir(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "actions-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startSleeper starts a plain non-runner process in its own group and returns
// its pid (cleaned up with the test).
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })
	return pid
}

// fakeScalerClient fakes the GitHub-side surface the scaler needs.
type fakeScalerClient struct {
	mu           sync.Mutex
	minted       []string
	removed      []int64
	runnerExists bool // GetRunnerByName: true -> record present, false -> (nil, nil)
	getErr       error
}

func (f *fakeScalerClient) GenerateJitRunnerConfig(_ context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, setting.Name)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: 7000 + len(f.minted), Name: setting.Name},
		EncodedJITConfig: "jit-blob-for-" + setting.Name,
	}, nil
}

func (f *fakeScalerClient) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.runnerExists {
		return &scaleset.RunnerReference{ID: 1, Name: name}, nil
	}
	return nil, nil
}

func (f *fakeScalerClient) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeScalerClient) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.removed...)
}

// newRemoteScaler builds a scaler wired to the given fake ssh binary with
// test-speed remote polling.
func newRemoteScaler(t *testing.T, client scalerClient, pool *slotPool, sshBin string) *Scaler {
	t.Helper()
	cfg := &config{
		maxRunners:      pool.size(),
		minRunners:      0,
		sweepFDB:        true,
		grace:           2 * time.Second,
		jobStartTimeout: time.Minute,
		remoteSSHKey:    "/test/remote_id",
	}
	s := newScaler(discardLogger(), client, 1, cfg, pool)
	s.sshBin = sshBin
	s.sshConnectTimeout = 2 * time.Second
	s.remoteCfg = remoteProcConfig{pollInterval: 30 * time.Millisecond, probeTimeout: 5 * time.Second, unreachableLimit: 3}
	s.unhealthyCooldown = 150 * time.Millisecond
	return s
}

// remotePool builds a 2-slot pool: slot 0 local (from a template), slot 1 on
// the given "host" with the given remote dirs (local temp paths, reached
// through the fake ssh).
func remotePool(t *testing.T, host, remoteRunner, remoteWork string) *slotPool {
	t.Helper()
	p, err := newSlotPool(t.TempDir(), templateRunner(t), 2, remoteSlotConfig{
		hosts:     []string{host},
		runnerDir: remoteRunner,
		workBase:  remoteWork,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func takeSlot(t *testing.T, p *slotPool, index int) *slot {
	t.Helper()
	var held []*slot
	for {
		s := p.take()
		if s == nil {
			t.Fatalf("slot %d not available", index)
		}
		if s.index == index {
			for _, h := range held {
				p.give(h)
			}
			return s
		}
		held = append(held, s)
	}
}

func argvLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestSlotPoolRemoteSlots pins the slot topology: slot 0 local (cloned runner
// dir), slots 1..n remote with on-host paths and NO local directories — the
// local workBase/slot-* scan of reconcileStrayRunners must never see them.
func TestSlotPoolRemoteSlots(t *testing.T) {
	t.Parallel()

	wb := t.TempDir()
	p, err := newSlotPool(wb, templateRunner(t), 3, remoteSlotConfig{
		hosts:     []string{"runner@10.0.0.2", "runner@10.0.0.3"},
		runnerDir: "/home/runner/actions-runner",
		workBase:  "/mnt/ci-data/bazelwork",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.size() != 3 {
		t.Fatalf("size = %d, want 3", p.size())
	}
	if p.all[0].host != "" {
		t.Fatalf("slot 0 must be local, got host %q", p.all[0].host)
	}
	for i, wantHost := range []string{"runner@10.0.0.2", "runner@10.0.0.3"} {
		sl := p.all[i+1]
		if sl.host != wantHost {
			t.Fatalf("slot %d host = %q, want %q", i+1, sl.host, wantHost)
		}
		if sl.runnerDir != "/home/runner/actions-runner" || sl.path != "/mnt/ci-data/bazelwork/slot-0" {
			t.Fatalf("slot %d remote paths = (%q, %q)", i+1, sl.runnerDir, sl.path)
		}
		if _, err := os.Stat(filepath.Join(wb, fmt.Sprintf("slot-%d", i+1))); !os.IsNotExist(err) {
			t.Fatalf("remote slot %d must not create a local slot dir", i+1)
		}
	}
}

// TestSlotPoolUnhealthySkipAndRevive pins the capacity contract: an unhealthy
// slot is not handed out until its cooldown expires, then it is.
func TestSlotPoolUnhealthySkipAndRevive(t *testing.T) {
	t.Parallel()

	p, err := newSlotPool(t.TempDir(), templateRunner(t), 1, remoteSlotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sl := p.take()
	p.markUnhealthy(sl, time.Now().Add(80*time.Millisecond))
	if got := p.take(); got != nil {
		t.Fatalf("take returned unhealthy slot %d before cooldown", got.index)
	}
	waitFor(t, 2*time.Second, func() bool { return p.take() != nil })
}

// TestRemoteLaunchArgvAndJitDelivery pins the launch mechanics end-to-end
// through the real wrapper script: ssh gets BatchMode/ConnectTimeout/
// accept-new/-i/host, the JIT blob travels via STDIN into the runner's
// environment (never argv), the session writes its pidfile, and when the
// runner exits the liveness poll reclaims the slot and the idle sweep runs on
// THAT host over ssh.
func TestRemoteLaunchArgvAndJitDelivery(t *testing.T) {
	t.Parallel()

	sshBin, argvLog := fakeSSH(t)
	rdir := remoteRunnerDir(t, `printf '%s' "$ACTIONS_RUNNER_INPUT_JITCONFIG" > jit.txt
sleep 0.2`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	client := &fakeScalerClient{runnerExists: true}
	s := newRemoteScaler(t, client, pool, sshBin)

	sl := takeSlot(t, pool, 1)
	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("remote launch: %v", err)
	}

	// Launch argv shape: options, identity, host, all recorded by the fake.
	lines := argvLines(t, argvLog)
	if len(lines) == 0 {
		t.Fatal("fake ssh never invoked")
	}
	launchLine := lines[0]
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=2", "StrictHostKeyChecking=accept-new", "-i /test/remote_id", "runner@10.0.0.2"} {
		if !strings.Contains(launchLine, want) {
			t.Fatalf("launch argv missing %q: %s", want, launchLine)
		}
	}
	if strings.Contains(launchLine, "jit-blob-for-") {
		t.Fatalf("JIT config leaked into argv: %s", launchLine)
	}

	// The blob reached the runner's environment via stdin -> export.
	jitFile := filepath.Join(rdir, "jit.txt")
	waitFor(t, 5*time.Second, func() bool {
		b, err := os.ReadFile(jitFile)
		return err == nil && strings.HasPrefix(string(b), "jit-blob-for-bazelscaleset-s1-")
	})

	// The session recorded its own pid AND the runner's name (the second line is
	// what makes the session adoptable by a later incarnation).
	pidData, err := os.ReadFile(filepath.Join(rwork, "slot-0", runnerPIDFile))
	if err != nil {
		t.Fatalf("remote pidfile missing: %v", err)
	}
	pid, name := parseRunnerPIDFile(pidData)
	if pid <= 1 {
		t.Fatalf("remote pidfile content %q", pidData)
	}
	if !strings.HasPrefix(name, "bazelscaleset-s1-") {
		t.Fatalf("remote pidfile name line = %q, want the runner name", name)
	}

	// Runner exits on its own; the liveness poll reaps it and returns its slot.
	// This fake runner never took a job, so the slot comes back BENCHED — the
	// remote path must account for a no-job exit exactly as the local one does.
	waitFor(t, 10*time.Second, slotReturnedBenched(pool, 1))

	// The idle sweep ran remotely on the runner's host.
	waitFor(t, 5*time.Second, func() bool {
		for _, l := range argvLines(t, argvLog) {
			if strings.Contains(l, "runner@10.0.0.2") && strings.Contains(l, "foundationdb/foundationdb") {
				return true
			}
		}
		return false
	})
}

// TestRemoteRunnerSurvivesSSHDrop pins the detachment property: the launch ssh
// returns while the runner keeps running in its own session, and the runner
// completes its work with no ssh connection attached — the supervisor's
// connection dropping mid-job must never kill the job.
func TestRemoteRunnerSurvivesSSHDrop(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSH(t)
	rdir := remoteRunnerDir(t, `sleep 0.3
echo done > completed.txt`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)

	sl := takeSlot(t, pool, 1)
	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("remote launch: %v", err)
	}
	// launch has returned: the launching ssh process is gone. The runner must
	// still be alive right now (mid-sleep)...
	pidData, err := os.ReadFile(filepath.Join(rwork, "slot-0", runnerPIDFile))
	if err != nil {
		t.Fatalf("remote pidfile missing: %v", err)
	}
	pid, _ := parseRunnerPIDFile(pidData)
	if !alive(pid) {
		t.Fatal("runner died with the launch ssh — not detached into its own session")
	}
	// ...and complete on its own.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(rdir, "completed.txt"))
		return err == nil
	})
}

// TestRemoteKillSignalsSessionGroup pins the kill path: signalling a remote
// runner kills the whole detached session (run.sh and its children), observed
// as the recorded pid dying and the slot being reclaimed.
func TestRemoteKillSignalsSessionGroup(t *testing.T) {
	t.Parallel()

	sshBin, argvLog := fakeSSH(t)
	rdir := remoteRunnerDir(t, `sleep 300`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)

	sl := takeSlot(t, pool, 1)
	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("remote launch: %v", err)
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

	r.proc.signal(syscall.SIGKILL)

	waitFor(t, 5*time.Second, func() bool { return !alive(r.proc.pid()) })
	// The liveness poll notices and returns the slot, benched: this runner was
	// killed before it ever reported JobStarted, so its launch produced nothing.
	waitFor(t, 10*time.Second, slotReturnedBenched(pool, 1))
	// And the kill went over ssh as a process-group kill.
	found := false
	for _, l := range argvLines(t, argvLog) {
		if strings.Contains(l, "runner@10.0.0.2") && strings.Contains(l, `kill -9 -- "-$pid"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no remote group-kill recorded in ssh argv")
	}
}

// TestShutdownEscalatesRemoteRunner pins TERM -> grace -> KILL for a remote
// runner that ignores TERM: shutdown must still get the slot's host clean.
func TestShutdownEscalatesRemoteRunner(t *testing.T) {
	t.Parallel()

	sshBin, argvLog := fakeSSH(t)
	rdir := remoteRunnerDir(t, `trap '' TERM
sleep 300 &
wait`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)
	s.grace = 300 * time.Millisecond

	sl := takeSlot(t, pool, 1)
	if err := s.launch(context.Background(), sl); err != nil {
		t.Fatalf("remote launch: %v", err)
	}
	pidData, err := os.ReadFile(filepath.Join(rwork, "slot-0", runnerPIDFile))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := parseRunnerPIDFile(pidData)

	s.shutdown()

	if alive(pid) {
		t.Fatal("remote runner survived shutdown escalation")
	}
	var sawTerm, sawKill bool
	for _, l := range argvLines(t, argvLog) {
		if strings.Contains(l, `kill -15 -- "-$pid"`) {
			sawTerm = true
		}
		if strings.Contains(l, `kill -9 -- "-$pid"`) {
			sawKill = true
		}
	}
	if !sawTerm || !sawKill {
		t.Fatalf("expected TERM then KILL over ssh, saw term=%v kill=%v", sawTerm, sawKill)
	}
}

// TestRemoteLaunchFailureBenchesSlot pins the host-health contract: an
// unreachable host must not crash the supervisor (no error out of
// HandleDesiredRunnerCount — an error there exits listener.Run) and must not
// count toward capacity until its cooldown expires; the ghost JIT registration
// is removed server-side.
func TestRemoteLaunchFailureBenchesSlot(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSHFailing(t)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", "/home/runner/actions-runner", rwork)
	client := &fakeScalerClient{runnerExists: true}
	s := newRemoteScaler(t, client, pool, sshBin)

	// Pin the local slot so only the remote slot can serve the acquisition.
	local := takeSlot(t, pool, 0)
	defer pool.give(local)

	current, err := s.HandleDesiredRunnerCount(context.Background(), 1)
	if err != nil {
		t.Fatalf("a dead pool box must not error out the listener: %v", err)
	}
	if current != 0 {
		t.Fatalf("running = %d, want 0 (launch failed)", current)
	}
	// The dead host's slot is benched: no capacity during the cooldown.
	if got := pool.take(); got != nil {
		t.Fatalf("unhealthy remote slot %d handed out during cooldown", got.index)
	}
	// The never-launched JIT registration was cleaned up.
	if ids := client.removedIDs(); len(ids) != 1 {
		t.Fatalf("RemoveRunner calls = %v, want exactly one", ids)
	}
	// After the cooldown the host gets another chance.
	waitFor(t, 2*time.Second, func() bool {
		got := pool.take()
		if got == nil {
			return false
		}
		defer pool.give(got)
		return got.index == 1
	})
}

// TestAdoptRemoteLiveRunner pins the remote restart-adopts-live-runner
// contract: a runner session recorded (pid + name) on a pool box and still
// alive across a supervisor restart is ADOPTED — left running, tracked under
// its recorded name, its slot occupied — and its exit frees the slot through
// the normal remote liveness poll.
func TestAdoptRemoteLiveRunner(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSH(t)
	rdir := remoteRunnerDir(t, `while true; do sleep 1; done`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)

	stray := startStrayRunner(t, rdir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	slotDir := filepath.Join(rwork, "slot-0")
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(slotDir, runnerPIDFile)
	const name = "bazelscaleset-s1-99-6"
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(pid)+"\n"+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.adoptRemoteStrays()

	if !alive(pid) {
		t.Fatal("remote adoption killed the live runner — the restart-kills-jobs bug")
	}
	s.mu.Lock()
	_, tracked := s.running[name]
	s.mu.Unlock()
	if !tracked {
		t.Fatalf("adopted remote runner not tracked under its recorded name %q", name)
	}
	if got := pool.takeIndex(1); got != nil {
		t.Fatal("remote slot handed out while its adopted runner is alive")
	}
	// The pidfile survives adoption (it is the session's own record).
	if _, err := os.Stat(pidfile); err != nil {
		t.Fatal("remote pidfile removed by adoption")
	}
	// The runner exits; the liveness poll frees the slot.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_, _ = stray.Process.Wait()
	waitFor(t, 10*time.Second, func() bool {
		got := pool.takeIndex(1)
		if got == nil {
			return false
		}
		pool.give(got)
		return true
	})
}

// TestAdoptRemoteKillsLegacyNamelessPidfile pins the remote transition path: a
// pidfile in the pre-adoption single-line format has no runner name, so the
// session cannot be tracked; it gets the old kill-reconcile and the record is
// cleared, before the slot advertises capacity.
func TestAdoptRemoteKillsLegacyNamelessPidfile(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSH(t)
	rdir := remoteRunnerDir(t, `while true; do sleep 1; done`)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", rdir, rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)

	// A stray runner-looking process group, recorded in the legacy format.
	stray := startStrayRunner(t, rdir)
	pid := stray.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _, _ = stray.Process.Wait() })
	slotDir := filepath.Join(rwork, "slot-0")
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(slotDir, runnerPIDFile)
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	s.adoptRemoteStrays()

	// The stray is a direct child of this test: it stays a zombie (which still
	// answers kill(pid, 0)) until reaped, so death is observed via Wait.
	done := make(chan struct{})
	go func() { _, _ = stray.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy nameless remote stray was not killed")
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatal("legacy remote pidfile was not cleared")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (nameless session cannot be adopted)", s.count())
	}
}

// TestAdoptRemoteSparesNonRunnerPID pins the pid-reuse guard: a recorded pid
// now worn by a non-runner process is left alive and not adopted (only the
// stale pidfile is cleared, by the inspect script itself).
func TestAdoptRemoteSparesNonRunnerPID(t *testing.T) {
	t.Parallel()

	sshBin, _ := fakeSSH(t)
	rwork := filepath.Join(t.TempDir(), "rwork")
	pool := remotePool(t, "runner@10.0.0.2", "/home/runner/actions-runner", rwork)
	s := newRemoteScaler(t, &fakeScalerClient{runnerExists: true}, pool, sshBin)

	sleeper := startSleeper(t)
	slotDir := filepath.Join(rwork, "slot-0")
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(slotDir, runnerPIDFile)
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(sleeper)+"\nbazelscaleset-s1-99-7\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.adoptRemoteStrays()

	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatal("startup did not clear the stale remote pidfile")
	}
	if !alive(sleeper) {
		t.Fatal("startup killed a non-runner process — cmdline guard failed")
	}
	if s.count() != 0 {
		t.Fatalf("running = %d, want 0 (a reused non-runner pid must not be adopted)", s.count())
	}
}
