package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Scaler implements listener.Scaler. It launches the stock actions/runner as a
// native JIT-ephemeral subprocess (one job per process, then it exits) pinned to
// a warm work-slot, and reaps it when it exits. Native (not Docker) so jobs keep
// using the host Docker for FDB testcontainers and share the slot's warm bazel
// state. There is no long-lived listener to wedge — the bug class RFC-155 targets
// is gone structurally.
type Scaler struct {
	logger     *slog.Logger
	client     *scaleset.Client
	scaleSetID int
	minRunners int
	maxRunners int

	runnerDir       string // dir containing run.sh (the extracted actions/runner)
	sweepFDB        bool
	grace           time.Duration
	jobStartTimeout time.Duration

	pool *slotPool

	nonce int64         // per-process base for runner names (unique across restarts)
	seq   atomic.Uint64 // monotonic suffix for runner names

	mu      sync.Mutex
	running map[string]*runner // keyed by JIT runner name
	wg      sync.WaitGroup
}

type runner struct {
	name string
	slot *slot
	cmd  *exec.Cmd
	busy bool          // set true once the runner reports JobStarted
	done chan struct{} // closed by wait once the process is reaped
}

var _ listener.Scaler = (*Scaler)(nil)

func newScaler(logger *slog.Logger, client *scaleset.Client, scaleSetID int, cfg *config, pool *slotPool) *Scaler {
	return &Scaler{
		logger:          logger,
		client:          client,
		scaleSetID:      scaleSetID,
		minRunners:      cfg.minRunners,
		maxRunners:      cfg.maxRunners,
		runnerDir:       cfg.runnerDir,
		sweepFDB:        cfg.sweepFDB,
		grace:           cfg.grace,
		jobStartTimeout: cfg.jobStartTimeout,
		pool:            pool,
		nonce:           time.Now().Unix(),
		running:         make(map[string]*runner),
	}
}

// HandleDesiredRunnerCount ensures up to min(maxRunners, minRunners+count) runner
// processes are alive, launching one per free slot until the target is reached. It
// never scales down here: a JIT runner exits on its own after a single job, and the
// wait goroutine frees its slot (see launch / wait).
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	current := len(s.running)
	s.mu.Unlock()

	target := min(s.maxRunners, s.minRunners+count)
	need := target - current
	if need <= 0 {
		return current, nil
	}

	s.logger.Info("scaling up",
		slog.Int("current", current),
		slog.Int("target", target),
		slog.Int("assignedJobs", count))

	for range need {
		sl := s.pool.take()
		if sl == nil {
			// Invariant: need <= free slots, so this should not happen; guard anyway.
			s.logger.Warn("no free slot for desired runner", slog.Int("target", target))
			break
		}
		if err := s.launch(ctx, sl); err != nil {
			s.pool.give(sl)
			return s.count(), fmt.Errorf("launching runner: %w", err)
		}
	}
	return s.count(), nil
}

func (s *Scaler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// launch mints a JIT runner config bound to the slot's work folder and starts
// run.sh in its own process group. A goroutine waits for it to exit, frees the
// slot, and (when the box goes idle) sweeps orphaned FDB testcontainers.
func (s *Scaler) launch(ctx context.Context, sl *slot) error {
	name := fmt.Sprintf("bazelscaleset-s%d-%d-%d", sl.index, s.nonce, s.seq.Add(1))

	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: sl.path,
	}, s.scaleSetID)
	if err != nil {
		return fmt.Errorf("generating JIT config: %w", err)
	}

	cmd := exec.Command(filepath.Join(s.runnerDir, "run.sh"))
	cmd.Dir = s.runnerDir
	cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit.EncodedJITConfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group so shutdown can signal the whole runner tree
	// (run.sh -> Runner.Listener -> Runner.Worker -> job steps) at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting run.sh: %w", err)
	}

	r := &runner{name: name, slot: sl, cmd: cmd, done: make(chan struct{})}
	s.mu.Lock()
	s.running[name] = r
	s.mu.Unlock()

	// Record the runner's PGID so a crashed supervisor's restart can reap this slot's
	// stray process group (see reconcileStrayRunners). Pid == PGID: Setpgid made the
	// child its own group leader.
	writeRunnerPID(s.logger, sl.path, cmd.Process.Pid)

	s.logger.Info("runner started",
		slog.String("name", name),
		slog.Int("slot", sl.index),
		slog.Int("pid", cmd.Process.Pid))

	s.wg.Add(1)
	go s.wait(r)

	// On-demand runners (min-runners=0) are launched only because a job was
	// assigned and acquired, so one should arrive within seconds. If it does not
	// — e.g. the run was cancelled mid-flight, the churn case that triggered
	// RFC-155 — the runner would idle forever and pin its slot. Reclaim it. With
	// min-runners>0, pre-warmed runners are expected to idle, so this is disabled.
	if s.jobStartTimeout > 0 && s.minRunners == 0 {
		s.wg.Add(1)
		go s.watchJobStart(r)
	}
	return nil
}

// wait reaps a runner process, frees its slot, and sweeps orphaned FDB
// testcontainers when no other runner remains (so a concurrent job's containers
// are never touched).
func (s *Scaler) wait(r *runner) {
	defer s.wg.Done()

	err := r.cmd.Wait()
	close(r.done) // let watchJobStart exit
	removeRunnerPID(r.slot.path)

	s.mu.Lock()
	delete(s.running, r.name)
	remaining := len(s.running)
	s.mu.Unlock()
	s.pool.give(r.slot)

	s.logger.Info("runner exited",
		slog.String("name", r.name),
		slog.Int("slot", r.slot.index),
		slog.Any("err", err))

	if s.sweepFDB && remaining == 0 {
		s.sweepOrphanFDB()
	}
}

// watchJobStart kills a runner that never picked up a job within jobStartTimeout
// and lets wait reclaim its slot. Without this, a runner launched for a job that
// was cancelled before it connected would idle forever and pin its (only) slot.
func (s *Scaler) watchJobStart(r *runner) {
	defer s.wg.Done()

	timer := time.NewTimer(s.jobStartTimeout)
	defer timer.Stop()

	select {
	case <-r.done:
		return
	case <-timer.C:
		s.mu.Lock()
		busy := r.busy
		s.mu.Unlock()
		if busy {
			return
		}
		s.logger.Warn("runner started no job within timeout; killing and reclaiming slot",
			slog.String("name", r.name),
			slog.Int("slot", r.slot.index),
			slog.Duration("timeout", s.jobStartTimeout))
		s.signalGroup(r.cmd, syscall.SIGKILL)
	}
}

// HandleJobStarted records that the runner picked up its one job. The lifecycle is
// process-driven (the runner exits on its own), so this is bookkeeping only.
func (s *Scaler) HandleJobStarted(_ context.Context, info *scaleset.JobStarted) error {
	s.mu.Lock()
	if r, ok := s.running[info.RunnerName]; ok {
		r.busy = true
	}
	s.mu.Unlock()
	s.logger.Info("job started",
		slog.String("runner", info.RunnerName),
		slog.String("jobId", info.JobID),
		slog.String("job", info.JobDisplayName))
	return nil
}

// HandleJobCompleted records completion. The JIT runner process exits on its own
// afterwards; the wait goroutine frees the slot and sweeps. Nothing to tear down.
func (s *Scaler) HandleJobCompleted(_ context.Context, info *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		slog.String("runner", info.RunnerName),
		slog.String("jobId", info.JobID),
		slog.String("result", info.Result))
	return nil
}

// shutdown signals every running runner's process group to stop, waits up to the
// grace period for them to exit, then force-kills any stragglers. Called after
// listener.Run returns (i.e. on SIGTERM/SIGINT).
func (s *Scaler) shutdown() {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.running))
	for _, r := range s.running {
		runners = append(runners, r)
	}
	s.mu.Unlock()

	if len(runners) == 0 {
		return
	}

	s.logger.Info("shutting down runners",
		slog.Int("count", len(runners)),
		slog.Duration("grace", s.grace))
	for _, r := range runners {
		s.signalGroup(r.cmd, syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.Info("all runners exited gracefully")
		return
	case <-time.After(s.grace):
		s.logger.Warn("grace period elapsed, killing runners")
	}

	s.mu.Lock()
	for _, r := range s.running {
		s.signalGroup(r.cmd, syscall.SIGKILL)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// signalGroup sends sig to the whole process group of cmd (Setpgid was set at
// launch), so the runner and every child it spawned are signalled together.
func (s *Scaler) signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		s.logger.Warn("signalling runner group failed",
			slog.Int("pid", cmd.Process.Pid),
			slog.String("signal", sig.String()),
			slog.Any("err", err))
	}
}

// sweepOrphanFDB removes any lingering foundationdb/foundationdb containers. It
// runs only when no runner is active, so a dead test's leaked FDB container is an
// orphan and removing it cannot disturb a concurrent job. It never touches the
// bazel cache. This replaces the cloud-init orphan-fdb-sweep timer; see RFC-155 §5.
func (s *Scaler) sweepOrphanFDB() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.ID}} {{.Image}}").Output()
	if err != nil {
		s.logger.Warn("fdb sweep: docker ps failed", slog.Any("err", err))
		return
	}

	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.HasPrefix(fields[1], "foundationdb/foundationdb") {
			ids = append(ids, fields[0])
		}
	}
	if len(ids) == 0 {
		return
	}

	s.logger.Warn("sweeping orphaned FDB testcontainers", slog.Int("count", len(ids)))
	if err := exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, ids...)...).Run(); err != nil {
		s.logger.Warn("fdb sweep: docker rm failed", slog.Any("err", err))
	}
}

// runnerPIDFile is written into a slot dir with the launched runner's PGID while a
// runner occupies the slot, and removed when it exits cleanly. A leftover file
// therefore marks a slot whose runner the previous supervisor incarnation crashed
// out from under.
const runnerPIDFile = ".bazelscaleset-runner.pid"

func writeRunnerPID(logger *slog.Logger, slotPath string, pid int) {
	p := filepath.Join(slotPath, runnerPIDFile)
	if err := os.WriteFile(p, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		logger.Warn("could not write runner pid file", slog.String("path", p), slog.Any("err", err))
	}
}

func removeRunnerPID(slotPath string) {
	_ = os.Remove(filepath.Join(slotPath, runnerPIDFile))
}

// reconcileStrayRunners kills the leftover runner of any slot whose pid file
// survived — i.e. a runner the previous incarnation crashed out from under before
// it could clean up. We must do this before accepting work: a new runner launched
// into a slot a stray job is still writing would corrupt that job's checkout.
//
// It kills the whole process GROUP (negative pid) so the stray run.sh AND its
// children (Runner.Listener / Runner.Worker / the job's bazel *client*) all die,
// not just the named process. The bazel *server* lives in its own session and
// survives, staying warm (killing the client already releases the output_base
// lock — no `bazel shutdown`). It is scoped to *our* slot pid files, so it never
// touches a classic or other runner sharing the host (the side-by-side migration).
// A cmdline check guards against the recorded PID having been reused since the crash.
func reconcileStrayRunners(logger *slog.Logger, pool *slotPool, runnerDir string) {
	for _, sl := range pool.all {
		p := filepath.Join(sl.path, runnerPIDFile)
		data, err := os.ReadFile(p)
		if err != nil {
			continue // no stray recorded for this slot — the normal, healthy case
		}
		_ = os.Remove(p)

		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			continue
		}
		if !cmdlineMatchesRunner(procCmdline(pid), runnerDir) {
			continue // process gone, or the PID was reused by something unrelated
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			logger.Warn("reconcile: kill stray runner group failed", slog.Int("pgid", pid), slog.Any("err", err))
			continue
		}
		logger.Warn("reconciled stray runner from a previous incarnation",
			slog.Int("slot", sl.index), slog.Int("pgid", pid))
	}
}

func procCmdline(pid int) []byte {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	return data
}

// cmdlineMatchesRunner reports whether a /proc cmdline (NUL-separated argv) looks
// like one of our runner processes, so reconcile won't SIGKILL a reused PID.
func cmdlineMatchesRunner(cmdline []byte, runnerDir string) bool {
	if len(cmdline) == 0 {
		return false
	}
	cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
	return strings.Contains(cmd, runnerDir) || strings.Contains(cmd, "Runner.Listener") || strings.Contains(cmd, "Runner.Worker")
}

// writeHeartbeat atomically records "the supervisor's poll loop is making progress"
// as a unix timestamp, for the external systemd watchdog to read (restart on a stale
// heartbeat while capacity is advertised). No-op when no path is configured.
func writeHeartbeat(path string) {
	if path == "" {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// timeoutClient wraps the scaleset message-session client to put a hard ceiling on
// each long-poll. The scaleset session is the only long-lived loop in this design,
// so a half-open poll connection that never returns would reproduce the classic
// "online but not pulling jobs" wedge. On timeout GetMessage returns an error,
// listener.Run returns, and the supervisor exits for systemd to restart it with a
// fresh session.
type timeoutClient struct {
	inner         listener.Client
	pollTimeout   time.Duration
	heartbeatFile string
}

var _ listener.Client = (*timeoutClient)(nil)

func (c *timeoutClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, c.pollTimeout)
	defer cancel()
	msg, err := c.inner.GetMessage(cctx, lastMessageID, maxCapacity)
	if err == nil {
		// A successful poll means the loop is cycling. If a downstream scaler cycle
		// (HandleDesiredRunnerCount) ever deadlocked, the listener would stop reaching
		// the next GetMessage, the heartbeat would go stale, and the watchdog would
		// restart us — so this single signal covers both a half-open poll and a stuck cycle.
		writeHeartbeat(c.heartbeatFile)
	}
	return msg, err
}

func (c *timeoutClient) DeleteMessage(ctx context.Context, messageID int) error {
	return c.inner.DeleteMessage(ctx, messageID)
}

func (c *timeoutClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	return c.inner.AcquireJobs(ctx, requestIDs)
}

func (c *timeoutClient) Session() scaleset.RunnerScaleSetSession {
	return c.inner.Session()
}
