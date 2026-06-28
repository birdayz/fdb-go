//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
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

var _ = Describe("VERSION Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *VersionIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("vidx_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewVersionIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes with VERSION index, both scan", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			// Save 3 orders in separate transactions (different versionstamps)
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderGo(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify entries are ordered by versionstamp (ascending commit order)
			// Since we saved 1, 2, 3 in order, PKs should be 1, 2, 3
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(1)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(2)))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(3)))

			// Versionstamps should be strictly increasing
			Expect(goEntries[0].VersionBytes < goEntries[1].VersionBytes).To(BeTrue())
			Expect(goEntries[1].VersionBytes < goEntries[2].VersionBytes).To(BeTrue())
		})
	})

	Describe("Java writes with VERSION index, both scan", func() {
		It("should produce identical index entries visible to both Go and Java", func() {
			for i := int64(1); i <= 3; i++ {
				err := store.SaveOrderJava(ctx, &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 200)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// PKs in commit order
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(1)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(2)))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(3)))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce identically ordered entries with all records visible", func() {
			// Go writes order 1
			err := store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes order 2
			err = store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			})
			Expect(err).NotTo(HaveOccurred())

			// Go writes order 3
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(300),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// All 3 records visible, ordered by commit time → PK order 1, 2, 3
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(1)))
			Expect(toInt64(goEntries[1].PrimaryKey[0])).To(Equal(int64(2)))
			Expect(toInt64(goEntries[2].PrimaryKey[0])).To(Equal(int64(3)))

			// Versionstamps strictly increasing
			Expect(goEntries[0].VersionBytes < goEntries[1].VersionBytes).To(BeTrue())
			Expect(goEntries[1].VersionBytes < goEntries[2].VersionBytes).To(BeTrue())
		})
	})

	Describe("Delete removes VERSION index entry cross-language", func() {
		It("should remove entry when Go deletes a Java-written record", func() {
			// Java writes 2 records
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
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

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(2)))
		})

		It("should remove entry when Java deletes a Go-written record", func() {
			// Go writes 2 records
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

			// Java deletes order 2
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(1)))
		})
	})

	Describe("Update replaces VERSION index entry cross-language", func() {
		It("should replace entry when Go updates a Java-written record", func() {
			// Java writes order 1
			err := store.SaveOrderJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())

			// Capture the version before update
			entriesBefore, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entriesBefore).To(HaveLen(1))
			vsBefore := entriesBefore[0].VersionBytes

			// Go updates order 1 (new price)
			err = store.SaveOrderGo(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(999),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1), "old entry should be removed, only new entry remains")

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Versionstamp should have changed (new transaction)
			Expect(goEntries[0].VersionBytes).NotTo(Equal(vsBefore))
			Expect(toInt64(goEntries[0].PrimaryKey[0])).To(Equal(int64(1)))
		})
	})

	Describe("Multiple records in one transaction share global version", func() {
		It("Go saves multiple records in one tx, both see correct local versions", func() {
			// Save 3 records in one Go transaction
			_, _, err := env.RecordDB.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(store.MetaData).
					SetSubspace(store.Keyspace).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					_, err = st.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanVersionIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanVersionIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareVersionIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// All 3 should share the same global version but differ in local version
			// Since versionstamp = global(10 bytes) + local(2 bytes), and local
			// increments per record in the same transaction, the hex strings
			// should share a common prefix but differ in the last few characters
			Expect(goEntries[0].VersionBytes).NotTo(Equal(goEntries[1].VersionBytes))
			Expect(goEntries[1].VersionBytes).NotTo(Equal(goEntries[2].VersionBytes))
			Expect(goEntries[0].VersionBytes < goEntries[1].VersionBytes).To(BeTrue())
			Expect(goEntries[1].VersionBytes < goEntries[2].VersionBytes).To(BeTrue())
		})
	})
})

// VersionIndexEntryResult represents a single VERSION index entry for comparison.
// The versionstamp is stored as hex-encoded 12-byte string for cross-language comparison.
// Hex is used instead of base64 because hex encoding preserves byte ordering
// (0-9a-f sorts correctly in ASCII), enabling string < comparisons on versionstamps.
type VersionIndexEntryResult struct {
	VersionBytes string // hex encoded 12-byte versionstamp
	PrimaryKey   []any  // Primary key extracted from the entry
}

// CompareVersionIndexEntries compares Go and Java VERSION index scan results.
func CompareVersionIndexEntries(goEntries, javaEntries []VersionIndexEntryResult) error {
	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d", len(goEntries), len(javaEntries))
	}
	for i := range goEntries {
		if goEntries[i].VersionBytes != javaEntries[i].VersionBytes {
			return fmt.Errorf("entry %d versionstamp mismatch:\n  go:   %s\n  java: %s",
				i, goEntries[i].VersionBytes, javaEntries[i].VersionBytes)
		}
		if !sliceEqualNormalized(goEntries[i].PrimaryKey, javaEntries[i].PrimaryKey) {
			return fmt.Errorf("entry %d PK mismatch:\n  go:   %v\n  java: %v",
				i, goEntries[i].PrimaryKey, javaEntries[i].PrimaryKey)
		}
	}
	return nil
}

// versionstampToHex converts a tuple.Versionstamp to hex-encoded 12-byte string.
// Format: 10 bytes TransactionVersion + 2 bytes UserVersion (big-endian).
// Hex preserves byte ordering under string comparison (unlike base64).
func versionstampToHex(vs tuple.Versionstamp) string {
	bytes := make([]byte, 12)
	copy(bytes[:10], vs.TransactionVersion[:])
	binary.BigEndian.PutUint16(bytes[10:], vs.UserVersion)
	return hex.EncodeToString(bytes)
}

// VersionIndexConformanceStore wraps record operations with a VERSION index on Order.
type VersionIndexConformanceStore struct {
	RecordDB     *recordlayer.FDBDatabase
	MetaData     *recordlayer.RecordMetaData
	VersionIndex *recordlayer.Index
	Keyspace     subspace.Subspace
	java         *JavaInvoker
	clusterFile  string
	tenantName   string
}

func NewVersionIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*VersionIndexConformanceStore, error) {
	versionIdx := recordlayer.NewVersionIndex("Order$version", recordlayer.VersionKey())

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", versionIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &VersionIndexConformanceStore{
		RecordDB:     recordDB,
		MetaData:     md,
		VersionIndex: versionIdx,
		Keyspace:     ks,
		java:         NewJavaInvoker(),
		clusterFile:  clusterFile,
		tenantName:   tenantName,
	}, nil
}

func (s *VersionIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *VersionIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
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

func (s *VersionIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithVersionIndex", params, nil)
}

func (s *VersionIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *VersionIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithVersionIndex", params, nil)
}

// ScanVersionIndexGo scans the VERSION index using Go and returns results.
func (s *VersionIndexConformanceStore) ScanVersionIndexGo(ctx context.Context) ([]VersionIndexEntryResult, error) {
	var results []VersionIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, st.ScanIndex(s.VersionIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			result := VersionIndexEntryResult{
				PrimaryKey: tupleToSlice(e.PrimaryKey()),
			}
			// First key element is the Versionstamp
			if len(e.Key) > 0 {
				if vs, ok := e.Key[0].(tuple.Versionstamp); ok {
					result.VersionBytes = versionstampToHex(vs)
				}
			}
			results = append(results, result)
		}
		return nil, nil
	})
	return results, err
}

// ScanVersionIndexJava scans the VERSION index using Java and returns results.
func (s *VersionIndexConformanceStore) ScanVersionIndexJava(ctx context.Context) ([]VersionIndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanVersionIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanVersionIndex failed: %w", err)
	}

	var results []VersionIndexEntryResult
	for _, m := range javaResults {
		entry := VersionIndexEntryResult{}
		if pkRaw, ok := m["primaryKey"]; ok {
			entry.PrimaryKey = toInterfaceSlice(pkRaw)
		}
		// Key[0] is hex versionstamp string from Java
		if keyRaw, ok := m["key"]; ok {
			keySlice := toInterfaceSlice(keyRaw)
			if len(keySlice) > 0 {
				if vsStr, ok := keySlice[0].(string); ok {
					entry.VersionBytes = vsStr
				}
			}
		}
		results = append(results, entry)
	}
	return results, nil
}
