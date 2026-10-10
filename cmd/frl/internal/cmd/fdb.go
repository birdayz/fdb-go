package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
	"fdb.dev/cmd/frl/internal/config"
)

// newFdbCmd manages a throwaway single-node FoundationDB in Docker, for local
// development. `frl fdb up` is the one command that turns an empty machine into
// a working cluster: it starts the container, configures it, copies out the
// cluster file, and writes a frl context pointing at it, so `frl sql` and the
// rest of the CLI work immediately. It shells out to the `docker` CLI (the same
// steps as cmd/frl/demo/README.md), so Docker is the only prerequisite.
func newFdbCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fdb",
		Short: "Run a local FoundationDB in Docker for development",
		Long: "Manage a throwaway single-node FoundationDB container for local " +
			"development. `up` starts and configures it and writes a frl " +
			"context so the rest of the CLI works immediately; `down` removes " +
			"it; `status` reports cluster health. Docker is the only prerequisite.",
	}
	c.AddCommand(newFdbUpCmd(), newFdbDownCmd(), newFdbStatusCmd())
	return c
}

const (
	defaultFdbContainer = "frl-fdb"
	defaultFdbImage     = "foundationdb/foundationdb:7.3.77"
)

func newFdbUpCmd() *cobra.Command {
	var name, image, ctxName, keyspace, outputFmt string
	var port int
	c := &cobra.Command{
		Use:   "up",
		Short: "Start and configure a local FoundationDB, then point frl at it",
		Long: "Starts a single-node FoundationDB container, runs `configure new " +
			"single memory`, waits for it to become available, copies its " +
			"cluster file next to the frl config, and writes (and activates) a " +
			"frl context pointing at it. After this, `frl sql` and the other " +
			"commands work with no further setup.\n\n" +
			"UNIX stdout contract: progress goes to stderr; stdout carries " +
			"exactly the cluster-file path, so the command chains:\n\n" +
			"  frl sql --cluster-file $(frl fdb up) --database /FRL/demo\n\n" +
			"--output / -o: 'text' (default — bare path) or 'json' " +
			"({cluster_file, container, context}).\n\n" +
			"The server is published on 127.0.0.1 only and advertises " +
			"127.0.0.1 for host clients on Linux and Docker Desktop (macOS). " +
			"Requires a local Docker socket; remote Docker daemons and " +
			"clients in other containers are not supported. This is an " +
			"unauthenticated development database, not a production setup. " +
			"--port sets that port (the same inside the container and on the " +
			"host) — pick distinct ports to run several instances.",
		Example: `  frl fdb up
  frl fdb up --name myfdb --context myfdb --port 4689
  frl sql --cluster-file $(frl fdb up) --database /FRL/demo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("port must be between 1 and 65535")
			}
			// Progress is chatter, not output — stderr, so that
			// $(frl fdb up) captures only the cluster-file path.
			progress := cmd.ErrOrStderr()
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if err := requireLocalDocker(cmd.Context()); err != nil {
				return err
			}
			if running, err := dockerContainerExists(cmd.Context(), name); err != nil {
				return err
			} else if running {
				return fmt.Errorf("container %q already exists; run `frl fdb down --name %s` first (or pick --name)", name, name)
			}
			// A first-run pull can take minutes; only Ctrl-C bounds it.
			if err := ensureFdbImage(cmd.Context(), image, progress, runDocker, pullDockerImage); err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			// Remove a container this command created but could not finish starting.
			defer func() {
				if err != nil {
					_, _ = runDocker(context.WithoutCancel(cmd.Context()), "rm", "-fv", name)
				}
			}()
			fmt.Fprintf(progress, "Starting %s (container %q, 127.0.0.1:%d)...\n", image, name, port)
			if o, err := runDocker(ctx, fdbRunArgs(name, image, port)...); err != nil {
				return fmt.Errorf("docker run: %w\n%s", err, o)
			}

			// fdbcli is not reachable the instant the container starts; retry
			// the one-time `configure new` until it takes.
			fmt.Fprintln(progress, "Configuring (new single memory)...")
			if err := retry(ctx, 15, 2*time.Second, func() error {
				o, err := runDocker(ctx, "exec", name, "fdbcli", "--timeout", "5", "--exec", "configure new single memory")
				return configureNewOutcome(o, err)
			}); err != nil {
				return fmt.Errorf("configure cluster (is the image healthy?): %w", err)
			}

			fmt.Fprint(progress, "Waiting for the database to become available")
			if err := retry(ctx, 30, 2*time.Second, func() error {
				fmt.Fprint(progress, ".")
				o, err := runDocker(ctx, "exec", name, "fdbcli", "--timeout", "5", "--exec", "status minimal")
				if err != nil {
					return err
				}
				if strings.Contains(o, "is available") {
					return nil
				}
				return fmt.Errorf("not available yet")
			}); err != nil {
				fmt.Fprintln(progress)
				return fmt.Errorf("database did not become available: %w", err)
			}
			fmt.Fprintln(progress, " ready")

			cluster, err := runDocker(ctx, "exec", name, "cat", "/var/fdb/fdb.cluster")
			if err != nil {
				return fmt.Errorf("read container cluster file: %w\n%s", err, cluster)
			}
			if strings.TrimSpace(cluster) != fdbClusterString(port) {
				return fmt.Errorf("container cluster file does not match the requested loopback endpoint; check --image")
			}
			// Keep the actual container cluster file beside the frl config.
			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			clusterFile := filepath.Join(filepath.Dir(cfgPath), name+".cluster")
			if err := os.MkdirAll(filepath.Dir(clusterFile), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(clusterFile), err)
			}
			if err := os.WriteFile(clusterFile, []byte(cluster), 0o644); err != nil {
				return fmt.Errorf("write cluster file: %w", err)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			setContext(cfg, ctxName, clusterFile, keyspace)
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Fprintf(progress, "\nFoundationDB is up. Context %q is active (cluster file %s).\n", ctxName, clusterFile)
			fmt.Fprintf(progress, "Try: frl tx read-version   |   frl sql --database /FRL/myapp\n")
			fmt.Fprintf(progress, "Tear down with: frl fdb down --name %s\n", name)

			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]string{
					"cluster_file": clusterFile,
					"container":    name,
					"context":      ctxName,
				})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), clusterFile)
			return err
		},
	}
	c.Flags().StringVar(&name, "name", defaultFdbContainer, "Docker container name")
	c.Flags().StringVar(&image, "image", defaultFdbImage, "FoundationDB Docker image")
	c.Flags().StringVar(&ctxName, "context", defaultFdbContainer, "frl context name to write and activate")
	c.Flags().StringVar(&keyspace, "keyspace", "/dev", "keyspace_path for the written context")
	c.Flags().IntVar(&port, "port", 4500, "fdbserver port, published on 127.0.0.1 (same port inside the container)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "stdout format: text (bare cluster-file path) or json")
	return c
}

func newFdbDownCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:     "down",
		Short:   "Remove the local FoundationDB container",
		Args:    cobra.NoArgs,
		Example: "  frl fdb down\n  frl fdb down --name myfdb",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if o, err := runDocker(cmd.Context(), "rm", "-fv", name); err != nil {
				return fmt.Errorf("docker rm: %w\n%s", err, o)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed container %q. (The frl context and cluster file are left in place.)\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", defaultFdbContainer, "Docker container name")
	return c
}

func newFdbStatusCmd() *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "status",
		Short: "Report the local FoundationDB cluster status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if ok, err := dockerContainerExists(cmd.Context(), name); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("container %q is not running; `frl fdb up` to start it", name)
			}
			o, err := runDocker(cmd.Context(), "exec", name, "fdbcli", "--timeout", "5", "--exec", "status minimal")
			if err != nil {
				return fmt.Errorf("fdbcli status: %w\n%s", err, o)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), o)
			return err
		},
	}
	c.Flags().StringVar(&name, "name", defaultFdbContainer, "Docker container name")
	return c
}

// fdbClusterString is the cluster file of the dev container: the image's
// default description/ID, coordinated by the loopback-published port.
func fdbClusterString(port int) string {
	return fmt.Sprintf("docker:docker@127.0.0.1:%d", port)
}

// The image's "host" mode advertises loopback; Docker still uses a bridge.
// Matching ports lets host clients reach every advertised FDB role via NAT.
func fdbRunArgs(name, image string, port int) []string {
	return []string{
		"run", "-d", "--name", name, "--network", "bridge",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"--env", "FDB_NETWORKING_MODE=host",
		"--env", fmt.Sprintf("FDB_PORT=%d", port),
		"--env", "FDB_CLUSTER_FILE_CONTENTS=" + fdbClusterString(port),
		image,
	}
}

// setContext upserts a context by name (updating its cluster file and keyspace)
// and makes it the active one. Pure config mutation, unit-tested without Docker.
func setContext(cfg *configv1.Config, name, clusterFile, keyspace string) {
	for _, ctx := range cfg.GetContexts() {
		if ctx.GetName() == name {
			ctx.ClusterFile = clusterFile
			ctx.KeyspacePath = keyspace
			cfg.CurrentContext = name
			return
		}
	}
	cfg.Contexts = append(cfg.GetContexts(), &configv1.Context{
		Name:         name,
		ClusterFile:  clusterFile,
		KeyspacePath: keyspace,
	})
	cfg.CurrentContext = name
}

// configureNewOutcome interprets one `fdbcli configure new` attempt.
// `configure new` is NOT idempotent: if an earlier attempt succeeded
// server-side (including a half-acknowledged one where fdbcli exited
// nonzero anyway), every subsequent attempt reports "Database already
// exists" until the retry budget is exhausted — failing a perfectly
// healthy cluster. That response means the database is configured, so
// it is success, not an error.
func configureNewOutcome(output string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(output, "Database already exists") {
		return nil
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
}

func requireLocalDocker(ctx context.Context) error {
	endpoint := os.Getenv("DOCKER_HOST")
	// With DOCKER_CONTEXT set, ask the CLI which endpoint it actually selects.
	if endpoint == "" || os.Getenv("DOCKER_CONTEXT") != "" {
		out, err := runDocker(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return fmt.Errorf("inspect Docker endpoint: %w\n%s", err, out)
		}
		endpoint = strings.TrimSpace(out)
	}
	return validateLocalDockerEndpoint(endpoint)
}

func validateLocalDockerEndpoint(endpoint string) error {
	if strings.HasPrefix(endpoint, "unix:///") || strings.HasPrefix(endpoint, "npipe:///") {
		return nil
	}
	return fmt.Errorf("frl fdb up requires a local Docker socket (unix or npipe); the selected endpoint is not supported because the cluster advertises 127.0.0.1")
}

// ensureFdbImage pulls image only when it is absent, under the caller's context.
func ensureFdbImage(ctx context.Context, image string, progress io.Writer,
	inspect func(context.Context, ...string) (string, error),
	pull func(context.Context, string, io.Writer) error,
) error {
	if _, err := inspect(ctx, "image", "inspect", image); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprintf(progress, "Pulling %s (first run only; this can take several minutes)...\n", image)
	return pull(ctx, image, progress)
}

func pullDockerImage(ctx context.Context, image string, progress io.Writer) error {
	command := exec.CommandContext(ctx, "docker", "pull", image)
	command.Stdout, command.Stderr = progress, progress
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("docker pull %s: %w", image, err)
	}
	return nil
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "docker", args...)
	command.WaitDelay = time.Second
	out, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

// dockerContainerExists reports whether a container with the given name exists
// (running or stopped).
func dockerContainerExists(ctx context.Context, name string) (bool, error) {
	out, err := runDocker(ctx, "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == name, nil
}

func retry(ctx context.Context, attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = fn(); err == nil {
			return ctx.Err()
		}
		if i+1 < attempts {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
