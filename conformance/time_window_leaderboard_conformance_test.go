package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

var _ = Describe("TIME_WINDOW_LEADERBOARD Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *LeaderboardConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("twlb_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewLeaderboardConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())

		// Set up leaderboard windows via Java's performIndexOperation FIRST,
		// before any records are saved. This creates the all-time leaderboard
		// directory that both Go and Java will use.
		err = store.SetupWindowsJava(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan BY_VALUE", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 300, 100},
				{2, 100, 200},
				{3, 200, 300},
			}
			for _, o := range orders {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Sorted by price (score): 100, 200, 300
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(200)))
			Expect(toInt64(goEntries[2].Key[0])).To(Equal(int64(300)))
		})
	})

	Describe("Java writes, both scan BY_VALUE", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 500, 10},
				{2, 250, 20},
				{3, 750, 30},
			}
			for _, o := range orders {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
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
			// Go writes order 1
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(300),
				Quantity: proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes order 2
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(60),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes order 3
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId:  proto.Int64(3),
				Price:    proto.Int32(200),
				Quantity: proto.Int32(70),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
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
		It("Go deletes a Java-written record", func() {
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(400),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(200),
				Quantity: proto.Int32(20),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go deletes order 1
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(200)))
		})

		It("Java deletes a Go-written record", func() {
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(150),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(350),
				Quantity: proto.Int32(20),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(150)))
		})
	})

	Describe("Rank cross-validation", func() {
		It("Go and Java agree on rank of records", func() {
			orders := []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 300, 10},
				{2, 100, 20},
				{3, 500, 30},
				{4, 200, 40},
				{5, 400, 50},
			}
			for _, o := range orders {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Expected ranks: price=100->0, 200->1, 300->2, 400->3, 500->4
			expectedRanks := map[int64]int64{1: 2, 2: 0, 3: 4, 4: 1, 5: 3}

			for orderID, expectedRank := range expectedRanks {
				goRank, err := store.RankForRecordGo(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(goRank).NotTo(BeNil(), "Go rank nil for orderID=%d", orderID)
				Expect(*goRank).To(Equal(expectedRank), "Go rank mismatch for orderID=%d", orderID)

				javaRank, err := store.RankForRecordJava(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaRank).NotTo(BeNil(), "Java rank nil for orderID=%d", orderID)
				Expect(*javaRank).To(Equal(expectedRank), "Java rank mismatch for orderID=%d", orderID)
			}
		})
	})

	Describe("Update score cross-language", func() {
		It("Java writes, Go overwrites with new price, both scan updated", func() {
			// Java writes price=100
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(300),
				Quantity: proto.Int32(20),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go updates order 1 price to 500
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(500),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanLeaderboardGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanLeaderboardJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Sorted by price: 300(pk=2), 500(pk=1)
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(300)))
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(2)))
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(500)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(1)))
		})
	})
})

// leaderboardWindowUpdater is a duck-typed interface for the unexported
// timeWindowLeaderboardIndexMaintainer.PerformWindowUpdate method.
type leaderboardWindowUpdater interface {
	PerformWindowUpdate(update *recordlayer.TimeWindowLeaderboardWindowUpdate, store *recordlayer.FDBRecordStore) error
}

// LeaderboardConformanceStore wraps record operations with a TIME_WINDOW_LEADERBOARD
// index on Order using Concat(Field("price"), Field("quantity")).
// Tests BY_VALUE scanning and rank queries across Go and Java.
type LeaderboardConformanceStore struct {
	RecordDB         *recordlayer.FDBDatabase
	MetaData         *recordlayer.RecordMetaData
	LeaderboardIndex *recordlayer.Index
	Keyspace         subspace.Subspace
	java             *JavaInvoker
	clusterFile      string
	tenantName       string
}

func buildLeaderboardConformanceMetadata() (*recordlayer.RecordMetaData, *recordlayer.Index) {
	idx := recordlayer.NewTimeWindowLeaderboardIndex("leaderboard_score",
		recordlayer.Ungrouped(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity"))))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		panic(fmt.Sprintf("buildLeaderboardConformanceMetadata: %v", err))
	}
	return md, idx
}

func NewLeaderboardConformanceStore(
	recordDB *recordlayer.FDBDatabase,
	keyspace subspace.Subspace,
	clusterFile string,
	tenantName string,
) (*LeaderboardConformanceStore, error) {
	md, idx := buildLeaderboardConformanceMetadata()

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &LeaderboardConformanceStore{
		RecordDB:         recordDB,
		MetaData:         md,
		LeaderboardIndex: idx,
		Keyspace:         ks,
		java:             NewJavaInvoker(),
		clusterFile:      clusterFile,
		tenantName:       tenantName,
	}, nil
}

func (s *LeaderboardConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SetupWindowsJava calls Java's performIndexOperation to create the all-time
// leaderboard window. This must be called before any records are saved.
func (s *LeaderboardConformanceStore) SetupWindowsJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "setupLeaderboardWindows", params, nil)
}

// setupWindowsGo creates the all-time leaderboard window via Go's PerformWindowUpdate.
func (s *LeaderboardConformanceStore) setupWindowsGo(store *recordlayer.FDBRecordStore) error {
	maintainer := store.GetIndexMaintainer(s.LeaderboardIndex)
	lm, ok := maintainer.(leaderboardWindowUpdater)
	if !ok {
		return fmt.Errorf("index maintainer %T does not implement leaderboardWindowUpdater", maintainer)
	}
	return lm.PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
		UpdateTimestamp: 0,
		AllTime:         true,
		Rebuild:         recordlayer.TimeWindowRebuildIfOverlappingChanged,
	}, store)
}

func (s *LeaderboardConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *LeaderboardConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithLeaderboard", params, nil)
}

func (s *LeaderboardConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *LeaderboardConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithLeaderboard", params, nil)
}

// ScanLeaderboardGo scans the TIME_WINDOW_LEADERBOARD index BY_VALUE (all-time)
// using Go's ScanTimeWindowLeaderboard.
func (s *LeaderboardConformanceStore) ScanLeaderboardGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanTimeWindowLeaderboard(
			s.LeaderboardIndex,
			recordlayer.IndexScanByTimeWindow,
			recordlayer.AllTimeLeaderboardType,
			0,
			recordlayer.TupleRangeAll,
			nil,
			recordlayer.ForwardScan(),
		))
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

// ScanLeaderboardJava scans the TIME_WINDOW_LEADERBOARD index BY_VALUE using Java.
func (s *LeaderboardConformanceStore) ScanLeaderboardJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanLeaderboardIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanLeaderboardIndex failed: %w", err)
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

// RankForRecordGo evaluates the rank of a record within the all-time leaderboard
// using Go's EvaluateRecordFunction.
func (s *LeaderboardConformanceStore) RankForRecordGo(ctx context.Context, orderID int64) (*int64, error) {
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
			Operand: recordlayer.Ungrouped(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity"))),
			Index:   "leaderboard_score",
		}
		rank, err = store.EvaluateRecordFunction(fn, rec)
		return nil, err
	})
	return rank, err
}

// RankForRecordJava evaluates the rank of a record within the all-time leaderboard
// using Java's evaluateRecordFunction.
func (s *LeaderboardConformanceStore) RankForRecordJava(ctx context.Context, orderID int64) (*int64, error) {
	params := s.buildJavaParams()
	params["orderID"] = orderID

	var result *float64
	if err := s.java.InvokeAs(ctx, "leaderboardRankForRecord", params, &result); err != nil {
		return nil, fmt.Errorf("java leaderboardRankForRecord failed: %w", err)
	}
	if result == nil {
		return nil, nil
	}
	rank := int64(*result)
	return &rank, nil
}
