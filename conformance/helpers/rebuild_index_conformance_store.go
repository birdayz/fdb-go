package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// RebuildIndexConformanceStore provides operations for testing index rebuild
// conformance between Go and Java. Records are saved without an index, then
// the index is rebuilt and the results are compared.
type RebuildIndexConformanceStore struct {
	RecordDB      *recordlayer.FDBDatabase
	MetaDataNoIdx *recordlayer.RecordMetaData // Metadata WITHOUT index (for saving)
	MetaDataIdx   *recordlayer.RecordMetaData // Metadata WITH index (for rebuild/scan)
	PriceIndex    *recordlayer.Index
	Keyspace      subspace.Subspace
	java          *JavaInvoker
	clusterFile   string
	tenantName    string
}

// NewRebuildIndexConformanceStore creates a conformance store for index rebuild testing.
func NewRebuildIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*RebuildIndexConformanceStore, error) {
	// Metadata without index (for saving records)
	builderNoIdx := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderNoIdx.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderNoIdx.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	mdNoIdx, err := builderNoIdx.Build()
	if err != nil {
		return nil, err
	}

	// Metadata with index (for rebuild/scan)
	priceIndex := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
	builderIdx := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderIdx.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderIdx.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderIdx.AddIndex("Order", priceIndex)
	mdIdx, err := builderIdx.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &RebuildIndexConformanceStore{
		RecordDB:      recordDB,
		MetaDataNoIdx: mdNoIdx,
		MetaDataIdx:   mdIdx,
		PriceIndex:    priceIndex,
		Keyspace:      ks,
		java:          NewJavaInvoker(),
		clusterFile:   clusterFile,
		tenantName:    tenantName,
	}, nil
}

func (s *RebuildIndexConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order WITHOUT index using Go.
func (s *RebuildIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataNoIdx).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(order)
		return nil, err
	})
	return err
}

// SaveOrderJava saves an order WITHOUT index using Java.
func (s *RebuildIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrder", params, nil)
}

// RebuildIndexGo opens store with indexed metadata and rebuilds the index within one transaction.
func (s *RebuildIndexConformanceStore) RebuildIndexGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return nil, store.RebuildIndex(s.PriceIndex)
	})
	return err
}

// RebuildIndexJava calls Java's FDBRecordStore.rebuildIndex() via the conformance server.
func (s *RebuildIndexConformanceStore) RebuildIndexJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "rebuildIndex", params, nil)
}

// ScanIndexGo scans the price index using Go.
func (s *RebuildIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.PriceIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexJava scans the price index using Java.
func (s *RebuildIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$price"

	var javaResults []map[string]interface{}
	if err := s.java.InvokeAs(ctx, "scanIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanIndex failed: %w", err)
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
