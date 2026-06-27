//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"math"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// TypedRecordConformanceStore wraps operations for TypedRecord with indexes on every field type.
type TypedRecordConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Indexes     map[string]*recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewTypedRecordConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*TypedRecordConformanceStore, error) {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	indexes := map[string]*recordlayer.Index{}
	// All field types that Java Record Layer supports (unsigned types are rejected by Java)
	fields := []string{
		"int32", "int64", "sint32", "sint64",
		"sfixed32", "sfixed64", "float", "double",
		"bool", "string", "bytes", "enum",
	}
	for _, f := range fields {
		idx := recordlayer.NewIndex("idx_"+f, recordlayer.Field("val_"+f))
		builder.AddIndex("TypedRecord", idx)
		indexes["idx_"+f] = idx
	}

	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &TypedRecordConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Indexes:     indexes,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *TypedRecordConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *TypedRecordConformanceStore) SaveGo(ctx context.Context, record *gen.TypedRecord) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(record)
		return nil, err
	})
	return err
}

func (s *TypedRecordConformanceStore) SaveJava(ctx context.Context, record *gen.TypedRecord) error {
	params := s.buildJavaParams()
	params["record"] = record
	return s.java.InvokeAs(ctx, "saveTypedRecord", params, nil)
}

func (s *TypedRecordConformanceStore) ScanIndexGo(ctx context.Context, indexName string) ([]IndexEntryResult, error) {
	idx, ok := s.Indexes[indexName]
	if !ok {
		return nil, fmt.Errorf("unknown index: %s", indexName)
	}
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

func (s *TypedRecordConformanceStore) ScanIndexJava(ctx context.Context, indexName string) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()
	params["indexName"] = indexName

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanTypedIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanTypedIndex failed: %w", err)
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

func genColorPtr(c gen.Color) *gen.Color {
	return &c
}

// buildTypedRecord constructs a TypedRecord with all supported field types set.
func buildTypedRecord(id int64, i32 int32, i64 int64,
	si32 int32, si64 int64, sf32 int32, sf64 int64,
	vFloat float32, vDouble float64, vBool bool, vString string, vBytes []byte, vEnum gen.Color,
) *gen.TypedRecord {
	return &gen.TypedRecord{
		Id:          proto.Int64(id),
		ValInt32:    proto.Int32(i32),
		ValInt64:    proto.Int64(i64),
		ValSint32:   proto.Int32(si32),
		ValSint64:   proto.Int64(si64),
		ValSfixed32: proto.Int32(sf32),
		ValSfixed64: proto.Int64(sf64),
		ValFloat:    proto.Float32(vFloat),
		ValDouble:   proto.Float64(vDouble),
		ValBool:     proto.Bool(vBool),
		ValString:   proto.String(vString),
		ValBytes:    vBytes,
		ValEnum:     genColorPtr(vEnum),
	}
}

var _ = Describe("TypedRecord Conformance — FDB tuple encoding of all proto field types", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *TypedRecordConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("typed_%s", uuid.New().String())
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		store, err = NewTypedRecordConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	allIndexNames := func() []string {
		return []string{
			"idx_int32", "idx_int64", "idx_sint32", "idx_sint64",
			"idx_sfixed32", "idx_sfixed64", "idx_float", "idx_double",
			"idx_bool", "idx_string", "idx_bytes", "idx_enum",
		}
	}

	// verifyAllIndexes scans every index with both Go and Java and compares entries.
	verifyAllIndexes := func(expectedCount int) {
		for _, idxName := range allIndexNames() {
			goEntries, err := store.ScanIndexGo(ctx, idxName)
			Expect(err).NotTo(HaveOccurred(), "Go scan %s", idxName)
			Expect(goEntries).To(HaveLen(expectedCount), "Go %s count", idxName)

			javaEntries, err := store.ScanIndexJava(ctx, idxName)
			Expect(err).NotTo(HaveOccurred(), "Java scan %s", idxName)
			Expect(javaEntries).To(HaveLen(expectedCount), "Java %s count", idxName)

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred(), "mismatch on %s", idxName)
		}
	}

	Describe("Go writes, Java reads indexes", func() {
		It("basic values — all field types produce identical index entries", func() {
			rec := buildTypedRecord(
				1,                  // id
				42,                 // int32
				1000000,            // int64
				-42,                // sint32
				-1000000,           // sint64
				-42,                // sfixed32
				-1000000,           // sfixed64
				3.14,               // float
				2.718281828,        // double
				true,               // bool
				"hello",            // string
				[]byte{0xDE, 0xAD}, // bytes
				gen.Color_RED,      // enum
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})

	Describe("Java writes, Go reads indexes", func() {
		It("basic values — all field types produce identical index entries", func() {
			rec := buildTypedRecord(
				2, 99, 999999, -99, -999999, -99, -999999,
				1.5, 9.99, false, "world",
				[]byte{0xCA, 0xFE}, gen.Color_BLUE,
			)
			err := store.SaveJava(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})

	Describe("Edge cases — zero values", func() {
		It("all zeros produce identical index entries", func() {
			rec := buildTypedRecord(
				10, 0, 0, 0, 0, 0, 0,
				0.0, 0.0, false, "", []byte{}, gen.Color_RED,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})

	Describe("Edge cases — max signed values", func() {
		It("max int32/int64 produce identical index entries", func() {
			rec := buildTypedRecord(
				20,
				math.MaxInt32,   // 2147483647
				math.MaxInt64,   // 9223372036854775807
				math.MaxInt32,   // sint32
				math.MaxInt64,   // sint64
				math.MaxInt32,   // sfixed32
				math.MaxInt64,   // sfixed64
				math.MaxFloat32, // 3.4028235e+38
				math.MaxFloat64, // 1.7976931348623157e+308
				true,
				"maxvals",
				[]byte{0xFF, 0xFF, 0xFF},
				gen.Color_PINK,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})

	Describe("Edge cases — min signed values", func() {
		It("min int32/int64 produce identical index entries", func() {
			rec := buildTypedRecord(
				30,
				math.MinInt32,    // -2147483648
				math.MinInt64,    // -9223372036854775808
				math.MinInt32,    // sint32
				math.MinInt64,    // sint64
				math.MinInt32,    // sfixed32
				math.MinInt64,    // sfixed64
				-math.MaxFloat32, // float
				-math.MaxFloat64, // double
				false,
				"",
				nil,
				gen.Color_RED,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})

	Describe("Cross-language round-trip", func() {
		It("Go saves, Java loads — record values preserved", func() {
			rec := buildTypedRecord(
				50, -1, -1, -1, -1, -1, -1,
				-1.5, -1.5, true, "roundtrip", []byte{1, 2, 3}, gen.Color_YELLOW,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())

			params := store.buildJavaParams()
			params["id"] = int64(50)
			var loaded gen.TypedRecord
			err = store.java.InvokeAs(ctx, "loadTypedRecord", params, &loaded)
			Expect(err).NotTo(HaveOccurred())

			Expect(loaded.GetId()).To(Equal(rec.GetId()))
			Expect(loaded.GetValInt32()).To(Equal(rec.GetValInt32()))
			Expect(loaded.GetValInt64()).To(Equal(rec.GetValInt64()))
			Expect(loaded.GetValSint32()).To(Equal(rec.GetValSint32()))
			Expect(loaded.GetValSint64()).To(Equal(rec.GetValSint64()))
			Expect(loaded.GetValSfixed32()).To(Equal(rec.GetValSfixed32()))
			Expect(loaded.GetValSfixed64()).To(Equal(rec.GetValSfixed64()))
			Expect(loaded.GetValBool()).To(Equal(rec.GetValBool()))
			Expect(loaded.GetValString()).To(Equal(rec.GetValString()))
			Expect(loaded.GetValBytes()).To(Equal(rec.GetValBytes()))
			Expect(loaded.GetValEnum()).To(Equal(rec.GetValEnum()))
		})

		It("Java saves, Go loads — record values preserved", func() {
			rec := buildTypedRecord(
				51, 100, 200, -500, -600, -900, -1000,
				1.23, 4.56, false, "java2go", []byte{0xAB}, gen.Color_PINK,
			)
			err := store.SaveJava(ctx, rec)
			Expect(err).NotTo(HaveOccurred())

			var loaded *gen.TypedRecord
			_, err = store.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
				if err != nil {
					return nil, err
				}
				result, err := st.LoadRecord(tuple.Tuple{int64(51)})
				if err != nil {
					return nil, err
				}
				Expect(result).NotTo(BeNil())
				loaded = result.Record.(*gen.TypedRecord)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())

			Expect(loaded.GetValInt32()).To(Equal(rec.GetValInt32()))
			Expect(loaded.GetValInt64()).To(Equal(rec.GetValInt64()))
			Expect(loaded.GetValSint32()).To(Equal(rec.GetValSint32()))
			Expect(loaded.GetValSint64()).To(Equal(rec.GetValSint64()))
			Expect(loaded.GetValSfixed32()).To(Equal(rec.GetValSfixed32()))
			Expect(loaded.GetValSfixed64()).To(Equal(rec.GetValSfixed64()))
			Expect(loaded.GetValBool()).To(Equal(rec.GetValBool()))
			Expect(loaded.GetValString()).To(Equal(rec.GetValString()))
			Expect(loaded.GetValBytes()).To(Equal(rec.GetValBytes()))
			Expect(loaded.GetValEnum()).To(Equal(rec.GetValEnum()))
		})
	})

	Describe("Multiple records — ordering verified across languages", func() {
		It("int32 index ordering matches between Go and Java", func() {
			values := []int32{500, -100, 0, 300, -200}
			for i, v := range values {
				rec := buildTypedRecord(
					int64(60+i), v, 0, 0, 0, 0, 0,
					0, 0, false, "", []byte{}, gen.Color_RED,
				)
				err := store.SaveGo(ctx, rec)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx, "idx_int32")
			Expect(err).NotTo(HaveOccurred())
			javaEntries, err := store.ScanIndexJava(ctx, "idx_int32")
			Expect(err).NotTo(HaveOccurred())

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify sorted: -200, -100, 0, 300, 500
			expectedOrder := []int64{-200, -100, 0, 300, 500}
			for i, entry := range goEntries {
				Expect(toInt64(entry.Key[0])).To(Equal(expectedOrder[i]),
					"entry %d: expected %d", i, expectedOrder[i])
			}
		})
	})

	Describe("Special float values", func() {
		It("extreme float values produce identical entries", func() {
			// Use largest finite values — infinity can't survive JSON transport
			rec := buildTypedRecord(
				70, 0, 0, 0, 0, 0, 0,
				math.MaxFloat32, math.MaxFloat64,
				false, "", []byte{}, gen.Color_RED,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})

		It("negative extreme float values produce identical entries", func() {
			rec := buildTypedRecord(
				71, 0, 0, 0, 0, 0, 0,
				-math.MaxFloat32, -math.MaxFloat64,
				false, "", []byte{}, gen.Color_RED,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})

		It("smallest positive floats produce identical entries", func() {
			rec := buildTypedRecord(
				72, 0, 0, 0, 0, 0, 0,
				math.SmallestNonzeroFloat32, math.SmallestNonzeroFloat64,
				false, "", []byte{}, gen.Color_RED,
			)
			err := store.SaveGo(ctx, rec)
			Expect(err).NotTo(HaveOccurred())
			verifyAllIndexes(1)
		})
	})
})
