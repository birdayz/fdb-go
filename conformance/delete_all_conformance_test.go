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

var _ = Describe("DeleteAllRecords Conformance", func() {
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

		tenantName := fmt.Sprintf("da_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		db = env.RecordDB
		java = NewJavaInvoker()

		keyspace = subspace.Sub(tuple.Tuple{})

		// Build metadata matching Java's createIndexedMetaData()
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

	saveOrderGo := func(orderID int64, price int32) {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(orderID),
				Price:   proto.Int32(price),
			})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteAllRecordsGo := func() {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).Open()
			if err != nil {
				return nil, err
			}
			return nil, store.DeleteAllRecords()
		})
		Expect(err).NotTo(HaveOccurred())
	}

	countRecordsGo := func() int {
		var count int
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).Open()
			if err != nil {
				return nil, err
			}
			records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
			if err != nil {
				return nil, err
			}
			count = len(records)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return count
	}

	scanIndexGo := func() int {
		var count int
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).Open()
			if err != nil {
				return nil, err
			}
			entries, err := recordlayer.AsList(ctx, store.ScanIndex(priceIdx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
			if err != nil {
				return nil, err
			}
			count = len(entries)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return count
	}

	Describe("Go saves, Go deletes, Java confirms empty", func() {
		It("should clear all records and index entries", func() {
			// Go saves 3 orders with index
			saveOrderGo(1, 100)
			saveOrderGo(2, 200)
			saveOrderGo(3, 300)

			// Verify records exist via Java
			params := buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsWithIndex", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(3)))

			// Go deletes all
			deleteAllRecordsGo()

			// Java confirms: 0 records
			Expect(java.InvokeAs(ctx, "countRecordsWithIndex", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(0)))

			// Java confirms: 0 index entries
			params["indexName"] = "Order$price"
			var indexEntries []map[string]any
			Expect(java.InvokeAs(ctx, "scanIndex", params, &indexEntries)).To(Succeed())
			Expect(indexEntries).To(BeEmpty())
		})
	})

	Describe("Java saves, Java deletes, Go confirms empty", func() {
		It("should clear all records and index entries", func() {
			// Java saves 3 orders
			for i, price := range []int32{100, 200, 300} {
				params := buildJavaParams()
				params["order"] = &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				}
				Expect(java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)).To(Succeed())
			}

			// Verify records exist via Go
			Expect(countRecordsGo()).To(Equal(3))

			// Java deletes all
			params := buildJavaParams()
			Expect(java.InvokeAs(ctx, "deleteAllRecordsWithIndex", params, nil)).To(Succeed())

			// Go confirms: 0 records
			Expect(countRecordsGo()).To(Equal(0))

			// Go confirms: 0 index entries
			Expect(scanIndexGo()).To(Equal(0))
		})
	})

	Describe("Cross-write then Go deletes, Java confirms empty", func() {
		It("should clear records written by both implementations", func() {
			// Go saves order 1
			saveOrderGo(1, 100)

			// Java saves order 2
			params := buildJavaParams()
			params["order"] = &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			}
			Expect(java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)).To(Succeed())

			// Both confirm 2 records
			Expect(countRecordsGo()).To(Equal(2))
			params = buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsWithIndex", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(2)))

			// Go deletes all
			deleteAllRecordsGo()

			// Java confirms empty
			Expect(java.InvokeAs(ctx, "countRecordsWithIndex", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(0)))

			// Go confirms empty
			Expect(countRecordsGo()).To(Equal(0))
			Expect(scanIndexGo()).To(Equal(0))
		})
	})

	Describe("Delete then re-save works cross-platform", func() {
		It("should allow saving after DeleteAllRecords", func() {
			// Go saves
			saveOrderGo(1, 100)

			// Go deletes all
			deleteAllRecordsGo()

			// Java saves new records into the cleared store
			params := buildJavaParams()
			params["order"] = &gen.Order{
				OrderId: proto.Int64(10),
				Price:   proto.Int32(999),
			}
			Expect(java.InvokeAs(ctx, "saveOrderWithIndex", params, nil)).To(Succeed())

			// Go reads the Java-written record
			Expect(countRecordsGo()).To(Equal(1))
			Expect(scanIndexGo()).To(Equal(1))
		})
	})
})
