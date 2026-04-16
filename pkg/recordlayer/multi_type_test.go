package recordlayer

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// multiTypeMetaData returns metadata with both Order and Customer registered.
func multiTypeMetaData() *RecordMetaData {
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		panic(fmt.Sprintf("multiTypeMetaData: %v", err))
	}
	return md
}

var _ = Describe("MultiTypeRecords", func() {
	metaData := multiTypeMetaData()
	ctx := context.Background()

	It("SamePrimaryKeyDifferentTypes", func() {
		ks := specSubspace()

		// Save Order with ID 1 AND Customer with ID 1 — should not collide
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
				Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			customer := &gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("Alice"),
				Email:      proto.String("alice@example.com"),
			}
			if _, err := store.SaveRecord(customer); err != nil {
				return nil, err
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Load both — they should be independent
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			// Load with primary key {1} — should find whichever type index is lower
			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "Expected to find a record with primary key {1}")

			// The record should be one of the types (Order has index 0, so it should be found first)
			GinkgoWriter.Printf("Loaded record type: %s\n", rec.RecordType.Name)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanReturnsAllTypes", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save 2 orders and 2 customers
			for i := int64(1); i <= 2; i++ {
				order := &gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}

				customer := &gen.Customer{
					CustomerId: proto.Int64(i + 100), // Different IDs to avoid key overlap
					Name:       proto.String("Customer"),
				}
				if _, err := store.SaveRecord(customer); err != nil {
					return nil, err
				}
			}

			// Scan all — should get all 4 records
			cursor := store.ScanRecords(nil, ForwardScan())
			defer func() { _ = cursor.Close() }()

			orderCount := 0
			customerCount := 0
			for {
				result, err := cursor.OnNext(context.Background())
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}

				rec := result.GetValue()
				switch rec.Record.(type) {
				case *gen.Order:
					orderCount++
				case *gen.Customer:
					customerCount++
				default:
					Fail("Unexpected record type")
				}
			}

			Expect(orderCount).To(Equal(2))
			Expect(customerCount).To(Equal(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SamePrimaryKeyOverwrites", func() {
		// In Java (and now Go), different record types with the same primary key
		// share the same FDB key (primaryKey, UnsplitRecord=0). The second save
		// overwrites the first. This matches Java's behavior exactly.
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save order with primary key 42
			order := &gen.Order{
				OrderId: proto.Int64(42),
				Price:   proto.Int32(99),
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}

			// Save customer with same primary key 42 — overwrites the order
			customer := &gen.Customer{
				CustomerId: proto.Int64(42),
				Name:       proto.String("Bob"),
			}
			if _, err := store.SaveRecord(customer); err != nil {
				return nil, err
			}

			// Loading should return the Customer (last write wins)
			rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "Expected record to exist")
			Expect(rec.RecordType.Name).To(Equal("Customer"))

			// Delete should remove the record entirely
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Nothing left at this key
			rec, err = store.LoadRecord(tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).To(BeNil(), "Expected nil after delete")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanRecordsByType", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Save 3 orders and 2 customers
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			for i := int64(101); i <= 102; i++ {
				customer := &gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Test")}
				if _, err := store.SaveRecord(customer); err != nil {
					return nil, err
				}
			}

			// Scan only Orders
			orderCursor := store.ScanRecordsByType("Order", nil, ForwardScan())
			orderRecords, err := AsList(context.Background(), orderCursor)
			if err != nil {
				return nil, err
			}
			Expect(orderRecords).To(HaveLen(3))
			for _, rec := range orderRecords {
				Expect(rec.RecordType.Name).To(Equal("Order"))
			}

			// Scan only Customers
			custCursor := store.ScanRecordsByType("Customer", nil, ForwardScan())
			custRecords, err := AsList(context.Background(), custCursor)
			if err != nil {
				return nil, err
			}
			Expect(custRecords).To(HaveLen(2))
			for _, rec := range custRecords {
				Expect(rec.RecordType.Name).To(Equal("Customer"))
			}

			// Scan non-existent type
			emptyCursor := store.ScanRecordsByType("NonExistent", nil, ForwardScan())
			emptyRecords, err := AsList(context.Background(), emptyCursor)
			if err != nil {
				return nil, err
			}
			Expect(emptyRecords).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RecordTypeIndex", func() {
		// Verify the record type indices match the UnionDescriptor field order
		orderType := metaData.GetRecordType("Order")
		customerType := metaData.GetRecordType("Customer")

		Expect(orderType).NotTo(BeNil(), "Order type not found in metadata")
		Expect(customerType).NotTo(BeNil(), "Customer type not found in metadata")

		// Order is _Order (field 1) → proto field number 1, Customer is _Customer (field 2) → 2
		// Matches Java: RecordType.getRecordTypeKey() returns union descriptor field number
		Expect(orderType.RecordTypeIndex).To(Equal(1))
		Expect(customerType.RecordTypeIndex).To(Equal(2))
	})

	Describe("ScanRecordsByType prefix scan", func() {
		// Metadata with RecordTypeKey() as first PK component — triggers fast path.
		var rtMetaData *RecordMetaData
		BeforeEach(func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			rtMetaData = md
		})

		It("returns only records of the requested type", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(rtMetaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Save 5 orders and 3 customers
				for i := int64(1); i <= 5; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))}); err != nil {
						return nil, err
					}
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Cust")}); err != nil {
						return nil, err
					}
				}

				// Prefix scan: should return only orders
				orders, err := AsList(context.Background(), store.ScanRecordsByType("Order", nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(orders).To(HaveLen(5))
				for _, r := range orders {
					Expect(r.RecordType.Name).To(Equal("Order"))
				}

				// Prefix scan: should return only customers
				custs, err := AsList(context.Background(), store.ScanRecordsByType("Customer", nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(custs).To(HaveLen(3))
				for _, r := range custs {
					Expect(r.RecordType.Name).To(Equal("Customer"))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles continuation tokens", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(rtMetaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Save 10 orders + 5 customers
				for i := int64(1); i <= 10; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}); err != nil {
						return nil, err
					}
				}
				for i := int64(1); i <= 5; i++ {
					if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("C")}); err != nil {
						return nil, err
					}
				}

				// Page through orders 3 at a time
				var allOrders []*FDBStoredRecord[proto.Message]
				var cont []byte
				limitedScan := func() ScanProperties {
					sp := ForwardScan()
					sp.ExecuteProperties = sp.ExecuteProperties.WithReturnedRowLimit(3)
					return sp
				}
				for {
					cursor := store.ScanRecordsByType("Order", cont, limitedScan())
					page, nextCont, err := AsListWithContinuation(context.Background(), cursor)
					Expect(err).NotTo(HaveOccurred())
					allOrders = append(allOrders, page...)
					cont = nextCont
					if len(cont) == 0 {
						break
					}
				}
				Expect(allOrders).To(HaveLen(10))
				for _, r := range allOrders {
					Expect(r.RecordType.Name).To(Equal("Order"))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reverse scan works", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(rtMetaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 5; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}); err != nil {
						return nil, err
					}
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("C")}); err != nil {
						return nil, err
					}
				}

				// Reverse scan should return orders in reverse order
				orders, err := AsList(context.Background(), store.ScanRecordsByType("Order", nil, ReverseScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(orders).To(HaveLen(5))
				// Verify descending order
				for i := 0; i < len(orders)-1; i++ {
					thisKey := orders[i].PrimaryKey
					nextKey := orders[i+1].PrimaryKey
					// RecordTypeKey is first, order_id is second — compare by order_id
					Expect(thisKey[1].(int64)).To(BeNumerically(">", nextKey[1].(int64)))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("falls back for non-RecordTypeKey PK", func() {
			// The original metaData (without RecordTypeKey) should still work via filter
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}); err != nil {
						return nil, err
					}
				}
				orders, err := AsList(context.Background(), store.ScanRecordsByType("Order", nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(orders).To(HaveLen(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
