//go:build bazelrunfiles

package conformance_test

import (
	"context"
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
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

var _ = Describe("Store Lifecycle Conformance", func() {
	var (
		ctx      context.Context
		env      *TenantEnvironment
		java     *JavaInvoker
		db       *recordlayer.FDBDatabase
		keyspace subspace.Subspace
		priceIdx *recordlayer.Index
		md       *recordlayer.RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("lifecycle_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		db = env.RecordDB
		java = NewJavaInvoker()
		keyspace = subspace.Sub(tuple.Tuple{})

		priceIdx = recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", priceIdx)
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(keyspace.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	It("evolves Go-written union identities in Java and reads Java alias writes in Go", func() {
		old, current := buildUnionInteropSchema(1, false, false), buildUnionInteropSchema(2, true, false)
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(old).SetSubspace(keyspace).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			desc := old.GetRecordType("Alpha").Descriptor
			message := dynamicpb.NewMessage(desc)
			message.Set(desc.Fields().ByName("id"), protoreflect.ValueOfInt64(10))
			message.Set(desc.Fields().ByName("payload"), protoreflect.ValueOfString("retained"))
			message.Set(desc.Fields().ByName("state"), protoreflect.ValueOfEnum(1))
			_, err = store.SaveRecord(message)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		p, err := current.ToProto()
		Expect(err).NotTo(HaveOccurred())
		data, err := proto.Marshal(p)
		Expect(err).NotTo(HaveOccurred())
		params := buildJavaParams()
		params["protoBytes"] = bytesToInts(data)
		var result struct {
			RecordName string `json:"recordName"`
			Payload    string `json:"payload"`
			TypeKey    int64  `json:"typeKey"`
			Tag        int    `json:"tag"`
		}
		Expect(java.InvokeAs(ctx, "evolveUnionRecord", params, &result)).To(Succeed())
		Expect(result.RecordName).To(Equal("Beta"))
		Expect(result.Payload).To(Equal("retained"))
		Expect(result.TypeKey).To(Equal(int64(1)))
		Expect(result.Tag).To(Equal(9))
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(current).SetSubspace(keyspace).Open()
			Expect(err).NotTo(HaveOccurred())
			for _, id := range []int64{10, 11} {
				loaded, err := store.LoadRecord(tuple.Tuple{id})
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).NotTo(BeNil())
				message := loaded.Record.ProtoReflect()
				Expect(message.Descriptor().Name()).To(Equal(protoreflect.Name("Beta")))
				Expect(message.Get(message.Descriptor().Fields().ByName("payload")).String()).To(Equal("retained"))
				Expect(message.Get(message.Descriptor().Fields().ByName("state")).Enum()).To(Equal(protoreflect.EnumNumber(1)))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(current).SetSubspace(keyspace).Open()
			Expect(err).NotTo(HaveOccurred())
			entries, err := recordlayer.AsList(ctx, store.ScanIndex(current.GetIndex("by_payload"), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{"retained", int64(10)}))
			Expect(entries[1].Key).To(Equal(tuple.Tuple{"retained", int64(11)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		fmt.Fprintf(GinkgoWriter, "UNION_FDB_INTEROP old-tag=1 Java-write-tag=%d type=%s key=%d enum=READY index-rows=2\n", result.Tag, result.RecordName, result.TypeKey)
	})

	for _, seedInJava := range []bool{false, true} {
		label := "Go-written"
		if seedInJava {
			label = "Java-written"
		}
		It("resolves "+label+" overlapping-PK uniqueness violations in Java and Go", func() {
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			index := recordlayer.NewIndex("price", recordlayer.Field("price")).SetUnique()
			builder.AddIndex("Order", index)
			metadata, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			if !seedInJava {
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(keyspace).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					_, err = store.MarkIndexWriteOnly(index.Name)
					Expect(err).NotTo(HaveOccurred())
					for id, price := range []int32{100, 100, 100, 200, 200} {
						_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(id + 1)), Price: proto.Int32(price)})
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			params := buildJavaParams()
			params["seed"] = seedInJava
			var result struct {
				Keys   []string `json:"keys"`
				Values []string `json:"values"`
				Counts []int    `json:"counts"`
			}
			Expect(java.InvokeAs(ctx, "probeOverlappingUniqueViolations", params, &result)).To(Succeed())
			var wantKeys []string
			for id, price := range []int64{100, 100, 100, 200, 200} {
				wantKeys = append(wantKeys, hex.EncodeToString(tuple.Tuple{price, price, int64(id + 1)}.Pack()))
			}
			Expect(result.Keys).To(Equal(wantKeys))
			Expect(result.Values).To(HaveLen(5))
			for i, encoded := range result.Values {
				value, err := hex.DecodeString(encoded)
				Expect(err).NotTo(HaveOccurred())
				primaryKey, err := tuple.Unpack(value)
				Expect(err).NotTo(HaveOccurred())
				Expect(primaryKey).To(HaveLen(2), "values contain the full conflicting PK too")
				// Java's asynchronous check completion may choose a different
				// conflicting record. Check the encoded PK, not completion order.
				if i < 3 {
					Expect(primaryKey[0]).To(Equal(int64(100)))
					Expect(primaryKey[1]).To(BeElementOf(int64(1), int64(2), int64(3)))
				} else {
					Expect(primaryKey[0]).To(Equal(int64(200)))
					Expect(primaryKey[1]).To(BeElementOf(int64(4), int64(5)))
				}
				Expect(primaryKey[1]).NotTo(Equal(int64(i + 1)))
			}
			Expect(result.Counts).To(Equal([]int{5, 4, 2}))
			fmt.Fprintf(GinkgoWriter, "OVERLAPPING-PK-VIOLATIONS %s Java=%+v\n", label, result)
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(keyspace).Open()
				Expect(err).NotTo(HaveOccurred())
				violations, err := store.ScanUniquenessViolations(index)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(HaveLen(2))
				Expect(violations[0].PrimaryKey).To(Equal(tuple.Tuple{int64(200), int64(4)}))
				Expect(violations[1].PrimaryKey).To(Equal(tuple.Tuple{int64(200), int64(5)}))
				_, err = store.DeleteRecord(tuple.Tuple{int64(200), int64(5)})
				Expect(err).NotTo(HaveOccurred())
				violations, err = store.ScanUniquenessViolations(index)
				Expect(err).NotTo(HaveOccurred())
				Expect(violations).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("pins Java deletion with a pending replacement retirement callback", func() {
		var result struct {
			RemainingRows int  `json:"remainingRows"`
			HeaderPresent bool `json:"headerPresent"`
			OriginalState int  `json:"originalState"`
		}
		Expect(java.InvokeAs(ctx, "probeDeleteWithPendingReplacementRetirement", buildJavaParams(), &result)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "PENDING-RETIREMENT-DELETE Java=%+v\n", result)
		// The tagged callback retains the old store state and writes DISABLED
		// after deletion. Keep the oracle defect visible, not a parity waiver.
		Expect(result.HeaderPresent).To(BeFalse())
		Expect(result.RemainingRows).To(Equal(1))
		Expect(result.OriginalState).To(Equal(2))
	})

	Describe("DeleteAllRecords preserves store header", func() {
		It("header fields survive DeleteAllRecords and are readable by Java", func() {
			// Go creates store and saves a record
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Read header before delete
			headerStore, err := NewStoreHeaderConformanceStore(db, keyspace, env.ClusterFile, env.TenantName)
			Expect(err).NotTo(HaveOccurred())
			headerBefore, err := headerStore.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Go deletes all records
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).Open()
				if err != nil {
					return nil, err
				}
				return nil, store.DeleteAllRecords()
			})
			Expect(err).NotTo(HaveOccurred())

			// Header survives DeleteAllRecords — Go reads
			headerAfter, err := headerStore.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(headerAfter.FormatVersion).To(Equal(headerBefore.FormatVersion))
			Expect(headerAfter.MetaDataVersion).To(Equal(headerBefore.MetaDataVersion))

			// Java can also read the preserved header
			javaHeader, err := headerStore.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaHeader.FormatVersion).To(Equal(headerAfter.FormatVersion))
			Expect(javaHeader.MetaDataVersion).To(Equal(headerAfter.MetaDataVersion))
		})
	})

	Describe("DeleteAllRecords preserves index state", func() {
		It("index state WRITE_ONLY survives DeleteAllRecords cross-platform", func() {
			// Go creates store
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Go marks index WRITE_ONLY
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).
					SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexWriteOnly("Order$price")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Go saves a record and deletes all
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).
					SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				if err != nil {
					return nil, err
				}
				return nil, store.DeleteAllRecords()
			})
			Expect(err).NotTo(HaveOccurred())

			// Index state should still be WRITE_ONLY — Java reads raw
			idxStore, err := NewIndexStateConformanceStore(db, keyspace, env.ClusterFile, env.TenantName)
			Expect(err).NotTo(HaveOccurred())

			javaState, err := idxStore.GetIndexStateRawJava(ctx, "Order$price")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			goState, err := idxStore.GetIndexStateRawGo(ctx, "Order$price")
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Java deletes all, Go re-creates store and saves", func() {
		It("should allow Go to CreateOrOpen and save after Java DeleteAllRecords", func() {
			// Java saves records
			for i, price := range []int32{100, 200, 300} {
				params := buildJavaParams()
				params["order"] = &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				}
				Expect(java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)).To(Succeed())
			}

			// Java deletes all
			params := buildJavaParams()
			Expect(java.InvokeAs(ctx, "deleteAllRecordsWithIndex", params, nil)).To(Succeed())

			// Go re-creates store (CreateOrOpen on same subspace) and saves new records
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(999)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Java reads the Go-written record
			var javaCount int64
			params = buildJavaParams()
			Expect(java.InvokeAs(ctx, "countRecordsWithIndex", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(1)))

			// Java scans index — should have 1 entry
			params["indexName"] = "Order$price"
			var indexEntries []map[string]any
			Expect(java.InvokeAs(ctx, "scanIndex", params, &indexEntries)).To(Succeed())
			Expect(indexEntries).To(HaveLen(1))
		})
	})
})
