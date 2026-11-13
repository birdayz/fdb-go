package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// Cluster represents a multi-container FDB cluster.
// Each container runs one fdbserver process. The first is the coordinator.
//
// Example:
//
//	cluster, err := foundationdb.RunCluster(ctx, 3,
//	    foundationdb.WithStorageEngine("ssd"),
//	    foundationdb.WithRedundancyMode("double"),
//	)
//	defer func() {
//	    if err := cluster.Terminate(ctx); err != nil {
//	        log.Printf("terminate cluster: %v", err)
//	    }
//	}()
//	cf, _ := cluster.ClusterFile(ctx)
type Cluster struct {
	Coordinator *Container
	Replicas    []*Container // all containers including coordinator
	network     *testcontainers.DockerNetwork
	config      options
}

// RunCluster creates an N-container FDB cluster on a shared Docker network.
// The first container is the coordinator; additional containers join as storage.
//
// All options from Run() are supported. WithNetwork is set automatically. Like
// Run, it returns a [*DockerUnavailableError] when no Docker daemon is reachable.
func RunCluster(ctx context.Context, size int, opts ...testcontainers.ContainerCustomizer) (*Cluster, error) {
	if size < 1 || size > 10 {
		return nil, fmt.Errorf("cluster size must be 1-10, got %d", size)
	}

	// Parse options to get knobs, storage engine, etc.
	cfg := defaultOptions()
	for _, opt := range opts {
		if o, ok := opt.(Option); ok {
			if err := o.apply(&cfg); err != nil {
				return nil, err
			}
		}
	}

	if err := tcProber.check(ctx); err != nil {
		return nil, err
	}
	if err := holdReaper(ctx); err != nil {
		return nil, err
	}

	// Create shared network.
	nw, err := CreateNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	cluster := &Cluster{
		network: nw,
		config:  cfg,
	}

	// Start coordinator (first container).
	coordOpts := rebuildOpts(cfg, nw, "fdb-coordinator")
	coordinator, err := Run(ctx, "", coordOpts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start coordinator: %w", err), cluster.Terminate(ctx))
	}
	cluster.Coordinator = coordinator
	cluster.Replicas = append(cluster.Replicas, coordinator)

	if size == 1 {
		return cluster, nil
	}

	// Get the coordinator's internal cluster file for replicas.
	coordCF := coordinator.InternalClusterFile()

	// Start replica containers that join the coordinator's cluster.
	for i := 1; i < size; i++ {
		replicaOpts := rebuildOpts(cfg, nw, fmt.Sprintf("fdb-replica-%d", i))
		// Override cluster file to point to coordinator.
		replicaOpts = append(replicaOpts, WithoutInit()) // don't re-configure
		replicaOpts = append(replicaOpts,
			testcontainers.WithEnv(map[string]string{
				"FDB_CLUSTER_FILE_CONTENTS": coordCF,
			}),
		)

		replica, err := Run(ctx, "", replicaOpts...)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("start replica %d: %w", i, err), cluster.Terminate(ctx))
		}
		cluster.Replicas = append(cluster.Replicas, replica)
	}

	// Wait for all replicas to be visible in the cluster.
	if err := cluster.waitForCluster(ctx, size); err != nil {
		return nil, errors.Join(err, cluster.Terminate(ctx))
	}

	// Reconfigure redundancy now that all processes are up.
	if cfg.redundancyMode != "single" && size > 1 {
		cmd := fmt.Sprintf("configure %s", cfg.redundancyMode)
		if err := coordinator.configureWithRetry(ctx, "configure redundancy", cmd); err != nil {
			return nil, errors.Join(err, cluster.Terminate(ctx))
		}
		// Wait for the cluster to stabilize after redundancy change.
		// FDB needs time to replicate data to meet the new policy.
		if err := cluster.waitForHealthy(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("wait for healthy after redundancy change: %w", err), cluster.Terminate(ctx))
		}
	}

	return cluster, nil
}

// rebuildOpts constructs Run options from a parsed config + network.
func rebuildOpts(cfg options, nw *testcontainers.DockerNetwork, alias string) []testcontainers.ContainerCustomizer {
	var opts []testcontainers.ContainerCustomizer
	opts = append(opts, WithStorageEngine(cfg.storageEngine))
	opts = append(opts, WithRedundancyMode("single")) // always init as single
	opts = append(opts, WithTenantMode(cfg.tenantMode))
	opts = append(opts, WithNetwork(nw, alias))
	opts = append(opts, WithDirectIP())
	opts = append(opts, WithStartupTimeout(cfg.startupTimeout))
	for name, value := range cfg.knobs {
		opts = append(opts, WithKnob(name, value))
	}
	return opts
}

// waitForCluster waits until the coordinator sees the expected number of processes.
func (c *Cluster) waitForCluster(ctx context.Context, expected int) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		output, err := c.Coordinator.FDBCLIExec(ctx, "status minimal")
		if err == nil && DatabaseAvailable(output) {
			// Count processes in status details.
			details, _ := c.Coordinator.FDBCLIExec(ctx, "status details")
			processes := countProcessesInStatus(details)
			if processes >= expected {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %d processes in cluster", expected)
}

// waitForHealthy waits until fdbcli reports the database available.
func (c *Cluster) waitForHealthy(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := c.Coordinator.FDBCLIExec(ctx, "status minimal")
		if err == nil && DatabaseAvailable(output) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for healthy cluster")
}

// countProcessesInStatus parses "status details" output to count FDB processes.
// Looks for lines matching the "Process" section entries (IP:port patterns).
func countProcessesInStatus(details string) int {
	count := 0
	inProcessSection := false
	for _, line := range strings.Split(details, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Process performance details:" {
			inProcessSection = true
			continue
		}
		if inProcessSection && trimmed == "" {
			break // end of process section
		}
		if inProcessSection && strings.Contains(trimmed, ":45") {
			count++
		}
	}
	return count
}

// ClusterFile returns the cluster file for external clients (via coordinator).
func (c *Cluster) ClusterFile(ctx context.Context) (string, error) {
	return c.Coordinator.ClusterFile(ctx)
}

// GetContainerID returns the comma-separated IDs of all cluster containers.
func (c *Cluster) GetContainerID() string {
	ids := make([]string, 0, len(c.Replicas))
	for _, r := range c.Replicas {
		ids = append(ids, r.GetContainerID())
	}
	return strings.Join(ids, ",")
}

// Terminate stops all containers and removes the network, reporting every failure.
func (c *Cluster) Terminate(ctx context.Context) error {
	var errs []error
	for _, r := range c.Replicas {
		if err := Terminate(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	if c.network != nil {
		if err := c.network.Remove(ctx); err != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", c.network.ID, err))
		}
	}
	return errors.Join(errs...)
}

// InternalClusterFile returns the cluster file for containers on the same network.
func (c *Cluster) InternalClusterFile() string {
	return c.Coordinator.InternalClusterFile()
}

// Size returns the number of containers in the cluster.
func (c *Cluster) Size() int {
	return len(c.Replicas)
}
