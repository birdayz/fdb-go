//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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

			// Go OnlineIndexer builds the index → writes BY_RECORDS stamp
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

	Describe("Stamp survives after build completes", func() {
		It("stamp persists even after index becomes READABLE", func() {
			tenantName := fmt.Sprintf("bst_persist_%s", uuid.New().String())
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

				// Stamp still present (matching Java: clearReadableIndexBuildData does NOT clear stamp)
				stamp, err := st.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil(), "stamp should persist after build")
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Java also sees the stamp
			params := buildJavaParams()
			var result map[string]any
			err = java.InvokeAs(ctx, "loadIndexingTypeStamp", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result["exists"]).To(BeTrue())
			Expect(result["method"]).To(Equal("BY_RECORDS"))
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
