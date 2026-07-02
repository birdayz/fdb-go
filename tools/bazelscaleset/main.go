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

	// A previous incarnation that crashed (rather than shutting down cleanly) may have
	// left a runner still executing from its per-slot clone. Reap it BEFORE building the
	// pool: newSlotPool re-copies into the clones, and truncating a still-running runner
	// binary would fail with ETXTBSY and crash startup before the stray is ever reaped
	// (codex). Reconcile is filesystem-based (scans workBase/slot-*), so it needs no pool.
	// Scoped to our own slot pid files (never touches a classic/other runner) and leaves
	// warm bazel servers alone — a new runner reconnects to them.
	reconcileStrayRunners(logger, cfg.workBase, cfg.runnerDir)

	pool, err := newSlotPool(cfg.workBase, cfg.runnerDir, cfg.maxRunners)
	if err != nil {
		return fmt.Errorf("creating slot pool: %w", err)
	}
	logger.Info("warm slot pool ready", slog.Int("slots", pool.size()), slog.String("base", cfg.workBase))

	// Initial heartbeat so the watchdog sees a healthy start before the first poll.
	writeHeartbeat(cfg.heartbeatFile)

	scaler := newScaler(logger.WithGroup("scaler"), client, ss.ID, cfg, pool)
	defer scaler.shutdown()

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
	url             string
	name            string
	labelList       []string
	runnerGroup     string
	maxRunners      int
	minRunners      int
	runnerDir       string
	workBase        string
	sweepFDB        bool
	grace           time.Duration
	pollTimeout     time.Duration
	jobStartTimeout time.Duration
	heartbeatFile   string
	logLevel        string
	logFormat       string

	appClientID       string
	appInstallationID int64
	appPrivateKey     string // secret: BAZELSCALESET_APP_PRIVATE_KEY
	token             string // secret: BAZELSCALESET_TOKEN
}

func parseConfig() (*config, error) {
	c := &config{}
	var labels, instID string
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
	fs.StringVar(&c.heartbeatFile, "heartbeat-file", envOr("BAZELSCALESET_HEARTBEAT_FILE", ""), "if set, write a unix-timestamp heartbeat on each successful poll for an external watchdog to check (empty disables)")
	fs.StringVar(&c.logLevel, "log-level", envOr("BAZELSCALESET_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	fs.StringVar(&c.logFormat, "log-format", envOr("BAZELSCALESET_LOG_FORMAT", "text"), "log format (text, json)")
	fs.StringVar(&c.appClientID, "app-client-id", envOr("BAZELSCALESET_APP_CLIENT_ID", ""), "GitHub App client id (or app id)")
	fs.StringVar(&instID, "app-installation-id", envOr("BAZELSCALESET_APP_INSTALLATION_ID", ""), "GitHub App installation id")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(os.Args[1:]); err != nil {
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
