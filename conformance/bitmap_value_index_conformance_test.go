//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
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

var _ = Describe("BITMAP_VALUE Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *BitmapValueConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("bmp_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewBitmapValueConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan BITMAP_VALUE index", func() {
		It("should produce identical bitmap entries visible to both Go and Java", func() {
			// Save 3 orders in price=100 group at positions 3, 7, 20
			for _, id := range []int64{3, 7, 20} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(100),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan by group with Go
			goEntries, err := store.ScanBitmapByGroupGo(ctx, int64(100))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2)) // Two aligned blocks: 0 and 16

			// Scan by group with Java
			javaEntries, err := store.ScanBitmapByGroupJava(ctx, int64(100))
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = CompareBitmapEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify first block (aligned 0): bits 3 and 7 set
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(0)))
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 3)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 7)).To(BeTrue())

			// Verify second block (aligned 16): bit 4 set (position 20 - 16 = 4)
			Expect(toInt64(goEntries[1].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[1].Key[1])).To(Equal(int64(16)))
			Expect(hasBitmapBit(goEntries[1].BitmapHex, 4)).To(BeTrue())
		})
	})

	Describe("Java writes, both scan BITMAP_VALUE index", func() {
		It("should produce identical bitmap entries visible to both Go and Java", func() {
			// Java saves 3 orders in price=200 group at positions 1, 5, 10
			for _, id := range []int64{1, 5, 10} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(200),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan by group with Go
			goEntries, err := store.ScanBitmapByGroupGo(ctx, int64(200))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1)) // All positions < 16, one block

			// Scan by group with Java
			javaEntries, err := store.ScanBitmapByGroupJava(ctx, int64(200))
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Compare
			err = CompareBitmapEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify bits 1, 5, 10 are set
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 1)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 5)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 10)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 0)).To(BeFalse())
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined bitmaps", func() {
			// Go saves positions 2 and 8 in price=300
			for _, id := range []int64{2, 8} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(300),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Java saves positions 4 and 12 in price=300
			for _, id := range []int64{4, 12} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(300),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Both should see all 4 bits in one block
			goEntries, err := store.ScanBitmapByGroupGo(ctx, int64(300))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanBitmapByGroupJava(ctx, int64(300))
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareBitmapEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// All 4 bits set
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 2)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 4)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 8)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 12)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 0)).To(BeFalse())
		})
	})

	Describe("Go deletes Java-written record", func() {
		It("should clear the bit in both Go and Java scans", func() {
			// Java saves positions 3, 7, 11 in price=400
			for _, id := range []int64{3, 7, 11} {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(400),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify all 3 bits set
			goEntries, err := store.ScanBitmapByGroupGo(ctx, int64(400))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 3)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 7)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 11)).To(BeTrue())

			// Go deletes record at position 7
			deleted, err := store.DeleteOrderGo(ctx, 7)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see bits 3 and 11 only
			goEntries, err = store.ScanBitmapByGroupGo(ctx, int64(400))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 3)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 7)).To(BeFalse())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 11)).To(BeTrue())

			javaEntries, err := store.ScanBitmapByGroupJava(ctx, int64(400))
			Expect(err).NotTo(HaveOccurred())

			err = CompareBitmapEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java deletes Go-written record", func() {
		It("should clear the bit in both Go and Java scans", func() {
			// Go saves positions 1, 6, 14 in price=500
			for _, id := range []int64{1, 6, 14} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(500),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify all 3 bits set via Java
			javaEntries, err := store.ScanBitmapByGroupJava(ctx, int64(500))
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(hasBitmapBit(javaEntries[0].BitmapHex, 1)).To(BeTrue())
			Expect(hasBitmapBit(javaEntries[0].BitmapHex, 6)).To(BeTrue())
			Expect(hasBitmapBit(javaEntries[0].BitmapHex, 14)).To(BeTrue())

			// Java deletes record at position 6
			err = store.DeleteOrderJava(ctx, 6)
			Expect(err).NotTo(HaveOccurred())

			// Both should see bits 1 and 14 only
			goEntries, err := store.ScanBitmapByGroupGo(ctx, int64(500))
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 1)).To(BeTrue())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 6)).To(BeFalse())
			Expect(hasBitmapBit(goEntries[0].BitmapHex, 14)).To(BeTrue())

			javaEntries, err = store.ScanBitmapByGroupJava(ctx, int64(500))
			Expect(err).NotTo(HaveOccurred())

			err = CompareBitmapEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Aggregate cross-validation", func() {
		It("should produce identical aggregate bitmaps from Go and Java", func() {
			// Go saves positions 3, 7, 20 in price=600 (spans two aligned blocks)
			for _, id := range []int64{3, 7, 20} {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(id),
					Price:   proto.Int32(600),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Aggregate with Go
			goHex, err := store.AggregateGo(ctx, int64(600))
			Expect(err).NotTo(HaveOccurred())
			Expect(goHex).NotTo(BeEmpty())

			// Aggregate with Java
			javaHex, err := store.AggregateJava(ctx, int64(600))
			Expect(err).NotTo(HaveOccurred())
			Expect(javaHex).NotTo(BeEmpty())

			// Compare hex representations
			Expect(goHex).To(Equal(javaHex), "Go aggregate bitmap_hex=%s, Java aggregate bitmap_hex=%s", goHex, javaHex)

			// Verify bits 3, 7, 20 are set in aggregate
			Expect(hasBitmapBit(goHex, 3)).To(BeTrue())
			Expect(hasBitmapBit(goHex, 7)).To(BeTrue())
			Expect(hasBitmapBit(goHex, 20)).To(BeTrue())
			Expect(hasBitmapBit(goHex, 0)).To(BeFalse())
			Expect(hasBitmapBit(goHex, 15)).To(BeFalse())
		})
	})
})

// BitmapEntry represents a single BITMAP_VALUE index entry for comparison.
type BitmapEntry struct {
	Key       []any  // Tuple key (e.g., [price, alignedPosition])
	BitmapHex string // Lowercase hex encoding of raw bitmap bytes
}

// BitmapValueConformanceStore wraps record operations with a BITMAP_VALUE index.
// Index: GroupBy(Field("order_id"), Field("price")) with entrySize=16.
// This means price is the grouping key, order_id is the position column.
type BitmapValueConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	BitmapIndex *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewBitmapValueConformanceStore creates a conformance store with a BITMAP_VALUE index.
// The index definition must match the Java side's createBitmapValueMetaData() exactly.
func NewBitmapValueConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*BitmapValueConformanceStore, error) {
	// GroupBy(Field("order_id"), Field("price")) produces:
	//   wholeKey = Concat(price, order_id), groupedCount = 1
	// Matches Java's: new GroupingKeyExpression(concatenateFields("price", "order_id"), 1)
	bitmapIdx := recordlayer.NewBitmapValueIndex("Order$bitmapByPrice",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price")))
	bitmapIdx.Options[recordlayer.IndexOptionBitmapValueEntrySize] = "16"

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", bitmapIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &BitmapValueConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		BitmapIndex: bitmapIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *BitmapValueConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order via Go (with BITMAP_VALUE index maintenance).
func (s *BitmapValueConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with BITMAP_VALUE index maintenance).
func (s *BitmapValueConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveBitmapValueRecord", params, nil)
}

// DeleteOrderGo deletes an order via Go (with BITMAP_VALUE index maintenance).
func (s *BitmapValueConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with BITMAP_VALUE index maintenance).
func (s *BitmapValueConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteBitmapValueRecord", params, nil)
}

// ScanBitmapGo scans all BITMAP_VALUE index entries via Go.
func (s *BitmapValueConformanceStore) ScanBitmapGo(ctx context.Context) ([]BitmapEntry, error) {
	var results []BitmapEntry
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(
			s.BitmapIndex, recordlayer.IndexScanByGroup, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			bitmapBytes := extractBitmapBytes(e.Value)
			results = append(results, BitmapEntry{
				Key:       tupleToSlice(e.Key),
				BitmapHex: hex.EncodeToString(bitmapBytes),
			})
		}
		return nil, nil
	})
	return results, err
}

// ScanBitmapJava scans all BITMAP_VALUE index entries via Java.
func (s *BitmapValueConformanceStore) ScanBitmapJava(ctx context.Context) ([]BitmapEntry, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanBitmapValueIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanBitmapValueIndex failed: %w", err)
	}

	return parseBitmapJavaResults(javaResults), nil
}

// ScanBitmapByGroupGo scans BITMAP_VALUE index entries for a specific group key via Go.
func (s *BitmapValueConformanceStore) ScanBitmapByGroupGo(ctx context.Context, groupKey int64) ([]BitmapEntry, error) {
	var results []BitmapEntry
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		scanRange := recordlayer.TupleRangeAllOf(tuple.Tuple{groupKey})
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(
			s.BitmapIndex, recordlayer.IndexScanByGroup, scanRange, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			bitmapBytes := extractBitmapBytes(e.Value)
			results = append(results, BitmapEntry{
				Key:       tupleToSlice(e.Key),
				BitmapHex: hex.EncodeToString(bitmapBytes),
			})
		}
		return nil, nil
	})
	return results, err
}

// ScanBitmapByGroupJava scans BITMAP_VALUE index entries for a specific group key via Java.
func (s *BitmapValueConformanceStore) ScanBitmapByGroupJava(ctx context.Context, groupKey int64) ([]BitmapEntry, error) {
	params := s.buildJavaParams()
	params["groupKey"] = groupKey

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanBitmapValueIndexByGroup", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanBitmapValueIndexByGroup failed: %w", err)
	}

	return parseBitmapJavaResults(javaResults), nil
}

// AggregateGo evaluates the BITMAP_VALUE aggregate function via Go.
func (s *BitmapValueConformanceStore) AggregateGo(ctx context.Context, groupKey int64) (string, error) {
	var bitmapHex string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameBitmapValue,
				Operand: recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price")),
			},
			recordlayer.TupleRangeAllOf(tuple.Tuple{groupKey}), recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		if result == nil || len(result) == 0 {
			return nil, nil
		}
		bitmapBytes, ok := result[0].([]byte)
		if !ok {
			return nil, fmt.Errorf("expected []byte from aggregate, got %T", result[0])
		}
		bitmapHex = hex.EncodeToString(bitmapBytes)
		return nil, nil
	})
	return bitmapHex, err
}

// AggregateJava evaluates the BITMAP_VALUE aggregate function via Java.
func (s *BitmapValueConformanceStore) AggregateJava(ctx context.Context, groupKey int64) (string, error) {
	params := s.buildJavaParams()
	params["groupKey"] = groupKey

	var javaResult map[string]any
	if err := s.java.InvokeAs(ctx, "evaluateBitmapValueAggregate", params, &javaResult); err != nil {
		return "", fmt.Errorf("java evaluateBitmapValueAggregate failed: %w", err)
	}

	bitmapHex, ok := javaResult["bitmap_hex"].(string)
	if !ok {
		return "", fmt.Errorf("expected bitmap_hex string in Java result, got %T", javaResult["bitmap_hex"])
	}
	return bitmapHex, nil
}

// extractBitmapBytes extracts the raw bitmap byte slice from an IndexEntry's Value tuple.
func extractBitmapBytes(value tuple.Tuple) []byte {
	if len(value) == 0 {
		return nil
	}
	if b, ok := value[0].([]byte); ok {
		return b
	}
	return nil
}

// parseBitmapJavaResults converts Java JSON results to BitmapEntry slices.
func parseBitmapJavaResults(javaResults []map[string]any) []BitmapEntry {
	var results []BitmapEntry
	for _, m := range javaResults {
		entry := BitmapEntry{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if hexRaw, ok := m["bitmap_hex"]; ok {
			entry.BitmapHex, _ = hexRaw.(string)
		}
		results = append(results, entry)
	}
	return results
}

// CompareBitmapEntries compares Go and Java BITMAP_VALUE index scan results.
func CompareBitmapEntries(goEntries, javaEntries []BitmapEntry) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
		if goEntries[i].BitmapHex != javaEntries[i].BitmapHex {
			return fmt.Errorf("entry %d bitmap mismatch: go=%s java=%s",
				i, goEntries[i].BitmapHex, javaEntries[i].BitmapHex)
		}
	}
	return nil
}

// hasBitmapBit checks whether the bit at the given position is set in a hex-encoded bitmap.
func hasBitmapBit(bitmapHex string, position int) bool {
	bitmapBytes, err := hex.DecodeString(bitmapHex)
	if err != nil || len(bitmapBytes) == 0 {
		return false
	}
	byteIdx := position / 8
	if byteIdx >= len(bitmapBytes) {
		return false
	}
	return (bitmapBytes[byteIdx] & (1 << (position % 8))) != 0
}
