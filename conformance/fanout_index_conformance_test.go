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
)

var _ = Describe("Fan-Out Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *FanOutIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("fanout_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewFanOutIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with fan-out index, Java scans", func() {
		It("should produce one index entry per tag, visible to both Go and Java", func() {
			order := NewOrder(1).WithPrice(100).WithTags("urgent", "wholesale", "premium").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// All entries should point to PK=1
			for _, entry := range goEntries {
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(int64(1)))
			}
		})
	})

	Describe("Java writes with fan-out index, Go scans", func() {
		It("should produce identical fan-out entries visible to Go", func() {
			order := NewOrder(2).WithPrice(200).WithTags("bulk", "discount").Build()
			err := store.SaveOrderJava(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Multiple records with fan-out", func() {
		It("should produce correct total entries across records", func() {
			// Record 1: 2 tags, Record 2: 3 tags, Record 3: 1 tag = 6 total entries
			orders := []*gen.Order{
				NewOrder(10).WithPrice(100).WithTags("a", "b").Build(),
				NewOrder(20).WithPrice(200).WithTags("c", "d", "e").Build(),
				NewOrder(30).WithPrice(300).WithTags("f").Build(),
			}

			for _, order := range orders {
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(6))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(6))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Empty repeated field produces no entries", func() {
		It("should produce zero index entries for a record with no tags", func() {
			order := NewOrder(42).WithPrice(500).Build() // no tags
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Delete removes all fan-out entries", func() {
		It("should remove all index entries when record is deleted", func() {
			order := NewOrder(99).WithPrice(999).WithTags("x", "y", "z").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify 3 entries exist
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Delete
			deleted, err := store.DeleteOrderGo(ctx, 99)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify all entries removed
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Update changes fan-out entries", func() {
		It("should update index entries when tags change", func() {
			// Save with 2 tags
			order := NewOrder(50).WithPrice(100).WithTags("old1", "old2").Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Update with 3 different tags
			order2 := NewOrder(50).WithPrice(100).WithTags("new1", "new2", "new3").Build()
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Should now have 3 entries (old 2 removed, new 3 added)
			goEntries, err = store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Cross-write fan-out", func() {
		It("should produce identical entries whether Go or Java writes", func() {
			// Go writes record with tags
			goOrder := NewOrder(100).WithPrice(100).WithTags("alpha", "beta").Build()
			err := store.SaveOrderGo(ctx, goOrder)
			Expect(err).NotTo(HaveOccurred())

			// Java writes record with tags
			javaOrder := NewOrder(200).WithPrice(200).WithTags("gamma", "delta").Build()
			err = store.SaveOrderJava(ctx, javaOrder)
			Expect(err).NotTo(HaveOccurred())

			// Total: 4 entries (2 from each record)
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// FanOutIndexConformanceStore wraps record operations with a FanOut VALUE index on Order.tags
// and provides methods to cross-validate index entries between Go and Java.
type FanOutIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	TagsIndex   *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewFanOutIndexConformanceStore creates a conformance store with a FanOut VALUE index on Order.tags.
func NewFanOutIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*FanOutIndexConformanceStore, error) {
	tagsIndex := recordlayer.NewIndex("Order$tags", recordlayer.FanOut("tags"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", tagsIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &FanOutIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		TagsIndex:   tagsIndex,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *FanOutIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithFanOutIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with fan-out index maintenance).
func (s *FanOutIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithFanOutIndex", params, nil)
}

// ScanIndexGo scans the tags fan-out index using Go and returns results.
func (s *FanOutIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.TagsIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexJava scans the tags fan-out index using Java and returns results.
func (s *FanOutIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$tags"

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanFanOutIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanFanOutIndex failed: %w", err)
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
