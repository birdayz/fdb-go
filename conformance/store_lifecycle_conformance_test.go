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
