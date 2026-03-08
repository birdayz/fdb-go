package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// Java equivalent: FDBRecordStoreCrudTest.writeNotUnionType()
var _ = Describe("SaveRecord_NotInUnion", func() {
	var (
		recordMetaData *RecordMetaData
	)

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		recordMetaData = metaDataBuilder.Build()
	})

	It("OrderInUnion", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(100),
			}

			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InvalidTypeNotInUnion", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Flower is not a record type in the union
			flower := &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			}

			_, err = store.SaveRecord(flower)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("GetRecordType_Unknown", func() {
		unknownType := recordMetaData.GetRecordType("NonExistentType")
		Expect(unknownType).To(BeNil())

		flowerType := recordMetaData.GetRecordType("Flower")
		Expect(flowerType).To(BeNil())
	})
})

var _ = Describe("LoadRecord_InvalidRecordTypeKey", func() {
	var (
		recordMetaData *RecordMetaData
	)

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		recordMetaData = metaDataBuilder.Build()
	})

	It("LoadWithInvalidTypeIndex", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save a valid record first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
				Price:   proto.Int32(200),
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Manually write a key with an invalid/unknown record type index
		_, err = sharedDB.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
			invalidTypeIndex := int64(999)
			primaryKey := int64(3001)

			recordsSubspace := keyspace.Sub(RecordKey)
			key := recordsSubspace.Pack(tuple.Tuple{invalidTypeIndex, primaryKey})

			dummyData := []byte{0x08, 0x01} // Minimal valid protobuf
			tr.Set(key, dummyData)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now try to load this record - should not panic
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Try to load - should return nil or error gracefully, NOT panic
			_, _ = store.LoadRecord(tuple.Tuple{int64(3001)})

			return nil, nil
		})
		// Transaction should complete without panic; error is acceptable
	})
})

var _ = Describe("RecordExists_InvalidRecordTypeKey", func() {
	It("handles invalid type index gracefully", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		recordMetaData := metaDataBuilder.Build()

		// Manually write a key with invalid type index
		_, err := sharedDB.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
			invalidTypeIndex := int64(888)
			primaryKey := int64(4001)

			recordsSubspace := keyspace.Sub(RecordKey)
			key := recordsSubspace.Pack(tuple.Tuple{invalidTypeIndex, primaryKey})

			dummyData := []byte{0x08, 0x01}
			tr.Set(key, dummyData)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// RecordExists should handle invalid type index gracefully (not panic)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			_, _ = store.RecordExists(tuple.Tuple{int64(4001)}, IsolationLevelSerializable)

			// Should NOT panic
			return nil, nil
		})
		// Transaction should complete without panic; error is acceptable
	})
})

var _ = Describe("UnionDescriptor_Validation", func() {
	var (
		recordMetaData *RecordMetaData
	)

	BeforeEach(func() {
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		recordMetaData = metaDataBuilder.Build()
	})

	It("ValidateMessageIsInUnion", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(5001),
				Price:   proto.Int32(500),
			}

			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RejectMessageNotInUnion", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(keyspace).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			flower := &gen.Flower{
				Type:  proto.String("Tulip"),
				Color: gen.Color_BLUE.Enum(),
			}

			_, err = store.SaveRecord(flower)
			Expect(err).To(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
