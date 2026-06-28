//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

var _ = Describe("DeleteRecordsWhere Conformance", func() {
	var (
		ctx      context.Context
		env      *TenantEnvironment
		java     *JavaInvoker
		db       *recordlayer.FDBDatabase
		keyspace subspace.Subspace
		priceIdx *recordlayer.Index
		md       *recordlayer.RecordMetaData
	)

	// Record type keys from UnionDescriptor: _Order=1, _Customer=2
	const orderTypeKey int64 = 1
	const customerTypeKey int64 = 2

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("drw_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		db = env.RecordDB
		java = NewJavaInvoker()

		keyspace = subspace.Sub(tuple.Tuple{})

		// Build metadata with type-prefixed PKs matching Java's createTypePrefixedMetaData()
		priceIdx = recordlayer.NewIndex("Order$price_tp", recordlayer.Field("price"))
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(
			recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(
			recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(
			recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("id")))
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

	openStore := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(keyspace).CreateOrOpen()
	}

	saveOrderGo := func(orderID int64, price int32) {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx)
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

	saveCustomerGo := func(customerID int64, name string) {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx)
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(customerID),
				Name:       proto.String(name),
			})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteRecordsWhereGo := func(typeKey int64) {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx)
			if err != nil {
				return nil, err
			}
			return nil, store.DeleteRecordsWhere(tuple.Tuple{typeKey})
		})
		Expect(err).NotTo(HaveOccurred())
	}

	countRecordsGo := func() int {
		var count int
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := openStore(rtx)
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
			store, err := openStore(rtx)
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

	Describe("Go deletes Orders, Java verifies Customers survive", func() {
		It("should clear Order records and index entries while preserving Customers", func() {
			// Go saves 3 Orders and 2 Customers
			saveOrderGo(1, 100)
			saveOrderGo(2, 200)
			saveOrderGo(3, 300)
			saveCustomerGo(10, "Alice")
			saveCustomerGo(20, "Bob")

			// Java confirms all records present
			params := buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsTypePrefixed", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(5)))

			// Go deletes all Order records
			deleteRecordsWhereGo(orderTypeKey)

			// Java confirms: 2 records remain (Customers only)
			Expect(java.InvokeAs(ctx, "countRecordsTypePrefixed", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(2)))

			// Java confirms Customers survived
			params["customerId"] = int64(10)
			var customer gen.Customer
			Expect(java.InvokeAs(ctx, "loadCustomerTypePrefixed", params, &customer)).To(Succeed())
			Expect(customer.GetName()).To(Equal("Alice"))

			// Java confirms Order is gone
			params["orderId"] = int64(1)
			var nilOrder *gen.Order
			err := java.InvokeAs(ctx, "loadOrderTypePrefixed", params, &nilOrder)
			// loadOrderTypePrefixed returns null for missing records
			Expect(err).NotTo(HaveOccurred())

			// Go confirms: 0 index entries (Order$price_tp cleared)
			Expect(scanIndexGo()).To(Equal(0))
		})
	})

	Describe("Java deletes Orders, Go verifies Customers survive", func() {
		It("should clear Order records and index entries while preserving Customers", func() {
			// Go saves 2 Orders and 1 Customer
			saveOrderGo(1, 100)
			saveOrderGo(2, 200)
			saveCustomerGo(10, "Charlie")

			// Java deletes Order records
			params := buildJavaParams()
			params["recordType"] = "Order"
			Expect(java.InvokeAs(ctx, "deleteRecordsWhereType", params, nil)).To(Succeed())

			// Go confirms: 1 record remains (Customer only)
			Expect(countRecordsGo()).To(Equal(1))

			// Go confirms: 0 index entries
			Expect(scanIndexGo()).To(Equal(0))

			// Go loads the surviving Customer
			var loadedCustomer *gen.Customer
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rtx)
				if err != nil {
					return nil, err
				}
				rec, err := store.LoadRecord(tuple.Tuple{customerTypeKey, int64(10)})
				if err != nil {
					return nil, err
				}
				Expect(rec).NotTo(BeNil())
				c := rec.Record.(*gen.Customer)
				loadedCustomer = c
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loadedCustomer.GetName()).To(Equal("Charlie"))
		})
	})

	Describe("Go saves, Java deletes, Go re-inserts, Java reads", func() {
		It("should support delete-then-reinsert across implementations", func() {
			// Go saves 2 Orders
			saveOrderGo(1, 100)
			saveOrderGo(2, 200)

			// Java deletes all Order records
			params := buildJavaParams()
			params["recordType"] = "Order"
			Expect(java.InvokeAs(ctx, "deleteRecordsWhereType", params, nil)).To(Succeed())

			// Go confirms empty
			Expect(countRecordsGo()).To(Equal(0))
			Expect(scanIndexGo()).To(Equal(0))

			// Go re-inserts new orders
			saveOrderGo(5, 500)
			saveOrderGo(6, 600)

			// Java reads the new orders
			params = buildJavaParams()
			params["orderId"] = int64(5)
			var order gen.Order
			Expect(java.InvokeAs(ctx, "loadOrderTypePrefixed", params, &order)).To(Succeed())
			Expect(order.GetPrice()).To(Equal(int32(500)))

			// Java confirms 2 records
			params = buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsTypePrefixed", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(2)))

			// Java confirms 2 index entries
			params["indexName"] = "Order$price_tp"
			var indexEntries []map[string]any
			Expect(java.InvokeAs(ctx, "scanIndexTypePrefixed", params, &indexEntries)).To(Succeed())
			Expect(indexEntries).To(HaveLen(2))
		})
	})

	Describe("Java saves, Go deletes, Java verifies", func() {
		It("should clear Java-written records and index entries", func() {
			// Java saves 3 orders
			for i, price := range []int32{100, 200, 300} {
				params := buildJavaParams()
				params["order"] = &gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(price),
				}
				Expect(java.InvokeAs(ctx, "saveOrderTypePrefixed", params, nil)).To(Succeed())
			}

			// Java saves 1 customer
			params := buildJavaParams()
			params["customer"] = &gen.Customer{
				CustomerId: proto.Int64(99),
				Name:       proto.String("Diana"),
			}
			Expect(java.InvokeAs(ctx, "saveCustomerTypePrefixed", params, nil)).To(Succeed())

			// Go deletes Order records
			deleteRecordsWhereGo(orderTypeKey)

			// Java confirms: 1 record (Customer)
			params = buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsTypePrefixed", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(1)))

			// Java confirms: 0 index entries
			params["indexName"] = "Order$price_tp"
			var indexEntries []map[string]any
			Expect(java.InvokeAs(ctx, "scanIndexTypePrefixed", params, &indexEntries)).To(Succeed())
			Expect(indexEntries).To(BeEmpty())

			// Java confirms Customer survived
			params = buildJavaParams()
			params["customerId"] = int64(99)
			var customer gen.Customer
			Expect(java.InvokeAs(ctx, "loadCustomerTypePrefixed", params, &customer)).To(Succeed())
			Expect(customer.GetName()).To(Equal("Diana"))
		})
	})

	Describe("Mixed writes then cross-delete", func() {
		It("should handle interleaved Go/Java saves with Go delete", func() {
			// Go saves order 1
			saveOrderGo(1, 100)

			// Java saves order 2
			params := buildJavaParams()
			params["order"] = &gen.Order{
				OrderId: proto.Int64(2),
				Price:   proto.Int32(200),
			}
			Expect(java.InvokeAs(ctx, "saveOrderTypePrefixed", params, nil)).To(Succeed())

			// Go saves customer 10
			saveCustomerGo(10, "Eve")

			// Java saves customer 20
			params = buildJavaParams()
			params["customer"] = &gen.Customer{
				CustomerId: proto.Int64(20),
				Name:       proto.String("Frank"),
			}
			Expect(java.InvokeAs(ctx, "saveCustomerTypePrefixed", params, nil)).To(Succeed())

			// Verify total: 4 records
			Expect(countRecordsGo()).To(Equal(4))

			// Go deletes all Orders
			deleteRecordsWhereGo(orderTypeKey)

			// Both confirm: 2 remaining (both Customers)
			Expect(countRecordsGo()).To(Equal(2))

			params = buildJavaParams()
			var javaCount int64
			Expect(java.InvokeAs(ctx, "countRecordsTypePrefixed", params, &javaCount)).To(Succeed())
			Expect(javaCount).To(Equal(int64(2)))

			// Index fully cleared
			Expect(scanIndexGo()).To(Equal(0))
		})
	})
})
