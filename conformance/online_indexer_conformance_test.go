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

var _ = Describe("OnlineIndexer Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *OnlineIndexerConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("oidx_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewOnlineIndexerConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go saves, Go online-builds, Java scans", func() {
		It("should produce index entries readable by Java", func() {
			for i := int64(1); i <= 10; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			err := store.OnlineBuildIndexGo(ctx, 100)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(10))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(10))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 100, 200, ..., 1000
			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(int64((i + 1) * 100)))
			}
		})
	})

	Describe("Java saves, Go online-builds, both scan", func() {
		It("should index Java-written records correctly", func() {
			for i := int64(1); i <= 8; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 50)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			err := store.OnlineBuildIndexGo(ctx, 100)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(8))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(8))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(int64((i + 1) * 50)))
			}
		})
	})

	Describe("Chunked build with small limit", func() {
		It("should produce correct entries across multiple build transactions", func() {
			for i := int64(1); i <= 12; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Build with limit=3 → 4 chunks
			err := store.OnlineBuildIndexGo(ctx, 3)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(12))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(12))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(int64((i + 1) * 10)))
			}
		})
	})

	Describe("Go online-build vs Java rebuild produce identical results", func() {
		It("should produce byte-identical index entries", func() {
			for i := int64(1); i <= 5; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 150)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Go online-builds
			err := store.OnlineBuildIndexGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			onlineBuildEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(onlineBuildEntries).To(HaveLen(5))

			// Java rebuilds (single-tx, clears and rebuilds)
			err = store.RebuildIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			rebuildEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(rebuildEntries).To(HaveLen(5))

			err = CompareIndexEntries(onlineBuildEntries, rebuildEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Index state after Go online-build", func() {
		It("should be READABLE as seen by Java", func() {
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.OnlineBuildIndexGo(ctx, 100)
			Expect(err).NotTo(HaveOccurred())

			readable, err := store.IsIndexReadableJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(readable).To(BeTrue())
		})

		It("should be READABLE as seen by Go", func() {
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.OnlineBuildIndexGo(ctx, 100)
			Expect(err).NotTo(HaveOccurred())

			readable, err := store.IsIndexReadableGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(readable).To(BeTrue())
		})
	})

	Describe("Mixed writes then Go online-build", func() {
		It("should index all records regardless of which side wrote them", func() {
			// Go writes orders 1-3
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java writes orders 4-6
			for i := int64(4); i <= 6; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Go online-builds with chunking
			err := store.OnlineBuildIndexGo(ctx, 3)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(6))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(6))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(int64((i + 1) * 100)))
			}
		})
	})
})

// OnlineIndexerConformanceStore wraps operations for OnlineIndexer conformance testing.
type OnlineIndexerConformanceStore struct {
	RecordDB      *recordlayer.FDBDatabase
	MetaDataNoIdx *recordlayer.RecordMetaData
	MetaDataIdx   *recordlayer.RecordMetaData
	PriceIndex    *recordlayer.Index
	Keyspace      subspace.Subspace
	java          *JavaInvoker
	clusterFile   string
	tenantName    string
}

func NewOnlineIndexerConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*OnlineIndexerConformanceStore, error) {
	// Metadata WITHOUT index — for saving records before online build
	builderNoIdx := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderNoIdx.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderNoIdx.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderNoIdx.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	mdNoIdx, err := builderNoIdx.Build()
	if err != nil {
		return nil, err
	}

	// Metadata WITH index — for online build and scanning
	priceIndex := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
	builderIdx := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderIdx.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderIdx.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderIdx.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builderIdx.AddIndex("Order", priceIndex)
	mdIdx, err := builderIdx.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &OnlineIndexerConformanceStore{
		RecordDB:      recordDB,
		MetaDataNoIdx: mdNoIdx,
		MetaDataIdx:   mdIdx,
		PriceIndex:    priceIndex,
		Keyspace:      ks,
		java:          NewJavaInvoker(),
		clusterFile:   clusterFile,
		tenantName:    tenantName,
	}, nil
}

func (s *OnlineIndexerConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order WITHOUT index using Go.
func (s *OnlineIndexerConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataNoIdx).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = st.SaveRecord(order)
		return nil, err
	})
	return err
}

// SaveOrderJava saves an order WITHOUT index using Java.
func (s *OnlineIndexerConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderForOnlineBuild", params, nil)
}

// OnlineBuildIndexGo runs Go's OnlineIndexer to build the price index.
func (s *OnlineIndexerConformanceStore) OnlineBuildIndexGo(ctx context.Context, limit int) error {
	indexer, err := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(s.RecordDB).
		SetMetaData(s.MetaDataIdx).
		SetIndex(s.PriceIndex).
		SetSubspace(s.Keyspace).
		SetLimit(limit).
		Build()
	if err != nil {
		return err
	}
	_, err = indexer.BuildIndex(ctx)
	return err
}

// RebuildIndexJava runs Java's single-tx rebuild for comparison.
func (s *OnlineIndexerConformanceStore) RebuildIndexJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "rebuildIndex", params, nil)
}

// ScanIndexGo scans the price index using Go.
func (s *OnlineIndexerConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, st.ScanIndex(s.PriceIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexJava scans the price index using Java.
func (s *OnlineIndexerConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanIndexAfterOnlineBuild", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanIndexAfterOnlineBuild failed: %w", err)
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

// IsIndexReadableGo checks if the price index is READABLE using Go.
func (s *OnlineIndexerConformanceStore) IsIndexReadableGo(ctx context.Context) (bool, error) {
	var readable bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		readable = st.IsIndexReadable(s.PriceIndex.Name)
		return nil, nil
	})
	return readable, err
}

// IsIndexReadableJava checks if the price index is READABLE using Java.
func (s *OnlineIndexerConformanceStore) IsIndexReadableJava(ctx context.Context) (bool, error) {
	params := s.buildJavaParams()
	var readable bool
	if err := s.java.InvokeAs(ctx, "isIndexReadableAfterBuild", params, &readable); err != nil {
		return false, err
	}
	return readable, nil
}
