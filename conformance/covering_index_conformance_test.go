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
)

var _ = Describe("Covering Index (KeyWithValueExpression) Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *CoveringIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("covering_%s", uuid.New().String())
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		store, err = NewCoveringIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan covering index", func() {
		It("value portion is wire-compatible between Go and Java", func() {
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower("Rose", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanCoveringIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare key and primaryKey
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify value portion matches
			for i, ge := range goEntries {
				je := javaEntries[i]
				Expect(len(ge.Value)).To(Equal(len(je.Value)),
					"value length mismatch at entry %d", i)
				for j := range ge.Value {
					Expect(toInt64(ge.Value[j])).To(Equal(toInt64(je.Value[j])),
						"value[%d] mismatch at entry %d", j, i)
				}
			}

			// Verify entries are sorted by price and value contains flower type
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(goEntries[0].Value[0]).To(Equal("Rose"))

			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(goEntries[1].Value[0]).To(Equal("Rose"))

			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(300)))
			Expect(goEntries[2].Value[0]).To(Equal("Rose"))
		})
	})

	Describe("Java writes, both scan covering index", func() {
		It("Go reads covering index entries written by Java", func() {
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*200)).WithFlower("Tulip", gen.Color_BLUE).Build()
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanCoveringIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify value portion
			for i, ge := range goEntries {
				je := javaEntries[i]
				Expect(len(ge.Value)).To(Equal(len(je.Value)))
				for j := range ge.Value {
					Expect(toInt64(ge.Value[j])).To(Equal(toInt64(je.Value[j])))
				}
			}
		})
	})

	Describe("Cross-language delete removes covering index entry", func() {
		It("Go deletes Java-written record, both see updated index", func() {
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower("Rose", gen.Color_RED).Build()
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go deletes order 2
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanCoveringIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Remaining: price=100 (order_id=1) and price=300 (order_id=3)
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(goEntries[0].Value[0]).To(Equal("Rose"))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(300)))
			Expect(goEntries[1].Value[0]).To(Equal("Rose"))
		})
	})

	Describe("Update changes value portion consistently", func() {
		It("Go updates record, Java sees new value in covering index", func() {
			order := NewOrder(1).WithPrice(100).WithFlower("Rose", gen.Color_RED).Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify initial state
			javaEntries, err := store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(100)))

			// Update price from 100 to 500
			order2 := NewOrder(1).WithPrice(500).WithFlower("Rose", gen.Color_RED).Build()
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanCoveringIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))
			Expect(goEntries[0].Value[0]).To(Equal("Rose"))

			javaEntries, err = store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(500)))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Mixed writes, both scan covering index", func() {
		It("interleaved Go and Java writes produce consistent covering index", func() {
			// Go writes orders 1,3,5; Java writes orders 2,4
			for _, id := range []int64{1, 3, 5} {
				order := NewOrder(id).WithPrice(int32(id*100)).WithFlower("Rose", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}
			for _, id := range []int64{2, 4} {
				order := NewOrder(id).WithPrice(int32(id*100)).WithFlower("Tulip", gen.Color_BLUE).Build()
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanCoveringIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			javaEntries, err := store.ScanCoveringIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify sorted by price: 100, 200, 300, 400, 500
			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(int64((i + 1) * 100)))
				Expect(entry.Value).To(HaveLen(1)) // flower type in value
			}
		})
	})
})

// CoveringIndexConformanceStore wraps record operations with a covering index
// (KeyWithValueExpression) on Order for cross-validating index entries between Go and Java.
type CoveringIndexConformanceStore struct {
	RecordDB      *recordlayer.FDBDatabase
	MetaData      *recordlayer.RecordMetaData
	CoveringIndex *recordlayer.Index
	Keyspace      subspace.Subspace
	java          *JavaInvoker
	clusterFile   string
	tenantName    string
}

func NewCoveringIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*CoveringIndexConformanceStore, error) {
	// Covering index: key = [price], value = [flower.type]. Matches Java's createCoveringIndexedMetaData().
	coveringIndex := recordlayer.NewIndex("covering_price",
		recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Nest("flower", recordlayer.Field("type"))), 1))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", coveringIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &CoveringIndexConformanceStore{
		RecordDB:      recordDB,
		MetaData:      md,
		CoveringIndex: coveringIndex,
		Keyspace:      ks,
		java:          NewJavaInvoker(),
		clusterFile:   clusterFile,
		tenantName:    tenantName,
	}, nil
}

func (s *CoveringIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *CoveringIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *CoveringIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithCoveringIndex", params, nil)
}

func (s *CoveringIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *CoveringIndexConformanceStore) ScanCoveringIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.CoveringIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, IndexEntryResult{
				Key:        tupleToSlice(e.Key),
				PrimaryKey: tupleToSlice(e.PrimaryKey()),
				Value:      tupleToSlice(e.Value),
			})
		}
		return nil, nil
	})
	return results, err
}

func (s *CoveringIndexConformanceStore) ScanCoveringIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanCoveringIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanCoveringIndex failed: %w", err)
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
		if valRaw, ok := m["value"]; ok {
			entry.Value = toInterfaceSlice(valRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}
