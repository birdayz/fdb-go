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

var _ = Describe("MAX_EVER_LONG Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MaxEverLongIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("maxever_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMaxEverLongIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MAX_EVER_LONG index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{100, 300, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(300)))
		})
	})

	Describe("Java writes, both scan MAX_EVER_LONG index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{150, 450, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(450)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global max across both writers", func() {
			// Go writes: max so far = 200
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes: max becomes 500
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes lower: max stays 500
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert max (_EVER semantics)", func() {
		It("should preserve max after Go deletes the max record written by Java", func() {
			// Java saves: prices 100, 500
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify max = 500
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			// Go deletes order 2 (the max record)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Max should STILL be 500 (_EVER = irreversible)
			goEntries, err = store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should preserve max after Java deletes the max record written by Go", func() {
			// Go saves: prices 200, 800
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(800),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2 (the max record)
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Max should STILL be 800
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(800)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update never decreases max", func() {
		It("should keep max when Go updates to lower value", func() {
			// Save with price 700
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(700),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to lower price 200
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Max should still be 700
			goEntries, err := store.ScanMaxEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(700)))

			javaEntries, err := store.ScanMaxEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("MIN_EVER_LONG Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MinEverLongIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("minever_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMinEverLongIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MIN_EVER_LONG index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{300, 100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(100)))
		})
	})

	Describe("Java writes, both scan MIN_EVER_LONG index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{350, 50, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(50)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global min across both writers", func() {
			// Go writes: min so far = 300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes lower: min becomes 50
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes higher: min stays 50
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(50)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert min (_EVER semantics)", func() {
		It("should preserve min after Go deletes the min record written by Java", func() {
			// Java saves: prices 500, 100
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify min = 100
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			// Go deletes order 2 (the min record)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Min should STILL be 100 (_EVER = irreversible)
			goEntries, err = store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should preserve min after Java deletes the min record written by Go", func() {
			// Go saves: prices 400, 75
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(75),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2 (the min record)
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Min should STILL be 75
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(75)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Update never increases min", func() {
		It("should keep min when Go updates to higher value", func() {
			// Save with price 100
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update to higher price 900
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(900),
			})
			Expect(err).NotTo(HaveOccurred())

			// Min should still be 100
			goEntries, err := store.ScanMinEverIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(100)))

			javaEntries, err := store.ScanMinEverIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// MinMaxEverIndexEntryResult represents a single MIN/MAX_EVER_LONG index entry.
type MinMaxEverIndexEntryResult struct {
	Key   []any // Grouping key (empty for ungrouped)
	Value int64 // Min or max value for this grouping key
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
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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

// BareEverIndexConformanceStore wraps record operations with an index whose
// metadata carries the DEPRECATED bare type spelling — "max_ever" / "min_ever"
// rather than "max_ever_long" / "min_ever_long".
//
// It holds TWO metadata objects over the SAME index name, and that is the whole
// design. Because both spellings produce the same index name and therefore the
// same subspace key, records written through the bare metadata can be read back
// through the _LONG metadata against the identical subspace. If the bare
// spelling reaches the same atomic maintainer, that read succeeds and returns
// one aggregate entry. If it reaches any other maintainer, the bytes in that
// subspace are a different format and the _LONG read cannot reproduce them.
//
// That is the assertion the pre-existing suite could not make: it built _LONG
// indexes only, so a bare-spelled index falling through Go's maintainer dispatch
// to the VALUE maintainer — writing one per-record entry per record instead of a
// single aggregate — was invisible.
type BareEverIndexConformanceStore struct {
	RecordDB *recordlayer.FDBDatabase
	// MetaData declares the index with the bare spelling.
	MetaData *recordlayer.RecordMetaData
	// LongMetaData declares the SAME index name with the canonical _LONG
	// spelling, so a read through it proves the bare writer produced _LONG bytes.
	LongMetaData *recordlayer.RecordMetaData
	BareIndex    *recordlayer.Index
	LongIndex    *recordlayer.Index
	isMax        bool
	Keyspace     subspace.Subspace
	java         *JavaInvoker
	clusterFile  string
	tenantName   string
}

func NewBareEverIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile, tenantName string, isMax bool) (*BareEverIndexConformanceStore, error) {
	name := "min_ever_price"
	newLong := recordlayer.NewMinEverLongIndex
	bareType := recordlayer.IndexTypeMinEver
	if isMax {
		name = "max_ever_price"
		newLong = recordlayer.NewMaxEverLongIndex
		bareType = recordlayer.IndexTypeMaxEver
	}

	build := func(idx *recordlayer.Index) (*recordlayer.RecordMetaData, error) {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", idx)
		return builder.Build()
	}

	// The bare index is the _LONG constructor with the type re-stamped. That is
	// deliberately the only way to build one here: there is no bare-spelling
	// constructor to call, because nothing in this port should ever WRITE the
	// deprecated spelling. This is metadata as a Java app would have authored it.
	bareIdx := newLong(name, recordlayer.Ungrouped(recordlayer.Field("price")))
	bareIdx.Type = bareType
	bareMD, err := build(bareIdx)
	if err != nil {
		return nil, err
	}

	longIdx := newLong(name, recordlayer.Ungrouped(recordlayer.Field("price")))
	longMD, err := build(longIdx)
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &BareEverIndexConformanceStore{
		RecordDB:     recordDB,
		MetaData:     bareMD,
		LongMetaData: longMD,
		BareIndex:    bareIdx,
		LongIndex:    longIdx,
		isMax:        isMax,
		Keyspace:     ks,
		java:         NewJavaInvoker(),
		clusterFile:  clusterFile,
		tenantName:   tenantName,
	}, nil
}

func (s *BareEverIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
		"isMax":       s.isMax,
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves through the BARE-spelled metadata.
func (s *BareEverIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *BareEverIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithBareEverIndex", params, nil)
}

// scanGoWith scans the index using whichever metadata/index pair is handed in,
// so the same subspace can be read as bare-spelled or as _LONG.
func (s *BareEverIndexConformanceStore) scanGoWith(ctx context.Context, md *recordlayer.RecordMetaData, idx *recordlayer.Index) ([]MinMaxEverIndexEntryResult, error) {
	var results []MinMaxEverIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		results = nil
		for _, e := range entries {
			val := int64(0)
			if len(e.Value) > 0 {
				val = e.Value[0].(int64)
			}
			results = append(results, MinMaxEverIndexEntryResult{Key: tupleToSlice(e.Key), Value: val})
		}
		return nil, nil
	})
	return results, err
}

// ScanBareGo reads the index through the BARE-spelled metadata.
func (s *BareEverIndexConformanceStore) ScanBareGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	return s.scanGoWith(ctx, s.MetaData, s.BareIndex)
}

// ScanAsLongGo reads the SAME subspace through the canonical _LONG metadata.
func (s *BareEverIndexConformanceStore) ScanAsLongGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	return s.scanGoWith(ctx, s.LongMetaData, s.LongIndex)
}

func (s *BareEverIndexConformanceStore) ScanBareJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanBareEverIndex", s.buildJavaParams(), &javaResults); err != nil {
		return nil, fmt.Errorf("java scanBareEverIndex failed: %w", err)
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

// The DEPRECATED bare spellings, cross-engine.
//
// Java keeps IndexTypes.MIN_EVER / MAX_EVER because old meta-data contains them
// (IndexTypes.java:67-81) and folds each onto its _LONG behaviour unconditionally
// in AtomicMutationIndexMaintainer.getAtomicMutation. On a shared cluster that is
// not a legacy curiosity: it is metadata a Java app can hand this port today.
//
// Go answered an unrecognised index type with the VALUE maintainer, so a
// bare-spelled index was not rejected — it was maintained as a value index, one
// entry per record written into the aggregate index's own subspace. The existing
// specs in this file could not see it because every one of them builds a _LONG
// index. The bare spelling is the dimension that was missing, not the volume.
var _ = Describe("Deprecated bare MIN_EVER / MAX_EVER index conformance", func() {
	var (
		ctx context.Context
		env *TenantEnvironment
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("bareever_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	for _, tc := range []struct {
		name  string
		isMax bool
		// prices are written in an order where the extreme is not last, so a
		// maintainer that merely keeps the newest value cannot pass.
		prices []int32
		want   int64
	}{
		{name: "max_ever", isMax: true, prices: []int32{100, 300, 200}, want: 300},
		{name: "min_ever", isMax: false, prices: []int32{300, 100, 200}, want: 100},
	} {
		It("routes bare "+tc.name+" to the _LONG maintainer, provably on the bytes", func() {
			store, err := NewBareEverIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName, tc.isMax)
			Expect(err).NotTo(HaveOccurred())

			for i, price := range tc.prices {
				Expect(store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})).To(Succeed())
			}

			// ONE aggregate entry, not one per record. A value index over three
			// records yields three entries with the primary key in the key, which
			// is the exact shape the old fail-open default produced.
			bareEntries, err := store.ScanBareGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(bareEntries).To(HaveLen(1),
				"a bare-spelled _EVER index must maintain ONE aggregate entry; %d entries means it "+
					"reached a per-record maintainer, which is the value-index fail-open", len(bareEntries))
			Expect(bareEntries[0].Key).To(BeEmpty())
			Expect(bareEntries[0].Value).To(Equal(tc.want))

			// The bytes are _LONG bytes: the same subspace read through metadata
			// that spells the type the canonical way returns the same entry.
			longEntries, err := store.ScanAsLongGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(longEntries).To(Equal(bareEntries),
				"records written under the bare spelling must be byte-compatible with the _LONG "+
					"spelling — Java folds the two onto one maintainer, so the index contents cannot differ")

			// Cross-engine: Java reads what Go wrote through bare metadata.
			javaEntries, err := store.ScanBareJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(CompareMinMaxEverIndexEntries(bareEntries, javaEntries)).To(Succeed())
		})

		It("reads bare "+tc.name+" written by Java", func() {
			store, err := NewBareEverIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName, tc.isMax)
			Expect(err).NotTo(HaveOccurred())

			// The direction that matters most for a shared cluster: the metadata
			// AND the writes are Java's, and this port has to read them.
			for i, price := range tc.prices {
				Expect(store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})).To(Succeed())
			}

			goEntries, err := store.ScanBareGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(tc.want))

			javaEntries, err := store.ScanBareJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(CompareMinMaxEverIndexEntries(goEntries, javaEntries)).To(Succeed())
		})
	}
})
