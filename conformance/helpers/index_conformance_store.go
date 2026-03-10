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

// IndexConformanceStore wraps record operations with a VALUE index on Order.price
// and provides methods to cross-validate index entries between Go and Java.
type IndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	PriceIndex  *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// IndexEntryResult represents a single index entry for comparison.
type IndexEntryResult struct {
	Key        []any // Full key tuple (indexed values + primary key)
	PrimaryKey []any // Primary key extracted from the entry
}

// NewIndexConformanceStore creates a conformance store with a VALUE index on Order.price.
// The index definition must match the Java side's createIndexedMetaData() exactly.
func NewIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*IndexConformanceStore, error) {
	priceIndex := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", priceIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &IndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		PriceIndex:  priceIndex,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

// buildJavaParams builds base parameters for Java invocations.
func (s *IndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with index maintenance).
func (s *IndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with index maintenance).
func (s *IndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with index maintenance).
func (s *IndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with index maintenance).
func (s *IndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithIndex", params, nil)
}

// ScanIndexGo scans the price index using Go and returns results.
func (s *IndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
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

// ScanIndexJava scans the price index using Java and returns results.
func (s *IndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$price"

	var javaResults []map[string]any
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

// LoadOrderGo loads an order using Go.
func (s *IndexConformanceStore) LoadOrderGo(ctx context.Context, orderID int64) (*gen.Order, error) {
	var order *gen.Order
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
func (s *IndexConformanceStore) LoadOrderJava(ctx context.Context, orderID int64) (*gen.Order, error) {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	var order gen.Order
	if err := s.java.InvokeAs(ctx, "loadOrderWithIndex", params, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

// CompareIndexEntries compares Go and Java index scan results.
// Returns nil if they match, an error describing the mismatch otherwise.
func CompareIndexEntries(goEntries, javaEntries []IndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !entriesEqual(goEntries[i], javaEntries[i]) {
			return fmt.Errorf("entry %d mismatch:\n  go:   key=%v pk=%v\n  java: key=%v pk=%v",
				i, goEntries[i].Key, goEntries[i].PrimaryKey,
				javaEntries[i].Key, javaEntries[i].PrimaryKey)
		}
	}
	return nil
}

func entriesEqual(a, b IndexEntryResult) bool {
	return sliceEqualNormalized(a.Key, b.Key) && sliceEqualNormalized(a.PrimaryKey, b.PrimaryKey)
}

// sliceEqualNormalized compares two slices, normalizing numbers to int64 for comparison.
// Java sends numbers as float64 through JSON; Go uses int64 from FDB tuples.
func sliceEqualNormalized(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !normalizedEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func normalizedEqual(a, b any) bool {
	// Handle string comparison (e.g., fan-out index on string fields)
	aStr, aIsStr := a.(string)
	bStr, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return aStr == bStr
	}
	if aIsStr != bIsStr {
		return false
	}
	return toInt64(a) == toInt64(b)
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	default:
		return -1
	}
}

func tupleToSlice(t tuple.Tuple) []any {
	s := make([]any, len(t))
	for i, v := range t {
		s[i] = v
	}
	return s
}

func toInterfaceSlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	default:
		return nil
	}
}

// SaveRecord saves a record using Go, then verifies Java can read it and sees the same record.
func (s *IndexConformanceStore) SaveRecord(ctx context.Context, msg proto.Message) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records supported")
	}

	if err := s.SaveOrderGo(ctx, order); err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	// Cross-check: Java reads the record
	javaOrder, err := s.LoadOrderJava(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("java load after go save failed: %w", err)
	}

	goOrder, err := s.LoadOrderGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go load failed: %w", err)
	}

	if !proto.Equal(goOrder, javaOrder) {
		return fmt.Errorf("record mismatch: go=%v java=%v", goOrder, javaOrder)
	}

	return nil
}
