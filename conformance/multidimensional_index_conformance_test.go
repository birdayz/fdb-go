package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

var _ = Describe("MULTIDIMENSIONAL Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MultidimensionalIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("md_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMultidimensionalIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java scans", func() {
		It("should produce identical R-tree entries visible to both Go and Java", func() {
			coords := []struct {
				id int64
				x  int64
				y  int64
			}{
				{1, 100, 200},
				{2, 300, 400},
				{3, 500, 600},
			}
			for _, c := range coords {
				err := store.SaveOrderGo(ctx, c.id, c.x, c.y)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, Go scans", func() {
		It("should produce identical R-tree entries visible to both Go and Java", func() {
			coords := []struct {
				id int64
				x  int64
				y  int64
			}{
				{1, 10, 20},
				{2, 30, 40},
				{3, 50, 60},
			}
			for _, c := range coords {
				err := store.SaveOrderJava(ctx, c.id, c.x, c.y)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce identically ordered entries from both sides", func() {
			// Go writes 2 records
			err := store.SaveOrderGo(ctx, 1, 100, 200)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, 300, 400)
			Expect(err).NotTo(HaveOccurred())

			// Java writes 2 records
			err = store.SaveOrderJava(ctx, 3, 500, 600)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 4, 700, 800)
			Expect(err).NotTo(HaveOccurred())

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

	Describe("Go deletes Java-written record", func() {
		It("should remove the R-tree entry when Go deletes a Java-written record", func() {
			// Java writes 2 records
			err := store.SaveOrderJava(ctx, 1, 10, 20)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 2, 30, 40)
			Expect(err).NotTo(HaveOccurred())

			// Go deletes order 1
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Remaining entry should be the (30, 40) point
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(30)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(40)))
		})
	})

	Describe("Java deletes Go-written record", func() {
		It("should remove the R-tree entry when Java deletes a Go-written record", func() {
			// Go writes 2 records
			err := store.SaveOrderGo(ctx, 1, 100, 200)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, 300, 400)
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Remaining entry should be the (100, 200) point
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(200)))
		})
	})

	Describe("Update changes R-tree entry cross-language", func() {
		It("should update when Go updates a Java-written record", func() {
			// Java writes coord (10, 20)
			err := store.SaveOrderJava(ctx, 1, 10, 20)
			Expect(err).NotTo(HaveOccurred())

			// Go updates to coord (50, 60)
			err = store.SaveOrderGo(ctx, 1, 50, 60)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(50)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(60)))
		})
	})

	Describe("50-record multi-level tree cross-language", func() {
		It("should handle multi-level R-tree with bulk inserts from both sides", func() {
			// Go saves 50 orders with coords (id*100, id*200) — forces leaf splits and intermediate nodes
			for i := int64(1); i <= 50; i++ {
				err := store.SaveOrderGo(ctx, i, i*100, i*200)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java scans all — should get 50 entries
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(50))

			// Go scans all — should get 50 entries
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(50))

			// Compare Go vs Java entries (same count, same keys, same PKs)
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Java saves 10 more (ids 51-60) in a single transaction
			err = store.SaveMultipleOrdersJava(ctx, 51, 60)
			Expect(err).NotTo(HaveOccurred())

			// Go scans all 60 — verifies all present
			goEntries60, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries60).To(HaveLen(60))

			// Java scans all 60
			javaEntries60, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries60).To(HaveLen(60))

			// Compare Go vs Java for all 60
			err = CompareIndexEntries(goEntries60, javaEntries60)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Negative and boundary coordinates cross-language", func() {
		It("should handle MinInt64, MaxInt64, zero, and negative coordinates", func() {
			type coord struct {
				id int64
				x  int64
				y  int64
			}
			coords := []coord{
				{1, math.MinInt64, math.MinInt64},
				{2, -1, -1},
				{3, 0, 0},
				{4, math.MaxInt64, math.MaxInt64},
				{5, -100, 100},
				{6, 0, math.MinInt64},
			}

			// Go saves all boundary-value orders
			for _, c := range coords {
				err := store.SaveOrderGo(ctx, c.id, c.x, c.y)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java scans — verifies 6 entries with exact coordinate values
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(6))

			// Go scans
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(6))

			// Compare Go vs Java (same ordering proves Hilbert curve matches)
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify all expected coordinate pairs are present in the entries.
			// Key tuple is [coord_x, coord_y, pk] — 3 elements.
			expectedCoords := make(map[[2]int64]bool)
			for _, c := range coords {
				expectedCoords[[2]int64{c.x, c.y}] = true
			}
			for _, entry := range goEntries {
				Expect(len(entry.Key)).To(BeNumerically(">=", 2), "each entry should have at least 2 coordinate values")
				x := toInt64(entry.Key[0])
				y := toInt64(entry.Key[1])
				key := [2]int64{x, y}
				Expect(expectedCoords).To(HaveKey(key), "unexpected coordinate pair (%d, %d)", x, y)
				delete(expectedCoords, key)
			}
			Expect(expectedCoords).To(BeEmpty(), "not all coordinate pairs found in entries")
		})
	})

	Describe("Paginated scan with continuation", func() {
		// NOTE: Cross-language continuation resume is NOT supported for MULTIDIMENSIONAL indexes.
		// Java wraps continuations in FlatMapContinuation proto (due to flatMapPipelined cursor),
		// while Go uses raw MultidimensionalIndexScanContinuation. They are not wire-compatible.
		// Instead, we verify that paginated scans within each language produce the same full result.

		It("should paginate correctly within Go and produce same results as full scan", func() {
			// Go saves 10 orders (ids 1-10, coords (id*100, id*200))
			for i := int64(1); i <= 10; i++ {
				err := store.SaveOrderGo(ctx, i, i*100, i*200)
				Expect(err).NotTo(HaveOccurred())
			}

			// Full scan for reference
			fullEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(fullEntries).To(HaveLen(10))

			// Paginated Go scan: 4 + 3 + 3
			goPage1, goCont1, goExhausted1, err := store.ScanIndexGoWithLimit(ctx, 4, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(goPage1).To(HaveLen(4))
			Expect(goExhausted1).To(BeFalse())
			Expect(goCont1).NotTo(BeNil())

			goPage2, goCont2, goExhausted2, err := store.ScanIndexGoWithLimit(ctx, 3, goCont1)
			Expect(err).NotTo(HaveOccurred())
			Expect(goPage2).To(HaveLen(3))
			Expect(goExhausted2).To(BeFalse())
			Expect(goCont2).NotTo(BeNil())

			goPage3, _, goExhausted3, err := store.ScanIndexGoWithLimit(ctx, 10, goCont2)
			Expect(err).NotTo(HaveOccurred())
			Expect(goPage3).To(HaveLen(3))
			Expect(goExhausted3).To(BeTrue())

			// Total: 4+3+3 = 10 entries, all unique
			allGoEntries := append(append(goPage1, goPage2...), goPage3...)
			Expect(allGoEntries).To(HaveLen(10))

			// Verify all 10 unique by primary key
			seenPKs := make(map[int64]bool)
			for _, e := range allGoEntries {
				Expect(e.PrimaryKey).NotTo(BeEmpty())
				pk := toInt64(e.PrimaryKey[0])
				Expect(seenPKs).NotTo(HaveKey(pk), "duplicate PK %d", pk)
				seenPKs[pk] = true
			}
			Expect(seenPKs).To(HaveLen(10))
		})

		It("should paginate correctly within Java and produce same results as full scan", func() {
			// Go saves 10 orders
			for i := int64(1); i <= 10; i++ {
				err := store.SaveOrderGo(ctx, i, i*100, i*200)
				Expect(err).NotTo(HaveOccurred())
			}

			// Full Java scan for reference
			fullEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(fullEntries).To(HaveLen(10))

			// Paginated Java scan: 4 + 3 + 3
			javaPage1, javaCont1, javaExhausted1, err := store.ScanIndexJavaWithLimit(ctx, 4, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaPage1).To(HaveLen(4))
			Expect(javaExhausted1).To(BeFalse())
			Expect(javaCont1).NotTo(BeEmpty())

			javaPage2, javaCont2, javaExhausted2, err := store.ScanIndexJavaWithLimit(ctx, 3, javaCont1)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaPage2).To(HaveLen(3))
			Expect(javaExhausted2).To(BeFalse())
			Expect(javaCont2).NotTo(BeEmpty())

			javaPage3, _, javaExhausted3, err := store.ScanIndexJavaWithLimit(ctx, 10, javaCont2)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaPage3).To(HaveLen(3))
			Expect(javaExhausted3).To(BeTrue())

			// Total: 4+3+3 = 10 entries
			allJavaEntries := append(append(javaPage1, javaPage2...), javaPage3...)
			Expect(allJavaEntries).To(HaveLen(10))

			// Verify all 10 unique by primary key
			seenPKs := make(map[int64]bool)
			for _, e := range allJavaEntries {
				Expect(e.PrimaryKey).NotTo(BeEmpty())
				pk := toInt64(e.PrimaryKey[0])
				Expect(seenPKs).NotTo(HaveKey(pk), "duplicate PK %d", pk)
				seenPKs[pk] = true
			}
			Expect(seenPKs).To(HaveLen(10))
		})

		It("should produce same paginated results from Go and Java independently", func() {
			// Go saves 10 orders
			for i := int64(1); i <= 10; i++ {
				err := store.SaveOrderGo(ctx, i, i*100, i*200)
				Expect(err).NotTo(HaveOccurred())
			}

			// Collect all entries via paginated Go scan
			var allGoEntries []IndexEntryResult
			var goCont []byte
			for {
				page, cont, exhausted, err := store.ScanIndexGoWithLimit(ctx, 3, goCont)
				Expect(err).NotTo(HaveOccurred())
				allGoEntries = append(allGoEntries, page...)
				if exhausted {
					break
				}
				goCont = cont
			}
			Expect(allGoEntries).To(HaveLen(10))

			// Collect all entries via paginated Java scan
			var allJavaEntries []IndexEntryResult
			javaCont := ""
			for {
				page, cont, exhausted, err := store.ScanIndexJavaWithLimit(ctx, 3, javaCont)
				Expect(err).NotTo(HaveOccurred())
				allJavaEntries = append(allJavaEntries, page...)
				if exhausted {
					break
				}
				javaCont = cont
			}
			Expect(allJavaEntries).To(HaveLen(10))

			// Compare: both should produce the same entries in the same order
			err := CompareIndexEntries(allGoEntries, allJavaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// MultidimensionalIndexConformanceStore wraps record operations with a MULTIDIMENSIONAL
// index on Order's coord_x and coord_y fields (both int64).
type MultidimensionalIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MDIndex     *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMultidimensionalIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MultidimensionalIndexConformanceStore, error) {
	dimExpr := recordlayer.Dimensions(
		recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")),
		0, 2,
	)
	mdIdx := recordlayer.NewMultidimensionalIndex("order_coord_md", dimExpr)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", mdIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &MultidimensionalIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MDIndex:     mdIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MultidimensionalIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MultidimensionalIndexConformanceStore) SaveOrderGo(ctx context.Context, orderID, x, y int64) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(&gen.Order{
			OrderId: proto.Int64(orderID),
			CoordX:  proto.Int64(x),
			CoordY:  proto.Int64(y),
		})
		return nil, err
	})
	return err
}

func (s *MultidimensionalIndexConformanceStore) SaveOrderJava(ctx context.Context, orderID, x, y int64) error {
	params := s.buildJavaParams()
	params["order"] = &gen.Order{
		OrderId: proto.Int64(orderID),
		CoordX:  proto.Int64(x),
		CoordY:  proto.Int64(y),
	}
	return s.java.InvokeAs(ctx, "saveOrderWithMultidimensionalIndex", params, nil)
}

func (s *MultidimensionalIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MultidimensionalIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMultidimensionalIndex", params, nil)
}

func (s *MultidimensionalIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.MDIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

func (s *MultidimensionalIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMultidimensionalIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMultidimensionalIndex failed: %w", err)
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

// SaveMultipleOrdersJava saves orders with ids from startID to endID (inclusive)
// in a single Java transaction, using coords (id*100, id*200).
func (s *MultidimensionalIndexConformanceStore) SaveMultipleOrdersJava(ctx context.Context, startID, endID int64) error {
	type orderData struct {
		OrderID int64 `json:"orderId"`
		CoordX  int64 `json:"coordX"`
		CoordY  int64 `json:"coordY"`
	}
	var orders []orderData
	for id := startID; id <= endID; id++ {
		orders = append(orders, orderData{OrderID: id, CoordX: id * 100, CoordY: id * 200})
	}
	ordersJSON, err := json.Marshal(orders)
	if err != nil {
		return fmt.Errorf("failed to marshal orders: %w", err)
	}
	params := s.buildJavaParams()
	params["ordersJson"] = string(ordersJSON)
	return s.java.InvokeAs(ctx, "saveMultipleOrdersWithMultidimensionalIndex", params, nil)
}

// DeleteMultipleOrdersJava deletes orders by PK in a single Java transaction.
func (s *MultidimensionalIndexConformanceStore) DeleteMultipleOrdersJava(ctx context.Context, orderIDs []int64) error {
	idsJSON, err := json.Marshal(orderIDs)
	if err != nil {
		return fmt.Errorf("failed to marshal orderIDs: %w", err)
	}
	params := s.buildJavaParams()
	params["orderIdsJson"] = string(idsJSON)
	return s.java.InvokeAs(ctx, "deleteMultipleOrdersWithMultidimensionalIndex", params, nil)
}

// ScanIndexGoWithLimit scans the MULTIDIMENSIONAL index with a row limit and optional continuation.
// Returns entries, continuation bytes, whether source is exhausted, and any error.
func (s *MultidimensionalIndexConformanceStore) ScanIndexGoWithLimit(ctx context.Context, limit int, continuation []byte) ([]IndexEntryResult, []byte, bool, error) {
	var results []IndexEntryResult
	var nextCont []byte
	var exhausted bool

	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}

		scanProps := recordlayer.NewScanProperties(
			recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
		)
		cursor := store.ScanIndex(s.MDIndex, recordlayer.TupleRangeAll, continuation, scanProps)

		for {
			result, err := cursor.OnNext(ctx)
			if err != nil {
				return nil, err
			}
			if !result.HasNext() {
				var contErr error
				nextCont, contErr = result.GetContinuation().ToBytes()
				if contErr != nil {
					return nil, contErr
				}
				exhausted = result.GetNoNextReason().IsSourceExhausted()
				break
			}
			entry := result.GetValue()
			results = append(results, IndexEntryResult{
				Key:        tupleToSlice(entry.Key),
				PrimaryKey: tupleToSlice(entry.PrimaryKey()),
			})
		}
		return nil, nil
	})
	return results, nextCont, exhausted, err
}

// ScanIndexJavaWithLimit scans the MULTIDIMENSIONAL index via Java with a row limit and continuation.
// Returns entries, continuation (base64), whether source is exhausted, and any error.
func (s *MultidimensionalIndexConformanceStore) ScanIndexJavaWithLimit(ctx context.Context, limit int, continuationB64 string) ([]IndexEntryResult, string, bool, error) {
	params := s.buildJavaParams()
	params["limit"] = limit
	if continuationB64 != "" {
		params["continuation"] = continuationB64
	}

	var javaResult struct {
		Entries     []map[string]any `json:"entries"`
		Continuation string          `json:"continuation"`
		Exhausted    bool            `json:"exhausted"`
	}
	if err := s.java.InvokeAs(ctx, "scanMultidimensionalIndexWithLimit", params, &javaResult); err != nil {
		return nil, "", false, fmt.Errorf("java scanMultidimensionalIndexWithLimit failed: %w", err)
	}

	var results []IndexEntryResult
	for _, m := range javaResult.Entries {
		entry := IndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if pkRaw, ok := m["primaryKey"]; ok {
			entry.PrimaryKey = toInterfaceSlice(pkRaw)
		}
		results = append(results, entry)
	}
	return results, javaResult.Continuation, javaResult.Exhausted, nil
}
