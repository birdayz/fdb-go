package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("RecordExists_BasicFunctionality", func() {
	var recordMetaData *RecordMetaData

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		recordMetaData, buildErr = metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("NonExistentRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(tuple.Tuple{int64(99999)}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ExistingRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save a record
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(50),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Check if it exists
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(tuple.Tuple{int64(1001)}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeletedRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save a record
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(1002),
				Price:   proto.Int32(75),
				Flower: &gen.Flower{
					Type:  proto.String("Tulip"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Delete the record
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1002)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Check it no longer exists
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			exists, err := store.RecordExists(tuple.Tuple{int64(1002)}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("RecordExistenceCheck_ErrorIfExists", func() {
	var recordMetaData *RecordMetaData

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		recordMetaData, buildErr = metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("NewRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(2001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Daisy"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfExists)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ExistingRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// First save a record
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(2001),
				Price:   proto.Int32(100),
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to save again with ErrorIfExists
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(2001),
				Price:   proto.Int32(200),
				Flower: &gen.Flower{
					Type:  proto.String("Lily"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())
			var existsErr *RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("InsertRecord", func() {
	var recordMetaData *RecordMetaData

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		recordMetaData, buildErr = metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("NewRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(150),
				Flower: &gen.Flower{
					Type:  proto.String("Orchid"),
					Color: gen.Color_PINK.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ExistingRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Insert a record first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(150),
			}
			_, err = store.InsertRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to insert again - should fail
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(250),
				Flower: &gen.Flower{
					Type:  proto.String("Carnation"),
					Color: gen.Color_RED.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			Expect(err).To(HaveOccurred())
			var existsErr *RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("UpdateRecord", func() {
	var recordMetaData *RecordMetaData

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		recordMetaData, buildErr = metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("NonExistentRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(4001),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Iris"),
					Color: gen.Color_BLUE.Enum(),
				},
			}

			_, err = store.UpdateRecord(order)
			Expect(err).To(HaveOccurred())
			var notExistErr *RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ExistingRecord", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Insert a record first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(4002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Peony"),
					Color: gen.Color_PINK.Enum(),
				},
			}

			_, err = store.InsertRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Update it
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(4002),
				Price:   proto.Int32(200),
				Flower: &gen.Flower{
					Type:  proto.String("Peony"),
					Color: gen.Color_RED.Enum(),
				},
			}

			_, err = store.UpdateRecord(order)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify the update
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(4002)})
			Expect(err).NotTo(HaveOccurred())
			Expect(storedRecord).NotTo(BeNil())

			order := storedRecord.Record.(*gen.Order)
			Expect(*order.Price).To(Equal(int32(200)))
			Expect(*order.Flower.Color).To(Equal(gen.Color_RED))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("RecordExistenceCheck_ErrorIfTypeChanged", func() {
	var recordMetaData *RecordMetaData

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		recordMetaData, buildErr = metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("DifferentTypeReturnsError", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save an Order with order_id = 5001
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5001),
				Price:   proto.Int32(42),
				Flower: &gen.Flower{
					Type:  proto.String("Tulip"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to save a Customer with same primary key using ErrorIfTypeChanged
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			customer := &gen.Customer{
				CustomerId: proto.Int64(5001),
				Name:       proto.String("Alice"),
				Email:      proto.String("alice@example.com"),
			}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckErrorIfTypeChanged)
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var typeChangedErr *RecordTypeChangedError
		Expect(errors.As(err, &typeChangedErr)).To(BeTrue())
		Expect(typeChangedErr.ActualType).To(Equal("Order"))
		Expect(typeChangedErr.ExpectedType).To(Equal("Customer"))
	})

	It("SameTypeSucceeds", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5002),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Save same type again with ErrorIfTypeChanged - should succeed
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5002),
				Price:   proto.Int32(200),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_BLUE.Enum(),
				},
			}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfTypeChanged)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("NotExistsOrTypeChanged_NonExistent", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5003),
				Price:   proto.Int32(50),
			}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var notExistsErr *RecordDoesNotExistError
		Expect(errors.As(err, &notExistsErr)).To(BeTrue())
	})

	It("NotExistsOrTypeChanged_DifferentType", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save an Order first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5004),
				Price:   proto.Int32(75),
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Try to save Customer with same primary key using ErrorIfNotExistsOrTypeChanged
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			customer := &gen.Customer{
				CustomerId: proto.Int64(5004),
				Name:       proto.String("Bob"),
			}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			return nil, err
		})

		Expect(err).To(HaveOccurred())
		var typeChangedErr *RecordTypeChangedError
		Expect(errors.As(err, &typeChangedErr)).To(BeTrue())
	})

	It("NoExistingRecord_Succeeds", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			order := &gen.Order{
				OrderId: proto.Int64(5099),
				Price:   proto.Int32(999),
			}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfTypeChanged)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
