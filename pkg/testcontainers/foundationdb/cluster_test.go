package foundationdb

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunCluster_ThreeReplicas creates a 3-container FDB cluster and
// verifies all 3 processes are visible to the coordinator.
func TestRunCluster_ThreeReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cluster, err := RunCluster(ctx, 3,
		WithStorageEngine("ssd"),
		WithRedundancyMode("double"),
	)
	if err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	defer cluster.Terminate(ctx)

	// Verify cluster size.
	if cluster.Size() != 3 {
		t.Fatalf("Size: got %d, want 3", cluster.Size())
	}

	// Verify each container is running.
	for i, r := range cluster.Replicas {
		state, err := r.State(ctx)
		if err != nil {
			t.Fatalf("replica %d State: %v", i, err)
		}
		if !state.Running {
			t.Fatalf("replica %d not running", i)
		}
	}

	// Verify cluster file is non-empty.
	cf, err := cluster.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("ClusterFile: %v", err)
	}
	if cf == "" {
		t.Fatal("empty cluster file")
	}

	// Verify FDB sees all 3 processes.
	details, err := cluster.Coordinator.FDBCLIExec(ctx, "status details")
	if err != nil {
		t.Fatalf("status details: %v", err)
	}
	processes := countProcessesInStatus(details)
	if processes < 3 {
		t.Errorf("FDB sees %d processes, want >= 3", processes)
	}
	t.Logf("FDB cluster: %d processes across %d containers", processes, cluster.Size())

	// Verify database is available.
	output, err := cluster.Coordinator.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(output, "Healthy") && !strings.Contains(output, "available") {
		t.Fatalf("expected Healthy/available: %s", output)
	}
	t.Logf("status: %s", strings.TrimSpace(output))
}

// TestRunCluster_Single is the degenerate case — 1 container cluster.
func TestRunCluster_Single(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cluster, err := RunCluster(ctx, 1, WithStorageEngine("ssd"))
	if err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	defer cluster.Terminate(ctx)

	if cluster.Size() != 1 {
		t.Fatalf("Size: got %d, want 1", cluster.Size())
	}

	output, err := cluster.Coordinator.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(output, "Healthy") && !strings.Contains(output, "available") {
		t.Fatalf("expected Healthy: %s", output)
	}
}

// TestRunCluster_InvalidSize verifies validation.
func TestRunCluster_InvalidSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := RunCluster(ctx, 0)
	if err == nil {
		t.Error("expected error for size=0")
	}

	_, err = RunCluster(ctx, 11)
	if err == nil {
		t.Error("expected error for size=11")
	}
}
