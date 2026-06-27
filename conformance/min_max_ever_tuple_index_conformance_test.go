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

var _ = Describe("MAX_EVER_TUPLE Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MaxEverTupleIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("maxevertuple_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMaxEverTupleIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MAX_EVER_TUPLE index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{100, 300, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(300)))
		})
	})

	Describe("Java writes, both scan MAX_EVER_TUPLE index", func() {
		It("should produce identical max entries visible to both Go and Java", func() {
			for i, price := range []int32{150, 450, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(450)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global max across both writers", func() {
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMaxEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert max (_EVER semantics)", func() {
		It("should preserve max after Go deletes the max record written by Java", func() {
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

			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanMaxEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(500)))

			javaEntries, err := store.ScanMaxEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("MIN_EVER_TUPLE Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MinEverTupleIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("minevertuple_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMinEverTupleIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan MIN_EVER_TUPLE index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{300, 100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].Value).To(Equal(int64(100)))
		})
	})

	Describe("Java writes, both scan MIN_EVER_TUPLE index", func() {
		It("should produce identical min entries visible to both Go and Java", func() {
			for i, price := range []int32{350, 50, 250} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMinEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMinEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Value).To(Equal(int64(50)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should track the global min across both writers", func() {
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMinEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(50)))

			javaEntries, err := store.ScanMinEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete does NOT revert min (_EVER semantics)", func() {
		It("should preserve min after Java deletes the min record written by Go", func() {
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

			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMinEverTupleIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Value).To(Equal(int64(75)))

			javaEntries, err := store.ScanMinEverTupleIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareMinMaxEverIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// MaxEverTupleIndexConformanceStore wraps record operations with a MAX_EVER_TUPLE
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MAX_EVER_TUPLE
type MaxEverTupleIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MaxIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMaxEverTupleIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MaxEverTupleIndexConformanceStore, error) {
	maxIdx := recordlayer.NewMaxEverTupleIndex("max_ever_price_tuple", recordlayer.Ungrouped(recordlayer.Field("price")))

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

	return &MaxEverTupleIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MaxIndex:    maxIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MaxEverTupleIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MaxEverTupleIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *MaxEverTupleIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMaxEverTupleIndex", params, nil)
}

func (s *MaxEverTupleIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MaxEverTupleIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMaxEverTupleIndex", params, nil)
}

func (s *MaxEverTupleIndexConformanceStore) ScanMaxEverTupleIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
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

func (s *MaxEverTupleIndexConformanceStore) ScanMaxEverTupleIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMaxEverTupleIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMaxEverTupleIndex failed: %w", err)
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

// MinEverTupleIndexConformanceStore wraps record operations with a MIN_EVER_TUPLE
// index on Order.price ungrouped.
// Go: Ungrouped(Field("price"))
// Java: new GroupingKeyExpression(field("price"), 1), IndexTypes.MIN_EVER_TUPLE
type MinEverTupleIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MinIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMinEverTupleIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MinEverTupleIndexConformanceStore, error) {
	minIdx := recordlayer.NewMinEverTupleIndex("min_ever_price_tuple", recordlayer.Ungrouped(recordlayer.Field("price")))

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

	return &MinEverTupleIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MinIndex:    minIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MinEverTupleIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MinEverTupleIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *MinEverTupleIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMinEverTupleIndex", params, nil)
}

func (s *MinEverTupleIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MinEverTupleIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMinEverTupleIndex", params, nil)
}

func (s *MinEverTupleIndexConformanceStore) ScanMinEverTupleIndexGo(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
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

func (s *MinEverTupleIndexConformanceStore) ScanMinEverTupleIndexJava(ctx context.Context) ([]MinMaxEverIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMinEverTupleIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMinEverTupleIndex failed: %w", err)
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
