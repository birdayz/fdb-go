//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SUM Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *SumIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("sum_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewSumIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan SUM index", func() {
		It("should produce identical sum entries visible to both Go and Java", func() {
			// Save orders with known prices: 100 + 200 + 300 = 600
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			// Scan with Java
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Compare
			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify: ungrouped total sum = 600
			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Sum).To(Equal(int64(600)))
		})
	})

	Describe("Java writes, both scan SUM index", func() {
		It("should produce identical sum entries visible to both Go and Java", func() {
			// Java saves orders: 150 + 250 + 350 = 750
			for i, price := range []int32{150, 250, 350} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			// Scan with Java
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Compare
			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify: total = 750
			Expect(goEntries[0].Sum).To(Equal(int64(750)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined sum", func() {
			// Go saves: 100 + 200 = 300
			for i, price := range []int32{100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java saves: 300 + 400 = 700
			for i, price := range []int32{300, 400} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 3)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Both should see sum = 1000
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(1000)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete decrements sum cross-validated", func() {
		It("should decrement when Go deletes a Java-written record", func() {
			// Java saves: 100 + 200 + 300 = 600
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify sum = 600
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(600)))

			// Go deletes order 2 (price=200)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see sum = 400
			goEntries, err = store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(400)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should decrement when Java deletes a Go-written record", func() {
			// Go saves: 500 + 600 + 700 = 1800
			for i, price := range []int32{500, 600, 700} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify sum = 1800
			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Sum).To(Equal(int64(1800)))

			// Java deletes order 1 (price=500)
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 1300
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(1300)))

			javaEntries, err = store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update changes sum correctly", func() {
		It("should adjust sum when price changes via Go update", func() {
			// Save: 100 + 200 = 300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify sum = 300
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(300)))

			// Update order 1: price 100 → 500 (net change +400)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 700
			goEntries, err = store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(700)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should adjust sum when price changes via Java update", func() {
			// Java saves: 300 + 400 = 700
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java updates order 2: price 400 → 100 (net change -300)
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Both should see sum = 400
			goEntries, err := store.ScanSumIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Sum).To(Equal(int64(400)))

			javaEntries, err := store.ScanSumIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareSumIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// SumIndexEntryResult represents a single SUM index entry for comparison.
type SumIndexEntryResult struct {
	Key []any // Grouping key (empty for ungrouped)
	Sum int64 // Sum value for this grouping key
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
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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
