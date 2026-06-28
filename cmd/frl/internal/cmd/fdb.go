package cmd

import (
	"fmt"
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
	var name, image, ctxName, keyspace string
	c := &cobra.Command{
		Use:   "up",
		Short: "Start and configure a local FoundationDB, then point frl at it",
		Long: "Starts a single-node FoundationDB container, runs `configure new " +
			"single memory`, waits for it to become available, copies its " +
			"cluster file next to the frl config, and writes (and activates) a " +
			"frl context pointing at it. After this, `frl sql` and the other " +
			"commands work with no further setup.",
		Example: `  frl fdb up
  frl fdb up --name myfdb --context myfdb
  frl fdb up --image foundationdb/foundationdb:7.3.77`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if running, _ := dockerContainerExists(name); running {
				return fmt.Errorf("container %q already exists; run `frl fdb down --name %s` first (or pick --name)", name, name)
			}

			fmt.Fprintf(out, "Starting %s (container %q)...\n", image, name)
			if o, err := runDocker("run", "-d", "--name", name, "--network", "host", image); err != nil {
				return fmt.Errorf("docker run: %w\n%s", err, o)
			}

			// fdbcli is not reachable the instant the container starts; retry
			// the one-time `configure new` until it takes.
			fmt.Fprintln(out, "Configuring (new single memory)...")
			if err := retry(15, 2*time.Second, func() error {
				o, err := runDocker("exec", name, "fdbcli", "--exec", "configure new single memory")
				if err != nil {
					return fmt.Errorf("%w: %s", err, strings.TrimSpace(o))
				}
				return nil
			}); err != nil {
				return fmt.Errorf("configure cluster (is the image healthy? `frl fdb down --name %s` to clean up): %w", name, err)
			}

			fmt.Fprint(out, "Waiting for the database to become available")
			if err := retry(30, 2*time.Second, func() error {
				fmt.Fprint(out, ".")
				o, _ := runDocker("exec", name, "fdbcli", "--exec", "status minimal")
				if strings.Contains(o, "is available") {
					return nil
				}
				return fmt.Errorf("not available yet")
			}); err != nil {
				fmt.Fprintln(out)
				return fmt.Errorf("database did not become available: %w", err)
			}
			fmt.Fprintln(out, " ready")

			// Copy the cluster file next to the frl config so the written
			// context's path is stable and operator-discoverable.
			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			clusterFile := filepath.Join(filepath.Dir(cfgPath), name+".cluster")
			if err := os.MkdirAll(filepath.Dir(clusterFile), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(clusterFile), err)
			}
			if o, err := runDocker("cp", name+":/var/fdb/fdb.cluster", clusterFile); err != nil {
				return fmt.Errorf("copy cluster file: %w\n%s", err, o)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			setContext(cfg, ctxName, clusterFile, keyspace)
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Fprintf(out, "\nFoundationDB is up. Context %q is active (cluster file %s).\n", ctxName, clusterFile)
			fmt.Fprintf(out, "Try: frl tx read-version   |   frl sql --database /myapp\n")
			fmt.Fprintf(out, "Tear down with: frl fdb down --name %s\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", defaultFdbContainer, "Docker container name")
	c.Flags().StringVar(&image, "image", defaultFdbImage, "FoundationDB Docker image")
	c.Flags().StringVar(&ctxName, "context", defaultFdbContainer, "frl context name to write and activate")
	c.Flags().StringVar(&keyspace, "keyspace", "/dev", "keyspace_path for the written context")
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
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if o, err := runDocker("rm", "-f", name); err != nil {
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
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("docker not found on PATH: %w", err)
			}
			if ok, _ := dockerContainerExists(name); !ok {
				return fmt.Errorf("container %q is not running; `frl fdb up` to start it", name)
			}
			o, err := runDocker("exec", name, "fdbcli", "--exec", "status minimal")
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

func runDocker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// dockerContainerExists reports whether a container with the given name exists
// (running or stopped).
func dockerContainerExists(name string) (bool, error) {
	out, err := runDocker("ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == name, nil
}

func retry(attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return err
}
