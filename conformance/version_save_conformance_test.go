//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

var _ = Describe("Ordered version index validation matches Java", func() {
	metadataBuilder := func(index *recordlayer.Index) *recordlayer.RecordMetaDataBuilder {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.SetStoreRecordVersions(true).AddIndex("Order", index)
		return builder
	}
	orderedVersion := func() recordlayer.KeyExpression {
		return recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsLast, recordlayer.VersionKey())
	}

	It("refuses a VERSION index whose ordering function declares no version columns", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		md, err := metadataBuilder(recordlayer.NewVersionIndex("order$version", recordlayer.VersionKey())).Build()
		Expect(err).NotTo(HaveOccurred())
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		Expect(mdProto.GetIndexes()).To(HaveLen(1))
		mdProto.Indexes[0].RootExpression = orderedVersion().ToKeyExpression()
		mdBytes, err := proto.Marshal(mdProto)
		Expect(err).NotTo(HaveOccurred())
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		javaSS := subspace.Sub(tuple.Tuple{"ordered_version_index_refusal_java", uuid.NewString()}...)
		_, err = NewJavaInvoker().Invoke(ctx, "saveWithVersionsJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
			"recordTypeName": "Order", "records": [][]int{}, "versions": [][]int{}, "behaviors": []string{}, "formatVersion": 0,
		})
		var javaErr *JavaError
		Expect(errors.As(err, &javaErr)).To(BeTrue(), "%v", err)
		Expect(javaErr.ExceptionClass).To(Equal("InvalidExpressionException"))
		Expect(javaErr.ExceptionFullClass).To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
		Expect(javaErr.Message).To(Equal("there must be exactly 1 version entry in index"))
		Expect(javaErr.SQLState).To(BeEmpty())

		_, err = recordlayer.RecordMetaDataFromProto(mdProto)
		var keyErr *recordlayer.KeyExpressionError
		Expect(errors.As(err, &keyErr)).To(BeTrue(), "%v", err)
		Expect(keyErr.Message).To(Equal("there must be exactly 1 version entry in index"))
		_, err = metadataBuilder(recordlayer.NewVersionIndex("order$version", orderedVersion())).Build()
		keyErr = nil
		Expect(errors.As(err, &keyErr)).To(BeTrue(), "%v", err)
		Expect(keyErr.Message).To(Equal("there must be exactly 1 version entry in index"))
	})

	It("accepts the VALUE index metadata but aborts an automatic incomplete-version save", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		md, err := metadataBuilder(recordlayer.NewIndex("order$ordered_version", orderedVersion())).Build()
		Expect(err).NotTo(HaveOccurred())
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		decoded, err := recordlayer.RecordMetaDataFromProto(mdProto)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).NotTo(BeNil())
		mdBytes, err := proto.Marshal(mdProto)
		Expect(err).NotTo(HaveOccurred())
		record := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
		recordBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
		Expect(err).NotTo(HaveOccurred())
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		javaSS := subspace.Sub(tuple.Tuple{"ordered_version_value_java", uuid.NewString()}...)
		var java struct {
			Verdicts []string   `json:"verdicts"`
			KVs      [][]string `json:"kvs"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "saveWithVersionsJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
			"recordTypeName": "Order", "records": [][]int{BytesToIntArray(recordBytes)}, "versions": [][]int{{}},
			"behaviors": []string{recordlayer.VersionstampSaveBehaviorDefault.String()}, "formatVersion": 0,
		}, &java)).To(Succeed())
		Expect(java.Verdicts).To(Equal([]string{"java.lang.IllegalArgumentException"}))
		Expect(java.KVs).To(BeEmpty(), "the failed Java save must not persist record, index, count or version entries")

		goSS := subspace.Sub(tuple.Tuple{"ordered_version_value_go", uuid.NewString()}...)
		db := recordlayer.NewFDBDatabase(sharedDB)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(record)
		})
		var incomplete *recordlayer.IncompleteVersionstampError
		Expect(errors.As(err, &incomplete)).To(BeTrue(), "%v", err)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			for _, space := range []int64{1, 2, 4, 8} {
				rows, err := rtx.Transaction().GetRange(goSS.Sub(space), fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				Expect(rows).To(BeEmpty(), "the failed Go save must not persist subspace %d", space)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses persisted SQL DDL for a descending ROW_VERSION index with Java's SQLState", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		java := NewJavaInvoker()
		templateName := "RFC275_ORDERED_VERSION_" + uuid.NewString()
		body := `CREATE TABLE T (id BIGINT, col2 STRING, col3 BIGINT, col4 BIGINT, PRIMARY KEY(id))
			CREATE INDEX desc_v AS SELECT "__ROW_VERSION" FROM T ORDER BY "__ROW_VERSION" DESC
			WITH OPTIONS(store_row_versions=true)`
		var created struct {
			Created bool `json:"created"`
		}
		err = java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": templateName, "schemaTemplateBody": body,
		}, &created)
		if created.Created {
			DeferCleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				_, err := java.Invoke(cleanupCtx, "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile": clusterFile, "templateName": templateName,
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
		var javaErr *JavaError
		Expect(errors.As(err, &javaErr)).To(BeTrue(), "%v", err)
		Expect(javaErr.ExceptionClass).To(Equal("InvalidExpressionException"))
		Expect(javaErr.ExceptionFullClass).To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
		Expect(javaErr.Message).To(Equal("there must be exactly 1 version entry in index"))
		Expect(javaErr.SQLState).To(Equal("XXXXX"))
	})
})

// Both engines save the same records with the same versions and behaviors and must agree on
// every verdict and on the record (1), index (2), count (4) and legacy version (8) key-value pairs.
var _ = Describe("Explicit-version saves match Java", func() {
	type versionedSave struct {
		id       int64
		version  []byte // 12 complete bytes, or nil
		behavior recordlayer.VersionstampSaveBehavior
		want     string
	}
	complete := func(b byte) []byte { return []byte{0, 0, 0, 0, 0, 0, 0, 2, 0, b, 0, 1} }
	saves := []versionedSave{
		{1, complete(1), recordlayer.VersionstampSaveBehaviorDefault, "ok"},
		{2, complete(2), recordlayer.VersionstampSaveBehaviorIfPresent, "ok"},
		{3, nil, recordlayer.VersionstampSaveBehaviorIfPresent, "ok"},
		{4, nil, recordlayer.VersionstampSaveBehaviorNoVersion, "ok"},
		{5, complete(5), recordlayer.VersionstampSaveBehaviorWithVersion, "ok"},
		{6, complete(6), recordlayer.VersionstampSaveBehaviorNoVersion, "com.apple.foundationdb.record.RecordCoreException"},
		{1, complete(9), recordlayer.VersionstampSaveBehaviorIfPresent, "ok"}, // replaces the field-only key's version, or creates a distinct ordered-version key
		{1, nil, recordlayer.VersionstampSaveBehaviorNoVersion, "ok"},         // clears the field-only key's version, or creates the ordered-null version key
	}

	for _, tc := range []struct {
		name       string
		primaryKey recordlayer.KeyExpression
		refused    bool
	}{
		{"direct version", recordlayer.VersionKey(), true},
		{"concatenated version", recordlayer.Concat(recordlayer.VersionKey(), recordlayer.Field("order_id")), true},
		{"listed version", recordlayer.ListExpr(recordlayer.VersionKey(), recordlayer.Field("order_id")), true},
		{"split version", recordlayer.Split(recordlayer.VersionKey(), 1), true},
		{"dimension version", recordlayer.Dimensions(recordlayer.VersionKey(), 0, 1), true},
		{"function-wrapped version", recordlayer.FunctionExpr(recordlayer.OrderFuncAscNullsFirst, recordlayer.VersionKey()), false},
	} {
		It("matches primary-key metadata validation for "+tc.name, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			mdProto, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			injected := false
			for _, recordType := range mdProto.GetRecordTypes() {
				if recordType.GetName() == "Order" {
					recordType.PrimaryKey = tc.primaryKey.ToKeyExpression()
					injected = true
				}
			}
			Expect(injected).To(BeTrue())
			mdBytes, err := proto.Marshal(mdProto)
			Expect(err).NotTo(HaveOccurred())
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			javaSS := subspace.Sub(tuple.Tuple{"version_primary_key_metadata_java", uuid.NewString()}...)
			_, err = NewJavaInvoker().Invoke(ctx, "saveWithVersionsJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
				"recordTypeName": "Order", "records": [][]int{}, "versions": [][]int{}, "behaviors": []string{}, "formatVersion": 0,
			})
			if tc.refused {
				var javaErr *JavaError
				Expect(errors.As(err, &javaErr)).To(BeTrue(), "%v", err)
				Expect(javaErr.ExceptionClass).To(Equal("MetaDataException"))
				Expect(javaErr.ExceptionFullClass).To(Equal("com.apple.foundationdb.record.metadata.MetaDataException"))
				Expect(javaErr.Message).To(Equal("Version in primary key not supported"))
				Expect(javaErr.SQLState).To(BeEmpty())
			} else {
				Expect(err).NotTo(HaveOccurred(), "functions declare zero version columns in Java")
			}

			decoded, err := recordlayer.RecordMetaDataFromProto(mdProto)
			if tc.refused {
				var metaErr *recordlayer.MetaDataError
				Expect(errors.As(err, &metaErr)).To(BeTrue(), "%v", err)
				Expect(metaErr.Message).To(Equal("Version in primary key not supported"))
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(decoded).NotTo(BeNil())
			}
			builder.GetRecordType("Order").SetPrimaryKey(tc.primaryKey)
			built, err := builder.Build()
			if tc.refused {
				var metaErr *recordlayer.MetaDataError
				Expect(errors.As(err, &metaErr)).To(BeTrue(), "%v", err)
				Expect(metaErr.Message).To(Equal("Version in primary key not supported"))
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(built).NotTo(BeNil())
			}
		})
	}

	for _, orderedVersionPrimaryKey := range []bool{false, true} {
		for _, storeVersions := range []bool{false, true} {
			for _, format := range []int32{0, 5} {
				It(fmt.Sprintf("orderedVersionPrimaryKey=%v, storeRecordVersions=%v, format %d", orderedVersionPrimaryKey, storeVersions, format), func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					clusterFile, err := sharedContainer.ClusterFile(ctx)
					Expect(err).NotTo(HaveOccurred())

					builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
					builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
					orderedVersion := recordlayer.FunctionExpr(recordlayer.OrderFuncAscNullsFirst, recordlayer.VersionKey())
					if orderedVersionPrimaryKey {
						builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Concat(orderedVersion, recordlayer.Field("order_id")))
					}
					builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
					builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
					builder.SetStoreRecordVersions(storeVersions).SetRecordCountKey(recordlayer.VersionKey())
					if storeVersions {
						builder.AddIndex("Order", recordlayer.NewVersionIndex("order$version", recordlayer.VersionKey()))
					}
					md, err := builder.Build()
					Expect(err).NotTo(HaveOccurred())
					mdProto, err := md.ToProto()
					Expect(err).NotTo(HaveOccurred())
					mdBytes, err := proto.Marshal(mdProto)
					Expect(err).NotTo(HaveOccurred())

					records := make([][]int, len(saves))
					versions := make([][]int, len(saves))
					behaviors := make([]string, len(saves))
					var want []string
					for i, s := range saves {
						b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&demopb.Order{OrderId: proto.Int64(s.id), Price: proto.Int32(int32(s.id))})
						Expect(err).NotTo(HaveOccurred())
						records[i] = BytesToIntArray(b)
						versions[i] = BytesToIntArray(s.version)
						behaviors[i] = s.behavior.String()
						want = append(want, s.want)
					}

					javaSS := subspace.Sub(tuple.Tuple{"version_save_java", uuid.NewString()}...)
					var java struct {
						Verdicts []string   `json:"verdicts"`
						KVs      [][]string `json:"kvs"`
					}
					Expect(NewJavaInvoker().InvokeAs(ctx, "saveWithVersionsJava", map[string]any{
						"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
						"recordTypeName": "Order", "records": records, "versions": versions, "behaviors": behaviors, "formatVersion": format,
					}, &java)).To(Succeed())
					Expect(java.Verdicts).To(Equal(want), "Java")

					goSS := subspace.Sub(tuple.Tuple{"version_save_go", uuid.NewString()}...)
					db := recordlayer.NewFDBDatabase(sharedDB)
					var verdicts []string
					for _, s := range saves {
						var version *recordlayer.FDBRecordVersion
						if s.version != nil {
							version, err = recordlayer.CompleteVersionFromBytes(s.version)
							Expect(err).NotTo(HaveOccurred())
						}
						_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
							b := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS)
							if format != 0 {
								b.SetFormatVersion(format)
							}
							store, err := b.CreateOrOpen()
							if err != nil {
								return nil, err
							}
							return store.SaveRecordWithVersion(&demopb.Order{OrderId: proto.Int64(s.id), Price: proto.Int32(int32(s.id))},
								recordlayer.RecordExistenceCheckNone, version, s.behavior)
						})
						var coreErr *recordlayer.RecordCoreError
						switch {
						case err == nil:
							verdicts = append(verdicts, "ok")
						case errors.As(err, &coreErr):
							verdicts = append(verdicts, "com.apple.foundationdb.record.RecordCoreException")
						default:
							verdicts = append(verdicts, err.Error())
						}
					}
					Expect(verdicts).To(Equal(want), "Go")

					var goKVs [][]string
					_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						goKVs = [][]string{}
						if orderedVersionPrimaryKey {
							builder := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS)
							if format != 0 {
								builder.SetFormatVersion(format)
							}
							store, err := builder.Open()
							if err != nil {
								return nil, err
							}
							for _, b := range []byte{1, 9} {
								version, err := recordlayer.CompleteVersionFromBytes(complete(b))
								Expect(err).NotTo(HaveOccurred())
								evaluated, err := orderedVersion.Evaluate(&recordlayer.FDBStoredRecord[proto.Message]{Version: version}, nil)
								Expect(err).NotTo(HaveOccurred())
								Expect(evaluated).To(HaveLen(1))
								Expect(evaluated[0]).To(HaveLen(1))
								key := tuple.Tuple{evaluated[0][0], int64(1)}
								loaded, err := store.LoadRecord(key)
								Expect(err).NotTo(HaveOccurred())
								Expect(loaded).NotTo(BeNil(), "both ordered versions of id 1 must survive")
								Expect(loaded.PrimaryKey).To(Equal(key))
							}
						}
						for _, space := range []int64{1, 2, 4, 8} {
							kvs, err := rtx.Transaction().GetRange(goSS.Sub(space), fdb.RangeOptions{}).GetSliceWithError()
							if err != nil {
								return nil, err
							}
							for _, kv := range kvs {
								goKVs = append(goKVs, []string{hex.EncodeToString(kv.Key[len(goSS.Bytes()):]), hex.EncodeToString(kv.Value)})
							}
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(java.KVs).NotTo(BeEmpty())
					Expect(goKVs).To(Equal(java.KVs))
				})
			}
		}
	}
})
