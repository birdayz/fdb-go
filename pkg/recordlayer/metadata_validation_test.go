package recordlayer

import (
	"errors"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("MetadataValidation", func() {
	// Bug 1: Duplicate index names must error at Build() time.
	// Java's addIndexCommon() throws MetaDataException("Index X already defined").
	Describe("duplicate index name", func() {
		It("returns error from Build when same index name is added twice", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			idx1 := NewIndex("Order$price", Field("price"))
			idx2 := NewIndex("Order$price", Field("order_id")) // same name, different expression

			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("already defined"))
		})

		It("returns error when duplicate name across single-type and universal", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			idx1 := NewIndex("my_index", Field("price"))
			idx2 := NewIndex("my_index", Field("order_id"))

			builder.AddIndex("Order", idx1)
			builder.AddUniversalIndex(idx2)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("already defined"))
		})

		It("succeeds with distinct index names", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			builder.AddIndex("Order", NewIndex("Order$price", Field("price")))
			builder.AddIndex("Order", NewIndex("Order$order_id", Field("order_id")))

			_, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 2: AddIndex with unknown record type must error at Build() time.
	// Java's getIndexableRecordType() throws for unknown type names.
	Describe("unknown record type in AddIndex", func() {
		It("returns error from Build when record type does not exist", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			builder.AddIndex("NonExistentType", NewIndex("bogus_index", Field("price")))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("Unknown record type"))
			Expect(mdErr.Message).To(ContainSubstring("NonExistentType"))
		})
	})

	// Bug 3: AddMultiTypeIndex with unknown record type names must error at Build() time.
	// Java throws for unknown types rather than silently skipping.
	Describe("unknown record type in AddMultiTypeIndex", func() {
		It("returns error from Build when one record type does not exist", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			builder.AddMultiTypeIndex(
				[]string{"Order", "Ghost"},
				NewIndex("multi_idx", Field("price")),
			)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("Unknown record type"))
			Expect(mdErr.Message).To(ContainSubstring("Ghost"))
		})

		It("succeeds when all record types exist", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			builder.AddMultiTypeIndex(
				[]string{"Order", "Customer"},
				NewIndex("multi_idx", Field("price")),
			)

			_, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Bug 4: trimPrimaryKey with short primary key must return error, not panic.
	Describe("trimPrimaryKey bounds check", func() {
		It("returns error when primaryKeyComponentPositions exceeds primary key length", func() {
			idx := NewIndex("test", Concat(Field("price"), Field("order_id")))
			// Positions has 3 entries but PK only has 1 element
			idx.primaryKeyComponentPositions = []int{-1, 0, -1}

			pk := tuple.Tuple{int64(42)} // Only 1 element, but positions expects 3

			_, err := idx.TrimPrimaryKey(pk)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("out of bounds"))
		})

		It("returns error via indexEntryKey when PK is too short", func() {
			idx := NewIndex("test", Concat(Field("a"), Field("b")))
			idx.primaryKeyComponentPositions = []int{-1, -1, 0} // expects 3-element PK

			_, err := indexEntryKey(idx, tuple.Tuple{int64(1), int64(2)}, tuple.Tuple{int64(1)})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("out of bounds"))
		})

		It("succeeds when PK length matches positions length", func() {
			idx := NewIndex("test", Field("name"))
			idx.primaryKeyComponentPositions = []int{-1, 0}

			pk := tuple.Tuple{int64(1), "Alice"}
			trimmed, err := idx.TrimPrimaryKey(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(trimmed).To(Equal(tuple.Tuple{int64(1)}))
		})

		It("succeeds with nil positions (no deduplication)", func() {
			idx := NewIndex("test", Field("price"))

			pk := tuple.Tuple{int64(42)}
			trimmed, err := idx.TrimPrimaryKey(pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(trimmed).To(Equal(pk))
		})
	})
})
