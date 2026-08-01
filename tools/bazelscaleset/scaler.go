package main

import (
	"context"
	"errors"
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

// scalerClient is the subset of *scaleset.Client the scaler needs, as an
// interface so tests can fake JIT minting and runner-state queries.
type scalerClient interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, runnerName string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

var _ scalerClient = (*scaleset.Client)(nil)

// runnerProc abstracts "a launched runner we can wait on and signal": a local
// process group, or a detached session on a remote pool box polled over ssh.
type runnerProc interface {
	pid() int
	wait() error               // blocks until the runner is gone
	signal(sig syscall.Signal) // signals the runner's whole process group
}

// Scaler implements listener.Scaler. It launches the stock actions/runner as a
// native JIT-ephemeral subprocess (one job per process, then it exits) pinned to
// a warm work-slot, and reaps it when it exits. Native (not Docker) so jobs keep
// using the host Docker for FDB testcontainers and share the slot's warm bazel
// state. There is no long-lived listener to wedge — the bug class RFC-155 targets
// is gone structurally. Slots beyond 0 may live on remote pool boxes (see
// remote.go); the scaler is the single listener fanning jobs out to them.
type Scaler struct {
	logger     *slog.Logger
	client     scalerClient
	scaleSetID int
	minRunners int
	maxRunners int

	sweepFDB        bool
	grace           time.Duration
	jobStartTimeout time.Duration

	// Zombie-job watchdog: a runner whose job has been terminal on the GitHub
	// side for longer than jobTerminalGrace is killed and its slot reclaimed
	// (0 disables). terminalPoll is how often a busy runner's server-side
	// record is re-checked when no JobCompleted message arrived.
	jobTerminalGrace time.Duration
	terminalPoll     time.Duration

	// Remote-slot plumbing (empty/zero when no --remote-hosts).
	sshBin            string
	sshKey            string
	sshConnectTimeout time.Duration
	remoteCfg         remoteProcConfig
	unhealthyCooldown time.Duration

	pool *slotPool

	nonce int64         // per-process base for runner names (unique across restarts)
	seq   atomic.Uint64 // monotonic suffix for runner names

	mu      sync.Mutex
	running map[string]*runner // keyed by JIT runner name
	wg      sync.WaitGroup
}

type runner struct {
	name     string
	slot     *slot
	proc     runnerProc
	busy     bool          // set true once the runner reports JobStarted
	watching bool          // terminal watchdog goroutine started
	jobID    string        // the one job this JIT runner picked up (from JobStarted)
	terminal chan struct{} // closed when GitHub reports the job terminal (JobCompleted)
	termOnce sync.Once
	done     chan struct{} // closed by wait once the process is reaped
}

func (r *runner) markTerminal() { r.termOnce.Do(func() { close(r.terminal) }) }

var _ listener.Scaler = (*Scaler)(nil)

func newScaler(logger *slog.Logger, client scalerClient, scaleSetID int, cfg *config, pool *slotPool) *Scaler {
	// Poll a busy runner's server-side state well inside the terminal grace so a
	// missed JobCompleted message still trips the watchdog within ~2 grace periods.
	terminalPoll := max(cfg.jobTerminalGrace/5, 15*time.Second)
	return &Scaler{
		logger:            logger,
		client:            client,
		scaleSetID:        scaleSetID,
		minRunners:        cfg.minRunners,
		maxRunners:        cfg.maxRunners,
		sweepFDB:          cfg.sweepFDB,
		grace:             cfg.grace,
		jobStartTimeout:   cfg.jobStartTimeout,
		jobTerminalGrace:  cfg.jobTerminalGrace,
		terminalPoll:      terminalPoll,
		sshBin:            "ssh",
		sshKey:            cfg.remoteSSHKey,
		sshConnectTimeout: 10 * time.Second,
		remoteCfg: remoteProcConfig{
			pollInterval:     15 * time.Second,
			probeTimeout:     30 * time.Second,
			unreachableLimit: 20, // ~5 min of sustained unreachability before the slot is reclaimed
		},
		unhealthyCooldown: time.Minute,
		pool:              pool,
		nonce:             time.Now().Unix(),
		running:           make(map[string]*runner),
	}
}

// conn builds the ssh connection descriptor for a remote slot.
func (s *Scaler) conn(sl *slot) sshConn {
	return sshConn{bin: s.sshBin, keyFile: s.sshKey, host: sl.host, connectTimeout: s.sshConnectTimeout}
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

	for need > 0 {
		sl := s.pool.take()
		if sl == nil {
			// All remaining slots are busy or unhealthy. The un-launched jobs stay
			// acquired; every subsequent poll re-enters HandleDesiredRunnerCount, so
			// they land as soon as a slot frees or a cooldown expires.
			s.logger.Warn("no free healthy slot for desired runner", slog.Int("target", target))
			break
		}
		err := s.launch(ctx, sl)
		if err == nil {
			need--
			continue
		}
		var mintErr *jitMintError
		if errors.As(err, &mintErr) {
			// JIT minting talks to GitHub, not to the slot's host — failing it is a
			// supervisor-level problem. Propagate so the process exits and systemd
			// restarts it with a fresh session (the same recovery as a broken poll).
			s.pool.give(sl)
			return s.count(), fmt.Errorf("launching runner: %w", err)
		}
		// The slot's host (or local exec) failed. One dead pool box must not crash
		// the supervisor or black-hole the acquisition: bench the slot for a
		// cooldown and let the loop try the remaining slots for this job.
		s.pool.markUnhealthy(sl, time.Now().Add(s.unhealthyCooldown))
		s.logger.Error("slot launch failed; marking slot unhealthy",
			slog.Int("slot", sl.index),
			slog.String("host", sl.host),
			slog.Duration("cooldown", s.unhealthyCooldown),
			slog.Any("err", err))
	}
	return s.count(), nil
}

// jitMintError marks a GenerateJitRunnerConfig failure — a GitHub API problem,
// not a slot-host problem — so HandleDesiredRunnerCount can tell them apart.
type jitMintError struct{ err error }

func (e *jitMintError) Error() string { return e.err.Error() }
func (e *jitMintError) Unwrap() error { return e.err }

func (s *Scaler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// launch mints a JIT runner config bound to the slot's work folder and starts
// run.sh — locally in its own process group, or on the slot's remote host as a
// detached session. A goroutine waits for it to exit, frees the slot, and
// (when the slot's box goes idle) sweeps orphaned FDB testcontainers there.
func (s *Scaler) launch(ctx context.Context, sl *slot) error {
	name := fmt.Sprintf("bazelscaleset-s%d-%d-%d", sl.index, s.nonce, s.seq.Add(1))

	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: sl.path,
	}, s.scaleSetID)
	if err != nil {
		return &jitMintError{fmt.Errorf("generating JIT config: %w", err)}
	}

	var proc runnerProc
	if sl.host == "" {
		proc, err = s.startLocal(sl, jit.EncodedJITConfig)
	} else {
		proc, err = launchRemote(ctx, s.conn(sl), sl, jit.EncodedJITConfig, s.remoteCfg, s.logger)
	}
	if err != nil {
		// The JIT registration exists server-side but its runner never came up;
		// remove it so a dead host doesn't accumulate ghost runner records.
		if jit.Runner != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if rmErr := s.client.RemoveRunner(rctx, int64(jit.Runner.ID)); rmErr != nil {
				s.logger.Warn("removing unlaunched JIT runner failed",
					slog.String("name", name), slog.Any("err", rmErr))
			}
			cancel()
		}
		return err
	}

	r := &runner{name: name, slot: sl, proc: proc, terminal: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.running[name] = r
	s.mu.Unlock()

	// Record the local runner's PGID so a crashed supervisor's restart can reap this
	// slot's stray process group (see reconcileStrayRunners). Pid == PGID: Setpgid
	// made the child its own group leader. Remote slots keep their pidfile on the
	// remote host (written by the session itself; reaped by reconcileRemoteStrays) —
	// recording a REMOTE pid locally would aim reconcile's SIGKILL at whatever local
	// process happens to wear that number.
	if sl.host == "" {
		writeRunnerPID(s.logger, sl.path, proc.pid())
	}

	s.logger.Info("runner started",
		slog.String("name", name),
		slog.Int("slot", sl.index),
		slog.String("host", sl.host),
		slog.Int("pid", proc.pid()))

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

// startLocal starts run.sh from the slot's cloned runner dir in its own process
// group so shutdown can signal the whole runner tree (run.sh -> Runner.Listener
// -> Runner.Worker -> job steps) at once.
func (s *Scaler) startLocal(sl *slot, jitConfig string) (runnerProc, error) {
	cmd := exec.Command(filepath.Join(sl.runnerDir, "run.sh"))
	cmd.Dir = sl.runnerDir
	cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jitConfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting run.sh: %w", err)
	}
	return &localProc{cmd: cmd, logger: s.logger}, nil
}

// localProc adapts a local process-group child to runnerProc.
type localProc struct {
	cmd    *exec.Cmd
	logger *slog.Logger
}

func (p *localProc) pid() int    { return p.cmd.Process.Pid }
func (p *localProc) wait() error { return p.cmd.Wait() }

// signal sends sig to the whole process group (Setpgid was set at launch).
func (p *localProc) signal(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil {
		p.logger.Warn("signalling runner group failed",
			slog.Int("pid", p.cmd.Process.Pid),
			slog.String("signal", sig.String()),
			slog.Any("err", err))
	}
}

// wait reaps a runner, frees its slot, and sweeps orphaned FDB testcontainers
// on the runner's box when no other runner remains THERE (so a concurrent job's
// containers on that box are never touched; other boxes' runners are
// irrelevant — Docker state is per-host).
func (s *Scaler) wait(r *runner) {
	defer s.wg.Done()

	err := r.proc.wait()
	close(r.done) // let watchJobStart / watchTerminal exit
	if r.slot.host == "" {
		removeRunnerPID(r.slot.path)
	}

	s.mu.Lock()
	delete(s.running, r.name)
	remainingOnHost := 0
	for _, other := range s.running {
		if other.slot.host == r.slot.host {
			remainingOnHost++
		}
	}
	s.mu.Unlock()
	s.pool.give(r.slot)

	s.logger.Info("runner exited",
		slog.String("name", r.name),
		slog.Int("slot", r.slot.index),
		slog.String("host", r.slot.host),
		slog.Any("err", err))

	if s.sweepFDB && remainingOnHost == 0 {
		s.sweepOrphanFDB(r.slot)
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
		// Check busy and kill atomically under the lock: a JobStarted that lands while
		// we decide must not be missed. HandleJobStarted sets r.busy under the same lock,
		// so it either wins (we see busy and skip) or loses (we kill before it sets busy).
		// For a REMOTE runner the kill is an ssh round trip held under the lock — that
		// stall is bounded by the probe timeout and only paid on this rare path (a
		// runner that never picked up a job within jobStartTimeout); trading it for a
		// lock-free kill would reopen the missed-JobStarted race.
		s.mu.Lock()
		if r.busy {
			s.mu.Unlock()
			return
		}
		r.proc.signal(syscall.SIGKILL)
		s.mu.Unlock()
		s.logger.Warn("runner started no job within timeout; killed and reclaiming slot",
			slog.String("name", r.name),
			slog.Int("slot", r.slot.index),
			slog.Duration("timeout", s.jobStartTimeout))
	}
}

// HandleJobStarted records that the runner picked up its one job and arms the
// terminal watchdog for it. The normal lifecycle stays process-driven (the
// runner exits on its own); the watchdog only matters when it does not.
func (s *Scaler) HandleJobStarted(_ context.Context, info *scaleset.JobStarted) error {
	s.mu.Lock()
	if r, ok := s.running[info.RunnerName]; ok {
		r.busy = true
		r.jobID = info.JobID
		if s.jobTerminalGrace > 0 && !r.watching {
			r.watching = true
			s.wg.Add(1)
			go s.watchTerminal(r)
		}
	}
	s.mu.Unlock()
	s.logger.Info("job started",
		slog.String("runner", info.RunnerName),
		slog.String("jobId", info.JobID),
		slog.String("job", info.JobDisplayName))
	return nil
}

// HandleJobCompleted records completion — the job is now TERMINAL on the GitHub
// side. The JIT runner process normally exits on its own moments later and the
// wait goroutine frees the slot; marking the runner terminal starts the
// watchdog's grace clock for the case where it does not (a wedged
// Runner.Worker once squatted the only slot for 4+ hours after GitHub had
// already marked its run failed).
func (s *Scaler) HandleJobCompleted(_ context.Context, info *scaleset.JobCompleted) error {
	s.mu.Lock()
	if r, ok := s.running[info.RunnerName]; ok {
		r.markTerminal()
	}
	s.mu.Unlock()
	s.logger.Info("job completed",
		slog.String("runner", info.RunnerName),
		slog.String("jobId", info.JobID),
		slog.String("result", info.Result))
	return nil
}

// watchTerminal kills a runner whose job has been terminal on the GitHub side
// for longer than jobTerminalGrace while the runner process is still alive —
// the zombie that once pinned the only slot for 4+ hours. Two terminal
// signals feed it: the session's JobCompleted message (immediate), and — for
// the case where that message never arrives — a periodic probe of the JIT
// runner's server-side record, which GitHub deletes once the job is terminal
// (GetRunnerByName returns nil, nil for a name with no record).
func (s *Scaler) watchTerminal(r *runner) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.terminalPoll)
	defer ticker.Stop()

	var terminalAt time.Time
	termCh := r.terminal
	for {
		select {
		case <-r.done:
			return // normal lifecycle: the runner exited on its own
		case <-termCh:
			termCh = nil // a closed channel would spin the select
			terminalAt = time.Now()
		case <-ticker.C:
			if terminalAt.IsZero() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				ref, err := s.client.GetRunnerByName(ctx, r.name)
				cancel()
				if err != nil {
					// A failed probe proves nothing; the message session and the
					// next tick both remain as signals.
					continue
				}
				if ref != nil {
					continue // record still exists: job not terminal server-side
				}
				terminalAt = time.Now()
			}
			if time.Since(terminalAt) > s.jobTerminalGrace {
				s.logger.Warn("job terminal on GitHub but runner still alive past grace; killing and reclaiming slot",
					slog.String("name", r.name),
					slog.String("jobId", r.jobID),
					slog.Int("slot", r.slot.index),
					slog.String("host", r.slot.host),
					slog.Duration("grace", s.jobTerminalGrace))
				r.proc.signal(syscall.SIGKILL)
				return // wait() reaps and frees the slot
			}
		}
	}
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
		r.proc.signal(syscall.SIGTERM)
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
		r.proc.signal(syscall.SIGKILL)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// sweepOrphanFDB removes any lingering foundationdb/foundationdb containers on
// the box the given slot runs on (locally, or over ssh for a remote slot). It
// runs only when no runner is active on that box, so a dead test's leaked FDB
// container is an orphan and removing it cannot disturb a concurrent job. It
// never touches the bazel cache. This replaces the cloud-init orphan-fdb-sweep
// timer; see RFC-155 §5.
func (s *Scaler) sweepOrphanFDB(sl *slot) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if sl.host != "" {
		if out, err := s.conn(sl).run(ctx, 60*time.Second, "", remoteSweepScript); err != nil {
			s.logger.Warn("fdb sweep: remote sweep failed",
				slog.String("host", sl.host),
				slog.String("output", strings.TrimSpace(string(out))),
				slog.Any("err", err))
		}
		return
	}

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
//
// It scans EVERY slot dir on disk (workBase/slot-*), not just the current pool: a
// restart with a lower --max-runners than a previous run (e.g. 2 -> 1) would otherwise
// leave a stray runner in a now-out-of-pool higher slot unreaped while the supervisor
// re-advertises that capacity. Slot dirs persist, so the glob covers every slot
// any prior incarnation created.
func reconcileStrayRunners(logger *slog.Logger, workBase, runnerBase string) {
	runnerBase = filepath.Clean(runnerBase)
	matches, _ := filepath.Glob(filepath.Join(workBase, "slot-*"))
	for _, slotPath := range matches {
		p := filepath.Join(slotPath, runnerPIDFile)
		data, err := os.ReadFile(p)
		if err != nil {
			continue // no stray recorded for this slot — the normal, healthy case
		}
		_ = os.Remove(p)

		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			continue
		}
		// Derive this slot's runner dir from its index (slot-N -> <base>-slotN), matching
		// newSlotPool, so the cmdline guard checks the right runner root.
		idx := strings.TrimPrefix(filepath.Base(slotPath), "slot-")
		runnerDir := fmt.Sprintf("%s-slot%s", runnerBase, idx)
		// Decide on group MEMBERS, not just the leader: a process group outlives its
		// leader, so if run.sh exited but a child (Runner.Worker, etc.) is still alive
		// in the group it would keep writing the slot. Checking only the leader's
		// cmdline would miss that. Requiring a runner-like *member* (this slot's own
		// runner dir) also guards against the recorded PGID having been reused.
		if !groupHasRunnerMember(pid, runnerDir) {
			continue
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			logger.Warn("reconcile: kill stray runner group failed", slog.Int("pgid", pid), slog.Any("err", err))
			continue
		}
		logger.Warn("reconciled stray runner from a previous incarnation",
			slog.String("slot", filepath.Base(slotPath)), slog.Int("pgid", pid))
	}
}

func procCmdline(pid int) []byte {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	return data
}

// groupHasRunnerMember reports whether any live process in process group pgid has a
// runner-like cmdline. It scans /proc (rather than trusting the leader) so a group
// whose leader exited but whose Runner.Worker/run.sh child survives is still reaped,
// and so a reused PGID with no runner member is left alone.
func groupHasRunnerMember(pgid int, runnerDir string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		mpid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		if procPGID(mpid) != pgid {
			continue
		}
		if cmdlineMatchesRunner(procCmdline(mpid), runnerDir) {
			return true
		}
	}
	return false
}

// procPGID returns the process group id from /proc/<pid>/stat, or -1. The comm field
// (field 2) can contain spaces and parens, so we parse after the last ')'.
func procPGID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	rparen := strings.LastIndexByte(string(data), ')')
	if rparen < 0 {
		return -1
	}
	// After ')': state(0) ppid(1) pgrp(2) ...
	fields := strings.Fields(string(data)[rparen+1:])
	if len(fields) < 3 {
		return -1
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return -1
	}
	return pgrp
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
