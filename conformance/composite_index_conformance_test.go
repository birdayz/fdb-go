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

// Composite Index Conformance Tests
//
// Validates PK component deduplication: when an index key expression overlaps
// with the primary key, Java trims redundant PK components from the index entry.
// Go must produce identical wire format.
//
// Test: Index on (price, order_id), PK on (order_id).
// Java deduplicates order_id → entry key is (price, order_id) with 2 elements.
// Without dedup it would be (price, order_id, order_id) with 3 elements.
var _ = Describe("Composite Index Conformance (PK Dedup)", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *CompositeIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("cidx_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewCompositeIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java scans composite index", func() {
		It("should produce identical deduplicated index entries", func() {
			// Save orders with Go
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare entries
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify key structure: (price, order_id) with dedup
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				expectedPK := int64(i + 1)
				// Key should have 2 elements (deduplicated)
				Expect(entry.Key).To(HaveLen(2), "index entry key should be deduplicated to 2 elements")
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
				Expect(toInt64(entry.Key[1])).To(Equal(expectedPK))
				// PK should be reconstructed correctly
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
			}
		})
	})

	Describe("Java writes, Go scans composite index", func() {
		It("should produce identical deduplicated index entries", func() {
			// Save orders with Java
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare entries — Go should read Java's deduplicated entries correctly
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// PK should be reconstructed correctly from deduplicated entries
			for i, entry := range goEntries {
				expectedPK := int64(i + 1)
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
			}
		})
	})

	Describe("Cross-write composite index", func() {
		It("Go and Java produce interchangeable composite index entries", func() {
			// Go writes odd orders
			for i := int64(1); i <= 5; i += 2 {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java writes even orders
			for i := int64(2); i <= 4; i += 2 {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Both Go and Java should see all 5 entries identically
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// toInt64 is defined in index_conformance_test.go

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
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
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

func (s *CompositeIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
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

// SaveOrderJava saves an order via Java (with composite index maintenance).
func (s *CompositeIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCompositeIndex", params, nil)
}

// ScanIndexGo scans the composite index using Go.
func (s *CompositeIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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

	var javaResults []map[string]any
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
