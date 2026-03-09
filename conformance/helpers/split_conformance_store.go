package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// SplitConformanceStore wraps split record operations and cross-validates with Java.
// It uses split-enabled metadata on both Go and Java sides so that records >100KB
// are stored as multiple FDB key-value pairs and can be read by either implementation.
type SplitConformanceStore struct {
	recordDB    *recordlayer.FDBDatabase
	metaData    *recordlayer.RecordMetaData
	keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewSplitConformanceStore creates a split-enabled conformance store for cross-validation.
func NewSplitConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*SplitConformanceStore, error) {
	md, err := createSplitOrderMetaData()
	if err != nil {
		return nil, fmt.Errorf("failed to create split metadata: %w", err)
	}

	return &SplitConformanceStore{
		recordDB:    recordDB,
		metaData:    md,
		keyspace:    keyspace,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

// buildJavaParams builds base parameters for Java invocations.
func (s *SplitConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveRecord saves a record with Go (split enabled), then has Java load it to verify
// Java can read Go's split chunks. Also reads back with Go for sanity.
func (s *SplitConformanceStore) SaveRecord(ctx context.Context, msg proto.Message) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records are supported in split conformance tests")
	}

	// 1. Save with Go (split-enabled metadata)
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metaData).
			SetSubspace(s.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(msg)
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	// 2. Java loads what Go wrote (validates Java can read Go's split chunks)
	var javaOrder gen.Order
	params := s.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = s.java.InvokeAs(ctx, "loadSplitOrder", params, &javaOrder)
	if err != nil {
		return fmt.Errorf("java cross-check read of Go-written split record failed: %w", err)
	}

	// 3. Go loads back its own data
	goOrder, err := s.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go cross-check read failed: %w", err)
	}

	// 4. Compare
	if !proto.Equal(goOrder, &javaOrder) {
		return fmt.Errorf("split conformance mismatch: Java read differs from Go read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return nil
}

// JavaSaveThenGoLoad has Java save a record (with split enabled), then Go loads it.
// Validates Go can reassemble Java's split chunks.
func (s *SplitConformanceStore) JavaSaveThenGoLoad(ctx context.Context, order *gen.Order) (*gen.Order, error) {
	// 1. Java saves the record with split-enabled metadata
	params := s.buildJavaParams()
	params["order"] = order
	err := s.java.InvokeAs(ctx, "saveSplitOrder", params, nil)
	if err != nil {
		return nil, fmt.Errorf("java save of split record failed: %w", err)
	}

	// 2. Go loads what Java wrote
	goOrder, err := s.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return nil, fmt.Errorf("go load of Java-written split record failed: %w", err)
	}

	// 3. Also verify Java can read its own data
	var javaOrder gen.Order
	params = s.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = s.java.InvokeAs(ctx, "loadSplitOrder", params, &javaOrder)
	if err != nil {
		return nil, fmt.Errorf("java cross-check read of its own split record failed: %w", err)
	}

	// 4. Compare Go and Java reads
	if !proto.Equal(goOrder, &javaOrder) {
		return nil, fmt.Errorf("split conformance mismatch: Go read differs from Java read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return goOrder, nil
}

// loadRecordWithGo loads a record using only Go with split-enabled metadata.
func (s *SplitConformanceStore) loadRecordWithGo(ctx context.Context, orderID int64) (*gen.Order, error) {
	var order *gen.Order
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metaData).
			SetSubspace(s.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}

		if storedRecord == nil {
			return nil, fmt.Errorf("record not found: %d", orderID)
		}

		order = storedRecord.Record.(*gen.Order)
		return nil, nil
	})
	return order, err
}

// createSplitOrderMetaData creates RecordMetaData with split long records enabled.
// Must match the Java createSplitMetaData() configuration exactly.
func createSplitOrderMetaData() (*recordlayer.RecordMetaData, error) {
	builder := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto).
		SetSplitLongRecords(true)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	return builder.Build()
}
