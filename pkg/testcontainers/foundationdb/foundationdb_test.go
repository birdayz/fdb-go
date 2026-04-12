package foundationdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRun_DefaultOptions(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Verify default config.
	if container.APIVersion() != defaultAPIVersion {
		t.Fatalf("APIVersion: got %d, want %d", container.APIVersion(), defaultAPIVersion)
	}
	if container.Database() != "test" {
		t.Fatalf("Database: got %q, want %q", container.Database(), "test")
	}
	if container.FDBPort() != defaultFDBPort {
		t.Fatalf("FDBPort: got %d, want %d", container.FDBPort(), defaultFDBPort)
	}

	// Container should be running (auto-initialized).
	state, err := container.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Running {
		t.Fatal("container not running")
	}

	// Cluster file should be non-empty and use localhost:mappedPort.
	cf, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("ClusterFile: %v", err)
	}
	if cf == "" {
		t.Fatal("empty cluster file")
	}
	if !strings.Contains(cf, "docker:docker@") {
		t.Fatalf("cluster file %q missing docker:docker@ prefix", cf)
	}
	if !strings.Contains(cf, "localhost:") {
		t.Fatalf("cluster file %q missing localhost:", cf)
	}

	// ConnectionString should return host:mappedPort.
	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	if !strings.Contains(connStr, ":") {
		t.Fatalf("ConnectionString %q missing port separator", connStr)
	}
}

func TestRun_WithCustomOptions(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithDatabase("custom_db"),
		WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	if container.Database() != "custom_db" {
		t.Fatalf("Database: got %q, want %q", container.Database(), "custom_db")
	}
	if container.APIVersion() != 720 {
		t.Fatalf("APIVersion: got %d, want 720", container.APIVersion())
	}
}

func TestRun_WithoutInit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "", WithoutInit())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Container is running but database is NOT configured.
	state, err := container.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Running {
		t.Fatal("container not running")
	}

	// Now manually initialize.
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("InitializeDatabase: %v", err)
	}
}

func TestClusterFilePath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		t.Fatalf("ClusterFilePath: %v", err)
	}
	if path == "" {
		t.Fatal("empty path")
	}
	defer os.Remove(path)

	// Read the file and verify content matches ClusterFile.
	cf, _ := container.ClusterFile(ctx)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cluster file: %v", err)
	}
	if strings.TrimSpace(string(data)) != cf {
		t.Fatalf("file content %q != ClusterFile %q", string(data), cf)
	}
}

func TestMustClusterFile(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	cf := container.MustClusterFile(ctx)
	if cf == "" {
		t.Fatal("empty cluster file")
	}
}

func TestFDBCLIExec(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	output, err := container.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("FDBCLIExec: %v", err)
	}
	if output == "" {
		t.Fatal("empty status output")
	}
	t.Logf("FDB status: %s", strings.TrimSpace(output))
}

func TestInitializeDatabase_Idempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "") // auto-initialized
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Calling InitializeDatabase again should not fail.
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("second InitializeDatabase: %v", err)
	}
}

func TestInternalClusterFile(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	icf := container.InternalClusterFile()
	cf, _ := container.ClusterFile(ctx)

	// InternalClusterFile uses container IP, ClusterFile uses localhost:mappedPort.
	// Both should share the same description:id prefix.
	if !strings.Contains(icf, "docker:docker@") {
		t.Fatalf("InternalClusterFile %q missing docker:docker@ prefix", icf)
	}
	if !strings.Contains(cf, "docker:docker@") {
		t.Fatalf("ClusterFile %q missing docker:docker@ prefix", cf)
	}
	// Internal should have container IP, external should have localhost.
	if strings.Contains(icf, "localhost") {
		t.Fatalf("InternalClusterFile should use container IP, got %q", icf)
	}
	if !strings.Contains(cf, "localhost") {
		t.Fatalf("ClusterFile should use localhost, got %q", cf)
	}
}

func TestNetworkName_DefaultBridge(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	if name := container.NetworkName(); name != "" {
		t.Fatalf("NetworkName: got %q, want empty", name)
	}
}

func TestMultipleContainers_Isolation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c1, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run container 1: %v", err)
	}
	defer c1.Terminate(ctx)

	c2, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run container 2: %v", err)
	}
	defer c2.Terminate(ctx)

	// Different containers should have different IPs.
	if c1.containerIP == c2.containerIP {
		t.Fatalf("containers have same IP: %s", c1.containerIP)
	}

	// Different cluster files.
	cf1, _ := c1.ClusterFile(ctx)
	cf2, _ := c2.ClusterFile(ctx)
	if cf1 == cf2 {
		t.Fatalf("containers have same cluster file: %s", cf1)
	}

	// Both should be running.
	s1, _ := c1.State(ctx)
	s2, _ := c2.State(ctx)
	if !s1.Running || !s2.Running {
		t.Fatal("not all containers running")
	}
}

func TestPause_Unpause(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Pause the container.
	if err := container.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Verify paused state.
	inspect, err := container.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !inspect.State.Paused {
		t.Fatal("container not paused after Pause()")
	}

	// Unpause.
	if err := container.Unpause(ctx); err != nil {
		t.Fatalf("Unpause: %v", err)
	}

	// Verify running again.
	inspect, err = container.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspect.State.Paused {
		t.Fatal("container still paused after Unpause()")
	}

	// FDB should recover — verify with status command.
	_, err = container.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("FDBCLIExec after unpause: %v", err)
	}
}

func TestWithNetwork_SharedNetwork(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	nw, err := CreateNetwork(ctx)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	defer nw.Remove(ctx)

	container, err := Run(ctx, "", WithNetwork(nw, "fdb-test"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	if container.NetworkName() != nw.Name {
		t.Fatalf("NetworkName: got %q, want %q", container.NetworkName(), nw.Name)
	}

	cf, _ := container.ClusterFile(ctx)
	if cf == "" {
		t.Fatal("empty cluster file")
	}
}

func TestOptionValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		option Option
	}{
		{"invalid port low", WithFDBPort(0)},
		{"invalid port high", WithFDBPort(70000)},
		{"invalid tenant mode", WithTenantMode("invalid")},
		{"invalid storage engine", WithStorageEngine("invalid")},
		{"invalid redundancy", WithRedundancyMode("quad")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultOptions()
			err := tt.option.apply(&cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestWithVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithVersion("7.1.61"),
		WithAPIVersion(710),
	)
	if err != nil {
		t.Fatalf("Run with version 7.1.61: %v", err)
	}
	defer container.Terminate(ctx)

	if container.Version() != "7.1.61" {
		t.Fatalf("Version: got %q, want 7.1.61", container.Version())
	}
	if container.APIVersion() != 710 {
		t.Fatalf("APIVersion: got %d, want 710", container.APIVersion())
	}
}
