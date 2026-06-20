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

var _ = Describe("Index Rebuild Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *RebuildIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idxrebuild_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewRebuildIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go saves records, Go rebuilds index, Java scans", func() {
		It("should produce index entries readable by Java", func() {
			// Save 5 orders without index using Go
			for i := int64(1); i <= 5; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Go
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(5))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(5))

			// Compare — entries must be byte-identical
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 100, 200, 300, 400, 500
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Go saves records, Java rebuilds index, Go scans", func() {
		It("should produce index entries readable by Go", func() {
			// Save 4 orders without index using Go
			for i := int64(1); i <= 4; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 200)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Java
			err := store.RebuildIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan with Go
			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			// Scan with Java
			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			// Compare
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 200, 400, 600, 800
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 200)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Java saves records, Go rebuilds index, Java scans", func() {
		It("should produce index entries readable by Java", func() {
			// Save 3 orders without index using Java
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 300)),
				}
				err := store.SaveOrderJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index with Go
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

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
		})
	})

	Describe("Auto-rebuild: Go saves with counting metadata, Java auto-rebuilds on createOrOpen", func() {
		It("should auto-rebuild index via checkPossiblyRebuild and produce correct entries", func() {
			// Save 4 orders using Go with counting metadata (no index).
			// Record count is tracked atomically, enabling Java's default
			// UserVersionChecker to see recordCount <= 200 and return READABLE.
			for i := int64(1); i <= 4; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				}
				err := store.SaveOrderForAutoRebuildGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java opens with indexed+counting metadata using DEFAULT checker.
			// checkPossiblyRebuild() detects the metadata version change,
			// sees recordCount=4 <= MAX_RECORDS_FOR_REBUILD(200),
			// returns READABLE, and rebuilds the Order$price index inline.
			javaEntries, err := store.ScanIndexAfterAutoRebuildJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			// Go scans the same index (now rebuilt by Java) using counting metadata
			goEntries, err := store.ScanIndexWithCountingGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			// Both must agree
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 100, 200, 300, 400
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 100)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Auto-rebuild: Go saves with counting metadata, Go auto-rebuilds on CreateOrOpen", func() {
		It("should auto-rebuild index via checkPossiblyRebuild and produce correct entries", func() {
			// Save 3 orders using Go with counting metadata (no index)
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 250)),
				}
				err := store.SaveOrderForAutoRebuildGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go opens with indexed+counting metadata via CreateOrOpen().
			// Go's checkPossiblyRebuild() detects the metadata version change,
			// auto-rebuilds the Order$price index, and returns valid scan results.
			goEntries, err := store.AutoRebuildAndScanGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Java scans the same index (now rebuilt by Go) using counting metadata
			javaEntries, err := store.ScanIndexAfterAutoRebuildJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Both must agree
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify price ordering: 250, 500, 750
			for i, entry := range goEntries {
				expectedPrice := int64((i + 1) * 250)
				Expect(idxToInt64(entry.Key[0])).To(Equal(expectedPrice))
			}
		})
	})

	Describe("Auto-rebuild: Java saves with counting metadata, Go auto-rebuilds on CreateOrOpen", func() {
		It("should auto-rebuild index via checkPossiblyRebuild and produce correct entries", func() {
			// Save 3 orders using Java with counting metadata (no index)
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 300)),
				}
				err := store.SaveOrderForAutoRebuildJava(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go opens with indexed+counting metadata via CreateOrOpen().
			// Go's checkPossiblyRebuild() detects the metadata version change and auto-rebuilds.
			goEntries, err := store.AutoRebuildAndScanGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			// Java scans the same index (now rebuilt by Go) using counting metadata
			javaEntries, err := store.ScanIndexAfterAutoRebuildJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Both must agree
			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Cross-rebuild: both sides can rebuild the same data", func() {
		It("Go rebuild and Java rebuild produce identical results", func() {
			// Save 3 orders without index using Go
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 150)),
				}
				err := store.SaveOrderGo(ctx, order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild with Go first
			err := store.RebuildIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			goEntriesAfterGoRebuild, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntriesAfterGoRebuild).To(HaveLen(3))

			// Rebuild with Java (clears and rebuilds)
			err = store.RebuildIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			goEntriesAfterJavaRebuild, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntriesAfterJavaRebuild).To(HaveLen(3))

			// Both rebuilds should produce identical entries
			err = CompareIndexEntries(goEntriesAfterGoRebuild, goEntriesAfterJavaRebuild)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func idxToInt64(v any) int64 {
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

// RebuildIndexConformanceStore provides operations for testing index rebuild
// conformance between Go and Java. Records are saved without an index, then
// the index is rebuilt and the results are compared.
type RebuildIndexConformanceStore struct {
	RecordDB      *recordlayer.FDBDatabase
	MetaDataNoIdx *recordlayer.RecordMetaData // Metadata WITHOUT index (for saving)
	MetaDataIdx   *recordlayer.RecordMetaData // Metadata WITH index (for rebuild/scan)
	PriceIndex    *recordlayer.Index
	// Counting variants for auto-rebuild tests (record count enables default checker path)
	MetaDataNoIdxCounting *recordlayer.RecordMetaData
	MetaDataIdxCounting   *recordlayer.RecordMetaData
	PriceIndexCounting    *recordlayer.Index
	Keyspace              subspace.Subspace
	java                  *JavaInvoker
	clusterFile           string
	tenantName            string
}

// NewRebuildIndexConformanceStore creates a conformance store for index rebuild testing.
func NewRebuildIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*RebuildIndexConformanceStore, error) {
	// Metadata without index (for saving records)
	builderNoIdx := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderNoIdx.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderNoIdx.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderNoIdx.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	mdNoIdx, err := builderNoIdx.Build()
	if err != nil {
		return nil, err
	}

	// Metadata with index (for rebuild/scan)
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

	// Counting metadata WITHOUT index (for auto-rebuild: save phase)
	builderNoIdxCounting := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderNoIdxCounting.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderNoIdxCounting.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderNoIdxCounting.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builderNoIdxCounting.SetRecordCountKey(&recordlayer.EmptyKeyExpression{})
	mdNoIdxCounting, err := builderNoIdxCounting.Build()
	if err != nil {
		return nil, err
	}

	// Counting metadata WITH index (for auto-rebuild: open+scan phase)
	priceIndexCounting := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
	builderIdxCounting := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builderIdxCounting.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builderIdxCounting.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builderIdxCounting.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builderIdxCounting.SetRecordCountKey(&recordlayer.EmptyKeyExpression{})
	builderIdxCounting.AddIndex("Order", priceIndexCounting)
	mdIdxCounting, err := builderIdxCounting.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &RebuildIndexConformanceStore{
		RecordDB:              recordDB,
		MetaDataNoIdx:         mdNoIdx,
		MetaDataIdx:           mdIdx,
		PriceIndex:            priceIndex,
		MetaDataNoIdxCounting: mdNoIdxCounting,
		MetaDataIdxCounting:   mdIdxCounting,
		PriceIndexCounting:    priceIndexCounting,
		Keyspace:              ks,
		java:                  NewJavaInvoker(),
		clusterFile:           clusterFile,
		tenantName:            tenantName,
	}, nil
}

func (s *RebuildIndexConformanceStore) buildJavaParams() map[string]any {
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
func (s *RebuildIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataNoIdx).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(order)
		return nil, err
	})
	return err
}

// SaveOrderJava saves an order WITHOUT index using Java.
func (s *RebuildIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrder", params, nil)
}

// RebuildIndexGo opens store with indexed metadata and rebuilds the index within one transaction.
func (s *RebuildIndexConformanceStore) RebuildIndexGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return nil, store.RebuildIndex(s.PriceIndex)
	})
	return err
}

// RebuildIndexJava calls Java's FDBRecordStore.rebuildIndex() via the conformance server.
func (s *RebuildIndexConformanceStore) RebuildIndexJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "rebuildIndex", params, nil)
}

// SaveOrderForAutoRebuildGo saves an order using counting metadata (no index).
// The record count is tracked atomically, enabling Java's default UserVersionChecker
// to determine the record count for the inline rebuild threshold.
func (s *RebuildIndexConformanceStore) SaveOrderForAutoRebuildGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataNoIdxCounting).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(order)
		return nil, err
	})
	return err
}

// SaveOrderForAutoRebuildJava saves an order using Java's counting metadata (no index).
func (s *RebuildIndexConformanceStore) SaveOrderForAutoRebuildJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderForAutoRebuild", params, nil)
}

// AutoRebuildAndScanGo opens store with indexed+counting metadata via CreateOrOpen(),
// letting Go's checkPossiblyRebuild() detect the version change and auto-rebuild
// the index, then scans and returns entries. No explicit RebuildIndex() call.
func (s *RebuildIndexConformanceStore) AutoRebuildAndScanGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdxCounting).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.PriceIndexCounting, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexWithCountingGo scans the index using counting metadata (for reading
// after another side already rebuilt the index via auto-rebuild).
func (s *RebuildIndexConformanceStore) ScanIndexWithCountingGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdxCounting).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.PriceIndexCounting, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

// ScanIndexAfterAutoRebuildJava opens store with indexed metadata using Java's
// default UserVersionChecker (no ALWAYS_READABLE_CHECKER), letting Java's
// checkPossiblyRebuild() auto-rebuild the index, then scans and returns entries.
func (s *RebuildIndexConformanceStore) ScanIndexAfterAutoRebuildJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = "Order$price"

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanIndexAfterAutoRebuild", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanIndexAfterAutoRebuild failed: %w", err)
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

// ScanIndexGo scans the price index using Go.
func (s *RebuildIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaDataIdx).SetSubspace(s.Keyspace).Open()
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

// ScanIndexJava scans the price index using Java.
func (s *RebuildIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
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
