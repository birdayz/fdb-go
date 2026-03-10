package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// SumIndexEntryResult represents a single SUM index entry for comparison.
type SumIndexEntryResult struct {
	Key []any // Grouping key (empty for ungrouped)
	Sum int64         // Sum value for this grouping key
}

// SumIndexConformanceStore wraps record operations with a SUM index on
// Order.price ungrouped (total sum of all prices).
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1)
type SumIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	SumIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewSumIndexConformanceStore creates a conformance store with an ungrouped SUM
// index on Order.price. The index definition must match the Java side's
// createSumIndexedMetaData() exactly.
func NewSumIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*SumIndexConformanceStore, error) {
	// Ungrouped(Field("price")) = no grouping key, sum the price column
	// Matches Java's new GroupingKeyExpression(field("price"), 1)
	sumIdx := recordlayer.NewSumIndex("sum_price", recordlayer.Ungrouped(recordlayer.Field("price")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.AddIndex("Order", sumIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &SumIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		SumIndex:    sumIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *SumIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with SUM index maintenance).
func (s *SumIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with SUM index maintenance).
func (s *SumIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithSumIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with SUM index maintenance).
func (s *SumIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with SUM index maintenance).
func (s *SumIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithSumIndex", params, nil)
}

// ScanSumIndexGo scans the SUM index using Go and returns results.
func (s *SumIndexConformanceStore) ScanSumIndexGo(ctx context.Context) ([]SumIndexEntryResult, error) {
	var results []SumIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.SumIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			sum := int64(0)
			if len(e.Value) > 0 {
				sum = e.Value[0].(int64)
			}
			results = append(results, SumIndexEntryResult{
				Key: tupleToSlice(e.Key),
				Sum: sum,
			})
		}
		return nil, nil
	})
	return results, err
}

// ScanSumIndexJava scans the SUM index using Java and returns results.
func (s *SumIndexConformanceStore) ScanSumIndexJava(ctx context.Context) ([]SumIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanSumIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanSumIndex failed: %w", err)
	}

	var results []SumIndexEntryResult
	for _, m := range javaResults {
		entry := SumIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if sumRaw, ok := m["sum"]; ok {
			entry.Sum = int64(sumRaw.(float64)) // JSON numbers are float64
		}
		results = append(results, entry)
	}
	return results, nil
}

// CompareSumIndexEntries compares Go and Java SUM index scan results.
func CompareSumIndexEntries(goEntries, javaEntries []SumIndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
		if goEntries[i].Sum != javaEntries[i].Sum {
			return fmt.Errorf("entry %d sum mismatch: go=%d java=%d",
				i, goEntries[i].Sum, javaEntries[i].Sum)
		}
	}
	return nil
}
