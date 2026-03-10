package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// IndexStateConformanceStore wraps index state operations for cross-platform testing.
// Uses the indexed metadata (Order$price VALUE index) matching Java's createIndexedMetaData().
type IndexStateConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewIndexStateConformanceStore creates a conformance store for index state persistence tests.
func NewIndexStateConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*IndexStateConformanceStore, error) {
	idx := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &IndexStateConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *IndexStateConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// CreateStoreGo creates the indexed store using Go's CreateOrOpen.
func (s *IndexStateConformanceStore) CreateStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyGo marks an index as WRITE_ONLY using Go.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexWriteOnly(indexName)
		return nil, err
	})
	return err
}

// MarkIndexDisabledGo marks an index as DISABLED using Go.
func (s *IndexStateConformanceStore) MarkIndexDisabledGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexDisabled(indexName)
		return nil, err
	})
	return err
}

// MarkIndexReadableGo marks an index as READABLE using Go.
func (s *IndexStateConformanceStore) MarkIndexReadableGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexReadable(indexName)
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyJava marks an index as WRITE_ONLY using Java.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexWriteOnly", params, nil)
}

// MarkIndexDisabledJava marks an index as DISABLED using Java.
func (s *IndexStateConformanceStore) MarkIndexDisabledJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexDisabled", params, nil)
}

// MarkIndexReadableJava marks an index as READABLE using Java.
func (s *IndexStateConformanceStore) MarkIndexReadableJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexReadable", params, nil)
}

// GetIndexStateRawGo reads the raw index state from FDB using Go (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		isSubspace := s.Keyspace.Sub(int64(5)) // IndexStateSpaceKey = 5
		stateKey := fdb.Key(isSubspace.Pack(tuple.Tuple{indexName}))
		stateBytes, err := rtx.Transaction().Get(stateKey).Get()
		if err != nil {
			return nil, fmt.Errorf("failed to read index state: %w", err)
		}
		if stateBytes == nil {
			state = "READABLE" // Default
			return nil, nil
		}
		valueTuple, err := tuple.Unpack(stateBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to unpack index state value: %w", err)
		}
		code := valueTuple[0].(int64)
		switch recordlayer.IndexState(code) {
		case recordlayer.IndexStateReadable:
			state = "READABLE"
		case recordlayer.IndexStateWriteOnly:
			state = "WRITE_ONLY"
		case recordlayer.IndexStateDisabled:
			state = "DISABLED"
		case recordlayer.IndexStateReadableUniquePending:
			state = "READABLE_UNIQUE_PENDING"
		default:
			state = fmt.Sprintf("UNKNOWN(%d)", code)
		}
		return nil, nil
	})
	return state, err
}

// GetIndexStateRawJava reads the raw index state from FDB using Java (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawJava(ctx context.Context, indexName string) (string, error) {
	params := s.buildJavaParams()
	params["indexName"] = indexName

	var state string
	if err := s.java.InvokeAs(ctx, "getIndexStateRaw", params, &state); err != nil {
		return "", fmt.Errorf("java getIndexStateRaw failed: %w", err)
	}
	return state, nil
}

// GetIndexStateViaOpenGo opens the store with Go and reads the index state through the store API.
func (s *IndexStateConformanceStore) GetIndexStateViaOpenGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).Open()
		if err != nil {
			return nil, err
		}
		state = store.GetIndexState(indexName).String()
		return nil, nil
	})
	return state, err
}
