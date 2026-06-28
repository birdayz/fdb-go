package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

	runnerDir string // dir containing run.sh (the extracted actions/runner)
	sweepFDB  bool
	grace     time.Duration

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
	busy bool
}

var _ listener.Scaler = (*Scaler)(nil)

func newScaler(logger *slog.Logger, client *scaleset.Client, scaleSetID int, cfg *config, pool *slotPool) *Scaler {
	return &Scaler{
		logger:     logger,
		client:     client,
		scaleSetID: scaleSetID,
		minRunners: cfg.minRunners,
		maxRunners: cfg.maxRunners,
		runnerDir:  cfg.runnerDir,
		sweepFDB:   cfg.sweepFDB,
		grace:      cfg.grace,
		pool:       pool,
		nonce:      time.Now().Unix(),
		running:    make(map[string]*runner),
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

	r := &runner{name: name, slot: sl, cmd: cmd}
	s.mu.Lock()
	s.running[name] = r
	s.mu.Unlock()

	s.logger.Info("runner started",
		slog.String("name", name),
		slog.Int("slot", sl.index),
		slog.Int("pid", cmd.Process.Pid))

	s.wg.Add(1)
	go s.wait(r)
	return nil
}

// wait reaps a runner process, frees its slot, and sweeps orphaned FDB
// testcontainers when no other runner remains (so a concurrent job's containers
// are never touched).
func (s *Scaler) wait(r *runner) {
	defer s.wg.Done()

	err := r.cmd.Wait()
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
