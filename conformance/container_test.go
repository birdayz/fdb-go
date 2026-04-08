package conformance_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/gen"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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

	// Get pure Go FDB database connection
	db, err := openGoDatabase(ctx, container)
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
	DB          gofdb.Database
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	ClusterFile string
	TenantName  string
}

// SetupTenantEnvironment creates a tenant-isolated test environment
// This reuses the provided container and creates an FDB tenant for isolation
func SetupTenantEnvironment(ctx context.Context, container *foundationdbtc.Container, tenantName string) (*TenantEnvironment, error) {
	// Get pure Go database for tenant creation
	db, err := openGoDatabase(ctx, container)
	if err != nil {
		return nil, fmt.Errorf("failed to get database: %w", err)
	}

	// Create tenant via fdbcli and open with pure Go client
	tenant, err := createGoTenant(ctx, container, db, tenantName)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenant: %w", err)
	}

	// Create Record Layer database wrapper for tenant
	recordDB := recordlayer.NewFDBDatabaseFromTenant(tenant)

	// Create metadata
	metaData, err := createOrderMetaData()
	if err != nil {
		_, _, _ = container.Exec(ctx, []string{"/usr/bin/fdbcli", "--exec", fmt.Sprintf("deletetenant %s", tenantName)})
		return nil, fmt.Errorf("failed to create metadata: %w", err)
	}

	// Get cluster file
	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		_, _, _ = container.Exec(ctx, []string{"/usr/bin/fdbcli", "--exec", fmt.Sprintf("deletetenant %s", tenantName)})
		return nil, fmt.Errorf("failed to get cluster file: %w", err)
	}

	// Use root subspace - tenant provides isolation
	keyspace := subspace.Sub(tuple.Tuple{})

	return &TenantEnvironment{
		Container:   container,
		DB:          db,
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
		exitCode, _, err := env.Container.Exec(ctx, []string{
			"/usr/bin/fdbcli", "--exec", fmt.Sprintf("deletetenant %s", env.TenantName),
		})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("fdbcli deletetenant exit %d", exitCode)
		}
	}
	return nil
}

func openGoDatabase(ctx context.Context, container *foundationdbtc.Container) (gofdb.Database, error) {
	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		return gofdb.Database{}, err
	}
	gofdb.MustAPIVersion(730)
	return gofdb.OpenDatabase(path)
}

func createGoTenant(ctx context.Context, container *foundationdbtc.Container, db gofdb.Database, name string) (gofdb.Tenant, error) {
	// Create tenant via fdbcli — the special key space (\xff\xff) requires
	// client-side routing that our pure Go client doesn't yet support.
	exitCode, output, err := container.Exec(ctx, []string{
		"/usr/bin/fdbcli", "--exec", fmt.Sprintf("createtenant %s", name),
	})
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli createtenant: %w", err)
	}
	outputBytes, _ := io.ReadAll(output)
	if exitCode != 0 {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli createtenant exit %d: %s", exitCode, outputBytes)
	}

	// Get tenant ID via fdbcli gettenant (returns JSON with "id" field).
	exitCode, output, err = container.Exec(ctx, []string{
		"/usr/bin/fdbcli", "--exec", fmt.Sprintf("gettenant %s", name),
	})
	if err != nil {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli gettenant: %w", err)
	}
	outputBytes, _ = io.ReadAll(output)
	if exitCode != 0 {
		return gofdb.Tenant{}, fmt.Errorf("fdbcli gettenant exit %d: %s", exitCode, outputBytes)
	}

	// Parse "id" from fdbcli output. Docker exec prepends binary stream headers,
	// so we search for "id:" anywhere in the output.
	var tenantId int64
	outStr := string(outputBytes)
	if idx := strings.Index(outStr, "id:"); idx >= 0 {
		fmt.Sscanf(outStr[idx:], "id: %d", &tenantId)
	}
	if tenantId == 0 {
		return gofdb.Tenant{}, fmt.Errorf("could not parse tenant id from: %s", outputBytes)
	}

	return db.OpenTenantById(tenantId), nil
}

// createOrderMetaData creates RecordMetaData for the Order protobuf schema
func createOrderMetaData() (*recordlayer.RecordMetaData, error) {
	// Build metadata with primary key defined (matches pattern from record layer tests)
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	metaData, err := builder.Build()
	if err != nil {
		return nil, err
	}
	return metaData, nil
}
