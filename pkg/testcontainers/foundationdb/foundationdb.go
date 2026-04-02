// Package foundationdb provides a testcontainers module for FoundationDB.
//
// This module creates a FoundationDB container with proper networking using socat proxy
// to solve Docker port mapping issues. It provides an easy way to start FoundationDB
// containers for testing Go applications that use the FoundationDB Record Layer.
//
// The module handles the complex networking setup required by FoundationDB's strict
// port matching requirements while providing a simple API for container creation
// and database initialization.
package foundationdb

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/socat"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	defaultImage      = "foundationdb/foundationdb"
	defaultClientPort = "4500/tcp"
	defaultAPIVersion = 730
)

// fdbVersion returns the FDB version from the FDB_VERSION env var
// (set by .bazelrc for Bazel tests) or falls back to a default.
func fdbVersion() string {
	if v := os.Getenv("FDB_VERSION"); v != "" {
		return v
	}
	return "7.3.75" // fallback for non-Bazel runs
}

var (
	apiVersionOnce  sync.Once
	apiVersionMutex sync.RWMutex
	apiVersionSet   int
)

// Container represents a FoundationDB container instance with socat proxy
type Container struct {
	testcontainers.Container
	socatContainer  testcontainers.Container
	network         *testcontainers.DockerNetwork
	config          Config
	tempClusterFile string
	externalPort    int
	cachedDB        fdb.Database
	dbInitialized   bool
	dbMutex         sync.Mutex
}

// Config holds the configuration for the FoundationDB container
type Config struct {
	Database   string
	APIVersion int
	Memory     string
	Version    string
}

// Run creates and starts a FoundationDB container using socat proxy pattern
func Run(ctx context.Context, img string, opts ...testcontainers.ContainerCustomizer) (*Container, error) {
	config := Config{
		Database:   "test",
		APIVersion: defaultAPIVersion,
		Memory:     "4GB",
		Version:    fdbVersion(),
	}

	// Process config customizers first to get version
	for _, opt := range opts {
		if customizer, ok := opt.(*configCustomizer); ok {
			customizer.customize(&config)
		}
	}

	if img == "" {
		img = fmt.Sprintf("%s:%s", defaultImage, config.Version)
	}

	// Step 1: Create a dedicated network
	nw, err := network.New(ctx, network.WithDriver("bridge"))
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	// Step 2: Create custom socat container with custom entrypoint (waits for config)
	socatEntrypoint, err := mountsFS.ReadFile("mounts/socat-entrypoint.sh")
	if err != nil {
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("read socat entrypoint: %w", err)
	}

	socatReq := testcontainers.ContainerRequest{
		Image:        socat.DefaultImage,
		ExposedPorts: []string{"4500/tcp"}, // We'll map this dynamically
		Files: []testcontainers.ContainerFile{
			{
				Reader:            strings.NewReader(string(socatEntrypoint)),
				ContainerFilePath: "/entrypoint-tc.sh",
				FileMode:          0755,
			},
		},
		Entrypoint: []string{"/entrypoint-tc.sh"},
	}

	socatGenericReq := testcontainers.GenericContainerRequest{
		ContainerRequest: socatReq,
		Started:          false,
	}

	// Add to network
	socatGenericReq.Networks = []string{nw.Name}
	socatGenericReq.NetworkAliases = map[string][]string{
		nw.Name: {"socat"},
	}

	socatContainer, err := testcontainers.GenericContainer(ctx, socatGenericReq)
	if err != nil {
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("create socat container: %w", err)
	}

	// Step 3: Start socat container to get mapped port
	err = socatContainer.Start(ctx)
	if err != nil {
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("start socat container: %w", err)
	}

	// Get mapped port from socat container
	mappedPort, err := socatContainer.MappedPort(ctx, "4500/tcp")
	if err != nil {
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("get socat mapped port: %w", err)
	}

	// Use the external mapped port as the internal port that both containers will use
	sharedPort := mappedPort.Int()

	// Step 4: Inject socat configuration (listen on internal port 4500, forward to foundationdb on shared port)
	socatConfig := fmt.Sprintf("# Injected by testcontainers\nTARGET_PORT=%d", sharedPort)
	err = socatContainer.CopyToContainer(ctx, []byte(socatConfig), "/tmp/socat.conf", 0644)
	if err != nil {
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("copy socat config: %w", err)
	}

	// Step 5: Create FoundationDB container with custom entrypoint
	fdbEntrypoint, err := mountsFS.ReadFile("mounts/fdb-entrypoint.sh")
	if err != nil {
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("read FDB entrypoint: %w", err)
	}

	fdbReq := testcontainers.ContainerRequest{
		Image: img,
		Files: []testcontainers.ContainerFile{
			{
				Reader:            strings.NewReader(string(fdbEntrypoint)),
				ContainerFilePath: "/entrypoint-tc.sh",
				FileMode:          0755,
			},
		},
		Entrypoint: []string{"/entrypoint-tc.sh"},
		// Don't wait here - we'll wait after injecting config
	}

	fdbGenericReq := testcontainers.GenericContainerRequest{
		ContainerRequest: fdbReq,
		Started:          false,
	}

	// Apply custom options
	for _, opt := range opts {
		if _, ok := opt.(*configCustomizer); ok {
			continue
		}
		if err := opt.Customize(&fdbGenericReq); err != nil {
			_ = socatContainer.Terminate(ctx)
			_ = nw.Remove(ctx)
			return nil, fmt.Errorf("customize request: %w", err)
		}
	}

	// Add to network
	fdbGenericReq.Networks = []string{nw.Name}
	fdbGenericReq.NetworkAliases = map[string][]string{
		nw.Name: {"foundationdb"},
	}

	container, err := testcontainers.GenericContainer(ctx, fdbGenericReq)
	if err != nil {
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("create FDB container: %w", err)
	}

	// Step 6: Start FDB container
	err = container.Start(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("start FDB container: %w", err)
	}

	// Step 7: Inject FDB configuration with the shared port (same as socat)
	fdbConfig := fmt.Sprintf("# Injected by testcontainers\nFDB_PORT=%d", sharedPort)
	err = container.CopyToContainer(ctx, []byte(fdbConfig), "/tmp/fdb.conf", 0644)
	if err != nil {
		_ = container.Terminate(ctx)
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("copy FDB config: %w", err)
	}

	// Wait for FoundationDB to be ready
	waitStrategy := wait.ForLog("FDBD joined cluster").WithStartupTimeout(30 * time.Second)
	err = waitStrategy.WaitUntilReady(ctx, container)
	if err != nil {
		_ = container.Terminate(ctx)
		_ = socatContainer.Terminate(ctx)
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("wait for FDB: %w", err)
	}

	return &Container{
		Container:      container,
		socatContainer: socatContainer,
		network:        nw,
		config:         config,
		externalPort:   sharedPort, // This is the shared port for client connections
	}, nil
}

// ConnectionString returns the connection string to connect to the FoundationDB cluster via socat
func (c *Container) ConnectionString(ctx context.Context) (string, error) {
	// Get socat container host and mapped port
	host, err := c.socatContainer.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("get socat host: %w", err)
	}

	// Get the mapped port for our socat container
	port, err := c.socatContainer.MappedPort(ctx, "4500/tcp")
	if err != nil {
		return "", fmt.Errorf("get socat mapped port: %w", err)
	}

	return fmt.Sprintf("%s:%s", host, port.Port()), nil
}

// ClusterFile returns the cluster file content for external Go client via socat
func (c *Container) ClusterFile(ctx context.Context) (string, error) {
	// Get socat container host and mapped port
	host, err := c.socatContainer.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("get socat host: %w", err)
	}

	// Get the mapped port for our socat container
	port, err := c.socatContainer.MappedPort(ctx, "4500/tcp")
	if err != nil {
		return "", fmt.Errorf("get socat mapped port: %w", err)
	}

	// Return cluster file content (format must match Java: "docker:docker@host:port")
	return fmt.Sprintf("docker:docker@%s:%s", host, port.Port()), nil
}

// NetworkName returns the Docker network name this container is on.
// Use this to attach other containers to the same network.
func (c *Container) NetworkName() string {
	return c.network.Name
}

// InternalAddress returns the FDB address reachable from within the Docker network.
// Format: "foundationdb:<port>" — the container alias on the shared network.
func (c *Container) InternalAddress() string {
	return fmt.Sprintf("foundationdb:%d", c.externalPort)
}

// InternalClusterFile returns a cluster file string usable from within the Docker network.
func (c *Container) InternalClusterFile() string {
	return fmt.Sprintf("docker:docker@%s", c.InternalAddress())
}

// Exec runs a command inside the FDB container.
func (c *Container) Exec(ctx context.Context, cmd []string) (int, io.Reader, error) {
	return c.Container.Exec(ctx, cmd)
}

// APIVersion returns the configured FDB API version
func (c *Container) APIVersion() int {
	return c.config.APIVersion
}

// Database returns the configured database name
func (c *Container) Database() string {
	return c.config.Database
}

// Version returns the configured FoundationDB version
func (c *Container) Version() string {
	return c.config.Version
}

// InitializeDatabase configures the FoundationDB database for single-node operation
func (c *Container) InitializeDatabase(ctx context.Context) error {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before initialization: %w", err)
	}

	// Add a timeout for the database initialization to prevent hanging
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Give FDB a moment to be fully ready after "FDBD joined cluster" log appears
	time.Sleep(2 * time.Second)

	// Run fdbcli WITHOUT --cluster-file (it uses the default /etc/foundationdb/fdb.cluster)
	// Enable tenant_mode=optional_experimental to support multi-tenancy
	exitCode, output, err := c.Exec(initCtx, []string{
		"/usr/bin/fdbcli", "--exec", "configure new single memory tenant_mode=optional_experimental",
	})

	outputBytes, _ := io.ReadAll(output)
	outputStr := string(outputBytes)

	if err != nil {
		// Check if it was a timeout
		if initCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timeout initializing database (60s exceeded), output: %s", outputStr)
		}
		return fmt.Errorf("failed to run fdbcli: %w (output: %s)", err, outputStr)
	}

	if exitCode != 0 {
		return fmt.Errorf("fdbcli exited with code %d: %s", exitCode, outputStr)
	}

	// Check for success message
	if !strings.Contains(outputStr, "Database created") {
		return fmt.Errorf("database not created, output: %s", outputStr)
	}

	return nil
}

// GetFDBDatabase returns a ready-to-use FDB database connection for record layer
// This method caches the database connection - calling it multiple times returns the same connection
func (c *Container) GetFDBDatabase(ctx context.Context) (fdb.Database, error) {
	c.dbMutex.Lock()
	defer c.dbMutex.Unlock()

	// Return cached database if already initialized
	if c.dbInitialized {
		return c.cachedDB, nil
	}

	// Get cluster file content from container
	clusterFile, err := c.ClusterFile(ctx)
	if err != nil {
		var empty fdb.Database
		return empty, fmt.Errorf("failed to get cluster file: %w", err)
	}

	// Create temporary cluster file
	tmpFile, err := os.CreateTemp("", "fdb_cluster_*.txt")
	if err != nil {
		var empty fdb.Database
		return empty, fmt.Errorf("failed to create temp cluster file: %w", err)
	}

	if _, err := io.WriteString(tmpFile, clusterFile); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		var empty fdb.Database
		return empty, fmt.Errorf("failed to write cluster file: %w", err)
	}
	_ = tmpFile.Close()

	// Initialize FDB API version (only once per process)
	apiVersionOnce.Do(func() {
		fdb.MustAPIVersion(c.APIVersion())
		apiVersionMutex.Lock()
		apiVersionSet = c.APIVersion()
		apiVersionMutex.Unlock()
	})

	// Verify API version matches (in case different containers request different versions)
	apiVersionMutex.RLock()
	currentVersion := apiVersionSet
	apiVersionMutex.RUnlock()

	if currentVersion != c.APIVersion() {
		_ = os.Remove(tmpFile.Name())
		var empty fdb.Database
		return empty, fmt.Errorf("FDB API version mismatch: already set to %d, requested %d (can only set once per process)", currentVersion, c.APIVersion())
	}

	db, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		var empty fdb.Database
		return empty, fmt.Errorf("failed to open FDB database: %w", err)
	}

	// Store the temp file path for cleanup
	c.tempClusterFile = tmpFile.Name()

	// Cache the database connection
	c.cachedDB = db
	c.dbInitialized = true

	return db, nil
}

// CreateTenant creates an FDB tenant for test isolation
// The tenant provides a completely isolated keyspace within the same database
// Returns a Tenant handle that can be used with FDBDatabase
func (c *Container) CreateTenant(ctx context.Context, name string) (fdb.Tenant, error) {
	// Ensure database is initialized
	c.dbMutex.Lock()
	if !c.dbInitialized {
		c.dbMutex.Unlock()
		return fdb.Tenant{}, fmt.Errorf("database not initialized, call GetFDBDatabase first")
	}
	db := c.cachedDB
	c.dbMutex.Unlock()

	// Create tenant using FDB API
	tenantKey := fdb.Key(name)
	err := db.CreateTenant(tenantKey)
	if err != nil {
		return fdb.Tenant{}, fmt.Errorf("failed to create tenant %q: %w", name, err)
	}

	// Open and return tenant handle
	tenant, err := db.OpenTenant(tenantKey)
	if err != nil {
		// Try to clean up the tenant we just created
		_ = db.DeleteTenant(tenantKey)
		return fdb.Tenant{}, fmt.Errorf("failed to open tenant %q: %w", name, err)
	}

	return tenant, nil
}

// DeleteTenant deletes an FDB tenant and all its data
// This provides atomic cleanup of all tenant data
func (c *Container) DeleteTenant(ctx context.Context, name string) error {
	c.dbMutex.Lock()
	if !c.dbInitialized {
		c.dbMutex.Unlock()
		return nil // Nothing to clean up
	}
	db := c.cachedDB
	c.dbMutex.Unlock()

	tenantKey := fdb.Key(name)
	err := db.DeleteTenant(tenantKey)
	if err != nil {
		return fmt.Errorf("failed to delete tenant %q: %w", name, err)
	}

	return nil
}

// Terminate terminates both containers, cleans up network and temporary files
func (c *Container) Terminate(ctx context.Context) error {
	var errs []error

	// Clean up temporary files first (protected by mutex)
	c.dbMutex.Lock()
	if c.tempClusterFile != "" {
		if err := os.Remove(c.tempClusterFile); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove cluster file: %w", err))
		}
		c.tempClusterFile = ""
	}
	c.dbInitialized = false
	c.dbMutex.Unlock()

	// Terminate FoundationDB container
	if err := c.Container.Terminate(ctx); err != nil {
		errs = append(errs, fmt.Errorf("terminate FDB container: %w", err))
	}

	// Terminate socat container
	if c.socatContainer != nil {
		if err := c.socatContainer.Terminate(ctx); err != nil {
			errs = append(errs, fmt.Errorf("terminate socat container: %w", err))
		}
	}

	// Remove network
	if c.network != nil {
		if err := c.network.Remove(ctx); err != nil {
			errs = append(errs, fmt.Errorf("remove network: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}
	return nil
}
