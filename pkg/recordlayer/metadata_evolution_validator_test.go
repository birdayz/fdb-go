package recordlayer

import (
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MetaDataEvolutionValidator", func() {
	// buildMetaData sets version AFTER configure so AddIndex/RemoveIndex bumps
	// don't interfere with the final version.
	buildMetaData := func(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		if configure != nil {
			configure(builder)
		}
		builder.SetVersion(version)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	Describe("version validation", func() {
		It("rejects same version", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(1, nil)

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not have newer version"))
		})

		It("rejects older version", func() {
			old := buildMetaData(5, nil)
			new := buildMetaData(3, nil)

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not have newer version"))
		})

		It("allows same version when configured", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(1, nil)

			v := NewMetaDataEvolutionValidator().SetAllowNoVersionChange(true).Build()
			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts newer version", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("split long records", func() {
		It("rejects removing split", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.SetSplitLongRecords(true)
			})
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no longer splits"))
		})

		It("rejects adding split by default", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.SetSplitLongRecords(true)
			})

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("splits long records"))
		})

		It("allows adding split when configured", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.SetSplitLongRecords(true)
			})

			v := NewMetaDataEvolutionValidator().SetAllowUnsplitToSplit(true).Build()
			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("record type validation", func() {
		It("rejects primary key change", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").SetPrimaryKey(Field("price"))
			})

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("primary key changed"))
		})

		It("rejects record type key change", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").SetRecordTypeKey(int64(999))
			})

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("record type key changed"))
		})

		It("allows unchanged record types with higher version", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("index validation", func() {
		It("rejects removed index without former index", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})

			new := buildMetaData(3, nil)

			err := ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing in new meta-data"))
		})

		It("accepts removed index with former index", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})
			oldIdx := old.GetIndex("price_idx")

			// Build new with matching index added/removed at proper versions
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = oldIdx.AddedVersion
			idx.LastModifiedVersion = oldIdx.LastModifiedVersion
			builder.AddIndex("Order", idx)
			// Set version > old so RemoveIndex records proper removedVersion
			builder.SetVersion(2)
			builder.RemoveIndex("price_idx")
			builder.SetVersion(3)
			new, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects index type change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})
			oldIdx := old.GetIndex("price_idx")

			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				idx := NewCountIndex("price_idx", GroupAll(Field("price")))
				idx.AddedVersion = oldIdx.AddedVersion
				idx.LastModifiedVersion = oldIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
			})

			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("type changed"))
		})

		It("rejects index key expression change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})
			oldIdx := old.GetIndex("price_idx")

			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("price_idx", Field("order_id"))
				idx.AddedVersion = oldIdx.AddedVersion
				idx.LastModifiedVersion = oldIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
			})

			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("key expression changed"))
		})

		It("accepts new index with proper version", func() {
			old := buildMetaData(1, nil)
			// AddIndex at version > old.Version()
			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				b.SetVersion(2)
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects new index with old version", func() {
			old := buildMetaData(5, nil)

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			idx := NewIndex("price_idx", Field("price"))
			idx.LastModifiedVersion = 3 // Older than old metadata version
			builder.AddIndex("Order", idx)
			builder.SetVersion(6)
			new, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not newer than the old meta-data version"))
		})

		It("rejects last modified version decrease", func() {
			// Old has index with LastModifiedVersion=5
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			oldIdx := NewIndex("price_idx", Field("price"))
			oldIdx.AddedVersion = 2
			oldIdx.LastModifiedVersion = 5
			builder1.AddIndex("Order", oldIdx)
			builder1.SetVersion(6)
			old, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			// New has same index but LastModifiedVersion=3 (less than 5)
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			newIdx := NewIndex("price_idx", Field("price"))
			newIdx.AddedVersion = 2
			newIdx.LastModifiedVersion = 3 // Less than old's 5
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(7)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("last modified version"))
		})

		It("allows index rebuild when configured", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})
			oldIdx := old.GetIndex("price_idx")

			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("price_idx", Field("price"))
				idx.AddedVersion = oldIdx.AddedVersion
				b.AddIndex("Order", idx)
			})

			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("former index validation", func() {
		It("rejects removing a former index", func() {
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder1.RemoveIndex("price_idx")
			builder1.SetVersion(3)
			old, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(old.GetFormerIndexes()).To(HaveLen(1))

			new := buildMetaData(5, nil)

			err = ValidateEvolution(old, new)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("former index"))
			Expect(err.Error()).To(ContainSubstring("removed from meta-data"))
		})

		It("accepts preserved former index", func() {
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder1.RemoveIndex("price_idx")
			builder1.SetVersion(3)
			old, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			// Build new with same FormerIndex by replaying the same steps
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder2.RemoveIndex("price_idx")
			builder2.SetVersion(5)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("message descriptor validation", func() {
		It("passes with matching descriptors", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("default validator is strict", func() {
		It("has all restrictions enabled by default", func() {
			v := DefaultMetaDataEvolutionValidator()
			Expect(v.allowNoVersionChange).To(BeFalse())
			Expect(v.allowIndexRebuilds).To(BeFalse())
			Expect(v.allowUnsplitToSplit).To(BeFalse())
			Expect(v.disallowTypeRenames).To(BeFalse())
		})
	})

	Describe("convenience function", func() {
		It("ValidateEvolution uses default validator", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
