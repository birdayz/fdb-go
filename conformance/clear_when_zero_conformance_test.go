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

var _ = Describe("CLEAR_WHEN_ZERO Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ClearWhenZeroConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cwz_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewClearWhenZeroConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go insert+delete clears entry, Java confirms", func() {
		It("should have no index entries when all records are deleted", func() {
			// Go saves 2 orders with price=100
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=2
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			// Delete both — with CLEAR_WHEN_ZERO, the entry should be removed
			_, err = store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Go sees no entries
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty(), "Go should see no entries after CLEAR_WHEN_ZERO")

			// Java also sees no entries
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty(), "Java should see no entries after Go's CLEAR_WHEN_ZERO")
		})
	})

	Describe("Java insert+delete clears entry, Go confirms", func() {
		It("should have no index entries when all records are deleted by Java", func() {
			// Java saves 2 orders with price=200
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=2
			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(javaEntries[0].Count).To(Equal(int64(2)))

			// Java deletes both
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// Java sees no entries
			javaEntries, err = store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty(), "Java should see no entries after CLEAR_WHEN_ZERO")

			// Go also sees no entries
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty(), "Go should see no entries after Java's CLEAR_WHEN_ZERO")
		})
	})

	Describe("Cross-platform: Go inserts, Java deletes all, entry cleared", func() {
		It("should clear entry when Java deletes Go-written records to zero", func() {
			// Go saves 1 order with price=300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify count=1
			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(1)))

			// Java deletes it
			err = store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			// Both should see no entries
			goEntries, err = store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Partial delete leaves non-zero entry", func() {
		It("should keep entry when count is still > 0", func() {
			// Go saves 3 orders with price=400
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGoProto(ctx, i, 400)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete 1 — count should be 2
			_, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanCountIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].Count).To(Equal(int64(2)))

			javaEntries, err := store.ScanCountIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = CompareCountIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// ClearWhenZeroConformanceStore wraps record operations with a COUNT index
// that has CLEAR_WHEN_ZERO enabled. When a count entry reaches zero, it's
// atomically removed via FDB CompareAndClear.
type ClearWhenZeroConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	CountIndex  *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewClearWhenZeroConformanceStore creates a conformance store with a COUNT index
// with CLEAR_WHEN_ZERO on Order.price.
func NewClearWhenZeroConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*ClearWhenZeroConformanceStore, error) {
	countIdx := recordlayer.NewCountIndex("count_cwz_price", recordlayer.GroupAll(recordlayer.Field("price")))
	countIdx.SetClearWhenZero(true)

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

	return &ClearWhenZeroConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		CountIndex:  countIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *ClearWhenZeroConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *ClearWhenZeroConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *ClearWhenZeroConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCountCWZ", params, nil)
}

func (s *ClearWhenZeroConformanceStore) SaveOrderGoProto(ctx context.Context, orderID int64, price int32) error {
	return s.SaveOrderGo(ctx, &gen.Order{
		OrderId: proto.Int64(orderID),
		Price:   proto.Int32(price),
	})
}

func (s *ClearWhenZeroConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *ClearWhenZeroConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithCountCWZ", params, nil)
}

func (s *ClearWhenZeroConformanceStore) ScanCountIndexGo(ctx context.Context) ([]CountIndexEntryResult, error) {
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

func (s *ClearWhenZeroConformanceStore) ScanCountIndexJava(ctx context.Context) ([]CountIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanCountCWZIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanCountCWZIndex failed: %w", err)
	}

	var results []CountIndexEntryResult
	for _, m := range javaResults {
		entry := CountIndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if countRaw, ok := m["count"]; ok {
			entry.Count = int64(countRaw.(float64))
		}
		results = append(results, entry)
	}
	return results, nil
}
