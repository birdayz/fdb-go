package recordlayer

import (
	"errors"

	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var _ = Describe("MetaDataEvolutionValidator", func() {
	// buildMetaData sets version AFTER configure so AddIndex/RemoveIndex bumps
	// don't interfere with the final version.
	buildMetaData := func(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("does not have newer version"))
		})

		It("rejects older version", func() {
			old := buildMetaData(5, nil)
			new := buildMetaData(3, nil)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("does not have newer version"))
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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("no longer splits"))
		})

		It("rejects adding split by default", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.SetSplitLongRecords(true)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("splits long records"))
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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("primary key changed"))
		})

		It("rejects record type key change", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").SetRecordTypeKey(int64(999))
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("record type key changed"))
		})

		It("allows unchanged record types with higher version", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects since version change on existing type", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 1
			})
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 2
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("since version changed"))
		})

		It("allows same since version on existing type", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 3
			})
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 3
			})

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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("missing in new meta-data"))
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
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("type changed"))
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
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("key expression changed"))
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
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("price_idx", Field("price"))
			idx.LastModifiedVersion = 3 // Older than old metadata version
			builder.AddIndex("Order", idx)
			builder.SetVersion(6)
			new, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("not newer than the old meta-data version"))
		})

		It("rejects last modified version decrease", func() {
			// Old has index with LastModifiedVersion=5
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			newIdx := NewIndex("price_idx", Field("price"))
			newIdx.AddedVersion = 2
			newIdx.LastModifiedVersion = 3 // Less than old's 5
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(7)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("last modified version"))
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

		It("rejects changed primary key component positions", func() {
			// Build old with a composite index that overlaps the PK (order_id).
			// Concat(Field("price"), Field("order_id")) with PK = Field("order_id")
			// produces primaryKeyComponentPositions = [1] (PK component at index position 1).
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("composite_idx", Concat(Field("price"), Field("order_id"))))
			})
			oldIdx := old.GetIndex("composite_idx")
			Expect(oldIdx.HasPrimaryKeyComponentPositions()).To(BeTrue())
			Expect(oldIdx.PrimaryKeyComponentPositions()).To(Equal([]int{1}))

			// Build new with same index name/type/expression but manually set
			// different primaryKeyComponentPositions.
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			newIdx := NewIndex("composite_idx", Concat(Field("price"), Field("order_id")))
			newIdx.AddedVersion = oldIdx.AddedVersion
			newIdx.LastModifiedVersion = oldIdx.LastModifiedVersion
			// Force different positions by setting them manually before Build()
			// which won't overwrite non-nil positions.
			newIdx.primaryKeyComponentPositions = []int{0}
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(3)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("changes primary key component positions"))
		})

		It("rejects adding primary key component positions", func() {
			// Old: non-overlapping index → nil positions (Field("price") with PK=Field("order_id"))
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("price_idx", Field("price")))
			})
			oldIdx := old.GetIndex("price_idx")
			Expect(oldIdx.HasPrimaryKeyComponentPositions()).To(BeFalse())

			// New: same expression but pre-set non-nil positions before Build().
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			newIdx := NewIndex("price_idx", Field("price"))
			newIdx.AddedVersion = oldIdx.AddedVersion
			newIdx.LastModifiedVersion = oldIdx.LastModifiedVersion
			newIdx.primaryKeyComponentPositions = []int{-1} // Force non-nil; Build() won't overwrite
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(3)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("adds primary key component positions"))
		})

		It("accepts unchanged primary key component positions", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("composite_idx", Concat(Field("price"), Field("order_id"))))
			})
			oldIdx := old.GetIndex("composite_idx")
			Expect(oldIdx.HasPrimaryKeyComponentPositions()).To(BeTrue())

			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			newIdx := NewIndex("composite_idx", Concat(Field("price"), Field("order_id")))
			newIdx.AddedVersion = oldIdx.AddedVersion
			newIdx.LastModifiedVersion = oldIdx.LastModifiedVersion
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(3)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips primary key component positions check when index is rebuilt", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("composite_idx", Concat(Field("price"), Field("order_id"))))
			})
			oldIdx := old.GetIndex("composite_idx")
			Expect(oldIdx.HasPrimaryKeyComponentPositions()).To(BeTrue())

			// New index: same expression but different positions + higher lastModifiedVersion.
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			newIdx := NewIndex("composite_idx", Concat(Field("price"), Field("order_id")))
			newIdx.AddedVersion = oldIdx.AddedVersion
			newIdx.LastModifiedVersion = oldIdx.LastModifiedVersion + 1
			newIdx.primaryKeyComponentPositions = []int{0} // Different positions
			builder2.AddIndex("Order", newIdx)
			builder2.SetVersion(3)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// With allowIndexRebuilds, the positions check is skipped.
			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
			err = v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("former index validation", func() {
		It("rejects removing a former index", func() {
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder1.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder1.RemoveIndex("price_idx")
			builder1.SetVersion(3)
			old, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(old.GetFormerIndexes()).To(HaveLen(1))

			new := buildMetaData(5, nil)

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("former index"))
			Expect(evolErr.Message).To(ContainSubstring("removed from meta-data"))
		})

		It("accepts preserved former index", func() {
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder1.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder1.RemoveIndex("price_idx")
			builder1.SetVersion(3)
			old, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			// Build new with same FormerIndex by replaying the same steps
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder2.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder2.RemoveIndex("price_idx")
			builder2.SetVersion(5)
			new, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			err = ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("union validation", func() {
		It("passes when old and new use the same proto file", func() {
			// Same file descriptor → same union descriptor object → early return
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("getUnionDescriptor returns the UnionDescriptor message", func() {
			md := buildMetaData(1, nil)
			union := getUnionDescriptor(md)
			Expect(union).NotTo(BeNil())
			Expect(string(union.FullName())).To(ContainSubstring("UnionDescriptor"))

			// The union should have fields for Order, Customer, TypedRecord
			fields := union.Fields()
			Expect(fields.Len()).To(BeNumerically(">=", 3))

			// Verify all fields are message kind
			for i := 0; i < fields.Len(); i++ {
				f := fields.Get(i)
				Expect(f.Kind()).To(Equal(protoreflect.MessageKind))
			}
		})

		It("getUnionDescriptor returns nil for metadata with nil file descriptor", func() {
			// Build a metadata with nil fileDescriptor to test the nil guard
			md := &RecordMetaData{}
			union := getUnionDescriptor(md)
			Expect(union).To(BeNil())
		})

		It("validateUnion skips when either union is nil", func() {
			// If old has no file descriptor, validateUnion returns nil (skip)
			old := &RecordMetaData{version: 1}
			new := buildMetaData(2, nil)

			v := DefaultMetaDataEvolutionValidator()
			err := v.validateUnion(old, new)
			Expect(err).NotTo(HaveOccurred())

			// Same for new having no file descriptor
			old2 := buildMetaData(1, nil)
			new2 := &RecordMetaData{version: 2}
			err = v.validateUnion(old2, new2)
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
			Expect(v.allowNoSinceVersion).To(BeFalse())
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

	Describe("multi-version jump (v1 → v5)", func() {
		It("accepts large version jump with new index at intermediate version", func() {
			v := DefaultMetaDataEvolutionValidator()

			old := buildMetaData(1, nil)
			new := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("price_idx", Field("price"))
				idx.LastModifiedVersion = 3 // added at v3, between old(1) and new(5)
				idx.AddedVersion = 3
				b.AddIndex("Order", idx)
			})

			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects new index with version not exceeding old metadata version", func() {
			v := DefaultMetaDataEvolutionValidator()

			old := buildMetaData(3, nil)
			new := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("price_idx", Field("price"))
				idx.LastModifiedVersion = 2 // v2 < old v3 — not valid
				idx.AddedVersion = 2
				b.AddIndex("Order", idx)
			})

			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("not newer than the old meta-data version"))
		})

		It("accepts combined changes: add index + remove index (with former)", func() {
			v := DefaultMetaDataEvolutionValidator()

			oldIdx := NewIndex("old_idx", Field("price"))
			oldIdx.LastModifiedVersion = 1
			oldIdx.AddedVersion = 1
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", oldIdx)
			})

			new := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", oldIdx) // keep old index so RemoveIndex works
				b.RemoveIndex("old_idx")    // creates FormerIndex
				newIdx := NewIndex("new_idx", Field("price"))
				newIdx.LastModifiedVersion = 4
				newIdx.AddedVersion = 4
				b.AddIndex("Order", newIdx)
			})

			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("index version boundary checks", func() {
		It("rejects index addedVersion changing between old and new", func() {
			v := DefaultMetaDataEvolutionValidator()

			idx1 := NewIndex("price_idx", Field("price"))
			idx1.AddedVersion = 1
			idx1.LastModifiedVersion = 1
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx1)
			})

			idx2 := NewIndex("price_idx", Field("price"))
			idx2.AddedVersion = 2 // changed!
			idx2.LastModifiedVersion = 2
			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx2)
			})

			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("added version does not match"))
		})

		It("rejects lastModifiedVersion decreasing even with allowIndexRebuilds", func() {
			// With allowIndexRebuilds=true, changed lastModifiedVersion is allowed
			// (indexes get rebuilt). But the unconditional check rejects DECREASING
			// versions (old > new), matching Java line 634.
			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()

			idx1 := NewIndex("price_idx", Field("price"))
			idx1.AddedVersion = 1
			idx1.LastModifiedVersion = 3
			old := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx1)
			})

			idx2 := NewIndex("price_idx", Field("price"))
			idx2.AddedVersion = 1
			idx2.LastModifiedVersion = 2 // decreased!
			new := buildMetaData(4, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx2)
			})

			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("last-modified version newer than new index"))
		})
	})

	Describe("safe type promotion", func() {
		It("recognizes int32 to int64 as safe", func() {
			Expect(isSafeTypePromotion(protoreflect.Int32Kind, protoreflect.Int64Kind)).To(BeTrue())
		})

		It("recognizes sint32 to sint64 as safe", func() {
			Expect(isSafeTypePromotion(protoreflect.Sint32Kind, protoreflect.Sint64Kind)).To(BeTrue())
		})

		It("rejects int64 to int32 (narrowing)", func() {
			Expect(isSafeTypePromotion(protoreflect.Int64Kind, protoreflect.Int32Kind)).To(BeFalse())
		})

		It("rejects int32 to string (type change)", func() {
			Expect(isSafeTypePromotion(protoreflect.Int32Kind, protoreflect.StringKind)).To(BeFalse())
		})

		It("rejects same type (not a promotion)", func() {
			Expect(isSafeTypePromotion(protoreflect.Int32Kind, protoreflect.Int32Kind)).To(BeFalse())
		})
	})
})
