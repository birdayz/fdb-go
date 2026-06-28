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

var _ = Describe("PERMUTED_MAX Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *PermutedMaxConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("permmax_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewPermutedMaxConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan BY_VALUE + BY_GROUP", func() {
		It("should produce identical entries visible to both Go and Java", func() {
			// Save 3 orders: (id=1,price=100), (id=3,price=100), (id=2,price=200)
			for _, o := range []struct {
				id    int64
				price int32
			}{{1, 100}, {3, 100}, {2, 200}} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(o.id),
					Price:   proto.Int32(o.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_VALUE: expect 3 entries [(100,1), (100,3), (200,2)]
			goByValue, err := store.ScanByValueGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByValue).To(HaveLen(3))

			javaByValue, err := store.ScanByValueJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByValue, javaByValue)
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: expect 2 entries — max order_id per price group
			// price=100 max=3, price=200 max=2 → [(3,100), (2,200)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(2))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, both scan BY_VALUE + BY_GROUP", func() {
		It("should produce identical entries visible to both Go and Java", func() {
			for _, o := range []struct {
				id    int64
				price int32
			}{{5, 150}, {7, 150}, {4, 300}} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(o.id),
					Price:   proto.Int32(o.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_VALUE: 3 entries
			goByValue, err := store.ScanByValueGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByValue).To(HaveLen(3))

			javaByValue, err := store.ScanByValueJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByValue, javaByValue)
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: 2 entries — max per group
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(2))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce consistent BY_GROUP entries", func() {
			// Go saves (id=1,price=100)
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java saves (id=2,price=100)
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go saves (id=3,price=200)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: price=100 max=2, price=200 max=3
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(2))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Go deletes max record written by Java, BY_GROUP shows next max", func() {
		It("should update BY_GROUP after deleting the max record", func() {
			// Java saves (id=10,price=100), (id=20,price=100), (id=30,price=100)
			for _, id := range []int64{10, 20, 30} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_GROUP before delete: [(30,100)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			// Go deletes order 30 (the max)
			deleted, err := store.DeleteOrderGo(ctx, 30)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// BY_GROUP after delete: [(20,100)] — next max
			goByGroup, err = store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java deletes max record written by Go, BY_GROUP shows next max", func() {
		It("should update BY_GROUP after Java deletes the max record", func() {
			// Go saves (id=40,price=200), (id=50,price=200), (id=60,price=200)
			for _, id := range []int64{40, 50, 60} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(200),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java deletes order 60 (the max)
			err := store.DeleteOrderJava(ctx, 60)
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP after delete: [(50,200)] — next max
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Non-extremum delete doesn't change BY_GROUP", func() {
		It("should keep the same BY_GROUP entry when a non-max record is deleted", func() {
			// Go saves (id=1,price=100), (id=5,price=100), (id=3,price=100)
			for _, id := range []int64{1, 5, 3} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_GROUP before: [(5,100)] — max=5
			goByGroupBefore, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroupBefore).To(HaveLen(1))

			// Delete order 3 (not the max)
			deleted, err := store.DeleteOrderGo(ctx, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// BY_GROUP after: still [(5,100)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("PERMUTED_MIN Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *PermutedMinConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("permmin_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewPermutedMinConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan BY_VALUE + BY_GROUP", func() {
		It("should produce identical entries with min order_id per price group", func() {
			// Save 3 orders: (id=1,price=100), (id=3,price=100), (id=2,price=200)
			for _, o := range []struct {
				id    int64
				price int32
			}{{1, 100}, {3, 100}, {2, 200}} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(o.id),
					Price:   proto.Int32(o.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_VALUE: 3 entries
			goByValue, err := store.ScanByValueGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByValue).To(HaveLen(3))

			javaByValue, err := store.ScanByValueJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByValue, javaByValue)
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: min order_id per price → [(1,100), (2,200)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(2))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, both scan BY_VALUE + BY_GROUP", func() {
		It("should produce identical entries with min order_id per price group", func() {
			for _, o := range []struct {
				id    int64
				price int32
			}{{5, 150}, {7, 150}, {4, 300}} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(o.id),
					Price:   proto.Int32(o.price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_VALUE: 3 entries
			goByValue, err := store.ScanByValueGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByValue).To(HaveLen(3))

			javaByValue, err := store.ScanByValueJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByValue, javaByValue)
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: min per group → [(5,150), (4,300)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(2))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Go deletes min record written by Java, BY_GROUP shows next min", func() {
		It("should update BY_GROUP after deleting the min record", func() {
			// Java saves (id=10,price=100), (id=20,price=100), (id=30,price=100)
			for _, id := range []int64{10, 20, 30} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Go deletes order 10 (the min)
			deleted, err := store.DeleteOrderGo(ctx, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// BY_GROUP after delete: [(20,100)] — next min
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Non-extremum insert doesn't change BY_GROUP min", func() {
		It("should keep the same BY_GROUP min when a larger order_id is inserted", func() {
			// Go saves (id=1,price=100) — min=1
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go saves (id=5,price=100) — still min=1
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(5),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// BY_GROUP: [(1,100)]
			goByGroup, err := store.ScanByGroupGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goByGroup).To(HaveLen(1))

			javaByGroup, err := store.ScanByGroupJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = ComparePermutedIndexEntries(goByGroup, javaByGroup)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// PermutedIndexEntryResult represents a single PERMUTED_MIN/MAX index entry.
type PermutedIndexEntryResult struct {
	Key []any // Key elements from the index entry
}

// ComparePermutedIndexEntries compares Go and Java PERMUTED_MIN/MAX index scan results.
func ComparePermutedIndexEntries(goEntries, javaEntries []PermutedIndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
	}
	return nil
}

// PermutedMaxConformanceStore wraps record operations with a PERMUTED_MAX index
// on Order: GroupBy(Field("order_id"), Field("price")), permutedSize=1.
// BY_VALUE entries: [price, order_id] — all entries
// BY_GROUP entries: [order_id, price] — one per price group (the max order_id)
type PermutedMaxConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Index       *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewPermutedMaxConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*PermutedMaxConformanceStore, error) {
	idx := recordlayer.NewPermutedMaxIndex("Order$maxOrderByPrice",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price")), 1)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &PermutedMaxConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Index:       idx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *PermutedMaxConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *PermutedMaxConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *PermutedMaxConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithPermutedMaxIndex", params, nil)
}

func (s *PermutedMaxConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *PermutedMaxConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithPermutedMaxIndex", params, nil)
}

func (s *PermutedMaxConformanceStore) ScanByValueGo(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	var results []PermutedIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.Index, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, PermutedIndexEntryResult{Key: tupleToSlice(e.Key)})
		}
		return nil, nil
	})
	return results, err
}

func (s *PermutedMaxConformanceStore) ScanByValueJava(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	params := s.buildJavaParams()
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanPermutedMaxByValue", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanPermutedMaxByValue failed: %w", err)
	}
	var results []PermutedIndexEntryResult
	for _, m := range javaResults {
		entry := PermutedIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}

func (s *PermutedMaxConformanceStore) ScanByGroupGo(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	var results []PermutedIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(s.Index, recordlayer.IndexScanByGroup, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, PermutedIndexEntryResult{Key: tupleToSlice(e.Key)})
		}
		return nil, nil
	})
	return results, err
}

func (s *PermutedMaxConformanceStore) ScanByGroupJava(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	params := s.buildJavaParams()
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanPermutedMaxByGroup", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanPermutedMaxByGroup failed: %w", err)
	}
	var results []PermutedIndexEntryResult
	for _, m := range javaResults {
		entry := PermutedIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}

// PermutedMinConformanceStore wraps record operations with a PERMUTED_MIN index
// on Order: GroupBy(Field("order_id"), Field("price")), permutedSize=1.
// BY_VALUE entries: [price, order_id] — all entries
// BY_GROUP entries: [order_id, price] — one per price group (the min order_id)
type PermutedMinConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Index       *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewPermutedMinConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*PermutedMinConformanceStore, error) {
	idx := recordlayer.NewPermutedMinIndex("Order$minOrderByPrice",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price")), 1)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &PermutedMinConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Index:       idx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *PermutedMinConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *PermutedMinConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *PermutedMinConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithPermutedMinIndex", params, nil)
}

func (s *PermutedMinConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *PermutedMinConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithPermutedMinIndex", params, nil)
}

func (s *PermutedMinConformanceStore) ScanByValueGo(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	var results []PermutedIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.Index, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, PermutedIndexEntryResult{Key: tupleToSlice(e.Key)})
		}
		return nil, nil
	})
	return results, err
}

func (s *PermutedMinConformanceStore) ScanByValueJava(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	params := s.buildJavaParams()
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanPermutedMinByValue", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanPermutedMinByValue failed: %w", err)
	}
	var results []PermutedIndexEntryResult
	for _, m := range javaResults {
		entry := PermutedIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}

func (s *PermutedMinConformanceStore) ScanByGroupGo(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	var results []PermutedIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(s.Index, recordlayer.IndexScanByGroup, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, PermutedIndexEntryResult{Key: tupleToSlice(e.Key)})
		}
		return nil, nil
	})
	return results, err
}

func (s *PermutedMinConformanceStore) ScanByGroupJava(ctx context.Context) ([]PermutedIndexEntryResult, error) {
	params := s.buildJavaParams()
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanPermutedMinByGroup", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanPermutedMinByGroup failed: %w", err)
	}
	var results []PermutedIndexEntryResult
	for _, m := range javaResults {
		entry := PermutedIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}
