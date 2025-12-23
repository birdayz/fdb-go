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

// OpenRecordStore opens a record store in this environment
func (env *TestEnvironment) OpenRecordStore(ctx context.Context) (*recordlayer.FDBRecordStore, error) {
	var store *recordlayer.FDBRecordStore
	_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		var err error
		store, err = recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(env.MetaData).
			SetSubspace(env.Keyspace).
			CreateOrOpen()
		return nil, err
	})
	return store, err
}

// createOrderMetaData creates RecordMetaData for the Order protobuf schema
func createOrderMetaData() (*recordlayer.RecordMetaData, error) {
	// Build metadata with primary key defined (matches pattern from record layer tests)
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	metaData := builder.Build()
	return metaData, nil
}
