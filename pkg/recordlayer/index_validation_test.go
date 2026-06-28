package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("Index validation", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("ValidateIndex", func() {
		It("returns valid for a consistent index", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save some records
				for i := int64(1); i <= 3; i++ {
					price := int32(i * 100)
					_, err = store.SaveRecord(&gen.Order{OrderId: &i, Price: &price})
					if err != nil {
						return nil, err
					}
				}

				result, err := store.ValidateIndex(ctx, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsValid()).To(BeTrue())
				Expect(result.TotalRecordsScanned).To(Equal(3))
				Expect(result.TotalEntriesScanned).To(Equal(3))
				Expect(result.MissingEntries).To(BeEmpty())
				Expect(result.OrphanedEntries).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("detects orphaned index entries", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save a record (creates index entry)
				id := int64(1)
				price := int32(500)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}

				// Manually add an orphaned index entry (no corresponding record)
				indexSub := store.indexSubspace(priceIndex)
				orphanKey, _ := indexEntryKey(priceIndex, tuple.Tuple{int64(999)}, tuple.Tuple{int64(99)})
				rtx.Transaction().Set(fdb.Key(indexSub.Pack(orphanKey)), tuple.Tuple{}.Pack())

				result, err := store.ValidateIndex(ctx, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsValid()).To(BeFalse())
				Expect(result.OrphanedEntries).To(HaveLen(1))
				Expect(result.OrphanedEntries[0].IndexKey).To(Equal(tuple.Tuple{int64(999)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("detects missing index entries", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save a record (creates index entry)
				id := int64(1)
				price := int32(500)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}

				// Manually delete the index entry to create a "missing" entry
				indexSub := store.indexSubspace(priceIndex)
				entryKey, _ := indexEntryKey(priceIndex, tuple.Tuple{int64(500)}, tuple.Tuple{int64(1)})
				rtx.Transaction().Clear(fdb.Key(indexSub.Pack(entryKey)))

				result, err := store.ValidateIndex(ctx, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsValid()).To(BeFalse())
				Expect(result.MissingEntries).To(HaveLen(1))
				Expect(result.MissingEntries[0].PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns valid for empty store", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				result, err := store.ValidateIndex(ctx, priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsValid()).To(BeTrue())
				Expect(result.TotalRecordsScanned).To(Equal(0))
				Expect(result.TotalEntriesScanned).To(Equal(0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
