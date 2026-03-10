package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// FanOutIndexConformanceStore wraps record operations with a FanOut VALUE index on Order.tags
// and provides methods to cross-validate index entries between Go and Java.
type FanOutIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	TagsIndex   *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewFanOutIndexConformanceStore creates a conformance store with a FanOut VALUE index on Order.tags.
func NewFanOutIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*FanOutIndexConformanceStore, error) {
	tagsIndex := recordlayer.NewIndex("Order$tags", recordlayer.FanOut("tags"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", tagsIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &FanOutIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		TagsIndex:   tagsIndex,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *FanOutIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(order)
		return nil, err
	})
	return err
}

// SaveOrderJava saves an order via Java (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithFanOutIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		deleted, err = store.DeleteRecord(tuple.Tuple{orderID})
		return nil, err
	})
	return deleted, err
}

// DeleteOrderJava deletes an order via Java (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithFanOutIndex", params, nil)
}

// ScanIndexGo scans the tags fan-out index using Go and returns results.
func (s *FanOutIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.TagsIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, IndexEntryResult{
				Key:        tupleToSlice(e.Key),
				PrimaryKey: tupleToSlice(e.PrimaryKey()),
			})
		}
		return nil, nil
	})
	return results, err
}

// ScanIndexJava scans the tags fan-out index using Java and returns results.
func (s *FanOutIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$tags"

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanFanOutIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanFanOutIndex failed: %w", err)
	}

	var results []IndexEntryResult
	for _, m := range javaResults {
		entry := IndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if pkRaw, ok := m["primaryKey"]; ok {
			entry.PrimaryKey = toInterfaceSlice(pkRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}
