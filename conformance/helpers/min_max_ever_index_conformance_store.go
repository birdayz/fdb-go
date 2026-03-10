package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// MinMaxEverIndexEntryResult represents a single MIN/MAX_EVER_LONG index entry.
type MinMaxEverIndexEntryResult struct {
	Key   []any // Grouping key (empty for ungrouped)
	Value int64         // Min or max value for this grouping key
}

// MaxEverLongIndexConformanceStore wraps record operations with a MAX_EVER_LONG
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MAX_EVER_LONG
type MaxEverLongIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MaxIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMaxEverLongIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MaxEverLongIndexConformanceStore, error) {
	maxIdx := recordlayer.NewMaxEverLongIndex("max_ever_price", recordlayer.Ungrouped(recordlayer.Field("price")))

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

	return &MaxEverLongIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MaxIndex:    maxIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MaxEverLongIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MaxEverLongIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *MaxEverLongIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMaxEverLongIndex", params, nil)
}

func (s *MaxEverLongIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MaxEverLongIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMaxEverLongIndex", params, nil)
}

func (s *MaxEverLongIndexConformanceStore) ScanMaxEverIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	var results []MinMaxEverIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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

func (s *MaxEverLongIndexConformanceStore) ScanMaxEverIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMaxEverLongIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMaxEverLongIndex failed: %w", err)
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

// MinEverLongIndexConformanceStore wraps record operations with a MIN_EVER_LONG
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MIN_EVER_LONG
type MinEverLongIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MinIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMinEverLongIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MinEverLongIndexConformanceStore, error) {
	minIdx := recordlayer.NewMinEverLongIndex("min_ever_price", recordlayer.Ungrouped(recordlayer.Field("price")))

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

	return &MinEverLongIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MinIndex:    minIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MinEverLongIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MinEverLongIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *MinEverLongIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMinEverLongIndex", params, nil)
}

func (s *MinEverLongIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MinEverLongIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMinEverLongIndex", params, nil)
}

func (s *MinEverLongIndexConformanceStore) ScanMinEverIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	var results []MinMaxEverIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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

func (s *MinEverLongIndexConformanceStore) ScanMinEverIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMinEverLongIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMinEverLongIndex failed: %w", err)
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

// CompareMinMaxEverIndexEntries compares Go and Java MIN/MAX_EVER index scan results.
func CompareMinMaxEverIndexEntries(goEntries, javaEntries []MinMaxEverIndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
		if goEntries[i].Value != javaEntries[i].Value {
			return fmt.Errorf("entry %d value mismatch: go=%d java=%d",
				i, goEntries[i].Value, javaEntries[i].Value)
		}
	}
	return nil
}
