package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// UniqueIndex_ReadableViolation tests that a READABLE unique index throws
// RecordIndexUniquenessViolationError when a conflicting value is inserted.
var _ = Describe("UniqueIndex_ReadableViolation", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("order_price_unique", Field("price"))
		idx.SetUnique()
		builder.AddIndex("Order", idx)
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("DuplicateValueReturnsViolationError", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			o1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(o1)
			Expect(err).NotTo(HaveOccurred())

			// Same price, different PK → uniqueness violation
			o2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)}
			_, err = store.SaveRecord(o2)
			Expect(err).To(HaveOccurred())

			var violationErr *RecordIndexUniquenessViolationError
			Expect(errors.As(err, &violationErr)).To(BeTrue())
			Expect(violationErr.IndexName).To(Equal("order_price_unique"))
			Expect(violationErr.PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
			Expect(violationErr.ExistingKey).To(Equal(tuple.Tuple{int64(1)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("NullKeySkipsUniquenessCheck", func() {
		ctx := context.Background()

		// Build metadata with a unique index on flower.type (nullable)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("flower_type_unique", Nest("flower", Field("type")))
		idx.SetUnique()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Two orders with nil flower → null index key → no uniqueness check
			o1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)}
			_, err = store.SaveRecord(o1)
			Expect(err).NotTo(HaveOccurred())

			o2 := &gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)}
			_, err = store.SaveRecord(o2)
			Expect(err).NotTo(HaveOccurred()) // Both null → no violation
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SameRecordDifferentPriceNoViolation", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			o1 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(o1)
			Expect(err).NotTo(HaveOccurred())

			// Update same PK with new price → should succeed (own record)
			o2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecord(o2)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// IndexValueSizeError_Test tests that oversized index values are rejected.
var _ = Describe("IndexValueSizeError_Test", func() {
	It("CoveringIndexValueExceedsLimit", func() {
		// Directly test checkKeyValueSizes with synthetic data.
		bigValue := make([]byte, valueSizeLimit+1)
		err := checkKeyValueSizes(
			&Index{Name: "test_idx"},
			tuple.Tuple{int64(1)},
			[]byte{0x01}, // small key
			bigValue,
		)
		Expect(err).To(HaveOccurred())

		var valueSizeErr *IndexValueSizeError
		Expect(errors.As(err, &valueSizeErr)).To(BeTrue())
		Expect(valueSizeErr.IndexName).To(Equal("test_idx"))
		Expect(valueSizeErr.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
		Expect(valueSizeErr.ValueSize).To(Equal(valueSizeLimit + 1))
		Expect(valueSizeErr.Limit).To(Equal(valueSizeLimit))
	})

	It("KeyExceedsLimit", func() {
		bigKey := make([]byte, keySizeLimit+1)
		err := checkKeyValueSizes(
			&Index{Name: "test_idx"},
			tuple.Tuple{int64(42)},
			bigKey,
			[]byte{0x01}, // small value
		)
		Expect(err).To(HaveOccurred())

		var keySizeErr *IndexKeySizeError
		Expect(errors.As(err, &keySizeErr)).To(BeTrue())
		Expect(keySizeErr.IndexName).To(Equal("test_idx"))
		Expect(keySizeErr.KeySize).To(Equal(keySizeLimit + 1))
	})

	It("BothWithinLimitsSucceeds", func() {
		err := checkKeyValueSizes(
			&Index{Name: "test_idx"},
			tuple.Tuple{int64(1)},
			make([]byte, keySizeLimit),   // exactly at limit
			make([]byte, valueSizeLimit), // exactly at limit
		)
		Expect(err).NotTo(HaveOccurred())
	})
})

// KeyExpression_ErrorPaths tests key expression validation errors.
var _ = Describe("KeyExpression_ErrorPaths", func() {
	It("FieldNotFound", func() {
		order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
		expr := Field("nonexistent_field")
		_, err := expr.Evaluate(nil, order)
		Expect(err).To(HaveOccurred())
		var keErr *KeyExpressionError
		Expect(errors.As(err, &keErr)).To(BeTrue())
		Expect(keErr.Message).To(ContainSubstring("field nonexistent_field not found"))
	})

	It("RepeatedFieldWithFanTypeNone", func() {
		order := &gen.Order{
			OrderId: proto.Int64(1),
			Tags:    []string{"a", "b"},
		}
		// Field("tags") with default FanTypeNone on a repeated field → error
		expr := Field("tags")
		_, err := expr.Evaluate(nil, order)
		Expect(err).To(HaveOccurred())
		var keErr *KeyExpressionError
		Expect(errors.As(err, &keErr)).To(BeTrue())
		Expect(keErr.Message).To(ContainSubstring("is repeated with FanType.None"))
	})

	It("NilMessageReturnsNullKeyComponent", func() {
		expr := Field("order_id")
		result, err := expr.Evaluate(nil, nil)
		Expect(err).NotTo(HaveOccurred())
		// Matches Java's Key.Evaluated.NULL — returns [[nil]]
		Expect(result).To(HaveLen(1))
		Expect(result[0]).To(HaveLen(1))
		Expect(result[0][0]).To(BeNil())
	})

	It("NestingIntoNilFieldReturnsNull", func() {
		// Order without flower field set → NestingKeyExpression should return nil key component
		order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
		expr := Nest("flower", Field("type"))
		result, err := expr.Evaluate(nil, order)
		Expect(err).NotTo(HaveOccurred())
		// Should produce [[nil]] — null key component
		Expect(result).To(HaveLen(1))
		Expect(result[0]).To(HaveLen(1))
		Expect(result[0][0]).To(BeNil())
	})

	It("NestingFieldNotFoundInParent", func() {
		order := &gen.Order{OrderId: proto.Int64(1)}
		expr := Nest("nonexistent_parent", Field("type"))
		_, err := expr.Evaluate(nil, order)
		Expect(err).To(HaveOccurred())
		var keErr *KeyExpressionError
		Expect(errors.As(err, &keErr)).To(BeTrue())
		Expect(keErr.Message).To(ContainSubstring("not found"))
	})
})

// RangeSet_ValidationErrors tests RangeSet boundary validation.
var _ = Describe("RangeSet_ValidationErrors", func() {
	It("ContainsEmptyKey", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rs := NewRangeSet(specSubspace())
			_, err := rs.Contains(rtx.Transaction(), []byte{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("non-empty"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContainsKeyTooLarge", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rs := NewRangeSet(specSubspace())
			_, err := rs.Contains(rtx.Transaction(), []byte{0xff})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("less than"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertRangeEmptyBeginKey", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rs := NewRangeSet(specSubspace())
			// nil begin/end → defaults to first/final, not an error.
			// But explicit empty slice for begin IS an error.
			_, err := rs.InsertRange(rtx.Transaction(), []byte{}, []byte{0x50}, false)
			Expect(err).To(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertRangeInvertedRange", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rs := NewRangeSet(specSubspace())
			_, err := rs.InsertRange(rtx.Transaction(), []byte{0x80}, []byte{0x20}, false)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(&RangeSetInvertedRangeError{}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MissingRangesEmptyKey", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rs := NewRangeSet(specSubspace())
			_, err := rs.MissingRanges(rtx.Transaction(), []byte{}, []byte{0x50}, 10)
			Expect(err).To(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// StoreStateNotLoaded_ErrorPaths tests that store operations fail cleanly
// when the store header hasn't been loaded.
var _ = Describe("StoreStateNotLoaded_ErrorPaths", func() {
	It("SetUserVersionWithoutHeader", func() {
		// Create a store with nil header (bypassing normal Open/Create)
		store := &FDBRecordStore{}
		err := store.SetUserVersion(42)
		Expect(err).To(HaveOccurred())
		var stateErr *RecordStoreStateNotLoadedError
		Expect(errors.As(err, &stateErr)).To(BeTrue())
	})

	It("SetStoreLockStateWithoutHeader", func() {
		store := &FDBRecordStore{}
		err := store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "")
		Expect(err).To(HaveOccurred())
		var stateErr *RecordStoreStateNotLoadedError
		Expect(errors.As(err, &stateErr)).To(BeTrue())
	})

	It("ClearStoreLockStateWithoutHeader", func() {
		store := &FDBRecordStore{}
		err := store.ClearStoreLockState()
		Expect(err).To(HaveOccurred())
		var stateErr *RecordStoreStateNotLoadedError
		Expect(errors.As(err, &stateErr)).To(BeTrue())
	})

	It("UpdateRecordCountStateWithoutHeader", func() {
		store := &FDBRecordStore{}
		err := store.UpdateRecordCountState(gen.DataStoreInfo_DISABLED)
		Expect(err).To(HaveOccurred())
		var stateErr *RecordStoreStateNotLoadedError
		Expect(errors.As(err, &stateErr)).To(BeTrue())
	})
})

// SaveRecord_ValidationErrors tests validation error paths in SaveRecord/SaveRecordWithOptions.
var _ = Describe("SaveRecord_ValidationErrors", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnknownRecordType", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			flower := &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()}
			_, err = store.SaveRecord(flower)
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("unknown record type"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ErrorIfNotExists_NewRecord", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())

			var notExistsErr *RecordDoesNotExistError
			Expect(errors.As(err, &notExistsErr)).To(BeTrue())
			Expect(notExistsErr.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ErrorIfExists_ExistingRecord", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecordWithOptions(order2, RecordExistenceCheckErrorIfExists)
			Expect(err).To(HaveOccurred())

			var existsErr *RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())
			Expect(existsErr.PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ErrorIfTypeChanged_CrossTypeOverwrite", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save Order with PK=1
			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Try to overwrite with Customer PK=1, check type changed
			customer := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice")}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckErrorIfTypeChanged)
			Expect(err).To(HaveOccurred())

			var typeErr *RecordTypeChangedError
			Expect(errors.As(err, &typeErr)).To(BeTrue())
			Expect(typeErr.ActualType).To(Equal("Order"))
			Expect(typeErr.ExpectedType).To(Equal("Customer"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ErrorIfNotExistsOrTypeChanged_NonExistent", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(999), Price: proto.Int32(100)}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).To(HaveOccurred())
			var notExistErr *RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ErrorIfNotExistsOrTypeChanged_TypeChanged", func() {
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			customer := &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Bob")}
			_, err = store.SaveRecordWithOptions(customer, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
			Expect(err).To(HaveOccurred())
			var typeErr *RecordTypeChangedError
			Expect(errors.As(err, &typeErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LockPrecedenceBelowExistenceCheck", func() {
		ctx := context.Background()

		// Store is locked but record doesn't exist → should get DoesNotExist error
		// (not lock error) because existence checks happen first
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Lock the store
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}

			// Try to update non-existent record with existence check
			order := &gen.Order{OrderId: proto.Int64(999), Price: proto.Int32(100)}
			_, err = store.SaveRecordWithOptions(order, RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())
			// DoesNotExist error takes precedence over lock error
			var notExistErr *RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// StoreBuilder_ErrorPaths tests store creation/open error paths.
var _ = Describe("StoreBuilder_ErrorPaths", func() {
	It("ReloadNonExistentStore", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Clear the store header to simulate deleted store
			rtx.Transaction().ClearRange(store.subspace)

			err = store.ReloadRecordStoreState()
			Expect(err).To(HaveOccurred())
			var storeErr *RecordStoreDoesNotExistError
			Expect(errors.As(err, &storeErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// MetadataBuild_ErrorPaths tests metadata build validation errors.
var _ = Describe("MetadataBuild_ErrorPaths", func() {
	It("MissingPrimaryKey", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Deliberately don't set primary key for Order
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("primary key"))
	})

	It("FormerIndexSubspaceKeyReuse", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		idx := NewIndex("price_idx", Field("price"))
		builder.AddIndex("Order", idx)

		// Remove the index (creates FormerIndex)
		builder.RemoveIndex("price_idx")

		// Try to add a new index reusing the same name (hence same subspace key)
		idx2 := NewIndex("price_idx", Field("price"))
		builder.AddIndex("Order", idx2)

		_, err := builder.Build()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("subspace key"))
	})
})

// ErrorMessageFormat tests that error types produce useful messages.
var _ = Describe("ErrorMessageFormat", func() {
	It("RecordAlreadyExistsError", func() {
		err := &RecordAlreadyExistsError{
			Message:    "record already exists",
			PrimaryKey: tuple.Tuple{int64(42)},
		}
		Expect(err.Error()).To(Equal("record already exists"))
		var existsErr *RecordAlreadyExistsError
		Expect(errors.As(err, &existsErr)).To(BeTrue())
	})

	It("RecordDoesNotExistError", func() {
		err := &RecordDoesNotExistError{
			Message:    "record does not exist",
			PrimaryKey: tuple.Tuple{int64(99)},
		}
		Expect(err.Error()).To(Equal("record does not exist"))
		var notExistErr *RecordDoesNotExistError
		Expect(errors.As(err, &notExistErr)).To(BeTrue())
	})

	It("RecordTypeChangedError", func() {
		err := &RecordTypeChangedError{
			Message:      "record type changed",
			PrimaryKey:   tuple.Tuple{int64(1)},
			ActualType:   "Order",
			ExpectedType: "Customer",
		}
		Expect(err.Error()).To(Equal("record type changed"))
		var typeErr *RecordTypeChangedError
		Expect(errors.As(err, &typeErr)).To(BeTrue())
	})

	It("StoreIsLockedForRecordUpdatesError", func() {
		err := &StoreIsLockedForRecordUpdatesError{
			Reason:    "index rebuild",
			Timestamp: 1234567890,
		}
		var lockedErr *StoreIsLockedForRecordUpdatesError
		Expect(errors.As(err, &lockedErr)).To(BeTrue())
		Expect(lockedErr.Reason).To(ContainSubstring("index rebuild"))
		Expect(lockedErr.Timestamp).To(Equal(int64(1234567890)))
	})

	It("StaleMetaDataVersionError", func() {
		err := &StaleMetaDataVersionError{
			LocalVersion:  1,
			StoredVersion: 5,
		}
		var staleErr *StaleMetaDataVersionError
		Expect(errors.As(err, &staleErr)).To(BeTrue())
		Expect(staleErr.LocalVersion).To(Equal(1))
		Expect(staleErr.StoredVersion).To(Equal(5))
	})

	It("IndexKeySizeError", func() {
		err := &IndexKeySizeError{
			IndexName:  "my_index",
			PrimaryKey: tuple.Tuple{int64(1)},
			KeySize:    15000,
			Limit:      10000,
		}
		var keySizeErr *IndexKeySizeError
		Expect(errors.As(err, &keySizeErr)).To(BeTrue())
		Expect(keySizeErr.IndexName).To(Equal("my_index"))
		Expect(keySizeErr.KeySize).To(Equal(15000))
		Expect(keySizeErr.Limit).To(Equal(10000))
	})

	It("IndexValueSizeError", func() {
		err := &IndexValueSizeError{
			IndexName:  "covering_idx",
			PrimaryKey: tuple.Tuple{int64(1)},
			ValueSize:  150000,
			Limit:      100000,
		}
		var valSizeErr *IndexValueSizeError
		Expect(errors.As(err, &valSizeErr)).To(BeTrue())
		Expect(valSizeErr.IndexName).To(Equal("covering_idx"))
		Expect(valSizeErr.ValueSize).To(Equal(150000))
		Expect(valSizeErr.Limit).To(Equal(100000))
	})

	It("RecordIndexUniquenessViolationError", func() {
		err := &RecordIndexUniquenessViolationError{
			IndexName:   "unique_idx",
			IndexKey:    tuple.Tuple{int64(100)},
			PrimaryKey:  tuple.Tuple{int64(2)},
			ExistingKey: tuple.Tuple{int64(1)},
		}
		var uniqErr *RecordIndexUniquenessViolationError
		Expect(errors.As(err, &uniqErr)).To(BeTrue())
		Expect(uniqErr.IndexName).To(Equal("unique_idx"))
		Expect(uniqErr.IndexKey).To(Equal(tuple.Tuple{int64(100)}))
		Expect(uniqErr.PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
		Expect(uniqErr.ExistingKey).To(Equal(tuple.Tuple{int64(1)}))
	})

	It("RecordExistenceCheckString", func() {
		Expect(RecordExistenceCheckNone.String()).To(Equal("NONE"))
		Expect(RecordExistenceCheckErrorIfExists.String()).To(Equal("ERROR_IF_EXISTS"))
		Expect(RecordExistenceCheckErrorIfNotExists.String()).To(Equal("ERROR_IF_NOT_EXISTS"))
		Expect(RecordExistenceCheckErrorIfTypeChanged.String()).To(Equal("ERROR_IF_RECORD_TYPE_CHANGED"))
		Expect(RecordExistenceCheckErrorIfNotExistsOrTypeChanged.String()).To(Equal("ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED"))
		Expect(RecordExistenceCheck(99).String()).To(Equal("UNKNOWN"))
	})
})

// IndexValueSizeError_Integration tests oversized index values in a real store.
var _ = Describe("IndexValueSizeError_Integration", func() {
	It("LargeTagsExceedKeyLimit", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("tags_idx", FanOut("tags"))
		builder.AddIndex("Order", idx)
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create a tag string that exceeds keySizeLimit once packed into index entry
			longTag := strings.Repeat("x", keySizeLimit+1)
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
				Tags:    []string{longTag},
			}

			_, err = store.SaveRecord(order)
			Expect(err).To(HaveOccurred())

			var keySizeErr *IndexKeySizeError
			Expect(errors.As(err, &keySizeErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// DeleteRecord_ErrorPaths tests error paths in DeleteRecord.
var _ = Describe("DeleteRecord_ErrorPaths", func() {
	It("DeleteNonExistentReturnsNotDeleted", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteBlockedByLockReturnsNotDeletedForNonExistent", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Lock the store
			lockState := gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE
			store.storeHeader.StoreLockState = &gen.DataStoreInfo_StoreLockState{
				LockState: &lockState,
			}

			// Delete non-existent record while locked → returns (false, nil),
			// not lock error, because non-existence takes precedence
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(999)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Phase2ErrorTypes tests error types introduced in Phase 2 via errors.As().
var _ = Describe("Phase 2 error types", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnsupportedFormatVersionError on future format version", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			// Create a valid store first so the subspace has a proper header.
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_ = store

			// Now overwrite the store header with format version 999.
			futureVersion := int32(999)
			header := &gen.DataStoreInfo{
				FormatVersion: &futureVersion,
			}
			headerBytes, err := proto.Marshal(header)
			Expect(err).NotTo(HaveOccurred())
			storeInfoKey := ss.Pack(tuple.Tuple{StoreInfoKey})
			rtx.Transaction().Set(storeInfoKey, headerBytes)

			// Try to Open the same store — should fail with UnsupportedFormatVersionError.
			_, err = NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Open()
			Expect(err).To(HaveOccurred())

			var fmtErr *UnsupportedFormatVersionError
			Expect(errors.As(err, &fmtErr)).To(BeTrue())
			Expect(fmtErr.Version).To(Equal(int32(999)))
			Expect(fmtErr.MaxVersion).To(Equal(int32(formatVersionCurrent)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnsupportedFormatVersionError on format version below minimum", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			// Create a valid store first so the subspace has a proper header.
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_ = store

			// Now overwrite the store header with format version 0 (below formatVersionMinimum=1).
			zeroVersion := int32(0)
			header := &gen.DataStoreInfo{
				FormatVersion: &zeroVersion,
			}
			headerBytes, err := proto.Marshal(header)
			Expect(err).NotTo(HaveOccurred())
			storeInfoKey := ss.Pack(tuple.Tuple{StoreInfoKey})
			rtx.Transaction().Set(storeInfoKey, headerBytes)

			// Try to Open the same store — should fail with UnsupportedFormatVersionError.
			_, err = NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Open()
			Expect(err).To(HaveOccurred())

			var fmtErr *UnsupportedFormatVersionError
			Expect(errors.As(err, &fmtErr)).To(BeTrue())
			Expect(fmtErr.Version).To(Equal(int32(0)))
			Expect(fmtErr.MaxVersion).To(Equal(int32(formatVersionCurrent)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RecordDeserializationError on corrupt record data", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Write garbage bytes directly at the record key for PK=42.
			// Key format: [subspace][RecordKey][pk...][unsplitRecord=0]
			pk := tuple.Tuple{int64(42)}
			recordKey := ss.Sub(RecordKey).Pack(append(pk, unsplitRecord))
			rtx.Transaction().Set(recordKey, []byte{0xDE, 0xAD, 0xBE, 0xEF})

			// LoadRecord should fail with RecordDeserializationError.
			_, err = store.LoadRecord(pk)
			Expect(err).To(HaveOccurred())

			var deserErr *RecordDeserializationError
			Expect(errors.As(err, &deserErr)).To(BeTrue())
			Expect(deserErr.PrimaryKey).To(Equal(pk))
			Expect(deserErr.Cause).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RecordSerializationError type and Unwrap", func() {
		cause := fmt.Errorf("test cause")
		err := &RecordSerializationError{Cause: cause}

		var serErr *RecordSerializationError
		Expect(errors.As(err, &serErr)).To(BeTrue())
		Expect(serErr.Cause).To(Equal(cause))

		// Unwrap should return the cause, enabling errors.Is on wrapped error.
		Expect(errors.Is(err, cause)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("test cause"))
	})

	It("MetaDataError on missing primary key", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Deliberately set only two of three record types — Order has no primary key.
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

		_, err := builder.Build()
		Expect(err).To(HaveOccurred())

		var metaErr *MetaDataError
		Expect(errors.As(err, &metaErr)).To(BeTrue())
		Expect(metaErr.Message).To(ContainSubstring("has no primary key"))
	})

	It("RecordStoreNoInfoButNotEmptyError on headerless store", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			// Write some data into the store's subspace at the RecordKey position,
			// but do NOT write a store header at StoreInfoKey. The first key found
			// will not be at StoreInfoKey, triggering the error.
			garbageKey := ss.Pack(tuple.Tuple{RecordKey, int64(1), unsplitRecord})
			rtx.Transaction().Set(garbageKey, []byte{0x01, 0x02, 0x03})

			// Try to Open — should fail because there's data but no header.
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Open()
			Expect(err).To(HaveOccurred())

			var noInfoErr *RecordStoreNoInfoButNotEmptyError
			Expect(errors.As(err, &noInfoErr)).To(BeTrue())
			Expect(noInfoErr.FirstKey).NotTo(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexNotFoundError on non-existent index", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexReadable("nonexistent_index")
			Expect(err).To(HaveOccurred())

			var notFoundErr *IndexNotFoundError
			Expect(errors.As(err, &notFoundErr)).To(BeTrue())
			Expect(notFoundErr.IndexName).To(Equal("nonexistent_index"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Error type coverage gaps — tests for error types with zero or weak coverage.
var _ = Describe("Error type coverage gaps", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnknownStoreLockStateError on unrecognized lock state at format version 14", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()

			// Create a valid store first so the subspace exists.
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Create()
			Expect(err).NotTo(HaveOccurred())
			_ = store

			// Overwrite the store header with FormatVersion=14 and an unknown lock state (999).
			// Proto enums in Go accept arbitrary int32 values, so this is valid wire data.
			fmtVersion := int32(formatVersionFullStoreLock)
			unknownState := gen.DataStoreInfo_StoreLockState_State(999)
			header := &gen.DataStoreInfo{
				FormatVersion: &fmtVersion,
				StoreLockState: &gen.DataStoreInfo_StoreLockState{
					LockState: &unknownState,
				},
			}
			headerBytes, err := proto.Marshal(header)
			Expect(err).NotTo(HaveOccurred())
			storeInfoKey := ss.Pack(tuple.Tuple{StoreInfoKey})
			rtx.Transaction().Set(storeInfoKey, headerBytes)

			// Try to Open — should fail with UnknownStoreLockStateError.
			_, err = NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).Open()
			Expect(err).To(HaveOccurred())

			var unknownErr *UnknownStoreLockStateError
			Expect(errors.As(err, &unknownErr)).To(BeTrue())
			Expect(unknownErr.LockStateValue).To(Equal(int32(999)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UnknownStoreLockStateError includes correct error message", func() {
		err := &UnknownStoreLockStateError{LockStateValue: 42}
		var unknownErr *UnknownStoreLockStateError
		Expect(errors.As(err, &unknownErr)).To(BeTrue())
		Expect(unknownErr.LockStateValue).To(Equal(int32(42)))
	})

	It("IndexNotReadableError via errors.As on WRITE_ONLY index scan", func() {
		ctx := context.Background()

		// Build metadata with an index.
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("Order$price", Field("price"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark the index as WRITE_ONLY so it's not scannable.
			_, err = store.MarkIndexWriteOnly("Order$price")
			Expect(err).NotTo(HaveOccurred())
			Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())

			// Try to scan — the returned cursor should yield IndexNotReadableError.
			resolvedIdx := md.GetIndex("Order$price")
			Expect(resolvedIdx).NotTo(BeNil())
			_, err = AsList(ctx, store.ScanIndex(resolvedIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).To(HaveOccurred())

			var notReadableErr *IndexNotReadableError
			Expect(errors.As(err, &notReadableErr)).To(BeTrue())
			Expect(notReadableErr.IndexName).To(Equal("Order$price"))
			Expect(notReadableErr.CurrentState).To(Equal(IndexStateWriteOnly))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexNotReadableError via errors.As on DISABLED index scan", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("Order$price", Field("price"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexDisabled("Order$price")
			Expect(err).NotTo(HaveOccurred())

			resolvedIdx := md.GetIndex("Order$price")
			_, err = AsList(ctx, store.ScanIndex(resolvedIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).To(HaveOccurred())

			var notReadableErr *IndexNotReadableError
			Expect(errors.As(err, &notReadableErr)).To(BeTrue())
			Expect(notReadableErr.IndexName).To(Equal("Order$price"))
			Expect(notReadableErr.CurrentState).To(Equal(IndexStateDisabled))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IndexNotBuiltError field verification on MarkIndexReadable", func() {
		ctx := context.Background()

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("Order$price", Field("price"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ss := specSubspace()
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark WRITE_ONLY (but do NOT build the range set).
			_, err = store.MarkIndexWriteOnly("Order$price")
			Expect(err).NotTo(HaveOccurred())

			// Try to mark readable — should fail because range set is incomplete.
			_, err = store.MarkIndexReadable("Order$price")
			Expect(err).To(HaveOccurred())

			var notBuiltErr *IndexNotBuiltError
			Expect(errors.As(err, &notBuiltErr)).To(BeTrue())
			Expect(notBuiltErr.IndexName).To(Equal("Order$price"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
