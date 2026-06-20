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

var _ = Describe("RANK Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *RankIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("rank_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewRankIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan BY_VALUE", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			for i, price := range []int32{300, 100, 200} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Sorted by price: 100, 200, 300
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(300)))
		})
	})

	Describe("Java writes, both scan BY_VALUE", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			for i, price := range []int32{500, 250, 750} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Sorted by price: 250, 500, 750
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(250)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(500)))
			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(750)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce identically ordered entries", func() {
			// Go writes order 1 with price=300
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes order 2 with price=100
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes order 3 with price=200
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Sorted by price: 100(pk=2), 200(pk=3), 300(pk=1)
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(2)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(3)))
			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(300)))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(1)))
		})
	})

	Describe("Delete removes index entry cross-language", func() {
		It("should remove the index entry when Go deletes a Java-written record", func() {
			// Java writes
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(400),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go deletes order 1
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(200)))
		})

		It("should remove the index entry when Java deletes a Go-written record", func() {
			// Go writes
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(150),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(350),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(150)))
		})
	})

	Describe("Update changes index entry cross-language", func() {
		It("should update when Go updates a Java-written record", func() {
			// Java writes price=100
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go updates price to 500
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanRankIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanRankIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))
		})
	})

	Describe("BY_RANK scan cross-validated", func() {
		It("should produce identical results for rank range [0, 2) via both Go and Java", func() {
			// Insert 5 orders with distinct prices
			prices := []int32{500, 100, 300, 200, 400}
			for i, price := range prices {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_RANK [0, 2) = rank 0 and rank 1 = lowest 2 prices (100, 200)
			goEntries, err := store.ScanRankIndexByRankGo(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanRankIndexByRankJava(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// rank 0 = price 100, rank 1 = price 200
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
		})

		It("should produce identical results for full rank range via both Go and Java", func() {
			prices := []int32{300, 100, 200}
			for i, price := range prices {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// BY_RANK [0, 100) = all entries (only 3 records)
			goEntries, err := store.ScanRankIndexByRankGo(ctx, 0, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanRankIndexByRankJava(ctx, 0, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(300)))
		})
	})

	Describe("Ranked set wire compatibility", func() {
		It("Go writes, Java reads by rank — ranked set is shared", func() {
			// Go inserts 3 records
			for i, price := range []int32{600, 200, 400} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java does BY_RANK [1, 3) = rank 1 and rank 2 = middle and highest
			javaEntries, err := store.ScanRankIndexByRankJava(ctx, 1, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// rank 1 = 400, rank 2 = 600
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(400)))
			Expect(toInt64(javaEntries[1].Key[0])).To(Equal(int64(600)))

			// Go should agree
			goEntries, err := store.ScanRankIndexByRankGo(ctx, 1, 3)
			Expect(err).NotTo(HaveOccurred())

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Java writes, Go reads by rank — ranked set is shared", func() {
			// Java inserts 4 records
			for i, price := range []int32{800, 200, 500, 100} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Go does BY_RANK [0, 2) = rank 0 and rank 1 = lowest 2
			goEntries, err := store.ScanRankIndexByRankGo(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// rank 0 = 100, rank 1 = 200
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))

			// Java should agree
			javaEntries, err := store.ScanRankIndexByRankJava(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete updates ranked set cross-language", func() {
		It("should update rank positions after Go deletes from Java data", func() {
			// Java inserts 3 records
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Go deletes the middle one (price=200, order_id=2)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// BY_RANK [0, 2) should now return 100, 300 (rank 0 and 1)
			goEntries, err := store.ScanRankIndexByRankGo(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanRankIndexByRankJava(ctx, 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(300)))
		})
	})

	Describe("EvaluateRecordFunction cross-validation", func() {
		It("Go and Java agree on rank of records written by Go", func() {
			prices := []int32{300, 100, 500, 200, 400}
			for i, price := range prices {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Expected ranks: price=100→0, price=200→1, price=300→2, price=400→3, price=500→4
			expectedRanks := map[int64]int64{1: 2, 2: 0, 3: 4, 4: 1, 5: 3}

			for orderID, expectedRank := range expectedRanks {
				goRank, err := store.RankForRecordGo(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(goRank).NotTo(BeNil())
				Expect(*goRank).To(Equal(expectedRank), "Go rank mismatch for orderID=%d", orderID)

				javaRank, err := store.RankForRecordJava(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaRank).NotTo(BeNil())
				Expect(*javaRank).To(Equal(expectedRank), "Java rank mismatch for orderID=%d", orderID)
			}
		})

		It("Go and Java agree on rank of records written by Java", func() {
			prices := []int32{50, 150, 250}
			for i, price := range prices {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// price=50→rank 0, price=150→rank 1, price=250→rank 2
			for orderID := int64(1); orderID <= 3; orderID++ {
				goRank, err := store.RankForRecordGo(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(goRank).NotTo(BeNil())

				javaRank, err := store.RankForRecordJava(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaRank).NotTo(BeNil())

				Expect(*goRank).To(Equal(*javaRank), "rank mismatch for orderID=%d: Go=%d Java=%d", orderID, *goRank, *javaRank)
			}
		})

		It("ranks update consistently after cross-language delete", func() {
			// Go writes 3 records
			for i, price := range []int32{100, 200, 300} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java deletes the middle record (price=200)
			err := store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			// After delete: price=100→rank 0, price=300→rank 1
			goRank, err := store.RankForRecordGo(ctx, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(*goRank).To(Equal(int64(1)))

			javaRank, err := store.RankForRecordJava(ctx, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(*javaRank).To(Equal(int64(1)))

			goRank, err = store.RankForRecordGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*goRank).To(Equal(int64(0)))

			javaRank, err = store.RankForRecordJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*javaRank).To(Equal(int64(0)))
		})
	})
})

// RankIndexConformanceStore wraps record operations with a RANK index on Order.price.
// Tests both BY_VALUE and BY_RANK scanning across Go and Java.
type RankIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	RankIndex   *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewRankIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*RankIndexConformanceStore, error) {
	rankIdx := recordlayer.NewRankIndex("rank_by_price", recordlayer.GroupBy(recordlayer.Field("price")))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", rankIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &RankIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		RankIndex:   rankIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *RankIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *RankIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *RankIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithRankIndex", params, nil)
}

func (s *RankIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *RankIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithRankIndex", params, nil)
}

// ScanRankIndexGo scans the RANK index BY_VALUE using Go.
func (s *RankIndexConformanceStore) ScanRankIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.RankIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanRankIndexJava scans the RANK index BY_VALUE using Java.
func (s *RankIndexConformanceStore) ScanRankIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanRankIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanRankIndex failed: %w", err)
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

// ScanRankIndexByRankGo scans the RANK index BY_RANK using Go.
// Range is [lowRank, highRank) (inclusive low, exclusive high).
func (s *RankIndexConformanceStore) ScanRankIndexByRankGo(ctx context.Context, lowRank, highRank int64) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		rankRange := recordlayer.TupleRange{
			Low:          tuple.Tuple{lowRank},
			High:         tuple.Tuple{highRank},
			LowEndpoint:  recordlayer.EndpointTypeRangeInclusive,
			HighEndpoint: recordlayer.EndpointTypeRangeExclusive,
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(
			s.RankIndex, recordlayer.IndexScanByRank, rankRange, nil, recordlayer.ForwardScan()))
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

// RankForRecordGo evaluates the rank of a record's price using Go.
func (s *RankIndexConformanceStore) RankForRecordGo(ctx context.Context, orderID int64) (*int64, error) {
	var rank *int64
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
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
		fn := &recordlayer.IndexRecordFunction{
			Name:    recordlayer.FunctionNameRank,
			Operand: recordlayer.GroupBy(recordlayer.Field("price")),
		}
		rank, err = store.EvaluateRecordFunction(fn, rec)
		return nil, err
	})
	return rank, err
}

// RankForRecordJava evaluates the rank of a record's price using Java.
func (s *RankIndexConformanceStore) RankForRecordJava(ctx context.Context, orderID int64) (*int64, error) {
	params := s.buildJavaParams()
	params["orderID"] = orderID

	var result *float64
	if err := s.java.InvokeAs(ctx, "rankForRecord", params, &result); err != nil {
		return nil, fmt.Errorf("java rankForRecord failed: %w", err)
	}
	if result == nil {
		return nil, nil
	}
	rank := int64(*result)
	return &rank, nil
}

// ScanRankIndexByRankJava scans the RANK index BY_RANK using Java.
// Range is [lowRank, highRank) (inclusive low, exclusive high).
func (s *RankIndexConformanceStore) ScanRankIndexByRankJava(ctx context.Context, lowRank, highRank int64) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["lowRank"] = lowRank
	params["highRank"] = highRank

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanRankIndexByRank", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanRankIndexByRank failed: %w", err)
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
