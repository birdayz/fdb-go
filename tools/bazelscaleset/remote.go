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
// whose process group kill/reconcile later target. The session also records the
// runner's NAME as the pidfile's second line: that is what lets a later
// incarnation adopt the still-running session (job messages and the terminal
// watchdog are keyed by runner name). All stdio is detached so the launching
// ssh returns immediately and the runner survives any later connection drop.
// The launch then waits briefly for the pidfile and echoes the pid for the
// supervisor's log.
//
// The stale-pidfile removal and the write-then-rename are what make that wait
// mean anything. A REMOTE pidfile outlives its runner — wait() only unlinks
// local ones, because a remote file has no tracking incarnation to clean it —
// so the previous runner's file is still sitting there, non-empty, when the
// next launch starts. Without the rm, `[ -s pidfile ]` is satisfied on the
// first iteration by that corpse and the loop never waits for the new session
// at all, leaving `head` racing the child's truncate two ways, both bad:
//
//   - head wins: the launch reports the PREVIOUS runner's dead pid. The
//     supervisor tracks that pid, its liveness probe finds nothing runner-like
//     within ~15s, and it logs a clean `runner exited err=<nil>` and frees the
//     slot — while the real runner is alive and untracked on the box.
//   - head lands inside the truncate window: the file is momentarily empty and
//     the launch fails with "runner session wrote no pidfile" (exit 65).
//
// Measured over eight hours: 42 exit-65s across the pool, most of them on the
// LEAST loaded box, which is what finally ruled out disk pressure — the race is
// won more often when the box is fast. Publishing via `mv` (atomic within the
// directory) removes the truncate window outright, so a non-empty pidfile now
// always means "the new session published its pid".
func remoteLaunchScript(sl *slot, name string) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	work := shQuote(sl.path)
	rdir := shQuote(sl.runnerDir)
	logf := shQuote(sl.path + "/runner.log")
	tmpfile := shQuote(remotePIDFilePath(sl) + ".new")
	return `set -e
IFS= read -r jit
[ -n "$jit" ] || { echo "bazelscaleset: no jitconfig on stdin" >&2; exit 64; }
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
mkdir -p ` + work + `
cd ` + rdir + `
rm -f ` + pidfile + ` ` + tmpfile + `
setsid /bin/sh -c '{ echo $$; echo ` + shQuote(name) + `; } > ` + tmpfile + `; mv ` + tmpfile + ` ` + pidfile + `; exec ./run.sh' > ` + logf + ` 2>&1 < /dev/null &
i=0
while [ "$i" -lt 50 ]; do
  [ -s ` + pidfile + ` ] && break
  i=$((i+1)); sleep 0.1
done
pid=$(head -n 1 ` + pidfile + ` 2>/dev/null || true)
[ -n "$pid" ] || { echo "bazelscaleset: runner session wrote no pidfile" >&2; exit 65; }
echo "BAZELSCALESET_REMOTE_PID=$pid"
`
}

// remoteLivenessScript probes whether the recorded runner session is still
// alive. The cmdline guard means a recycled pid that is no longer a runner
// reads as dead rather than keeping the slot pinned.
func remoteLivenessScript(sl *slot) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	return `pid=$(head -n 1 ` + pidfile + ` 2>/dev/null); [ -n "$pid" ] || exit ` + fmt.Sprint(remoteDeadExit) + `
[ -d "/proc/$pid" ] || exit ` + fmt.Sprint(remoteDeadExit) + `
tr "\0" " " < "/proc/$pid/cmdline" 2>/dev/null | grep -q -e run.sh -e Runner.Listener -e Runner.Worker || exit ` + fmt.Sprint(remoteDeadExit) + `
exit 0
`
}

// remoteKillScript signals the runner session's whole process group.
func remoteKillScript(sl *slot, sig syscall.Signal) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	return `pid=$(head -n 1 ` + pidfile + ` 2>/dev/null); [ -n "$pid" ] || exit 0
kill -` + fmt.Sprint(int(sig)) + ` -- "-$pid" 2>/dev/null || true
`
}

// remoteInspectScript reports the recorded runner session on a pool box for
// startup adoption: exit remoteDeadExit when nothing live is recorded (removing
// a stale pidfile on the way — remote pidfiles have no tracking incarnation
// left to clean them), else print the session's pid and recorded name and keep
// the pidfile. The cmdline guard means a recycled pid reads as dead exactly
// like the liveness probe's.
func remoteInspectScript(sl *slot) string {
	pidfile := shQuote(remotePIDFilePath(sl))
	dead := fmt.Sprint(remoteDeadExit)
	return `[ -f ` + pidfile + ` ] || exit ` + dead + `
pid=$(head -n 1 ` + pidfile + ` 2>/dev/null)
name=$(sed -n 2p ` + pidfile + ` 2>/dev/null)
if [ -z "$pid" ] || [ ! -d "/proc/$pid" ] || ! tr "\0" " " < "/proc/$pid/cmdline" 2>/dev/null | grep -q -e run.sh -e Runner.Listener -e Runner.Worker; then
  rm -f ` + pidfile + `
  exit ` + dead + `
fi
echo "BAZELSCALESET_ADOPT_PID=$pid"
echo "BAZELSCALESET_ADOPT_NAME=$name"
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
func launchRemote(ctx context.Context, conn sshConn, sl *slot, name, jitConfig string, cfg remoteProcConfig, logger *slog.Logger) (*remoteProc, error) {
	out, err := conn.run(ctx, 2*conn.connectTimeout+30*time.Second, jitConfig+"\n", remoteLaunchScript(sl, name))
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
