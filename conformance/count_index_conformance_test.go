//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("COUNT Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *CountIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cnt_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewCountIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan COUNT index", func() {
		It("should produce identical count entries visible to both Go and Java", func() {
			// Save 3 orders with price=100, 2 with price=200
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(200),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Scan with Java
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify values: price=100 count=3, price=200 count=2
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(goEntries[0].Count).To(Equal(int64(3)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(goEntries[1].Count).To(Equal(int64(2)))
		})
	})

	Describe("Java writes, both scan COUNT index", func() {
		It("should produce identical count entries visible to both Go and Java", func() {
			// Java saves 2 orders with price=300, 1 with price=400
			for i := int64(1); i <= 2; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(300),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Scan with Java
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(300)))
			Expect(goEntries[0].Count).To(Equal(int64(2)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(400)))
			Expect(goEntries[1].Count).To(Equal(int64(1)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined counts", func() {
			// Go saves 2 orders with price=500
			for i := int64(1); i <= 2; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java saves 3 more orders with price=500
			for i := int64(3); i <= 5; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Both should see count=5 for price=500
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))
			Expect(goEntries[0].Count).To(Equal(int64(5)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete decrements count cross-validated", func() {
		It("should decrement when Go deletes a Java-written record", func() {
			// Java saves 3 orders with price=600
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(600),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(3)))

			// Go deletes one record
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see count=2
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should decrement when Java deletes a Go-written record", func() {
			// Go saves 3 orders with price=700
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(700),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify count=3
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Count).To(Equal(int64(3)))

			// Java deletes one
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see count=2
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err = store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update changes count correctly", func() {
		It("should move counts when price changes via Go update", func() {
			// Save 2 orders: price=800 and price=900
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(800),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify: 800=1, 900=1
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Update order 1: price 800 → 900
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Now: 800=0 (gone), 900=2
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Should have at least the 900=2 entry
			// Note: COUNT index may or may not retain 0-count entries depending
			// on implementation. Both Go and Java use atomic ADD so a 0-count
			// entry may still exist as a key with value 0.
			found900 := false
			for _, e := range goEntries {
				if toInt64(e.Key[0]) == int64(900) {
					Expect(e.Count).To(Equal(int64(2)))
					found900 = true
				}
			}
			Expect(found900).To(BeTrue(), "expected entry for price=900 with count=2")
		})
	})
})

// CountIndexEntryResult represents a single COUNT index entry for comparison.
type CountIndexEntryResult struct {
	Key   []any // Grouping key (e.g., [price])
	Count int64 // Count for this grouping key
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
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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
