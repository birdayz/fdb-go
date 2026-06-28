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
	"google.golang.org/protobuf/proto"
)

var _ = Describe("MAX_EVER_VERSION Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MaxEverVersionConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("maxeverv_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMaxEverVersionConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan", func() {
		It("should produce identical max-ever-version entries visible to both Go and Java", func() {
			// Save 3 orders in separate transactions (different versionstamps)
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1), "ungrouped → single entry")

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Ungrouped key should be empty
			Expect(goEntries[0].Key).To(BeEmpty())
			// Versionstamp should be non-empty (from the last tx = max)
			Expect(goEntries[0].VersionBytes).NotTo(BeEmpty())
		})
	})

	Describe("Java writes, both scan", func() {
		It("should produce identical max-ever-version entries visible to both Go and Java", func() {
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 200)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Key).To(BeEmpty())
			Expect(goEntries[0].VersionBytes).NotTo(BeEmpty())
		})
	})

	Describe("Mixed writes: Go and Java alternating", func() {
		It("should track the global max version across both writers", func() {
			// Go saves order 1
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java saves order 2
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go saves order 3 (latest tx = largest versionstamp)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Max version = from last tx (order 3)
			Expect(goEntries[0].VersionBytes).NotTo(BeEmpty())
		})
	})

	Describe("Delete is no-op for _EVER semantics", func() {
		It("should preserve max version after Go deletes a record", func() {
			// Save 2 orders in separate transactions
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Capture max version before delete
			entriesBefore, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entriesBefore).To(HaveLen(1))
			vsBefore := entriesBefore[0].VersionBytes

			// Delete order 2 (which was the latest write)
			deleted, err := store.DeleteOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Max version should still be present (_EVER = irreversible)
			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].VersionBytes).To(Equal(vsBefore))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Later write updates max cross-language", func() {
		It("should update max when Go writes after Java", func() {
			// Java saves order 1
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Capture Java's max version
			javaOnly, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaOnly).To(HaveLen(1))
			javaVS := javaOnly[0].VersionBytes

			// Go saves order 2 (later tx = larger versionstamp)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Max should now be from Go's tx (larger versionstamp)
			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Go's versionstamp should be > Java's (hex comparison preserves byte order)
			Expect(goEntries[0].VersionBytes > javaVS).To(BeTrue(),
				"Go's later tx should have larger versionstamp: go=%s java=%s",
				goEntries[0].VersionBytes, javaVS)
		})
	})

	Describe("Go deletes Java-written record, max persists", func() {
		It("should preserve max version after deleting the only record", func() {
			// Java saves order 1
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Capture max version
			entriesBefore, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entriesBefore).To(HaveLen(1))
			vsBefore := entriesBefore[0].VersionBytes

			// Go deletes order 1
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Max version should still be there (_EVER semantics)
			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))
			Expect(goEntries[0].VersionBytes).To(Equal(vsBefore))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareMaxEverVersionEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Wire format: versionstamp bytes match between Go and Java", func() {
		It("should produce identical hex-encoded versionstamp bytes", func() {
			// Save a record via Go
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(42),
			})
			Expect(err).NotTo(HaveOccurred())

			// Read the index entry from both sides
			goEntries, err := store.ScanMaxEverVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanMaxEverVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			// The hex versionstamp bytes must be identical
			Expect(goEntries[0].VersionBytes).To(Equal(javaEntries[0].VersionBytes),
				"versionstamp wire format mismatch:\n  go:   %s\n  java: %s",
				goEntries[0].VersionBytes, javaEntries[0].VersionBytes)

			// Sanity: versionstamp should be 12 bytes = 24 hex chars
			Expect(goEntries[0].VersionBytes).To(HaveLen(24),
				"versionstamp should be 12 bytes (24 hex chars)")
		})
	})
})

// MaxEverVersionEntry represents a single MAX_EVER_VERSION index entry for comparison.
type MaxEverVersionEntry struct {
	Key          []any  // Grouping key (empty for ungrouped)
	VersionBytes string // Hex-encoded 12-byte versionstamp from the value
}

// CompareMaxEverVersionEntries compares Go and Java MAX_EVER_VERSION index scan results.
func CompareMaxEverVersionEntries(goEntries, javaEntries []MaxEverVersionEntry) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if !sliceEqualNormalized(goEntries[i].Key, javaEntries[i].Key) {
			return fmt.Errorf("entry %d key mismatch: go=%v java=%v",
				i, goEntries[i].Key, javaEntries[i].Key)
		}
		if goEntries[i].VersionBytes != javaEntries[i].VersionBytes {
			return fmt.Errorf("entry %d version mismatch: go=%s java=%s",
				i, goEntries[i].VersionBytes, javaEntries[i].VersionBytes)
		}
	}
	return nil
}

// MaxEverVersionConformanceStore wraps record operations with a MAX_EVER_VERSION
// index on Order (ungrouped versionstamp).
type MaxEverVersionConformanceStore struct {
	RecordDB        *recordlayer.FDBDatabase
	MetaData        *recordlayer.RecordMetaData
	MaxVersionIndex *recordlayer.Index
	Keyspace        subspace.Subspace
	java            *JavaInvoker
	clusterFile     string
	tenantName      string
}

func NewMaxEverVersionConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MaxEverVersionConformanceStore, error) {
	idx := recordlayer.NewMaxEverVersionIndex("Order$maxVersion", recordlayer.Ungrouped(recordlayer.VersionKey()))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &MaxEverVersionConformanceStore{
		RecordDB:        recordDB,
		MetaData:        md,
		MaxVersionIndex: idx,
		Keyspace:        ks,
		java:            NewJavaInvoker(),
		clusterFile:     clusterFile,
		tenantName:      tenantName,
	}, nil
}

func (s *MaxEverVersionConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MaxEverVersionConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, _, err := s.RecordDB.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = st.SaveRecord(order)
		return nil, err
	})
	return err
}

func (s *MaxEverVersionConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithMaxEverVersionIndex", params, nil)
}

func (s *MaxEverVersionConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, _, err := s.RecordDB.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		deleted, err = st.DeleteRecord(tuple.Tuple{orderID})
		return nil, err
	})
	return deleted, err
}

func (s *MaxEverVersionConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMaxEverVersionIndex", params, nil)
}

// ScanMaxEverVersionIndexGo scans the MAX_EVER_VERSION index using Go and returns results.
func (s *MaxEverVersionConformanceStore) ScanMaxEverVersionIndexGo(ctx context.Context) ([]MaxEverVersionEntry, error) {
	var results []MaxEverVersionEntry
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, st.ScanIndex(s.MaxVersionIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			result := MaxEverVersionEntry{
				Key: tupleToSlice(e.Key),
			}
			// Value tuple contains the versionstamp
			if len(e.Value) > 0 {
				if vs, ok := e.Value[0].(tuple.Versionstamp); ok {
					result.VersionBytes = versionstampToHex(vs)
				}
			}
			results = append(results, result)
		}
		return nil, nil
	})
	return results, err
}

// ScanMaxEverVersionIndexJava scans the MAX_EVER_VERSION index using Java and returns results.
func (s *MaxEverVersionConformanceStore) ScanMaxEverVersionIndexJava(ctx context.Context) ([]MaxEverVersionEntry, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMaxEverVersionIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMaxEverVersionIndex failed: %w", err)
	}

	var results []MaxEverVersionEntry
	for _, m := range javaResults {
		entry := MaxEverVersionEntry{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if valueRaw, ok := m["value"]; ok {
			valueSlice := toInterfaceSlice(valueRaw)
			if len(valueSlice) > 0 {
				if vsStr, ok := valueSlice[0].(string); ok {
					entry.VersionBytes = vsStr
				}
			}
		}
		results = append(results, entry)
	}
	return results, nil
}
