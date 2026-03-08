package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestEnvironment encapsulates everything needed for a conformance test
type TestEnvironment struct {
	Container   *foundationdbtc.Container
	RecordDB    *recordlayer.FDBDatabase
	Keyspace    subspace.Subspace
	MetaData    *recordlayer.RecordMetaData
	ClusterFile string // FDB cluster file content for Java interop
}

// SetupTestEnvironment creates a complete test environment with FDB container and record store infrastructure
func SetupTestEnvironment(ctx context.Context, dbName string) (*TestEnvironment, error) {
	// Start FoundationDB container
	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
		foundationdbtc.WithDatabase(dbName),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start FDB container: %w", err)
	}

	// Initialize the database
	err = container.InitializeDatabase(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	// Get FDB database connection
	db, err := container.GetFDBDatabase(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get database connection: %w", err)
	}

	// Create Record Layer database wrapper
	recordDB := recordlayer.NewFDBDatabase(db)

	// Create subspace for this test (using database name as prefix)
	keyspace := subspace.Sub(tuple.Tuple{dbName})

	// Create RecordMetaData for Order schema
	metaData, err := createOrderMetaData()
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to create metadata: %w", err)
	}

	// Get cluster file for Java interop
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get cluster file: %w", err)
	}

	return &TestEnvironment{
		Container:   container,
		RecordDB:    recordDB,
		Keyspace:    keyspace,
		MetaData:    metaData,
		ClusterFile: clusterFile,
	}, nil
}

// Cleanup terminates the container and cleans up resources
func (env *TestEnvironment) Cleanup(ctx context.Context) error {
	if env.Container != nil {
		return env.Container.Terminate(ctx)
	}
	return nil
}

// TenantEnvironment encapsulates everything needed for a tenant-isolated conformance test
// Unlike TestEnvironment which creates a new container per test, this reuses a shared container
// and provides isolation via FDB tenants
type TenantEnvironment struct {
	Container   *foundationdbtc.Container
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	ClusterFile string
	TenantName  string
}

// SetupTenantEnvironment creates a tenant-isolated test environment
// This reuses the provided container and creates an FDB tenant for isolation
func SetupTenantEnvironment(ctx context.Context, container *foundationdbtc.Container, tenantName string) (*TenantEnvironment, error) {
	// Create tenant
	tenant, err := container.CreateTenant(ctx, tenantName)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}

	// Create Record Layer database wrapper for tenant
	recordDB := recordlayer.NewFDBDatabaseFromTenant(tenant)

	// Create metadata
	metaData, err := createOrderMetaData()
	if err != nil {
		_ = container.DeleteTenant(ctx, tenantName)
		return nil, fmt.Errorf("failed to create metadata: %w", err)
	}

	// Get cluster file
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		_ = container.DeleteTenant(ctx, tenantName)
		return nil, fmt.Errorf("failed to get cluster file: %w", err)
	}

	// Use root subspace - tenant provides isolation
	keyspace := subspace.Sub(tuple.Tuple{})

	return &TenantEnvironment{
		Container:   container,
		RecordDB:    recordDB,
		MetaData:    metaData,
		Keyspace:    keyspace,
		ClusterFile: clusterFile,
		TenantName:  tenantName,
	}, nil
}

// Cleanup deletes the tenant (not the container)
func (env *TenantEnvironment) Cleanup(ctx context.Context) error {
	if env.Container != nil && env.TenantName != "" {
		return env.Container.DeleteTenant(ctx, env.TenantName)
	}
	return nil
}

// createOrderMetaData creates RecordMetaData for the Order protobuf schema
func createOrderMetaData() (*recordlayer.RecordMetaData, error) {
	// Build metadata with primary key defined (matches pattern from record layer tests)
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaData := builder.Build()
	return metaData, nil
}
