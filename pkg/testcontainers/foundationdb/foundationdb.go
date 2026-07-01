// Package foundationdb provides a testcontainers module for FoundationDB.
//
// This module creates a single FoundationDB container (no socat proxy) and
// provides two connectivity modes:
//
//   - External (host/sandbox): via Docker port mapping (localhost:mappedPort).
//     Works from Bazel sandboxes, CI runners, Docker Desktop, etc.
//   - Internal (cross-container): via Docker bridge IP (containerIP:4500).
//     Used by sidecar containers on the same Docker network.
//
// The container is auto-initialized by default (configured for single-node
// operation with tenant support). Use [WithoutInit] to skip.
//
// Basic usage:
//
//	container, err := foundationdb.Run(ctx, "")
//	clusterFile, _ := container.ClusterFile(ctx)
//	db, _ := fdb.OpenDatabase(writeToFile(clusterFile))
//
// Multi-container usage (e.g., binding tester):
//
//	nw, _ := foundationdb.CreateNetwork(ctx)
//	fdb, _ := foundationdb.Run(ctx, "", foundationdb.WithNetwork(nw))
//	// Attach another container to the same network:
//	otherReq.Networks = []string{fdb.NetworkName()}
//	// Use InternalClusterFile() for the other container's cluster file.
package foundationdb

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	defaultImage      = "foundationdb/foundationdb"
	defaultFDBPort    = 4500
	defaultAPIVersion = 730
)

// fdbVersion returns the FDB version from the FDB_VERSION env var
// (set by .bazelrc for Bazel tests) or falls back to a default.
func fdbVersion() string {
	if v := os.Getenv("FDB_VERSION"); v != "" {
		return v
	}
	return "7.3.77"
}

// Container represents a running FoundationDB container.
//
// External clients (host, Bazel sandbox) connect via localhost:mappedPort.
// Internal clients (containers on the same Docker network) connect via containerIP:4500.
type Container struct {
	testcontainers.Container
	network     *testcontainers.DockerNetwork // user-provided network, nil if default bridge
	clusterFile string                        // external cluster file (localhost:mappedPort)
	internalCF  string                        // internal cluster file (containerIP:4500)
	containerIP string                        // cached bridge IP
	mappedPort  int                           // host port mapped to container's 4500
	config      options
}

// Run creates, starts, and (by default) initializes a FoundationDB container.
//
// The container exposes FDB port 4500 via Docker port mapping. External clients
// (including Bazel sandbox tests) connect via localhost:mappedPort. For cross-container
// communication, use [InternalClusterFile] with a shared network via [WithNetwork].
//
// By default, the database is auto-initialized for single-node operation with
// tenant_mode=optional_experimental. Use [WithoutInit] to skip initialization.
//
// The image parameter can be empty ("") to use the default image with the version
// from FDB_VERSION env var or the built-in default.
func Run(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (*Container, error) {
	return retryContainerStart(ctx, maxContainerStartAttempts,
		func(attempt int) time.Duration { return time.Duration(attempt) * time.Second },
		func() (*Container, error) { return runOnce(ctx, img, opts...) })
}

// maxContainerStartAttempts bounds Run's retry of the whole container bring-up when an attempt dies
// transiently. Each attempt creates a FRESH container, so a one-off death (e.g. the FDB container
// OOM-killed mid-`configure new` under CI's --local_test_jobs container concurrency) recovers on the next
// try once the memory spike has passed. Deterministic failures (bad option/config) are NOT retried.
const maxContainerStartAttempts = 3

// isTransientContainerErr reports whether a container bring-up failure is a transient death worth
// retrying with a fresh container — chiefly a Docker-daemon OOM-kill during InitializeDatabase under CI
// concurrency, which surfaces as "container <id> is not running" from the configure exec (the FDB process
// died between the FDBD-joined-cluster wait and `configure new`). A fresh container started a moment later
// typically succeeds. Config/option errors do NOT match, so they fail fast (recreating won't help them).
func isTransientContainerErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "is not running") || // container died (OOM-killed) mid-bring-up
		strings.Contains(s, "container exited") ||
		strings.Contains(s, "failed to start")
}

// retryContainerStart calls attempt up to maxAttempts times, retrying ONLY on isTransientContainerErr and
// backing off backoff(attempt) between tries (attempt is 1-based; no sleep after the last try). It stops
// early on ctx cancellation or a non-transient error. Split out from Run so the retry policy is unit-
// testable with a fake attempt/backoff (no real Docker).
func retryContainerStart(ctx context.Context, maxAttempts int, backoff func(attempt int) time.Duration, attempt func() (*Container, error)) (*Container, error) {
	var lastErr error
	for i := 1; i <= maxAttempts; i++ {
		c, err := attempt()
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err() // cancelled/expired — surface cancellation, don't keep recreating
		}
		if !isTransientContainerErr(err) {
			return nil, err // deterministic failure — recreating won't help
		}
		if i < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
	}
	return nil, fmt.Errorf("container start failed after %d transient attempts: %w", maxAttempts, lastErr)
}

// runOnce performs a single container bring-up (create + init) with no retry. Run wraps it in
// retryContainerStart. Each call creates a brand-new container.
func runOnce(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (*Container, error) {
	cfg := defaultOptions()

	// Apply our module options to extract config.
	for _, opt := range opts {
		if o, ok := opt.(Option); ok {
			if err := o.apply(&cfg); err != nil {
				return nil, fmt.Errorf("apply option: %w", err)
			}
		}
	}

	if img == "" {
		img = fmt.Sprintf("%s:%s", defaultImage, cfg.version)
	}

	// Build the container request.
	// Port mapping for external access (localhost:randomPort → container:4500).
	env := map[string]string{}
	if cfg.fdbPort != defaultFDBPort {
		env["FDB_PORT"] = fmt.Sprintf("%d", cfg.fdbPort)
	}

	portStr := fmt.Sprintf("%d/tcp", cfg.fdbPort)
	req := testcontainers.ContainerRequest{
		Image:        img,
		Env:          env,
		ExposedPorts: []string{portStr},
		WaitingFor: wait.ForLog("FDBD joined cluster").
			WithStartupTimeout(cfg.startupTimeout),
	}
	// Mount tmpfs over /var/fdb/data to prevent the VOLUME directive in the
	// FDB Docker image from creating anonymous volumes that leak on removal.
	// Without this, every container leaks ~90MB that persists after termination.
	// WithDataOnDisk skips this so datasets larger than RAM can be stored on disk
	// (the anonymous volume on the host filesystem); it is cleaned up with the
	// container.
	if !cfg.dataOnDisk {
		req.Tmpfs = map[string]string{"/var/fdb/data": ""}
	}

	// When knobs are set, override the entrypoint to patch the startup script.
	// sed inserts knob args into the fdbserver command line before exec'ing
	// the original script. This passes --knob_NAME=VALUE to fdbserver.
	if len(cfg.knobs) > 0 {
		var knobArgs string
		for name, value := range cfg.knobs {
			knobArgs += fmt.Sprintf(" --knob_%s=%s", name, value)
		}
		sedCmd := fmt.Sprintf(`sed -i 's|^fdbserver |fdbserver%s |' /var/fdb/scripts/fdb.bash && exec /var/fdb/scripts/fdb.bash`, knobArgs)
		req.Entrypoint = []string{"/usr/bin/tini", "-g", "--", "/bin/bash", "-c", sedCmd}
	}

	// Attach to custom network if provided.
	if cfg.network != nil {
		aliases := []string{"foundationdb"}
		aliases = append(aliases, cfg.networkAliases...)
		req.Networks = []string{cfg.network.Name}
		req.NetworkAliases = map[string][]string{
			cfg.network.Name: aliases,
		}
	}

	genReq := testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	}

	// Apply testcontainers-native customizers (WithEnv, WithCmd, etc.)
	for _, opt := range opts {
		if _, ok := opt.(Option); ok {
			continue // already applied above
		}
		if err := opt.Customize(&genReq); err != nil {
			return nil, fmt.Errorf("customize request: %w", err)
		}
	}

	ctr, err := testcontainers.GenericContainer(ctx, genReq)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	// Get the host-mapped port for external connectivity.
	mapped, err := ctr.MappedPort(ctx, nat.Port(portStr))
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("get mapped port: %w", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("get host: %w", err)
	}

	// Get the container's bridge IP for internal (cross-container) connectivity.
	containerIP, err := ctr.ContainerIP(ctx)
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("get container IP: %w", err)
	}

	// Read the cluster file description:id from inside the container.
	// FDB generates "docker:docker@containerIP:port" — we need the "docker:docker" prefix
	// to construct compatible cluster files for both external and internal use.
	rawCF, err := readContainerFile(ctx, ctr, "/var/fdb/fdb.cluster")
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("read cluster file: %w", err)
	}

	// Parse cluster file: "description:id@coordinator1,coordinator2,..."
	// Keep the "description:id@" prefix, replace coordinators.
	atIdx := strings.Index(rawCF, "@")
	if atIdx < 0 {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("malformed cluster file: %q", rawCF)
	}
	prefix := rawCF[:atIdx+1] // "docker:docker@"

	// External: use localhost:mappedPort (works from Bazel sandbox, CI, Docker Desktop)
	externalCF := fmt.Sprintf("%s%s:%s", prefix, host, mapped.Port())

	// Internal: use containerIP:port (works from containers on same Docker network)
	internalCF := rawCF // already has containerIP:port

	// WithDirectIP: use internal (bridge IP) cluster file for external clients too.
	// Avoids DNAT assertion spam but requires direct bridge IP routing.
	primaryCF := externalCF
	if cfg.directIP {
		primaryCF = internalCF
	}

	c := &Container{
		Container:   ctr,
		network:     cfg.network,
		clusterFile: primaryCF,
		internalCF:  internalCF,
		containerIP: containerIP,
		mappedPort:  mapped.Int(),
		config:      cfg,
	}

	// Auto-initialize unless disabled.
	if cfg.autoInit {
		if cfg.processCount > 1 {
			// Multi-process: init with single redundancy first (so the DB is
			// available with just 1 process), start additional processes, then
			// reconfigure to the requested redundancy mode.
			savedMode := c.config.redundancyMode
			c.config.redundancyMode = "single"
			if err := c.InitializeDatabase(ctx); err != nil {
				_ = c.Terminate(ctx)
				return nil, fmt.Errorf("initialize database: %w", err)
			}
			if err := c.startAdditionalProcesses(ctx); err != nil {
				_ = c.Terminate(ctx)
				return nil, fmt.Errorf("start additional processes: %w", err)
			}
			// Reconfigure to requested redundancy now that all processes are up.
			c.config.redundancyMode = savedMode
			if savedMode != "single" {
				cmd := fmt.Sprintf("configure %s", savedMode)
				if err := c.configureWithRetry(ctx, "reconfigure redundancy", cmd); err != nil {
					_ = c.Terminate(ctx)
					return nil, err
				}
			}
		} else {
			if err := c.InitializeDatabase(ctx); err != nil {
				_ = c.Terminate(ctx)
				return nil, fmt.Errorf("initialize database: %w", err)
			}
		}
	}

	return c, nil
}

// ClusterFile returns the cluster file content for external clients.
// Uses localhost:mappedPort, which works from Bazel sandboxes, CI runners, etc.
func (c *Container) ClusterFile(_ context.Context) (string, error) {
	return c.clusterFile, nil
}

// MustClusterFile returns the cluster file content, panicking on error.
func (c *Container) MustClusterFile(ctx context.Context) string {
	cf, err := c.ClusterFile(ctx)
	if err != nil {
		panic(fmt.Sprintf("ClusterFile: %v", err))
	}
	return cf
}

// ClusterFilePath writes the cluster file to a temp file and returns the path.
// The file persists until the caller removes it.
func (c *Container) ClusterFilePath(_ context.Context) (string, error) {
	f, err := os.CreateTemp("", "fdb_cluster_*.txt")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.WriteString(c.clusterFile); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write cluster file: %w", err)
	}
	f.Close()
	return f.Name(), nil
}

// ConnectionString returns "host:port" for the FDB server (external, port-mapped).
func (c *Container) ConnectionString(ctx context.Context) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, c.mappedPort), nil
}

// NetworkName returns the Docker network name. Returns empty string if the container
// is on the default bridge (no custom network). Use this to attach other containers
// to the same network for inter-container communication.
func (c *Container) NetworkName() string {
	if c.network != nil {
		return c.network.Name
	}
	return ""
}

// InternalAddress returns the FDB address reachable from within Docker networks.
// Uses the container's bridge IP: "containerIP:4500".
func (c *Container) InternalAddress() string {
	return fmt.Sprintf("%s:%d", c.containerIP, c.config.fdbPort)
}

// InternalClusterFile returns a cluster file string usable from containers on the
// same Docker network. Uses the container's bridge IP (no port mapping needed).
func (c *Container) InternalClusterFile() string {
	return c.internalCF
}

// APIVersion returns the configured FDB API version.
func (c *Container) APIVersion() int {
	return c.config.apiVersion
}

// Database returns the configured database name.
func (c *Container) Database() string {
	return c.config.database
}

// Version returns the configured FoundationDB version.
func (c *Container) Version() string {
	return c.config.version
}

// FDBPort returns the FDB port inside the container.
func (c *Container) FDBPort() int {
	return c.config.fdbPort
}

// MappedPort returns the host port mapped to FDB's container port.
func (c *Container) MappedPort() int {
	return c.mappedPort
}

// InitializeDatabase configures the database for single-node operation.
// This is called automatically by [Run] unless [WithoutInit] is used.
//
// The configure command uses the storage engine and redundancy mode from options.
// Default: "memory" engine, "single" redundancy, tenant_mode=optional_experimental.
//
// This method is idempotent — calling it on an already-configured database is safe.
func (c *Container) InitializeDatabase(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := fmt.Sprintf("configure new %s %s tenant_mode=%s",
		c.config.redundancyMode, c.config.storageEngine, c.config.tenantMode)
	return c.configureWithRetry(initCtx, "configure new", cmd)
}

// configureWithRetry runs an fdbcli `configure ...` command, retrying on transient
// failures. Configure on a freshly started or just-grown cluster is timing- and
// resource-fragile: the coordinator's fdbserver may not be reachable yet, newly
// started processes may not be registered as storage servers yet, and fdbcli can be
// SIGKILLed under memory pressure on a loaded CI runner (exit 137) or refuse the
// change while the cluster is not yet healthy (exit 1). All of these are transient,
// so retry with backoff. An "already exists" output (from `configure new` against an
// already-initialized DB) is success, since configure is idempotent.
func (c *Container) configureWithRetry(ctx context.Context, what, cmd string) error {
	var lastErr error
	for attempt := 1; attempt <= 6; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("%s: %w (last attempt: %v)", what, err, lastErr)
			}
			return fmt.Errorf("%s: context cancelled: %w", what, err)
		}
		// Backoff also gives FDB time to stabilize between attempts.
		time.Sleep(time.Duration(attempt) * time.Second)

		output, err := c.FDBCLIExec(ctx, cmd)
		if err == nil || strings.Contains(output, "already exists") {
			return nil
		}
		lastErr = fmt.Errorf("%s (attempt %d/6): %w (output: %s)", what, attempt, err, output)
	}
	return lastErr
}

// startAdditionalProcesses starts extra fdbserver processes on ports 4501..4500+n-1.
// Each process uses a separate data directory and the same cluster file.
// The processes join the existing cluster as additional storage servers.
func (c *Container) startAdditionalProcesses(ctx context.Context) error {
	publicIP := c.containerIP

	// Build knob args string for additional processes (same knobs as primary).
	var knobArgs string
	for name, value := range c.config.knobs {
		knobArgs += fmt.Sprintf(" --knob_%s=%s", name, value)
	}

	for i := 1; i < c.config.processCount; i++ {
		port := c.config.fdbPort + i
		dataDir := fmt.Sprintf("/var/fdb/data/proc%d", i)

		// Create data directory and start fdbserver in background.
		cmd := fmt.Sprintf(
			"mkdir -p %s && fdbserver --listen-address 0.0.0.0:%d --public-address %s:%d "+
				"--datadir %s --logdir /var/fdb/logs --class storage%s &",
			dataDir, port, publicIP, port, dataDir, knobArgs,
		)
		_, reader, err := c.Exec(ctx, []string{"/bin/bash", "-c", cmd}, tcexec.Multiplexed())
		if err != nil {
			return fmt.Errorf("start process %d on port %d: %w", i, port, err)
		}
		// Drain output to prevent blocking.
		if reader != nil {
			io.ReadAll(reader)
		}
	}

	// Wait for all processes to join the cluster.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := c.FDBCLIExec(ctx, "status minimal")
		if err == nil && (strings.Contains(output, "Healthy") || strings.Contains(output, "available")) {
			// Count processes via ps aux (more reliable than parsing status details).
			procCount, _ := c.countProcesses(ctx)
			if procCount >= c.config.processCount {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %d processes to join cluster", c.config.processCount)
}

// countProcesses returns the number of fdbserver processes running in the container.
// Uses pgrep for precise matching (avoids counting bash wrappers or fdbcli).
func (c *Container) countProcesses(ctx context.Context) (int, error) {
	_, reader, err := c.Exec(ctx, []string{"pgrep", "-c", "fdbserver"}, tcexec.Multiplexed())
	if err != nil {
		// pgrep returns exit 1 if no matches.
		return 0, nil
	}
	out, _ := io.ReadAll(reader)
	var count int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count)
	return count, nil
}

// FDBCLIExec runs an fdbcli command inside the container and returns the output.
//
// Example:
//
//	output, err := container.FDBCLIExec(ctx, "status details")
func (c *Container) FDBCLIExec(ctx context.Context, command string) (string, error) {
	exitCode, reader, err := c.Exec(ctx, []string{
		"/usr/bin/fdbcli", "--exec", command,
	}, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("exec fdbcli: %w", err)
	}

	outputBytes, _ := io.ReadAll(reader)
	output := string(outputBytes)

	if exitCode != 0 {
		return output, fmt.Errorf("fdbcli exited with code %d", exitCode)
	}

	return output, nil
}

// Status returns the FDB cluster status output.
func (c *Container) Status(ctx context.Context) (string, error) {
	return c.FDBCLIExec(ctx, "status details")
}

// Pause freezes all processes in the container using Docker pause.
// The container remains running but FDB becomes unreachable. TCP connections stay
// open but will time out. Use this to simulate network partitions or FDB hangs.
//
// Call [Unpause] to resume.
func (c *Container) Pause(ctx context.Context) error {
	dockerClient, err := newDockerClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dockerClient.Close()

	return dockerClient.ContainerPause(ctx, c.GetContainerID())
}

// Unpause resumes a paused container. See [Pause].
func (c *Container) Unpause(ctx context.Context) error {
	dockerClient, err := newDockerClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dockerClient.Close()

	return dockerClient.ContainerUnpause(ctx, c.GetContainerID())
}

// Terminate terminates the container. The network, if user-provided via
// [WithNetwork], is NOT removed — the caller owns it.
func (c *Container) Terminate(ctx context.Context) error {
	return c.Container.Terminate(ctx)
}

// readContainerFile reads a file from inside a running container.
// Uses Multiplexed() to strip Docker exec stream headers from the output.
func readContainerFile(ctx context.Context, ctr testcontainers.Container, path string) (string, error) {
	exitCode, reader, err := ctr.Exec(ctx, []string{"cat", path}, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("exec cat %s: %w", path, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("read output: %w", err)
	}
	if exitCode != 0 {
		return "", fmt.Errorf("cat %s exited %d: %s", path, exitCode, string(data))
	}
	return strings.TrimSpace(string(data)), nil
}

// newDockerClient creates a Docker API client using default environment config.
func newDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}
