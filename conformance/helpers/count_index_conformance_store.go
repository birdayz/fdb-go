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

// CountIndexEntryResult represents a single COUNT index entry for comparison.
type CountIndexEntryResult struct {
	Key   []any // Grouping key (e.g., [price])
	Count int64         // Count for this grouping key
}

// CountIndexConformanceStore wraps record operations with a COUNT index on
// Order.price grouped by all columns (per-price count).
// Go: GroupAll(Field("price"))
// Java: field("price").groupBy(empty())
type CountIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	CountIndex  *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewCountIndexConformanceStore creates a conformance store with a COUNT index
// on Order.price. The index definition must match the Java side's
// createCountIndexedMetaData() exactly.
func NewCountIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*CountIndexConformanceStore, error) {
	// GroupAll(Field("price")) = group by all columns = per-price count
	// Matches Java's field("price").groupBy(empty())
	countIdx := recordlayer.NewCountIndex("count_by_price", recordlayer.GroupAll(recordlayer.Field("price")))

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

	return &CountIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		CountIndex:  countIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *CountIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with COUNT index maintenance).
func (s *CountIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with COUNT index maintenance).
func (s *CountIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCountIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with COUNT index maintenance).
func (s *CountIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with COUNT index maintenance).
func (s *CountIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithCountIndex", params, nil)
}

// ScanCountIndexGo scans the COUNT index using Go and returns results.
func (s *CountIndexConformanceStore) ScanCountIndexGo(ctx context.Context) ([]CountIndexEntryResult, error) {
	var results []CountIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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

// ScanCountIndexJava scans the COUNT index using Java and returns results.
func (s *CountIndexConformanceStore) ScanCountIndexJava(ctx context.Context) ([]CountIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanCountIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanCountIndex failed: %w", err)
	}

	var results []CountIndexEntryResult
	for _, m := range javaResults {
		entry := CountIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if countRaw, ok := m["count"]; ok {
			entry.Count = int64(countRaw.(float64)) // JSON numbers are float64
		}
		results = append(results, entry)
	}
	return results, nil
}

// CompareCountIndexEntries compares Go and Java COUNT index scan results.
func CompareCountIndexEntries(goEntries, javaEntries []CountIndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
		if goEntries[i].Count != javaEntries[i].Count {
			return fmt.Errorf("entry %d count mismatch: go=%d java=%d",
				i, goEntries[i].Count, javaEntries[i].Count)
		}
	}
	return nil
}

// SaveOrderGoProto is a convenience for creating and saving an order.
func (s *CountIndexConformanceStore) SaveOrderGoProto(ctx context.Context, orderID int64, price int32) error {
	order := &gen.Order{
		OrderId: proto.Int64(orderID),
		Price:   proto.Int32(price),
	}
	return s.SaveOrderGo(ctx, order)
}
