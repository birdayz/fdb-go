package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// CountNotNullIndexConformanceStore wraps record operations with a COUNT_NOT_NULL
// index on Order.price. Records where price is null (unset) are not counted.
type CountNotNullIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	CountIndex  *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewCountNotNullIndexConformanceStore creates a conformance store with a
// COUNT_NOT_NULL index on Order.price.
func NewCountNotNullIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*CountNotNullIndexConformanceStore, error) {
	countIdx := recordlayer.NewCountNotNullIndex("count_not_null_price", recordlayer.Ungrouped(recordlayer.Field("price")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", countIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &CountNotNullIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		CountIndex:  countIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *CountNotNullIndexConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *CountNotNullIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *CountNotNullIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCountNotNullIndex", params, nil)
}

func (s *CountNotNullIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *CountNotNullIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithCountNotNullIndex", params, nil)
}

func (s *CountNotNullIndexConformanceStore) ScanCountIndexGo(ctx context.Context) ([]CountIndexEntryResult, error) {
	var results []CountIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.CountIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			count := int64(0)
			if len(e.Value) > 0 {
				count = e.Value[0].(int64)
			}
			results = append(results, CountIndexEntryResult{
				Key:   tupleToSlice(e.Key),
				Count: count,
			})
		}
		return nil, nil
	})
	return results, err
}

func (s *CountNotNullIndexConformanceStore) ScanCountIndexJava(ctx context.Context) ([]CountIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]interface{}
	if err := s.java.InvokeAs(ctx, "scanCountNotNullIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanCountNotNullIndex failed: %w", err)
	}

	var results []CountIndexEntryResult
	for _, m := range javaResults {
		entry := CountIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if countRaw, ok := m["count"]; ok {
			entry.Count = int64(countRaw.(float64))
		}
		results = append(results, entry)
	}
	return results, nil
}
