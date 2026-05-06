package conformance_test

import (
	"context"
	"fmt"
	"math"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Index Entry Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *IndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idx_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with index, Java scans index", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			// Save 5 orders with Go — each gets an index entry on price
			for i := int64(1); i <= 5; i++ {
				order := NewOrder(i).WithPrice(int32(i*100)).WithFlower("Rose", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			// Scan index with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Compare: entries should be identical
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify ordering: ascending by price (100, 200, 300, 400, 500)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				expectedPK := int64(i + 1)
				Expect(entry.PrimaryKey).To(HaveLen(1))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
				// Key = (price, pk)
				Expect(entry.Key).To(HaveLen(2))
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Java writes with index, Go scans index", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			// Save 3 orders with Java
			for i := int64(1); i <= 3; i++ {
				order := NewOrder(i).WithPrice(int32(i*200)).WithFlower("Tulip", gen.Color_BLUE).Build()
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

			// Compare
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify ordering: ascending by price (200, 400, 600)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 200)
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Delete removes index entry", func() {
		It("should remove the index entry when Go deletes a record", func() {
			// Save with Go
			order := NewOrder(42).WithPrice(999).WithFlower("Orchid", gen.Color_PINK).Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify index has 1 entry (Java sees it)
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// Delete with Go
			deleted, err := store.DeleteOrderGo(ctx, 42)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify index is empty (Java sees no entries)
			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())

			// Go also sees no entries
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())
		})
	})

	Describe("Update changes index entry", func() {
		It("should update the index entry when price changes", func() {
			// Save with Go, price=100
			order := NewOrder(77).WithPrice(100).WithFlower("Daisy", gen.Color_YELLOW).Build()
			err := store.SaveOrderGo(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			// Verify Java sees price=100 in index
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(100)))

			// Update with Go, price=500
			order2 := NewOrder(77).WithPrice(500).WithFlower("Daisy", gen.Color_YELLOW).Build()
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Verify Java sees price=500 (old entry removed, new entry added)
			javaEntries, err = store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))
			Expect(toInt64(javaEntries[0].Key[0])).To(Equal(int64(500)))

			// Go also sees price=500
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(500)))

			// Cross-validate
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Multiple records sorted by price", func() {
		It("should produce identically sorted entries in both Go and Java", func() {
			// Insert orders with non-sequential prices to verify sort order
			prices := []int32{500, 100, 300, 200, 400}
			for i, price := range prices {
				order := NewOrder(int64(i+1)).WithPrice(price).WithFlower("Mix", gen.Color_RED).Build()
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Both should be sorted by price ascending: 100, 200, 300, 400, 500
			expectedPrices := []int64{100, 200, 300, 400, 500}
			// PK order for these prices: 2(100), 4(200), 3(300), 5(400), 1(500)
			expectedPKs := []int64{2, 4, 3, 5, 1}

			for i := range goEntries {
				Expect(toInt64(goEntries[i].Key[0])).To(Equal(expectedPrices[i]),
					"Go entry %d price mismatch", i)
				Expect(toInt64(goEntries[i].PrimaryKey[0])).To(Equal(expectedPKs[i]),
					"Go entry %d PK mismatch", i)
			}

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// toInt64 normalizes numeric values to int64 for comparison.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	default:
		return -1
	}
}

// IndexConformanceStore wraps record operations with a VALUE index on Order.price
// and provides methods to cross-validate index entries between Go and Java.
type IndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	PriceIndex  *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// IndexEntryResult represents a single index entry for comparison.
type IndexEntryResult struct {
	Key        []any // Full key tuple (indexed values + primary key)
	PrimaryKey []any // Primary key extracted from the entry
	Value      []any // Value tuple (non-empty for covering indexes with KeyWithValueExpression)
}

// NewIndexConformanceStore creates a conformance store with a VALUE index on Order.price.
// The index definition must match the Java side's createIndexedMetaData() exactly.
func NewIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*IndexConformanceStore, error) {
	priceIndex := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", priceIndex)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &IndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		PriceIndex:  priceIndex,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

// buildJavaParams builds base parameters for Java invocations.
func (s *IndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with index maintenance).
func (s *IndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// SaveOrderJava saves an order via Java (with index maintenance).
func (s *IndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)
}

// DeleteOrderGo deletes an order with Go (with index maintenance).
func (s *IndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// DeleteOrderJava deletes an order via Java (with index maintenance).
func (s *IndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithIndex", params, nil)
}

// ScanIndexGo scans the price index using Go and returns results.
func (s *IndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.PriceIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexJava scans the price index using Java and returns results.
func (s *IndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$price"

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanIndex failed: %w", err)
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
func (s *IndexConformanceStore) LoadOrderGo(ctx context.Context, orderID int64) (*gen.Order, error) {
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
func (s *IndexConformanceStore) LoadOrderJava(ctx context.Context, orderID int64) (*gen.Order, error) {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	var order gen.Order
	if err := s.java.InvokeAs(ctx, "loadOrderWithIndex", params, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

// CompareIndexEntries compares Go and Java index scan results.
// Returns nil if they match, an error describing the mismatch otherwise.
func CompareIndexEntries(goEntries, javaEntries []IndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !entriesEqual(goEntries[i], javaEntries[i]) {
			return fmt.Errorf("entry %d mismatch:\n  go:   key=%v pk=%v\n  java: key=%v pk=%v",
				i, goEntries[i].Key, goEntries[i].PrimaryKey,
				javaEntries[i].Key, javaEntries[i].PrimaryKey)
		}
	}
	return nil
}

func entriesEqual(a, b IndexEntryResult) bool {
	return sliceEqualNormalized(a.Key, b.Key) && sliceEqualNormalized(a.PrimaryKey, b.PrimaryKey)
}

// sliceEqualNormalized compares two slices, normalizing numbers to int64 for comparison.
// Java sends numbers as float64 through JSON; Go uses int64 from FDB tuples.
func sliceEqualNormalized(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !normalizedEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func normalizedEqual(a, b any) bool {
	// Handle nil comparison (e.g., nil bytes from proto2 optional fields)
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Handle string comparison (e.g., fan-out index on string fields)
	aStr, aIsStr := a.(string)
	bStr, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return aStr == bStr
	}
	if aIsStr != bIsStr {
		return false
	}

	// Handle boolean comparison
	aBool, aIsBool := a.(bool)
	bBool, bIsBool := b.(bool)
	if aIsBool && bIsBool {
		return aBool == bBool
	}
	if aIsBool || bIsBool {
		return false
	}

	// Handle byte slice comparison (Go: []byte, Java via JSON: []any of float64)
	aBytes, aIsBytes := a.([]byte)
	if aIsBytes {
		return bytesEqualJSON(aBytes, b)
	}
	bBytes, bIsBytes := b.([]byte)
	if bIsBytes {
		return bytesEqualJSON(bBytes, a)
	}

	// Numeric comparison: normalize to float64 to handle both float32↔float64
	// and int64↔float64 (JSON always sends numbers as float64).
	// Using float64 avoids overflow when int64 values lose precision in JSON.
	af, bf := toFloat64(a), toFloat64(b)
	if math.IsNaN(af) || math.IsNaN(bf) {
		return false
	}
	// When one side is float32 (from Go tuple decode) and the other is float64
	// (from Java JSON), compare at float32 precision. Gson serializes Float(3.14)
	// as "3.14" which Go deserializes as float64(3.14), but Go's tuple decoder
	// returns float32(3.14). The float32→float64 promotion differs from parsing
	// "3.14" as float64, causing ~1e-7 relative error that exceeds 1e-9 tolerance.
	_, aIsF32 := a.(float32)
	_, bIsF32 := b.(float32)
	if aIsF32 || bIsF32 {
		return float32(af) == float32(bf)
	}
	if af == bf {
		return true
	}
	diff := math.Abs(af - bf)
	mag := math.Max(math.Abs(af), math.Abs(bf))
	if mag == 0 {
		return diff == 0
	}
	return diff/mag < 1e-9
}

// toFloat64 converts any numeric type to float64 for comparison.
func toFloat64(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	default:
		return math.NaN()
	}
}

// bytesEqualJSON compares a Go []byte against a JSON-deserialized []any of float64.
func bytesEqualJSON(goBytes []byte, jsonVal any) bool {
	jsonArr, ok := jsonVal.([]any)
	if !ok {
		return false
	}
	if len(goBytes) != len(jsonArr) {
		return false
	}
	for i, b := range goBytes {
		jf, ok := jsonArr[i].(float64)
		if !ok || byte(jf) != b {
			return false
		}
	}
	return true
}

func tupleToSlice(t tuple.Tuple) []any {
	s := make([]any, len(t))
	for i, v := range t {
		s[i] = v
	}
	return s
}

func toInterfaceSlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	default:
		return nil
	}
}

// SaveRecord saves a record using Go, then verifies Java can read it and sees the same record.
func (s *IndexConformanceStore) SaveRecord(ctx context.Context, msg proto.Message) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records supported")
	}

	if err := s.SaveOrderGo(ctx, order); err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	// Cross-check: Java reads the record
	javaOrder, err := s.LoadOrderJava(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("java load after go save failed: %w", err)
	}

	goOrder, err := s.LoadOrderGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go load failed: %w", err)
	}

	if !proto.Equal(goOrder, javaOrder) {
		return fmt.Errorf("record mismatch: go=%v java=%v", goOrder, javaOrder)
	}

	return nil
}
