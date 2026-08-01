// Command bazelscaleset supervises a GitHub Actions runner scale set on a single
// self-hosted box. It launches the stock actions/runner as native JIT-ephemeral
// subprocesses (one job per process) pinned to warm bazel work-slots, replacing
// the wedge-prone classic register-and-listen runner. See rfcs/155-bazelscaleset.md.
//
// It lives in its own Go module so the scaleset dependency closure (golang-jwt,
// google/uuid, hashicorp/go-retryablehttp) never enters the FDB module.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bazelscaleset: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}
	logger := cfg.logger()

	// SIGTERM/SIGINT cancel the run context; listener.Run returns and the deferred
	// teardown (kill runners, close the message session) executes. The scale set
	// itself is a durable resource and is never deleted here — see
	// ensureRunnerScaleSet's doc comment.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := cfg.newClient()
	if err != nil {
		return fmt.Errorf("creating scaleset client: %w", err)
	}

	groupID := 1 // DefaultRunnerGroup is always group 1.
	if cfg.runnerGroup != scaleset.DefaultRunnerGroup {
		g, err := client.GetRunnerGroupByName(ctx, cfg.runnerGroup)
		if err != nil {
			return fmt.Errorf("looking up runner group %q: %w", cfg.runnerGroup, err)
		}
		groupID = g.ID
	}

	// A scale set is a durable resource: reuse it by name if it already exists
	// (patching in place if its config drifted), create it only if missing. Never
	// delete it here — see ensureRunnerScaleSet's doc comment for the production
	// incident that "delete stale set, then always recreate" caused.
	ss, err := ensureRunnerScaleSet(ctx, client, logger, groupID, &scaleset.RunnerScaleSet{
		Name:          cfg.name,
		RunnerGroupID: groupID,
		Labels:        cfg.labels(),
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return err
	}
	client.SetSystemInfo(systemInfo(ss.ID))

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = fmt.Sprintf("bazelscaleset-%d", ss.ID)
	}

	session, err := client.MessageSessionClient(ctx, ss.ID, hostname)
	if err != nil {
		return fmt.Errorf("opening message session: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	pool, err := newSlotPool(cfg.workBase, cfg.runnerDir, cfg.maxRunners, remoteSlotConfig{
		hosts:     cfg.remoteHosts,
		runnerDir: cfg.remoteRunnerDir,
		workBase:  cfg.remoteWorkBase,
	})
	if err != nil {
		return fmt.Errorf("creating slot pool: %w", err)
	}
	logger.Info("warm slot pool ready",
		slog.Int("slots", pool.size()),
		slog.Int("remote", len(cfg.remoteHosts)),
		slog.String("base", cfg.workBase))

	// Initial heartbeat so the watchdog sees a healthy start before the first poll.
	writeHeartbeat(cfg.heartbeatFile)

	scaler := newScaler(logger.WithGroup("scaler"), client, ss.ID, cfg, pool)
	defer finishRunners(ctx, scaler)

	// A previous incarnation may have left runners behind — deliberately (a
	// detach exit for restart) or by crashing. Adopt the live ones and reap the
	// dead records BEFORE advertising capacity: a new runner launched into a
	// slot a live job is still writing would corrupt that job's checkout, and a
	// live runner adopted mid-job must never be killed for a supervisor
	// restart. This runs after newSlotPool on purpose: the clone re-sync is
	// safe over a live adopted runner (copyFile unlinks before writing, so an
	// executing binary keeps its old inode — no ETXTBSY — and runtime-state
	// dirs are excluded from the copy).
	scaler.adoptOrReapStrayRunners()
	if len(cfg.remoteHosts) > 0 {
		scaler.adoptRemoteStrays()
	}

	// Bound each long-poll: the scaleset session is now the only long-lived loop in
	// this design, so a half-open poll ("online but not pulling jobs") would be the
	// very wedge RFC-155 removes. On timeout listener.Run returns and the supervisor
	// exits for systemd to restart with a fresh session; each successful poll also
	// stamps the heartbeat file for the external watchdog.
	lis, err := listener.New(&timeoutClient{inner: session, pollTimeout: cfg.pollTimeout, heartbeatFile: cfg.heartbeatFile}, listener.Config{
		ScaleSetID: ss.ID,
		MaxRunners: cfg.maxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}

	logger.Info("listening for jobs",
		slog.String("scaleSet", cfg.name),
		slog.Int("maxRunners", cfg.maxRunners),
		slog.Int("minRunners", cfg.minRunners))
	if err := lis.Run(ctx, scaler); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener: %w", err)
	}
	logger.Info("shutting down")
	return nil
}

// finishRunners decides what a supervisor exit does to its runners. A canceled
// ctx means SIGTERM/SIGINT — systemctl stop, the operator wants the box quiet —
// so runners get the TERM/grace/KILL escalation. Any other exit (poll timeout,
// listener error, JIT-mint error) is a self-exit so systemd restarts us with a
// fresh session: the runners are independent processes mid-job or
// warm-listening, and they are left running for the next incarnation to adopt.
// Killing them on that path canceled in-flight CI jobs on every quiet-poll
// timeout. NOTE: the systemd unit must run with KillMode=process (and
// OOMPolicy=continue) for this to hold — with the default control-group kill
// mode, systemd itself would kill the local runner the moment the main process
// exits, no matter what this function does.
func finishRunners(ctx context.Context, s *Scaler) {
	if ctx.Err() != nil {
		s.shutdown()
	} else {
		s.detach()
	}
}

func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "bazelscaleset",
		Subsystem:  "supervisor",
		Version:    version,
		CommitSHA:  "NA",
		ScaleSetID: scaleSetID,
	}
}

// config is the fully-parsed configuration. Non-secret values come from flags
// (with env fallbacks); secrets (app private key, PAT) come from env only so they
// never appear in the process argv / table.
type config struct {
	url              string
	name             string
	labelList        []string
	runnerGroup      string
	maxRunners       int
	minRunners       int
	runnerDir        string
	workBase         string
	sweepFDB         bool
	grace            time.Duration
	pollTimeout      time.Duration
	jobStartTimeout  time.Duration
	jobTerminalGrace time.Duration
	heartbeatFile    string
	logLevel         string
	logFormat        string

	// Remote pool slots: slot 0 stays local, slot i>0 runs on remoteHosts[i-1]
	// over SSH (see remote.go).
	remoteHosts     []string
	remoteSSHKey    string
	remoteRunnerDir string
	remoteWorkBase  string

	appClientID       string
	appInstallationID int64
	appPrivateKey     string // secret: BAZELSCALESET_APP_PRIVATE_KEY
	token             string // secret: BAZELSCALESET_TOKEN
}

func parseConfig() (*config, error) {
	return parseConfigArgs(os.Args[1:])
}

func parseConfigArgs(args []string) (*config, error) {
	c := &config{}
	var labels, instID, remoteHosts string
	var showVersion bool

	fs := flag.NewFlagSet("bazelscaleset", flag.ContinueOnError)
	fs.StringVar(&c.url, "url", envOr("BAZELSCALESET_URL", ""), "REQUIRED: GitHub repo/org URL to register the scale set against (e.g. https://github.com/birdayz/fdb-go)")
	fs.StringVar(&c.name, "name", envOr("BAZELSCALESET_NAME", ""), "REQUIRED: scale set name (also the default runs-on label)")
	fs.StringVar(&labels, "labels", envOr("BAZELSCALESET_LABELS", ""), "comma-separated runs-on labels (default: --name)")
	fs.StringVar(&c.runnerGroup, "runner-group", envOr("BAZELSCALESET_RUNNER_GROUP", scaleset.DefaultRunnerGroup), "runner group name")
	fs.IntVar(&c.maxRunners, "max-runners", envIntOr("BAZELSCALESET_MAX_RUNNERS", 1), "max concurrent runners (= number of warm slots)")
	fs.IntVar(&c.minRunners, "min-runners", envIntOr("BAZELSCALESET_MIN_RUNNERS", 0), "min pre-warmed idle runners")
	fs.StringVar(&c.runnerDir, "runner-dir", envOr("BAZELSCALESET_RUNNER_DIR", "/home/runner/actions-runner"), "base/template actions/runner dir (contains run.sh); each slot gets its own clone <runner-dir>-slot<N> so concurrent runners don't share .runner/.credentials")
	fs.StringVar(&c.workBase, "work-base", envOr("BAZELSCALESET_WORK_BASE", "/mnt/ci-data/bazelwork"), "base directory for warm per-slot work folders (keep on the CI data volume, same filesystem as the bazel output_base, not the root disk)")
	fs.BoolVar(&c.sweepFDB, "sweep-fdb", envBoolOr("BAZELSCALESET_SWEEP_FDB", true), "remove orphaned foundationdb/foundationdb containers when the box goes idle")
	fs.DurationVar(&c.grace, "grace-period", envDurOr("BAZELSCALESET_GRACE_PERIOD", 60*time.Second), "shutdown grace period before SIGKILLing in-flight runners")
	fs.DurationVar(&c.pollTimeout, "poll-timeout", envDurOr("BAZELSCALESET_POLL_TIMEOUT", 2*time.Minute), "hard timeout for a single long-poll; on timeout the supervisor exits and systemd restarts it with a fresh session (must exceed the ~50s idle long-poll)")
	fs.DurationVar(&c.jobStartTimeout, "job-start-timeout", envDurOr("BAZELSCALESET_JOB_START_TIMEOUT", 5*time.Minute), "kill a launched runner that never starts a job within this long and reclaim its slot (on-demand only, i.e. min-runners=0; 0 disables)")
	fs.DurationVar(&c.jobTerminalGrace, "job-terminal-grace", envDurOr("BAZELSCALESET_JOB_TERMINAL_GRACE", 10*time.Minute), "kill a runner whose job has been terminal (completed/failed/cancelled) on the GitHub side for longer than this while the runner process is still alive, and reclaim its slot (0 disables)")
	fs.StringVar(&remoteHosts, "remote-hosts", envOr("BAZELSCALESET_REMOTE_HOSTS", ""), "comma-separated user@host list of pool boxes; slot 0 stays local, slot i runs on host i-1 over SSH. When set, --max-runners defaults to 1+len(hosts)")
	fs.StringVar(&c.remoteSSHKey, "remote-ssh-key", envOr("BAZELSCALESET_REMOTE_SSH_KEY", "/etc/bazelscaleset/remote_id"), "SSH identity file for --remote-hosts")
	fs.StringVar(&c.remoteRunnerDir, "remote-runner-dir", envOr("BAZELSCALESET_REMOTE_RUNNER_DIR", "/home/runner/actions-runner"), "extracted actions/runner dir on every remote pool box (cloud-init provisions it)")
	fs.StringVar(&c.remoteWorkBase, "remote-work-base", envOr("BAZELSCALESET_REMOTE_WORK_BASE", "/mnt/ci-data/bazelwork"), "warm work base on every remote pool box (each host runs one slot at <base>/slot-0)")
	fs.StringVar(&c.heartbeatFile, "heartbeat-file", envOr("BAZELSCALESET_HEARTBEAT_FILE", ""), "if set, write a unix-timestamp heartbeat on each successful poll for an external watchdog to check (empty disables)")
	fs.StringVar(&c.logLevel, "log-level", envOr("BAZELSCALESET_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	fs.StringVar(&c.logFormat, "log-format", envOr("BAZELSCALESET_LOG_FORMAT", "text"), "log format (text, json)")
	fs.StringVar(&c.appClientID, "app-client-id", envOr("BAZELSCALESET_APP_CLIENT_ID", ""), "GitHub App client id (or app id)")
	fs.StringVar(&instID, "app-installation-id", envOr("BAZELSCALESET_APP_INSTALLATION_ID", ""), "GitHub App installation id")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// Secrets: env only, never flags (so they never reach the process table / argv).
	c.appPrivateKey = os.Getenv("BAZELSCALESET_APP_PRIVATE_KEY")
	c.token = os.Getenv("BAZELSCALESET_TOKEN")

	if instID != "" {
		v, err := strconv.ParseInt(instID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid --app-installation-id %q: %w", instID, err)
		}
		c.appInstallationID = v
	}
	for _, l := range strings.Split(labels, ",") {
		if t := strings.TrimSpace(l); t != "" {
			c.labelList = append(c.labelList, t)
		}
	}
	for _, h := range strings.Split(remoteHosts, ",") {
		if t := strings.TrimSpace(h); t != "" {
			c.remoteHosts = append(c.remoteHosts, t)
		}
	}

	// With remote hosts and no explicit --max-runners (flag or env), capacity
	// defaults to every box: the local slot plus one slot per pool box.
	if len(c.remoteHosts) > 0 {
		maxSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "max-runners" {
				maxSet = true
			}
		})
		if _, ok := os.LookupEnv("BAZELSCALESET_MAX_RUNNERS"); ok {
			maxSet = true
		}
		if !maxSet {
			c.maxRunners = 1 + len(c.remoteHosts)
		}
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *config) validate() error {
	if _, err := url.ParseRequestURI(c.url); err != nil {
		return fmt.Errorf("invalid --url %q (want e.g. https://github.com/org/repo): %w", c.url, err)
	}
	if c.name == "" {
		return errors.New("--name is required")
	}
	if c.maxRunners < 1 {
		return fmt.Errorf("--max-runners must be >= 1, got %d", c.maxRunners)
	}
	if c.minRunners < 0 || c.minRunners > c.maxRunners {
		return fmt.Errorf("--min-runners must be in [0, max-runners], got %d", c.minRunners)
	}
	if c.runnerDir == "" {
		return errors.New("--runner-dir is required")
	}
	if c.workBase == "" {
		return errors.New("--work-base is required")
	}
	if c.pollTimeout < 60*time.Second {
		// The idle long-poll blocks ~50s before returning; a tighter cap would
		// restart the supervisor on every healthy idle poll.
		return fmt.Errorf("--poll-timeout must be >= 60s, got %s", c.pollTimeout)
	}
	if c.jobStartTimeout < 0 {
		return fmt.Errorf("--job-start-timeout must be >= 0, got %s", c.jobStartTimeout)
	}
	if c.jobTerminalGrace < 0 {
		return fmt.Errorf("--job-terminal-grace must be >= 0, got %s", c.jobTerminalGrace)
	}
	if len(c.remoteHosts) > 0 {
		if c.maxRunners > 1+len(c.remoteHosts) {
			// Slot i>0 maps to remoteHosts[i-1]; slots past the host list would
			// have nowhere to execute.
			return fmt.Errorf("--max-runners %d exceeds 1 local + %d remote slots", c.maxRunners, len(c.remoteHosts))
		}
		for _, h := range c.remoteHosts {
			if strings.ContainsAny(h, " \t") || strings.HasPrefix(h, "-") {
				return fmt.Errorf("invalid --remote-hosts entry %q (want user@host)", h)
			}
		}
		if c.remoteSSHKey == "" {
			return errors.New("--remote-ssh-key is required with --remote-hosts")
		}
		if c.remoteRunnerDir == "" || c.remoteWorkBase == "" {
			return errors.New("--remote-runner-dir and --remote-work-base are required with --remote-hosts")
		}
	}
	hasApp := c.appClientID != "" && c.appInstallationID != 0 && c.appPrivateKey != ""
	if !hasApp && c.token == "" {
		return errors.New("no credentials: set BAZELSCALESET_APP_PRIVATE_KEY (with --app-client-id and --app-installation-id) or BAZELSCALESET_TOKEN")
	}
	return nil
}

func (c *config) newClient() (*scaleset.Client, error) {
	if c.appClientID != "" && c.appInstallationID != 0 && c.appPrivateKey != "" {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: c.url,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       c.appClientID,
				InstallationID: c.appInstallationID,
				PrivateKey:     c.appPrivateKey,
			},
			SystemInfo: systemInfo(0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     c.url,
		PersonalAccessToken: c.token,
		SystemInfo:          systemInfo(0),
	})
}

func (c *config) labels() []scaleset.Label {
	if len(c.labelList) == 0 {
		return []scaleset.Label{{Name: c.name}}
	}
	out := make([]scaleset.Label, len(c.labelList))
	for i, n := range c.labelList {
		out[i] = scaleset.Label{Name: n}
	}
	return out
}

func (c *config) logger() *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(c.logLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.ToLower(c.logFormat) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBoolOr(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDurOr(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
