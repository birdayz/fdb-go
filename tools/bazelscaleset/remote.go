package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Remote slots run the stock actions/runner on a pool box over SSH. There is
// exactly ONE message session per scale set, so the supervisor on the main box
// owns the listener and fans acquired jobs out: slot 0 executes locally
// (unchanged), slot i>0 executes on remote host i-1.
//
// Mechanism, chosen for survivability over elegance:
//   - The system ssh binary (BatchMode, ConnectTimeout, accept-new) — not a Go
//     ssh library. Key auth via --remote-ssh-key; host-key policy stays in
//     ssh_config where it is auditable.
//   - The JIT config travels on STDIN into a remote wrapper script, never in
//     argv (argv is world-readable in the remote process table, and the blob
//     can exceed comfortable single-arg sizes).
//   - The wrapper starts run.sh under setsid in a NEW SESSION with all stdio
//     detached, so the runner survives the supervisor's ssh connection
//     dropping (sshd HUPs the ssh session's process group on disconnect; a
//     separate session is untouchable by that). The session leader's pid is
//     written to a pidfile by the session itself (`echo $$`), which makes the
//     pidfile authoritative even if the launching shell's bookkeeping races.
//   - Liveness is polled with a small ssh probe against the pidfile; kill is
//     a remote `kill -- -pid` of the session's process group. Exit code 0 =
//     alive, exit code 3 = dead, 255 = ssh transport failure (counted, and
//     only a sustained run of them declares the runner lost).

// remoteDeadExit is the liveness probe's "runner is gone" exit code. Distinct
// from 255, which is ssh's own transport-failure code.
const remoteDeadExit = 3

// sshConn is one remote host plus everything needed to exec commands on it.
type sshConn struct {
	bin            string // ssh binary (tests substitute a fake)
	keyFile        string
	host           string // user@host
	connectTimeout time.Duration
}

// args builds the full ssh argv for running remoteCmd on the host.
func (c sshConn) args(remoteCmd string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", max(1, int(c.connectTimeout.Seconds()))),
		"-o", "StrictHostKeyChecking=accept-new",
		"-i", c.keyFile,
		c.host,
		remoteCmd,
	}
}

// run executes remoteCmd on the host with the given stdin and a hard timeout,
// returning combined output. The error preserves ssh's exit code (via
// *exec.ExitError) so callers can tell "remote command said no" from "ssh
// could not reach the host".
func (c sshConn) run(ctx context.Context, timeout time.Duration, stdin string, remoteCmd string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.bin, c.args(remoteCmd)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if cctx.Err() != nil && err != nil {
		// A timed-out probe is a transport failure, not a remote verdict.
		return out.Bytes(), fmt.Errorf("ssh to %s timed out after %s: %w", c.host, timeout, err)
	}
	return out.Bytes(), err
}

// exitCode extracts the process exit code from a run error: -1 for transport /
// non-exit errors, ssh's own 255 for connection failures, and the remote
// command's code otherwise.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// shQuote single-quotes s for safe embedding in a POSIX shell command line.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remotePIDFilePath is where a remote slot's session-leader pid lives on the
// remote host. Same file name as the local slot pidfile, same meaning.
func remotePIDFilePath(sl *slot) string {
	return sl.path + "/" + runnerPIDFile
}

// remoteLaunchScript builds the wrapper that the launch ssh runs on the pool
// box. It reads the JIT config from stdin, exports it (env is private to the
// user, unlike argv), and starts run.sh as the leader of a fresh session whose
// own `echo $$` writes the pidfile — so the recorded pid is exactly the pid
// whose process group kill/reconcile later target. All stdio is detached so
// the launching ssh returns immediately and the runner survives any later
// connection drop. The launch then waits briefly for the pidfile and echoes
// the pid for the supervisor's log.
func remoteLaunchScript(sl *slot) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	work := shQuote(sl.path)
	rdir := shQuote(sl.runnerDir)
	logf := shQuote(sl.path + "/runner.log")
	return `set -e
IFS= read -r jit
[ -n "$jit" ] || { echo "bazelscaleset: no jitconfig on stdin" >&2; exit 64; }
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
mkdir -p ` + work + `
cd ` + rdir + `
setsid /bin/sh -c 'echo $$ > ` + pidfile + `; exec ./run.sh' > ` + logf + ` 2>&1 < /dev/null &
i=0
while [ "$i" -lt 50 ]; do
  [ -s ` + pidfile + ` ] && break
  i=$((i+1)); sleep 0.1
done
pid=$(cat ` + pidfile + ` 2>/dev/null || true)
[ -n "$pid" ] || { echo "bazelscaleset: runner session wrote no pidfile" >&2; exit 65; }
echo "BAZELSCALESET_REMOTE_PID=$pid"
`
}

// remoteLivenessScript probes whether the recorded runner session is still
// alive. The cmdline guard means a recycled pid that is no longer a runner
// reads as dead rather than keeping the slot pinned.
func remoteLivenessScript(sl *slot) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	return `pid=$(cat ` + pidfile + ` 2>/dev/null); [ -n "$pid" ] || exit ` + fmt.Sprint(remoteDeadExit) + `
[ -d "/proc/$pid" ] || exit ` + fmt.Sprint(remoteDeadExit) + `
tr "\0" " " < "/proc/$pid/cmdline" 2>/dev/null | grep -q -e run.sh -e Runner.Listener -e Runner.Worker || exit ` + fmt.Sprint(remoteDeadExit) + `
exit 0
`
}

// remoteKillScript signals the runner session's whole process group.
func remoteKillScript(sl *slot, sig syscall.Signal) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	return `pid=$(cat ` + pidfile + ` 2>/dev/null); [ -n "$pid" ] || exit 0
kill -` + fmt.Sprint(int(sig)) + ` -- "-$pid" 2>/dev/null || true
`
}

// remoteReconcileScript is the remote analogue of reconcileStrayRunners: a
// leftover pidfile from a supervisor incarnation that crashed marks a runner
// nobody is tracking. Kill its whole process group (cmdline-guarded against
// pid reuse) before advertising the slot's capacity again.
func remoteReconcileScript(sl *slot) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	return `[ -f ` + pidfile + ` ] || exit 0
pid=$(cat ` + pidfile + `); rm -f ` + pidfile + `
[ -n "$pid" ] || exit 0
[ -d "/proc/$pid" ] || exit 0
tr "\0" " " < "/proc/$pid/cmdline" 2>/dev/null | grep -q -e run.sh -e Runner.Listener -e Runner.Worker || exit 0
kill -9 -- "-$pid" 2>/dev/null || true
echo "reaped stray remote runner pid $pid"
`
}

// remoteSweepScript is the remote analogue of sweepOrphanFDB, run when a
// remote slot goes idle: a dead test on the pool box can leak
// foundationdb/foundationdb containers exactly like on the main box, and each
// pins ~700 MB of RSS. No runner is active on the host when this runs (one
// slot per host), so anything matching is an orphan.
const remoteSweepScript = `docker ps --format '{{.ID}} {{.Image}}' 2>/dev/null | while read -r id image; do
  case "$image" in
    foundationdb/foundationdb*) docker rm -fv "$id" 2>/dev/null || true ;;
  esac
done
exit 0
`

// remoteProcConfig bundles the tunables for a remoteProc; tests shrink the
// intervals.
type remoteProcConfig struct {
	pollInterval     time.Duration // gap between liveness probes
	probeTimeout     time.Duration // hard ceiling per probe
	unreachableLimit int           // consecutive transport failures before the runner is declared lost
}

// remoteProc tracks one runner session on a remote host. It implements the
// same surface as a local process-group child: wait until gone, signal the
// group. Liveness is polled — there is nothing to Wait(2) on across ssh, and
// polling is precisely what survives the supervisor's connection dropping.
type remoteProc struct {
	conn   sshConn
	sl     *slot
	pidNum int
	cfg    remoteProcConfig
	logger *slog.Logger
}

func (p *remoteProc) pid() int { return p.pidNum }

// wait polls the remote session until it is gone. A sustained run of ssh
// transport failures (host down, network partition) also ends the wait: the
// slot must not stay occupied forever for a host that no longer exists. If
// the host was merely partitioned, the runner finishes its one JIT job on its
// own and exits — reclaiming the slot early never double-runs a job.
func (p *remoteProc) wait() error {
	unreachable := 0
	for {
		time.Sleep(p.cfg.pollInterval)
		_, err := p.conn.run(context.Background(), p.cfg.probeTimeout, "", remoteLivenessScript(p.sl))
		switch code := exitCode(err); {
		case code == 0:
			unreachable = 0
		case code == remoteDeadExit:
			return nil // runner exited (or pid recycled to a non-runner)
		default:
			// 255 (ssh transport), -1 (timeout/other): the HOST is unreachable,
			// which says nothing about the runner. Only a sustained run gives up.
			unreachable++
			if unreachable >= p.cfg.unreachableLimit {
				return fmt.Errorf("remote runner on %s unreachable for %d consecutive probes: %w",
					p.conn.host, unreachable, err)
			}
		}
	}
}

// signal signals the remote runner session's process group. Failures are
// logged, not returned: the caller's escalation (TERM, grace, KILL) and the
// liveness poll are the actual enforcement.
func (p *remoteProc) signal(sig syscall.Signal) {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.probeTimeout)
	defer cancel()
	if out, err := p.conn.run(ctx, p.cfg.probeTimeout, "", remoteKillScript(p.sl, sig)); err != nil {
		p.logger.Warn("remote signal failed",
			slog.String("host", p.conn.host),
			slog.Int("pid", p.pidNum),
			slog.String("signal", sig.String()),
			slog.String("output", strings.TrimSpace(string(out))),
			slog.Any("err", err))
	}
}

// launchRemote mints nothing itself — the caller already has the JIT config —
// it delivers the config over stdin and starts the detached runner session,
// returning a proc that tracks it.
func launchRemote(ctx context.Context, conn sshConn, sl *slot, jitConfig string, cfg remoteProcConfig, logger *slog.Logger) (*remoteProc, error) {
	out, err := conn.run(ctx, 2*conn.connectTimeout+30*time.Second, jitConfig+"\n", remoteLaunchScript(sl))
	if err != nil {
		return nil, fmt.Errorf("remote launch on %s failed (exit %d): %s: %w",
			conn.host, exitCode(err), strings.TrimSpace(string(out)), err)
	}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "BAZELSCALESET_REMOTE_PID="); ok {
			if _, err := fmt.Sscanf(rest, "%d", &pid); err != nil || pid <= 1 {
				return nil, fmt.Errorf("remote launch on %s reported unparsable pid %q", conn.host, rest)
			}
		}
	}
	if pid == 0 {
		return nil, fmt.Errorf("remote launch on %s reported no pid: %s", conn.host, strings.TrimSpace(string(out)))
	}
	return &remoteProc{conn: conn, sl: sl, pidNum: pid, cfg: cfg, logger: logger}, nil
}

// reconcileRemoteStrays reaps, on every remote slot's host, a runner session a
// crashed supervisor incarnation left behind — the remote analogue of
// reconcileStrayRunners, run before the pool starts advertising capacity. An
// unreachable host is logged and skipped: launch-time health marks its slot
// unhealthy the moment it is actually needed.
func reconcileRemoteStrays(logger *slog.Logger, conns map[int]sshConn, pool *slotPool, probeTimeout time.Duration) {
	for _, sl := range pool.all {
		if sl.host == "" {
			continue
		}
		conn := conns[sl.index]
		out, err := conn.run(context.Background(), probeTimeout, "", remoteReconcileScript(sl))
		if err != nil {
			logger.Warn("remote reconcile failed; slot health will be probed at launch",
				slog.String("host", sl.host), slog.Int("slot", sl.index), slog.Any("err", err))
			continue
		}
		if msg := strings.TrimSpace(string(out)); msg != "" {
			logger.Warn("remote reconcile", slog.String("host", sl.host), slog.String("msg", msg))
		}
	}
}
