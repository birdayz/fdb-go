package foundationdb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

func TestFoundationDBContainer(t *testing.T) {
	ctx := context.Background()

	container, err := Run(ctx, "",
		WithDatabase("test_db"),
		WithAPIVersion(720),
	)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize the database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Test that container is running
	state, err := container.State(ctx)
	if err != nil {
		t.Fatalf("Failed to get container state: %v", err)
	}
	if !state.Running {
		t.Fatal("Expected container to be running")
	}

	// Test connection string
	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("Failed to get connection string: %v", err)
	}
	if connStr == "" {
		t.Fatal("Expected non-empty connection string")
	}
	if !strings.Contains(connStr, ":") {
		t.Fatalf("Expected connection string to contain port, got: %s", connStr)
	}

	// Test cluster file
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("Failed to get cluster file: %v", err)
	}
	if !strings.Contains(clusterFile, "docker:docker@") {
		t.Fatalf("Expected cluster file to contain docker format, got: %s", clusterFile)
	}

	// Test configuration
	if container.APIVersion() != 720 {
		t.Fatalf("Expected API version 720, got: %d", container.APIVersion())
	}
	if container.Database() != "test_db" {
		t.Fatalf("Expected database 'test_db', got: %s", container.Database())
	}
}

func TestFoundationDBContainerWithDefaults(t *testing.T) {
	ctx := context.Background()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize the database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Test default configuration
	if container.APIVersion() != defaultAPIVersion {
		t.Fatalf("Expected default API version %d, got: %d", defaultAPIVersion, container.APIVersion())
	}
	if container.Database() != "test" {
		t.Fatalf("Expected default database 'test', got: %s", container.Database())
	}
}

func TestFoundationDBContainerWithCustomOptions(t *testing.T) {
	ctx := context.Background()

	container, err := Run(ctx, "",
		WithDatabase("custom_db"),
		WithAPIVersion(630),
		WithMemory("2GB"),
		WithVersion("7.1.61"),
		testcontainers.WithEnv(map[string]string{
			"CUSTOM_ENV": "test_value",
		}),
	)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// Initialize the database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		t.Fatalf("Failed to initialize database: %v", err)
	}

	// Test custom configuration
	if container.Database() != "custom_db" {
		t.Fatalf("Expected database 'custom_db', got: %s", container.Database())
	}
	if container.APIVersion() != 630 {
		t.Fatalf("Expected API version 630, got: %d", container.APIVersion())
	}
	if container.Version() != "7.1.61" {
		t.Fatalf("Expected version '7.1.61', got: %s", container.Version())
	}
}

func TestFoundationDBContainerStartup(t *testing.T) {
	ctx := context.Background()

	// Test with a reasonable timeout
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// If we get here, the container started successfully
	t.Log("FoundationDB container started successfully")
}

func TestFoundationDBContainerVersionOption(t *testing.T) {
	ctx := context.Background()

	// Test with a different version
	container, err := Run(ctx, "",
		WithVersion("7.1.61"),
		WithAPIVersion(710),
	)
	if err != nil {
		t.Fatalf("Failed to start container with version 7.1.61: %v", err)
	}
	defer func() {
		if err := container.Terminate(ctx); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// Test that the version was set correctly
	if container.Version() != "7.1.61" {
		t.Fatalf("Expected version '7.1.61', got: %s", container.Version())
	}

	// Test that the API version was set correctly
	if container.APIVersion() != 710 {
		t.Fatalf("Expected API version 710, got: %d", container.APIVersion())
	}

	// Test that container starts successfully
	state, err := container.State(ctx)
	if err != nil {
		t.Fatalf("Failed to get container state: %v", err)
	}
	if !state.Running {
		t.Fatal("Expected container to be running")
	}

	t.Logf("Successfully started FoundationDB version %s with API version %d",
		container.Version(), container.APIVersion())
}
