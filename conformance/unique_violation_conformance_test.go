//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// ViolationResult represents a uniqueness violation entry for cross-language comparison.
type ViolationResult struct {
	IndexKey    []any
	PrimaryKey  []any
	ExistingKey []any // may be nil
}

// UniqueViolationConformanceStore wraps record operations with a unique VALUE index
// on Order.price for cross-language uniqueness violation testing.
type UniqueViolationConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	UniqueIndex *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewUniqueViolationConformanceStore creates a conformance store with a unique VALUE
// index on Order.price. The index definition must match the Java side exactly.
func NewUniqueViolationConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*UniqueViolationConformanceStore, error) {
	uniqueIdx := recordlayer.NewIndex("Order$price_unique", recordlayer.Field("price"))
	uniqueIdx.SetUnique()

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", uniqueIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &UniqueViolationConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		UniqueIndex: uniqueIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *UniqueViolationConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrderGo saves an order with Go (with unique index maintenance).
func (s *UniqueViolationConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

// DeleteOrderGo deletes an order with Go (with unique index maintenance).
func (s *UniqueViolationConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

// ScanUniqueIndexGo scans the unique index using Go and returns results.
func (s *UniqueViolationConformanceStore) ScanUniqueIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.UniqueIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanUniquenessViolationsGo scans uniqueness violations from subspace 7 using Go.
func (s *UniqueViolationConformanceStore) ScanUniquenessViolationsGo(ctx context.Context) ([]ViolationResult, error) {
	var results []ViolationResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		violations, err := store.ScanUniquenessViolations(s.UniqueIndex)
		if err != nil {
			return nil, err
		}
		for _, v := range violations {
			vr := ViolationResult{
				IndexKey:   tupleToSlice(v.IndexKey),
				PrimaryKey: tupleToSlice(v.PrimaryKey),
			}
			if v.ExistingKey != nil {
				vr.ExistingKey = tupleToSlice(v.ExistingKey)
			}
			results = append(results, vr)
		}
		return nil, nil
	})
	return results, err
}

// MarkIndexWriteOnlyGo marks the unique index as WRITE_ONLY using Go.
func (s *UniqueViolationConformanceStore) MarkIndexWriteOnlyGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexWriteOnly("Order$price_unique")
		return nil, err
	})
	return err
}

// GetIndexStateGo returns the index state as a string using Go.
func (s *UniqueViolationConformanceStore) GetIndexStateGo(ctx context.Context) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		st := store.GetIndexState("Order$price_unique")
		switch st {
		case recordlayer.IndexStateReadable:
			state = "READABLE"
		case recordlayer.IndexStateWriteOnly:
			state = "WRITE_ONLY"
		case recordlayer.IndexStateDisabled:
			state = "DISABLED"
		case recordlayer.IndexStateReadableUniquePending:
			state = "READABLE_UNIQUE_PENDING"
		default:
			state = fmt.Sprintf("UNKNOWN(%d)", st)
		}
		return nil, nil
	})
	return state, err
}

// SaveOrderJava saves an order via Java (with unique index maintenance).
func (s *UniqueViolationConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveWithUniqueIndex", params, nil)
}

// DeleteOrderJava deletes an order via Java (with unique index maintenance).
func (s *UniqueViolationConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteWithUniqueIndex", params, nil)
}

// ScanUniqueIndexJava scans the unique index using Java and returns results.
func (s *UniqueViolationConformanceStore) ScanUniqueIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanUniqueIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanUniqueIndex failed: %w", err)
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

// SaveDuplicateJava saves two orders with the same price in a single Java transaction.
// Returns the Java exception if a uniqueness violation is detected.
func (s *UniqueViolationConformanceStore) SaveDuplicateJava(ctx context.Context, order1 *gen.Order, order2 *gen.Order) error {
	params := s.buildJavaParams()
	params["order1"] = order1
	params["order2"] = order2
	return s.java.InvokeAs(ctx, "saveDuplicateWithUniqueIndex", params, nil)
}

// MarkIndexWriteOnlyJava marks the unique index as WRITE_ONLY using Java.
func (s *UniqueViolationConformanceStore) MarkIndexWriteOnlyJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "markUniqueIndexWriteOnly", params, nil)
}

// SaveDuringWriteOnlyJava saves an order via Java when the index is in WRITE_ONLY state.
func (s *UniqueViolationConformanceStore) SaveDuringWriteOnlyJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveWithUniqueIndexDuringWriteOnly", params, nil)
}

// ScanUniquenessViolationsJava scans uniqueness violations from subspace 7 using Java.
func (s *UniqueViolationConformanceStore) ScanUniquenessViolationsJava(ctx context.Context) ([]ViolationResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanUniquenessViolations", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanUniquenessViolations failed: %w", err)
	}

	var results []ViolationResult
	for _, m := range javaResults {
		vr := ViolationResult{}
		if keyRaw, ok := m["indexKey"]; ok {
			vr.IndexKey = toInterfaceSlice(keyRaw)
		}
		if pkRaw, ok := m["primaryKey"]; ok {
			vr.PrimaryKey = toInterfaceSlice(pkRaw)
		}
		if existingRaw, ok := m["existingKey"]; ok && existingRaw != nil {
			vr.ExistingKey = toInterfaceSlice(existingRaw)
		}
		results = append(results, vr)
	}
	return results, nil
}

// GetIndexStateJava returns the index state as a string using Java.
func (s *UniqueViolationConformanceStore) GetIndexStateJava(ctx context.Context) (string, error) {
	params := s.buildJavaParams()

	var state string
	if err := s.java.InvokeAs(ctx, "getUniqueIndexState", params, &state); err != nil {
		return "", fmt.Errorf("java getUniqueIndexState failed: %w", err)
	}
	return state, nil
}

// CompareViolations compares Go and Java violation scan results.
// Compares by indexKey and primaryKey. ExistingKey is informational and compared
// only when both sides have non-nil values.
func CompareViolations(goViolations, javaViolations []ViolationResult) error {
	if len(goViolations) != len(javaViolations) {
		return fmt.Errorf("violation count mismatch: go=%d java=%d", len(goViolations), len(javaViolations))
	}
	for i := range goViolations {
		if !sliceEqualNormalized(goViolations[i].IndexKey, javaViolations[i].IndexKey) {
			return fmt.Errorf("violation %d indexKey mismatch: go=%v java=%v",
				i, goViolations[i].IndexKey, javaViolations[i].IndexKey)
		}
		if !sliceEqualNormalized(goViolations[i].PrimaryKey, javaViolations[i].PrimaryKey) {
			return fmt.Errorf("violation %d primaryKey mismatch: go=%v java=%v",
				i, goViolations[i].PrimaryKey, javaViolations[i].PrimaryKey)
		}
		// Compare existingKey only when both are non-nil
		if goViolations[i].ExistingKey != nil && javaViolations[i].ExistingKey != nil {
			if !sliceEqualNormalized(goViolations[i].ExistingKey, javaViolations[i].ExistingKey) {
				return fmt.Errorf("violation %d existingKey mismatch: go=%v java=%v",
					i, goViolations[i].ExistingKey, javaViolations[i].ExistingKey)
			}
		}
	}
	return nil
}

var _ = Describe("Unique Violation Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *UniqueViolationConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("uniq_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewUniqueViolationConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Cross-language unique violation detection (READABLE)", func() {
		It("Go saves record, Java detects unique violation", func() {
			// Go saves Order(id=1, price=500)
			order1 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			}
			err := store.SaveOrderGo(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Java saves Order(id=2, price=500) in a separate tx -- should throw
			// RecordIndexUniquenessViolation because price=500 entry already exists.
			order2 := &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			}
			javaErr := store.SaveOrderJava(ctx, order2)
			expectJavaException(javaErr, "RecordIndexUniquenessViolation")
		})

		It("Java saves record, Go detects unique violation", func() {
			// Java saves Order(id=1, price=500)
			order1 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			}
			err := store.SaveOrderJava(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Go saves Order(id=2, price=500) -- should throw RecordIndexUniquenessViolationError
			order2 := &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			}
			goErr := store.SaveOrderGo(ctx, order2)
			Expect(goErr).To(HaveOccurred())
			var violationErr *recordlayer.RecordIndexUniquenessViolationError
			Expect(errors.As(goErr, &violationErr)).To(BeTrue(),
				"expected RecordIndexUniquenessViolationError, got: %v", goErr)
			Expect(violationErr.IndexName).To(Equal("Order$price_unique"))
		})
	})

	Describe("Unique index entries cross-language", func() {
		It("Go writes, both scan unique index", func() {
			// Go saves 3 orders with distinct prices
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go scans index
			goEntries, err := store.ScanUniqueIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Java scans index
			javaEntries, err := store.ScanUniqueIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify sort order: ascending by price (100, 200, 300)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				expectedPK := int64(i + 1)
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
				Expect(toInt64(entry.PrimaryKey[0])).To(Equal(expectedPK))
			}
		})

		It("Java writes, both scan unique index", func() {
			// Java saves 3 orders with distinct prices
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 200)),
				}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go scans
			goEntries, err := store.ScanUniqueIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Java scans
			javaEntries, err := store.ScanUniqueIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify sort order: ascending by price (200, 400, 600)
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 200)
				Expect(toInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("WRITE_ONLY violation wire format", func() {
		It("Go creates violations during WRITE_ONLY, Java reads them", func() {
			// Go creates store and marks index WRITE_ONLY
			err := store.MarkIndexWriteOnlyGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify state
			state, err := store.GetIndexStateGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(Equal("WRITE_ONLY"))

			// Go saves Order(id=1, price=500) -- no violation yet
			order1 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			}
			err = store.SaveOrderGo(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Go saves Order(id=2, price=500) -- violation stored, NOT thrown
			order2 := &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			}
			err = store.SaveOrderGo(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Go scans violations -- should find entries for both PKs
			goViolations, err := store.ScanUniquenessViolationsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goViolations).To(HaveLen(2), "expected 2 violation entries (one per conflicting PK)")

			// Java scans violations -- should see the same wire format
			javaViolations, err := store.ScanUniquenessViolationsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaViolations).To(HaveLen(2))

			// Compare
			err = CompareViolations(goViolations, javaViolations)
			Expect(err).NotTo(HaveOccurred())

			// Both violations should reference price=500
			for _, v := range goViolations {
				Expect(v.IndexKey).To(HaveLen(1))
				Expect(toInt64(v.IndexKey[0])).To(Equal(int64(500)))
			}
		})

		It("Java creates violations during WRITE_ONLY, Go reads them", func() {
			// Go creates store (to set up metadata), then marks index WRITE_ONLY
			err := store.MarkIndexWriteOnlyGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java saves Order(id=1, price=500)
			order1 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(500),
			}
			err = store.SaveDuringWriteOnlyJava(ctx, order1)
			Expect(err).NotTo(HaveOccurred())

			// Java saves Order(id=2, price=500) -- violation stored
			order2 := &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(500),
			}
			err = store.SaveDuringWriteOnlyJava(ctx, order2)
			Expect(err).NotTo(HaveOccurred())

			// Go scans violations
			goViolations, err := store.ScanUniquenessViolationsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goViolations).To(HaveLen(2))

			// Java scans violations
			javaViolations, err := store.ScanUniquenessViolationsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaViolations).To(HaveLen(2))

			// Compare
			err = CompareViolations(goViolations, javaViolations)
			Expect(err).NotTo(HaveOccurred())

			// Both violations should reference price=500
			for _, v := range goViolations {
				Expect(v.IndexKey).To(HaveLen(1))
				Expect(toInt64(v.IndexKey[0])).To(Equal(int64(500)))
			}
		})
	})
})
