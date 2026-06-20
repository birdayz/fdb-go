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

var _ = Describe("Nesting Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *NestingIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("nesting_%s", uuid.New().String())
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		store, err = NewNestingIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("flower.type index (string field)", func() {
		It("Go saves, Go scans flower.type index", func() {
			for i := int64(1); i <= 3; i++ {
				flowers := []string{"Rose", "Tulip", "Orchid"}
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower(flowers[i-1], gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanNestingIndexGo(ctx, store.FlowerTypeIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Entries sorted by type string: Orchid < Rose < Tulip
			Expect(goEntries[0].Key[0]).To(Equal("Orchid"))
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(3)))

			Expect(goEntries[1].Key[0]).To(Equal("Rose"))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(1)))

			Expect(goEntries[2].Key[0]).To(Equal("Tulip"))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(2)))
		})

		It("Go saves, Java scans flower.type index", func() {
			for i := int64(1); i <= 3; i++ {
				flowers := []string{"Rose", "Tulip", "Orchid"}
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower(flowers[i-1], gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanNestingIndexGo(ctx, store.FlowerTypeIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanNestingIndexJava(ctx, "order_flower_type")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Java saves, Go scans flower.type index", func() {
			for i := int64(1); i <= 3; i++ {
				flowers := []string{"Daisy", "Lily", "Sunflower"}
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower(flowers[i-1], gen.Color_BLUE).Build()
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanNestingIndexGo(ctx, store.FlowerTypeIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanNestingIndexJava(ctx, "order_flower_type")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Entries sorted by type string: Daisy < Lily < Sunflower
			Expect(goEntries[0].Key[0]).To(Equal("Daisy"))
			Expect(goEntries[1].Key[0]).To(Equal("Lily"))
			Expect(goEntries[2].Key[0]).To(Equal("Sunflower"))
		})
	})

	Describe("flower.color index (enum field)", func() {
		It("Go saves, Go scans flower.color index", func() {
			colors := []gen.Color{gen.Color_BLUE, gen.Color_RED, gen.Color_YELLOW}
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower("Rose", colors[i-1]).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanNestingIndexGo(ctx, store.FlowerColorIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Enum values sorted numerically: RED=1, BLUE=2, YELLOW=3
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(gen.Color_RED)))
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(2)))

			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(gen.Color_BLUE)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(1)))

			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(gen.Color_YELLOW)))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(3)))
		})

		It("Go saves, Java scans flower.color index", func() {
			colors := []gen.Color{gen.Color_PINK, gen.Color_RED, gen.Color_BLUE}
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower("Tulip", colors[i-1]).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanNestingIndexGo(ctx, store.FlowerColorIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanNestingIndexJava(ctx, "order_flower_color")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// NestingIndexConformanceStore wraps record operations with VALUE indexes on
// nested message fields (flower.type and flower.color) for cross-validating
// index entries between Go and Java.
type NestingIndexConformanceStore struct {
	RecordDB         *recordlayer.FDBDatabase
	MetaData         *recordlayer.RecordMetaData
	FlowerTypeIndex  *recordlayer.Index
	FlowerColorIndex *recordlayer.Index
	Keyspace         subspace.Subspace
	java             *JavaInvoker
	clusterFile      string
	tenantName       string
}

func NewNestingIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*NestingIndexConformanceStore, error) {
	flowerTypeIndex := recordlayer.NewIndex("order_flower_type",
		recordlayer.Nest("flower", recordlayer.Field("type")))
	flowerColorIndex := recordlayer.NewIndex("order_flower_color",
		recordlayer.Nest("flower", recordlayer.Field("color")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", flowerTypeIndex)
	builder.AddIndex("Order", flowerColorIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &NestingIndexConformanceStore{
		RecordDB:         recordDB,
		MetaData:         md,
		FlowerTypeIndex:  flowerTypeIndex,
		FlowerColorIndex: flowerColorIndex,
		Keyspace:         ks,
		java:             NewJavaInvoker(),
		clusterFile:      clusterFile,
		tenantName:       tenantName,
	}, nil
}

func (s *NestingIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *NestingIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *NestingIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithNestingIndex", params, nil)
}

func (s *NestingIndexConformanceStore) ScanNestingIndexGo(ctx context.Context, index *recordlayer.Index) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(index, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

func (s *NestingIndexConformanceStore) ScanNestingIndexJava(ctx context.Context, indexName string) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanNestingIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanNestingIndex failed: %w", err)
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
