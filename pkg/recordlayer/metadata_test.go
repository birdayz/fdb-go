package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// Java equivalent: FDBRecordStoreCrudTest.writeNotUnionType()
var _ = Describe("SaveRecord_NotInUnion", func() {
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

	It("OrderInUnion", func() {
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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

	It("LoadWithInvalidTypeIndex", func() {
		ctx := context.Background()
		keyspace := specSubspace()

		// Save a valid record first
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
				Price:   proto.Int32(200),
			}

			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// Manually write a key with an invalid/unknown record type index
		_, err = sharedDB.db.Transact(func(tr fdb.WritableTransaction) (any, error) {
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
		_, _ = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		recordMetaData, buildErr := metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		// Manually write a key with invalid type index
		_, err := sharedDB.db.Transact(func(tr fdb.WritableTransaction) (any, error) {
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
		_, _ = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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

	It("ValidateMessageIsInUnion", func() {
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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

var _ = Describe("RecordMetaDataBuilder_Validation", func() {
	It("Build returns error when primary key is missing", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Intentionally do NOT set any primary keys
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("has no primary key set"))
	})

	It("Build returns error when one record type lacks primary key", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		// Customer and TypedRecord primary keys intentionally not set
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("has no primary key set"))
	})

	It("Build succeeds when all primary keys are set", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(md).NotTo(BeNil())
		Expect(md.GetRecordType("Order")).NotTo(BeNil())
		Expect(md.GetRecordType("Customer")).NotTo(BeNil())
	})

	It("Build rejects primary key with fan-out (createsDuplicates)", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(FanOut("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("create duplicates"))
	})

	It("Build rejects duplicate record type keys", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// Set both types to the same record type key
		builder.GetRecordType("Order").SetRecordTypeKey(42)
		builder.GetRecordType("Customer").SetRecordTypeKey(42)
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("same record type key"))
	})

	It("Build rejects duplicate index subspace keys", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx1 := NewIndex("idx_price", Field("price"))
		idx1.SetSubspaceKey(int64(99))
		idx2 := NewIndex("idx_order_id", Field("order_id"))
		idx2.SetSubspaceKey(int64(99))
		builder.AddIndex("Order", idx1)
		builder.AddIndex("Order", idx2)
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("same subspace key"))
	})

	It("Build rejects former index with addedVersion > removedVersion", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// Add then remove an index to create a FormerIndex
		idx := NewIndex("temp_idx", Field("price"))
		builder.AddIndex("Order", idx)
		builder.RemoveIndex("temp_idx")
		// Corrupt the FormerIndex versions
		fi := builder.GetFormerIndexes()
		Expect(fi).To(HaveLen(1))
		fi[0].AddedVersion = 100
		fi[0].RemovedVersion = 50
		md, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(md).To(BeNil())
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("addedVersion"))
	})
})
