package recordlayer

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("RecordMetaDataBuilder advanced features", func() {
	Describe("RemoveIndex / FormerIndex", func() {
		It("removes index and creates FormerIndex", func() {
			idx := NewIndex("Order$price", Field("price"))
			idx.AddedVersion = 1

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)
			builder.RemoveIndex("Order$price")

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Index should be gone
			Expect(md.GetIndex("Order$price")).To(BeNil())
			Expect(md.GetIndexesForRecordType("Order")).To(BeEmpty())

			// FormerIndex should exist
			formers := md.GetFormerIndexes()
			Expect(formers).To(HaveLen(1))
			Expect(formers[0].FormerName).To(Equal("Order$price"))
			Expect(formers[0].SubspaceKey).To(Equal("Order$price"))
			Expect(formers[0].AddedVersion).To(Equal(1))
		})

		It("prevents subspace key reuse", func() {
			idx1 := NewIndex("old_idx", Field("price"))
			idx2 := NewIndex("new_idx", Field("price"))
			idx2.SetSubspaceKey("old_idx") // reuse old key

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx1)
			builder.RemoveIndex("old_idx")
			builder.AddIndex("Order", idx2)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("reuses subspace key"))
		})

		It("removing non-existent index is a no-op", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.RemoveIndex("nonexistent")

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetFormerIndexes()).To(BeEmpty())
		})

		It("removes universal index", func() {
			idx := NewIndex("universal_idx", Field("price"))
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddUniversalIndex(idx)
			builder.RemoveIndex("universal_idx")

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetUniversalIndexes()).To(BeEmpty())
			Expect(md.GetFormerIndexes()).To(HaveLen(1))
		})

		It("removes multi-type index", func() {
			idx := NewIndex("multi_idx", Field("price"))
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			builder.RemoveIndex("multi_idx")

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetIndexesForRecordType("Order")).To(BeEmpty())
			Expect(md.GetIndexesForRecordType("Customer")).To(BeEmpty())
			Expect(md.GetFormerIndexes()).To(HaveLen(1))
		})
	})

	Describe("SetRecordTypeKey / GetRecordTypeKey", func() {
		It("defaults to RecordTypeIndex", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderType := md.GetRecordType("Order")
			Expect(orderType.GetRecordTypeKey()).To(Equal(orderType.RecordTypeIndex))
		})

		It("overrides with explicit key", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id")).SetRecordTypeKey("custom_order_key")
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.GetRecordType("Order").GetRecordTypeKey()).To(Equal("custom_order_key"))
			// Customer still uses default
			Expect(md.GetRecordType("Customer").GetRecordTypeKey()).To(Equal(md.GetRecordType("Customer").RecordTypeIndex))
		})

		It("supports integer keys", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id")).SetRecordTypeKey(int64(99))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.GetRecordType("Order").GetRecordTypeKey()).To(Equal(int64(99)))
		})
	})

	Describe("PrimaryKeyHasRecordTypePrefix", func() {
		It("returns false when primary keys don't start with RecordTypeKey", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.PrimaryKeyHasRecordTypePrefix()).To(BeFalse())
		})

		It("returns true when all primary keys start with RecordTypeKey", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.PrimaryKeyHasRecordTypePrefix()).To(BeTrue())
		})

		It("returns false when only some primary keys have prefix", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id")) // No prefix
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.PrimaryKeyHasRecordTypePrefix()).To(BeFalse())
		})

		It("returns true for standalone RecordTypeKey (not wrapped in Concat)", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(RecordTypeKey().Nest(Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(RecordTypeKey().Nest(Field("customer_id")))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(RecordTypeKey().Nest(Field("id")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.PrimaryKeyHasRecordTypePrefix()).To(BeTrue())
		})
	})

	Describe("SetVersion", func() {
		It("sets metadata version", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetVersion(42)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.Version()).To(Equal(42))
		})

		It("defaults to 0", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.Version()).To(Equal(0))
		})
	})

	Describe("GetRecordType panics on unknown type", func() {
		It("panics with MetaDataError for nonexistent type", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(func() {
				builder.GetRecordType("NonExistentType")
			}).To(PanicWith(MatchError(ContainSubstring("unknown record type"))))
		})

		It("panics with MetaDataError for empty string", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(func() {
				builder.GetRecordType("")
			}).To(PanicWith(MatchError(ContainSubstring("unknown record type"))))
		})
	})
})
