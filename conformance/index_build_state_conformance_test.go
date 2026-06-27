//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Index Build State Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	ks := subspace.Sub(tuple.Tuple{})

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(ks.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	// buildGoMetaData creates metadata WITHOUT index (for saving records before build).
	buildGoMetaData := func() *recordlayer.RecordMetaData {
		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// buildGoIndexedMetaData creates metadata WITH the Order$price VALUE index.
	buildGoIndexedMetaData := func() (*recordlayer.RecordMetaData, *recordlayer.Index) {
		priceIndex := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		b.AddIndex("Order", priceIndex)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, priceIndex
	}

	Describe("Go writes stamp, Java reads it", func() {
		It("Java reads BY_RECORDS stamp written by Go OnlineIndexer", func() {
			tenantName := fmt.Sprintf("bst_go2java_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			mdNoIdx := buildGoMetaData()

			// Save a record without index
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = st.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(100),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Go OnlineIndexer builds the index → writes BY_RECORDS stamp. Build with
			// markReadable=false: Java 4.12 (and now Go, RFC-137) erases the build stamp
			// once the index is readable, so to check cross-engine stamp-FORMAT compatibility
			// we inspect it while the index is still WRITE_ONLY (stamp intact).
			mdIdx, priceIndex := buildGoIndexedMetaData()
			indexer, err := recordlayer.NewOnlineIndexerBuilder().
				SetDatabase(env.RecordDB).
				SetMetaData(mdIdx).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetMarkReadable(false).
				Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java reads the stamp
			params := buildJavaParams()
			var result map[string]any
			err = java.InvokeAs(ctx, "loadIndexingTypeStamp", params, &result)
			Expect(err).NotTo(HaveOccurred())

			Expect(result["exists"]).To(BeTrue(), "stamp should exist")
			Expect(result["method"]).To(Equal("BY_RECORDS"))
			Expect(result["methodNumber"]).To(BeNumerically("==", 1))
		})
	})

	Describe("Java writes stamp, Go reads it", func() {
		It("Go reads BY_RECORDS stamp written by Java", func() {
			tenantName := fmt.Sprintf("bst_java2go_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			mdIdx, priceIndex := buildGoIndexedMetaData()

			// Create the store first so Java can open it
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Java writes a BY_RECORDS stamp
			params := buildJavaParams()
			err = java.InvokeAs(ctx, "saveIndexingTypeStampByRecords", params, nil)
			Expect(err).NotTo(HaveOccurred())

			// Go reads the stamp
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				if err != nil {
					return nil, err
				}
				Expect(stamp).NotTo(BeNil(), "stamp should exist")
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("No stamp before build", func() {
		It("both Go and Java see no stamp on a fresh store", func() {
			tenantName := fmt.Sprintf("bst_none_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			mdIdx, priceIndex := buildGoIndexedMetaData()

			// Create store without building index
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Go: no stamp
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil(), "no stamp should exist before build")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Java: no stamp
			params := buildJavaParams()
			var result map[string]any
			err = java.InvokeAs(ctx, "loadIndexingTypeStamp", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result["exists"]).To(BeFalse(), "Java should see no stamp")
		})
	})

	Describe("Stamp erased after build completes (Java 4.12)", func() {
		It("stamp is erased once the index becomes READABLE, cross-engine", func() {
			tenantName := fmt.Sprintf("bst_erased_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			mdNoIdx := buildGoMetaData()

			// Save a record
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = st.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(500),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Build index
			mdIdx, priceIndex := buildGoIndexedMetaData()
			indexer, err := recordlayer.NewOnlineIndexerBuilder().
				SetDatabase(env.RecordDB).
				SetMetaData(mdIdx).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify index is READABLE
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				Expect(st.IsIndexReadable(priceIndex.Name)).To(BeTrue())

				// Stamp erased once readable — Java 4.12 IndexingBase erases the build
				// bookkeeping after marking readable, and Go now matches (RFC-137).
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil(), "stamp should be erased after the index is readable")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Java reads the same FDB state and also sees no stamp — the erase is
			// cross-engine consistent.
			params := buildJavaParams()
			var result map[string]any
			err = java.InvokeAs(ctx, "loadIndexingTypeStamp", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result["exists"]).To(BeFalse(), "Java should also see no stamp after readable")
		})
	})

	Describe("Stamp cleared on rebuild", func() {
		It("clearAndMarkIndexWriteOnly clears the old stamp", func() {
			tenantName := fmt.Sprintf("bst_clear_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			mdNoIdx := buildGoMetaData()

			// Save a record
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = st.SaveRecord(&gen.Order{
					OrderId: proto.Int64(1),
					Price:   proto.Int32(200),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Build index with markReadable=false so the build stamp survives (Java 4.12
			// erases it once readable, RFC-137); this test exercises
			// clearAndMarkIndexWriteOnly clearing a still-present stamp.
			mdIdx, priceIndex := buildGoIndexedMetaData()
			indexer, err := recordlayer.NewOnlineIndexerBuilder().
				SetDatabase(env.RecordDB).
				SetMetaData(mdIdx).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetMarkReadable(false).
				Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify stamp exists
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// ClearAndMarkIndexWriteOnly clears the stamp (via clearIndexData)
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				st, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				_, err = st.ClearAndMarkIndexWriteOnly(priceIndex.Name)
				if err != nil {
					return nil, err
				}
				// Stamp should be gone now
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil(), "stamp should be cleared by clearAndMarkIndexWriteOnly")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
