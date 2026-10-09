//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"os"

	"fdb.dev/gen"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	. "github.com/onsi/gomega"
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
	// Reuse the shared database connection (opened once in BeforeSuite).
	db := sharedDB

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
func (env *TenantEnvironment) Cleanup(_ context.Context) error {
	deleteTenant(env.DB, env.TenantName)
	return nil
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

// writeClusterFileToTemp materialises the cluster file string contents
// (env.ClusterFile) to a temp file on disk and returns its path. The
// Go embedded SQL driver's DSN takes a `cluster_file=<path>` option,
// not the file contents — so the conformance test writes once per It
// block and removes it on cleanup.
func writeClusterFileToTemp(contents string) string {
	f, err := os.CreateTemp("", "fdb-conformance-*.cluster")
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(contents)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
	return f.Name()
}
