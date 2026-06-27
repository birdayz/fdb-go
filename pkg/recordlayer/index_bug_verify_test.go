package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("IndexBugVerify", func() {
	// Bug 1: checkUniqueness must use getEntryPrimaryKey (not raw slice) for PK dedup.
	// When a composite index has overlapping PK components, the raw slice after index
	// columns is the TRIMMED PK. Using it directly gives wrong comparisons and wrong
	// violation entries.
	Describe("checkUniqueness uses getEntryPrimaryKey", func() {
		It("unique index detects violations correctly", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("Order$price_unique", Field("price"))
			idx.SetUnique()
			builder.AddIndex("Order", idx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save first order with price=42.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Save second order with same price=42 — must get uniqueness violation.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(2), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).To(HaveOccurred())
			var uniquenessErr *RecordIndexUniquenessViolationError
			Expect(errors.As(err, &uniquenessErr)).To(BeTrue())
		})

		It("unique index allows same record to update without violation", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("Order$price_unique", Field("price"))
			idx.SetUnique()
			builder.AddIndex("Order", idx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Save and then update the same record — should NOT trigger violation.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				if err != nil {
					return nil, err
				}
				// Update same record (same PK) — no violation.
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 2: COUNT_NOT_NULL with NestingKeyExpression — must detect null nested fields.
	// Before fix: keyExpressionHasNullField was missing the NestingKeyExpression case,
	// so null nested fields were counted instead of skipped.
	Describe("COUNT_NOT_NULL with nested field null detection", func() {
		It("skips counting when nested field is null (flower unset)", func() {
			ctx := context.Background()
			ss := specSubspace()

			countIdx := NewIndex("Order$flower_type_count_not_null",
				Ungrouped(Nest("flower", Field("type"))),
			)
			countIdx.Type = "count_not_null"

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", countIdx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			builtIdx := md.GetIndex("Order$flower_type_count_not_null")
			Expect(builtIdx).NotTo(BeNil())

			// Save an order WITHOUT flower (nil nested message).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				// flower is nil — COUNT_NOT_NULL should skip this record.
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(10)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Check the count — should be 0 (record skipped because flower is null).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanIndex(builtIdx, TupleRangeAll, nil, ForwardScan())
				entries, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(entries).To(BeEmpty(), "no count entries when nested field is null")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("counts when nested field is set (flower present)", func() {
			ctx := context.Background()
			ss := specSubspace()

			countIdx := NewIndex("Order$flower_type_count_not_null",
				Ungrouped(Nest("flower", Field("type"))),
			)
			countIdx.Type = "count_not_null"

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", countIdx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			builtIdx := md.GetIndex("Order$flower_type_count_not_null")

			flowerType := "Rose"
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: intPtr(1),
					Price:   int32Ptr(10),
					Flower:  &gen.Flower{Type: &flowerType},
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Check count — should be 1.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanIndex(builtIdx, TupleRangeAll, nil, ForwardScan())
				entries, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(entries).To(HaveLen(1), "should have one count entry for set nested field")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 3: Unique index with composite key+PK overlap — verify getEntryPrimaryKey
	// reconstruction in checkUniqueness for composite keys.
	Describe("unique index with composite index+PK overlap", func() {
		It("works correctly with PK deduplication", func() {
			ctx := context.Background()
			ss := specSubspace()

			// Concat(order_id, price) — PK=order_id overlaps at position 0.
			idx := NewIndex("Order$id_price_unique", Concat(Field("order_id"), Field("price")))
			idx.SetUnique()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			builtIdx := md.GetIndex("Order$id_price_unique")

			// Save two orders — different index keys, no violation.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(2), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify we can scan and get both entries with correct PK reconstruction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor := store.ScanIndex(builtIdx, TupleRangeAll, nil, ForwardScan())
				entries, cErr := AsList(ctx, cursor)
				if cErr != nil {
					return nil, cErr
				}
				Expect(entries).To(HaveLen(2))

				// Verify PKs are reconstructed correctly.
				for _, entry := range entries {
					pk := entry.PrimaryKey()
					Expect(pk).To(HaveLen(1))
					Expect(pk[0]).To(BeAssignableToTypeOf(int64(0)))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Verify keyExpressionHasNullField with direct field (not nested).
	Describe("keyExpressionHasNullField basic", func() {
		It("returns true for nil optional field", func() {
			order := &gen.Order{OrderId: intPtr(1)} // price is nil
			Expect(keyExpressionHasNullField(order, Field("price"))).To(BeTrue())
		})

		It("returns false for set optional field", func() {
			order := &gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)}
			Expect(keyExpressionHasNullField(order, Field("price"))).To(BeFalse())
		})

		It("returns true for nil nested message", func() {
			order := &gen.Order{OrderId: intPtr(1)} // flower is nil
			Expect(keyExpressionHasNullField(order, Nest("flower", Field("type")))).To(BeTrue())
		})

		It("returns false for set nested message with set field", func() {
			flowerType := "Rose"
			order := &gen.Order{OrderId: intPtr(1), Flower: &gen.Flower{Type: &flowerType}}
			Expect(keyExpressionHasNullField(order, Nest("flower", Field("type")))).To(BeFalse())
		})

		It("returns true for set nested message with nil child field", func() {
			order := &gen.Order{OrderId: intPtr(1), Flower: &gen.Flower{}} // type is nil
			Expect(keyExpressionHasNullField(order, Nest("flower", Field("type")))).To(BeTrue())
		})
	})
})
