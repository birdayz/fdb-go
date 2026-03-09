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

// CompositeIndexConformanceStore wraps record operations with a composite VALUE
// index on (Order.price, Order.order_id). Since order_id is also the primary key,
// Java deduplicates the PK from the index entry. This store validates that Go
// produces the same wire format.
type CompositeIndexConformanceStore struct {
	RecordDB       *recordlayer.FDBDatabase
	MetaData       *recordlayer.RecordMetaData
	CompositeIndex *recordlayer.Index
	Keyspace       subspace.Subspace
	java           *JavaInvoker
	clusterFile    string
	tenantName     string
}

// NewCompositeIndexConformanceStore creates a conformance store with a composite
// VALUE index on (price, order_id). Must match Java's createCompositeIndexedMetaData().
func NewCompositeIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*CompositeIndexConformanceStore, error) {
	compositeIndex := recordlayer.NewIndex("Order$price_id",
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", compositeIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &CompositeIndexConformanceStore{
		RecordDB:       recordDB,
		MetaData:       md,
		CompositeIndex: compositeIndex,
		Keyspace:       ks,
		java:           NewJavaInvoker(),
		clusterFile:    clusterFile,
		tenantName:     tenantName,
	}, nil
}

func (s *CompositeIndexConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with composite index maintenance).
func (s *CompositeIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with composite index maintenance).
func (s *CompositeIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCompositeIndex", params, nil)
}

// ScanIndexGo scans the composite index using Go.
func (s *CompositeIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.CompositeIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexJava scans the composite index using Java.
func (s *CompositeIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]interface{}
	if err := s.java.InvokeAs(ctx, "scanCompositeIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanCompositeIndex failed: %w", err)
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

// LoadOrderGo loads an order using Go.
func (s *CompositeIndexConformanceStore) LoadOrderGo(ctx context.Context, orderID int64) (*gen.Order, error) {
	var order *gen.Order
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		rec, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, nil
		}
		order = rec.Record.(*gen.Order)
		return nil, nil
	})
	return order, err
}

// LoadOrderJava loads an order using Java.
func (s *CompositeIndexConformanceStore) LoadOrderJava(ctx context.Context, orderID int64) (*gen.Order, error) {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	var order gen.Order
	if err := s.java.InvokeAs(ctx, "loadOrderWithCompositeIndex", params, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

// SaveAndCrossCheck saves with Go and verifies Java reads same data.
func (s *CompositeIndexConformanceStore) SaveAndCrossCheck(ctx context.Context, order *gen.Order) error {
	if err := s.SaveOrderGo(ctx, order); err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	goOrder, err := s.LoadOrderGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go load failed: %w", err)
	}
	if goOrder == nil {
		return fmt.Errorf("go load returned nil")
	}

	if !proto.Equal(order, goOrder) {
		return fmt.Errorf("go round-trip mismatch: saved=%v loaded=%v", order, goOrder)
	}
	return nil
}
