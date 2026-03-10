package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// MaxEverTupleIndexConformanceStore wraps record operations with a MAX_EVER_TUPLE
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MAX_EVER_TUPLE
type MaxEverTupleIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MaxIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMaxEverTupleIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MaxEverTupleIndexConformanceStore, error) {
	maxIdx := recordlayer.NewMaxEverTupleIndex("max_ever_price_tuple", recordlayer.Ungrouped(recordlayer.Field("price")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", maxIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &MaxEverTupleIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MaxIndex:    maxIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MaxEverTupleIndexConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MaxEverTupleIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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

func (s *MaxEverTupleIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMaxEverTupleIndex", params, nil)
}

func (s *MaxEverTupleIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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

func (s *MaxEverTupleIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMaxEverTupleIndex", params, nil)
}

func (s *MaxEverTupleIndexConformanceStore) ScanMaxEverTupleIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	var results []MinMaxEverIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.MaxIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			val := int64(0)
			if len(e.Value) > 0 {
				val = e.Value[0].(int64)
			}
			results = append(results, MinMaxEverIndexEntryResult{
				Key:   tupleToSlice(e.Key),
				Value: val,
			})
		}
		return nil, nil
	})
	return results, err
}

func (s *MaxEverTupleIndexConformanceStore) ScanMaxEverTupleIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]interface{}
	if err := s.java.InvokeAs(ctx, "scanMaxEverTupleIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMaxEverTupleIndex failed: %w", err)
	}

	var results []MinMaxEverIndexEntryResult
	for _, m := range javaResults {
		entry := MinMaxEverIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if valRaw, ok := m["value"]; ok {
			entry.Value = int64(valRaw.(float64))
		}
		results = append(results, entry)
	}
	return results, nil
}

// MinEverTupleIndexConformanceStore wraps record operations with a MIN_EVER_TUPLE
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MIN_EVER_TUPLE
type MinEverTupleIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MinIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMinEverTupleIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MinEverTupleIndexConformanceStore, error) {
	minIdx := recordlayer.NewMinEverTupleIndex("min_ever_price_tuple", recordlayer.Ungrouped(recordlayer.Field("price")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", minIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &MinEverTupleIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MinIndex:    minIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MinEverTupleIndexConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MinEverTupleIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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

func (s *MinEverTupleIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMinEverTupleIndex", params, nil)
}

func (s *MinEverTupleIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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

func (s *MinEverTupleIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMinEverTupleIndex", params, nil)
}

func (s *MinEverTupleIndexConformanceStore) ScanMinEverTupleIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	var results []MinMaxEverIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.MinIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			val := int64(0)
			if len(e.Value) > 0 {
				val = e.Value[0].(int64)
			}
			results = append(results, MinMaxEverIndexEntryResult{
				Key:   tupleToSlice(e.Key),
				Value: val,
			})
		}
		return nil, nil
	})
	return results, err
}

func (s *MinEverTupleIndexConformanceStore) ScanMinEverTupleIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]interface{}
	if err := s.java.InvokeAs(ctx, "scanMinEverTupleIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMinEverTupleIndex failed: %w", err)
	}

	var results []MinMaxEverIndexEntryResult
	for _, m := range javaResults {
		entry := MinMaxEverIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if valRaw, ok := m["value"]; ok {
			entry.Value = int64(valRaw.(float64))
		}
		results = append(results, entry)
	}
	return results, nil
}
