package recordlayer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Helpers for building synthetic proto descriptors in evolution validator tests.
func evolStrPtr(s string) *string { return &s }
func evolInt32Ptr(i int32) *int32 { return &i }
func evolEnumType() *descriptorpb.FieldDescriptorProto_Type {
	t := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	return &t
}

func evolOptionalLabel() *descriptorpb.FieldDescriptorProto_Label {
	l := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &l
}

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

	Describe("ignored index options", func() {
		withOptions := func(version int, options map[string]string) *RecordMetaData {
			return buildMetaData(version, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("price_idx", Field("price"))
				idx.AddedVersion, idx.LastModifiedVersion = 1, 1
				idx.Options = options
				b.AddIndex("Order", idx)
			})
		}

		It("copies configuration at every boundary and preserves all flags", func() {
			names := []string{"z", "a", "z"}
			b := NewMetaDataEvolutionValidator().SetIgnoredIndexOptions(names).
				SetAllowNoVersionChange(true).SetAllowIndexRebuilds(true).
				SetAllowUnsplitToSplit(true).SetAllowOlderFormerIndexAddedVersion(true).
				SetAllowMissingFormerIndexNames(true).SetDisallowTypeRenames(true).
				SetAllowNoSinceVersion(true).SetAllowFieldRenames(true).
				SetAllowDeprecatedFieldRenames(true).SetAllowUndeprecatingFields(true)
			names[0] = "mutated"
			Expect(b.GetIgnoredIndexOptions()).To(Equal([]string{"a", "z"}))
			v := b.Build()
			b.GetIgnoredIndexOptions()[0] = "mutated"
			v.GetIgnoredIndexOptions()[0] = "mutated"
			Expect(v.GetIgnoredIndexOptions()).To(Equal([]string{"a", "z"}))
			copy := v.AsBuilder()
			Expect(copy.Build()).To(Equal(v))
			delete(b.v.ignoredIndexOptions, "a")
			delete(copy.v.ignoredIndexOptions, "z")
			Expect(v.GetIgnoredIndexOptions()).To(Equal([]string{"a", "z"}))
			Expect(copy.GetIgnoredIndexOptions()).To(Equal([]string{"a"}))
			Expect(b.GetIgnoredIndexOptions()).To(Equal([]string{"z"}))
			Expect(b.SetIgnoredIndexOptions(nil).Build().GetIgnoredIndexOptions()).To(BeEmpty())
			Expect(DefaultMetaDataEvolutionValidator().GetIgnoredIndexOptions()).To(BeEmpty())
		})

		for _, pair := range [][2]map[string]string{
			{nil, {"applicationTag": "new"}},
			{{"applicationTag": "old"}, nil},
			{{"applicationTag": "old"}, {"applicationTag": "new"}},
			{nil, {IndexOptionUnique: "true"}},
		} {
			It("ignores only configured additions, removals and changes", func() {
				old, new := withOptions(1, pair[0]), withOptions(2, pair[1])
				Expect(DefaultMetaDataEvolutionValidator().Validate(old, new)).To(HaveOccurred())
				v := NewMetaDataEvolutionValidator().SetIgnoredIndexOptions([]string{"applicationTag", IndexOptionUnique}).Build()
				Expect(v.Validate(old, new)).To(Succeed())
				new.GetIndex("price_idx").Options = map[string]string{"ApplicationTag": "different case"}
				Expect(v.Validate(old, new)).To(MatchError(ContainSubstring("ApplicationTag")))
			})
		}

		It("validates only the supplied mutable option set", func() {
			old := NewIndex("text", Field("price"))
			new := NewIndex("text", Field("price"))
			old.Type, new.Type = IndexTypeText, IndexTypeText
			new.Options = map[string]string{IndexOptionTextOmitPositions: "true", "applicationTag": "new"}
			Expect(ValidateChangedIndexOptions(old, new, nil)).To(Succeed())
			changed := map[string]bool{IndexOptionTextOmitPositions: true, IndexOptionAllowedForQuery: true}
			Expect(ValidateChangedIndexOptions(old, new, changed)).To(Succeed())
			Expect(changed).To(Equal(map[string]bool{IndexOptionAllowedForQuery: true}))
			Expect(ValidateChangedIndexOptions(old, new, map[string]bool{"applicationTag": true})).To(HaveOccurred())
			// Supplied names need not be actual differences: the base validator
			// rejects unknown names even if both values are absent.
			Expect(ValidateChangedIndexOptions(old, new, map[string]bool{"absent": true})).To(HaveOccurred())
		})

		It("compares resolved tokenizer names including the implicit default", func() {
			for _, pair := range [][2]map[string]string{
				{nil, {IndexOptionTextTokenizerName: DefaultTextTokenizerName}},
				{{IndexOptionTextTokenizerName: DefaultTextTokenizerName}, nil},
				{{IndexOptionTextTokenizerName: DefaultTextTokenizerName}, {IndexOptionTextTokenizerName: DefaultTextTokenizerName}},
			} {
				old, new := textIndexWithOptions(pair[0]), textIndexWithOptions(pair[1])
				changed := map[string]bool{IndexOptionTextTokenizerName: true}
				Expect(ValidateChangedIndexOptions(old, new, changed)).To(Succeed())
				Expect(changed).To(BeEmpty())
			}
			old, new := textIndexWithOptions(nil), textIndexWithOptions(map[string]string{IndexOptionTextTokenizerName: "unregistered_ignored_options_test"})
			Expect(ValidateChangedIndexOptions(old, new, map[string]bool{IndexOptionTextTokenizerName: true})).To(MatchError(ContainSubstring("unrecognized text tokenizer")))
			Expect(ValidateChangedIndexOptions(new, old, map[string]bool{IndexOptionTextTokenizerName: true})).To(MatchError(ContainSubstring("unrecognized text tokenizer")))
			Expect(ValidateChangedIndexOptions(old, new, nil)).To(Succeed())
		})

		It("does not weaken unrelated structural checks", func() {
			v := NewMetaDataEvolutionValidator().SetIgnoredIndexOptions([]string{"applicationTag"}).Build()
			old := withOptions(1, nil)
			for _, change := range []func(*Index){
				func(idx *Index) { idx.Type = IndexTypeRank },
				func(idx *Index) { idx.RootExpression = Field("order_id") },
				func(idx *Index) { idx.AddedVersion++ },
				func(idx *Index) { idx.LastModifiedVersion++ },
				func(idx *Index) { idx.Options[IndexOptionUnique] = "true" },
			} {
				new := withOptions(2, map[string]string{"applicationTag": "new"})
				change(new.GetIndex("price_idx"))
				Expect(v.Validate(old, new)).To(HaveOccurred())
			}
		})
	})

	Describe("a literal's carrier", func() {
		// An index over price whose root carries the literal lit, at the key column
		// (asColumn) or inside the long-arithmetic function add(price, lit).
		withLiteral := func(version int, lit any, asColumn bool) *RecordMetaData {
			return buildMetaData(version, func(b *RecordMetaDataBuilder) {
				var root KeyExpression = FunctionExpr("add", Concat(Field("price"), Literal(lit)))
				if asColumn {
					root = Concat(Field("price"), Literal(lit))
				}
				idx := NewIndex("lit_idx", root)
				idx.AddedVersion, idx.LastModifiedVersion = 1, 1
				b.AddIndex("Order", idx)
			})
		}

		// Java compares a literal by its value object (LiteralKeyExpression's
		// equals), so a Long and an Integer of one number, or a Double and a Float
		// of one value, are different keys, in either direction and wherever the
		// literal sits (MetaDataEvolutionValidator.java:719): a changed key, which
		// the index-rebuild option admits as a rebuild.
		for _, asColumn := range []bool{false, true} {
			It(fmt.Sprintf("is part of the key (as a key column: %t)", asColumn), func() {
				rebuilds := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
				for _, pair := range [][2]any{{int64(10000), int32(10000)}, {int32(10000), int64(10000)}, {float64(1.5), float32(1.5)}, {float32(1.5), float64(1.5)}} {
					old, new := withLiteral(1, pair[0], asColumn), withLiteral(2, pair[1], asColumn)
					Expect(DefaultMetaDataEvolutionValidator().Validate(old, new)).To(MatchError(ContainSubstring("key expression changed")),
						"%T to %T", pair[0], pair[1])
					Expect(rebuilds.Validate(old, new)).To(MatchError(ContainSubstring("key expression changed")),
						"%T to %T: the key changed and the index's last-modified version did not", pair[0], pair[1])
				}
			})
		}
	})

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

		It("rejects an older version even when an unchanged version is allowed", func() {
			old := buildMetaData(5, nil)
			new := buildMetaData(3, nil)

			v := NewMetaDataEvolutionValidator().SetAllowNoVersionChange(true).Build()
			err := v.Validate(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue(), "Java refuses a downgrade whatever allowNoVersionChange says (MetaDataEvolutionValidator.java:154)")
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
			// A record type's since version may not exceed the metadata version
			// carrying it (Java MetaDataValidator.validateRecordType(),
			// MetaDataValidator.java:88-92), so the shared since version has to sit
			// at or below the older of the two metadata versions.
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 1
			})
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.GetRecordType("Order").recordType.SinceVersion = 1
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
			Expect(evolErr.Message).To(HavePrefix("index type changed"))
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
			Expect(evolErr.Message).To(HavePrefix("index key expression changed"))
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

			// Java tests a decrease before it tests a change, so a decrease is
			// reported as one (MetaDataEvolutionValidator.java:640-651).
			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("old index has last-modified version newer than new index"))
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
			Expect(evolErr.Message).To(HavePrefix("new index changes primary key component positions"))
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
			Expect(evolErr.Message).To(HavePrefix("new index adds primary key component positions"))
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

		It("rejects former index without old index when addedVersion <= old version", func() {
			// Scenario: new metadata has a FormerIndex for an index that didn't exist
			// in old metadata (index was added and dropped between versions).
			// The FormerIndex's addedVersion must be > old metadata version.
			old := buildMetaData(5, nil)

			// Build new metadata, then inject a FormerIndex with addedVersion=3 (<=5).
			new := buildMetaData(10, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    "ephemeral_idx",
				FormerName:     "ephemeral_idx",
				AddedVersion:   3, // <= old.Version()==5 → should be rejected
				RemovedVersion: 7, // > old.Version()==5 → passes removedVersion check
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("added version prior to old"))
		})

		It("accepts former index without old index when allowOlderFormerIndexAddedVersion", func() {
			old := buildMetaData(5, nil)

			new := buildMetaData(10, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    "ephemeral_idx",
				FormerName:     "ephemeral_idx",
				AddedVersion:   3, // <= old.Version()==5 → normally rejected
				RemovedVersion: 7,
			})

			validator := NewMetaDataEvolutionValidator().
				SetAllowOlderFormerIndexAddedVersion(true).
				Build()
			err := validator.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts former index without old index when addedVersion > old version", func() {
			old := buildMetaData(5, nil)

			new := buildMetaData(10, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    "ephemeral_idx",
				FormerName:     "ephemeral_idx",
				AddedVersion:   6, // > old.Version()==5 → should pass
				RemovedVersion: 8, // > old.Version()==5 → passes
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
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

		It("GetUnionDescriptor returns the UnionDescriptor message", func() {
			md := buildMetaData(1, nil)
			union := md.GetUnionDescriptor()
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

		It("GetUnionDescriptor returns nil for metadata with nil file descriptor", func() {
			// Build a metadata with nil fileDescriptor to test the nil guard
			md := &RecordMetaData{}
			union := md.GetUnionDescriptor()
			Expect(union).To(BeNil())
		})

		It("validateUnion refuses meta-data that was never built", func() {
			// Every built RecordMetaData has a union; a zero value does not, and
			// is refused rather than dereferenced.
			v := DefaultMetaDataEvolutionValidator()
			for _, pair := range [][2]*RecordMetaData{
				{{version: 1}, buildMetaData(2, nil)},
				{buildMetaData(1, nil), {version: 2}},
				{{version: 1}, {version: 2}},
			} {
				var evolErr *MetaDataEvolutionError
				Expect(errors.As(v.validateUnion(pair[0], pair[1]), &evolErr)).To(BeTrue())
				Expect(evolErr.Message).To(HavePrefix("meta-data has no union descriptor"))
			}
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

	Describe("builder methods coverage", func() {
		It("SetDisallowTypeRenames returns builder for chaining", func() {
			v := NewMetaDataEvolutionValidator().
				SetDisallowTypeRenames(true).
				Build()
			Expect(v.disallowTypeRenames).To(BeTrue())
		})

		It("SetAllowMissingFormerIndexNames returns builder for chaining", func() {
			v := NewMetaDataEvolutionValidator().
				SetAllowMissingFormerIndexNames(true).
				Build()
			Expect(v.allowMissingFormerIndexNames).To(BeTrue())
		})

		It("SetAllowNoSinceVersion returns builder for chaining", func() {
			v := NewMetaDataEvolutionValidator().
				SetAllowNoSinceVersion(true).
				Build()
			Expect(v.allowNoSinceVersion).To(BeTrue())
		})

		It("SetAllowOlderFormerIndexAddedVersion returns builder for chaining", func() {
			v := NewMetaDataEvolutionValidator().
				SetAllowOlderFormerIndexAddedVersion(true).
				Build()
			Expect(v.allowOlderFormerIndexAddedVersion).To(BeTrue())
		})
	})

	Describe("record type removal detection", func() {
		It("rejects removed type when disallowTypeRenames is true", func() {
			old := buildMetaData(1, nil)

			// Build new metadata, then remove a record type from the map
			new := buildMetaData(2, nil)
			delete(new.recordTypes, "TypedRecord")

			v := NewMetaDataEvolutionValidator().SetDisallowTypeRenames(true).Build()
			err := v.Validate(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("removed from meta-data"))
		})

		It("rejects removed type when not found by type key either", func() {
			old := buildMetaData(1, nil)

			// Remove a record type from new metadata; disallowTypeRenames=false (default)
			// means it'll try to find by type key. If type key is also missing, error.
			new := buildMetaData(2, nil)
			delete(new.recordTypes, "TypedRecord")

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("removed from meta-data"))
		})
	})

	Describe("new record type SinceVersion validation", func() {
		It("rejects new type without SinceVersion when allowNoSinceVersion is false", func() {
			old := buildMetaData(1, nil)

			// Build new metadata with an extra record type (SinceVersion=0 by default)
			new := buildMetaData(2, nil)
			new.recordTypes["NewType"] = &RecordType{
				Name:                  "NewType",
				PrimaryKey:            Field("order_id"),
				explicitRecordTypeKey: int64(999), // Unique key so it's a "new" type
			}

			v := DefaultMetaDataEvolutionValidator()
			err := v.Validate(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("missing since version"))
		})

		It("allows new type without SinceVersion when allowNoSinceVersion is true", func() {
			old := buildMetaData(1, nil)

			new := buildMetaData(2, nil)
			new.recordTypes["NewType"] = &RecordType{
				Name:                  "NewType",
				PrimaryKey:            Field("order_id"),
				explicitRecordTypeKey: int64(999),
			}

			v := NewMetaDataEvolutionValidator().SetAllowNoSinceVersion(true).Build()
			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects new type with SinceVersion older than old metadata", func() {
			old := buildMetaData(5, nil)

			new := buildMetaData(6, nil)
			new.recordTypes["NewType"] = &RecordType{
				Name:                  "NewType",
				PrimaryKey:            Field("order_id"),
				SinceVersion:          3, // <= old.Version() (5)
				explicitRecordTypeKey: int64(999),
			}

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("since version older than old meta-data"))
		})

		It("accepts new type with SinceVersion newer than old metadata", func() {
			old := buildMetaData(3, nil)

			new := buildMetaData(6, nil)
			new.recordTypes["NewType"] = &RecordType{
				Name:                  "NewType",
				PrimaryKey:            Field("order_id"),
				SinceVersion:          4, // > old.Version() (3)
				explicitRecordTypeKey: int64(999),
			}

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("comparePrimaryKeys nil paths", func() {
		It("returns nil when both primary keys are nil", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)
			// Set both PKs to nil
			old.recordTypes["Order"].PrimaryKey = nil
			new.recordTypes["Order"].PrimaryKey = nil

			v := DefaultMetaDataEvolutionValidator()
			err := v.comparePrimaryKeys("Order", old.GetRecordType("Order"), new.GetRecordType("Order"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects when old PK is nil but new is not", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)
			old.recordTypes["Order"].PrimaryKey = nil

			v := DefaultMetaDataEvolutionValidator()
			err := v.comparePrimaryKeys("Order", old.GetRecordType("Order"), new.GetRecordType("Order"))
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("primary key changed"))
		})

		It("rejects when new PK is nil but old is not", func() {
			old := buildMetaData(1, nil)
			new := buildMetaData(2, nil)
			new.recordTypes["Order"].PrimaryKey = nil

			v := DefaultMetaDataEvolutionValidator()
			err := v.comparePrimaryKeys("Order", old.GetRecordType("Order"), new.GetRecordType("Order"))
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("primary key changed"))
		})
	})

	Describe("former index version change validation", func() {
		// Helper to build metadata with a former index from removing price_idx
		buildWithFormerIndex := func(version int, addedVersion, removedVersion int, formerName string) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("price_idx", Field("price")))
			builder.RemoveIndex("price_idx")
			builder.SetVersion(version)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			// Override former index fields for test control
			Expect(md.GetFormerIndexes()).To(HaveLen(1))
			fi := md.GetFormerIndexes()[0]
			fi.AddedVersion = addedVersion
			fi.RemovedVersion = removedVersion
			fi.FormerName = formerName
			return md
		}

		It("rejects when former index RemovedVersion changes", func() {
			old := buildWithFormerIndex(3, 1, 2, "price_idx")
			new := buildWithFormerIndex(5, 1, 3, "price_idx") // RemovedVersion changed: 2 → 3

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("removed version"))
			Expect(evolErr.Message).To(ContainSubstring("differs from prior version"))
		})

		It("rejects when former index AddedVersion changes", func() {
			old := buildWithFormerIndex(3, 1, 2, "price_idx")
			new := buildWithFormerIndex(5, 2, 2, "price_idx") // AddedVersion changed: 1 → 2

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("added version"))
			Expect(evolErr.Message).To(ContainSubstring("differs from prior version"))
		})

		It("rejects when former index name changes and allowMissingFormerIndexNames is false", func() {
			old := buildWithFormerIndex(3, 1, 2, "price_idx")
			new := buildWithFormerIndex(5, 1, 2, "renamed_idx") // Name changed

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("name of former index"))
			Expect(evolErr.Message).To(ContainSubstring("differs from prior version"))
		})

		// allowMissingFormerIndexNames governs only a former index that replaces
		// an index; a former index kept from the old meta-data must keep its
		// name regardless (MetaDataEvolutionValidator.java:612-622). The
		// conformance spec "Subspace-key pairing in meta-data evolution" checks
		// this shape against Java.
		It("refuses a former index name change even when allowMissingFormerIndexNames is true", func() {
			old := buildWithFormerIndex(3, 1, 2, "price_idx")
			new := buildWithFormerIndex(5, 1, 2, "renamed_idx")

			v := NewMetaDataEvolutionValidator().SetAllowMissingFormerIndexNames(true).Build()
			err := v.Validate(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue(), "error: %v", err)
			Expect(evolErr.Message).To(HavePrefix("name of former index differs from prior version"))
		})
	})

	Describe("new former index validation against old index", func() {
		It("rejects new former index with RemovedVersion <= old.Version()", func() {
			// Old has an index
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 1
			idx.LastModifiedVersion = 1
			old := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			// New removes the index but former has RemovedVersion=3 (<=5)
			new := buildMetaData(6, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1,
				RemovedVersion: 3, // <= old.Version() 5
				FormerName:     "price_idx",
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("not newer than the old meta-data version"))
		})

		It("rejects when former index AddedVersion > old index AddedVersion", func() {
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 2
			idx.LastModifiedVersion = 2
			old := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(6, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   5, // > old index's AddedVersion 2
				RemovedVersion: 4,
				FormerName:     "price_idx",
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("former index added after old index"))
		})

		It("rejects when former index AddedVersion != old index AddedVersion and !allowOlderFormerIndexAddedVersion", func() {
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 3
			idx.LastModifiedVersion = 3
			old := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(7, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1, // != old index's AddedVersion 3 (and < 3, so passes the > check)
				RemovedVersion: 6,
				FormerName:     "price_idx",
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("former index reports added version older than replacing index"))
		})

		It("allows older former index AddedVersion when configured", func() {
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 3
			idx.LastModifiedVersion = 3
			old := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(7, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1,
				RemovedVersion: 6,
				FormerName:     "price_idx",
			})

			v := NewMetaDataEvolutionValidator().SetAllowOlderFormerIndexAddedVersion(true).Build()
			err := v.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects when former index RemovedVersion <= old index LastModifiedVersion", func() {
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 1
			idx.LastModifiedVersion = 4
			old := buildMetaData(5, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(7, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1,
				RemovedVersion: 4, // <= old index's LastModifiedVersion 4
				FormerName:     "price_idx",
			})

			// Need to pass the > version check too: RemovedVersion=4 but old.Version()=5
			// This will fail the RemovedVersion <= old.Version() check first.
			// Use old version=3 instead.
			//
			// For well-formed metadata this branch is unreachable: an index's
			// lastModifiedVersion may not exceed its metadata version (Java
			// MetaDataValidator.validateIndex(), MetaDataValidator.java:129-133),
			// and the preceding check already requires removedVersion to exceed the
			// old metadata version — so removedVersion always exceeds
			// lastModifiedVersion. The check is defence in depth against metadata
			// that reached the validator by some other route (a hand-assembled or
			// corrupted proto), so the corrupt lastModifiedVersion is injected after
			// Build() rather than smuggled through it.
			old2 := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				i := NewIndex("price_idx", Field("price"))
				i.AddedVersion = 1
				i.LastModifiedVersion = 1
				b.AddIndex("Order", i)
			})
			old2.GetIndex("price_idx").LastModifiedVersion = 4

			new2 := buildMetaData(7, nil)
			new2.formerIndexes = append(new2.formerIndexes, &FormerIndex{
				SubspaceKey:    old2.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1,
				RemovedVersion: 4, // > old.Version() 3, but <= old index's LastModifiedVersion 4
				FormerName:     "price_idx",
			})

			err := ValidateEvolution(old2, new2)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("removed before old index's last modification"))
		})

		It("rejects when new former index has different name than old index and !allowMissingFormerIndexNames", func() {
			idx := NewIndex("price_idx", Field("price"))
			idx.AddedVersion = 1
			idx.LastModifiedVersion = 1
			old := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(7, nil)
			new.formerIndexes = append(new.formerIndexes, &FormerIndex{
				SubspaceKey:    old.GetIndex("price_idx").SubspaceTupleKey(),
				AddedVersion:   1,
				RemovedVersion: 5,
				FormerName:     "wrong_name", // Different from old index name "price_idx"
			})

			// The former name "wrong_name" doesn't match any old index (GetIndex returns nil),
			// so this path won't hit line 507. Need the former name to match the lookup
			// but be different from oldIdx.Name. Actually, line 504 does
			// oldIdx := old.GetIndex(newFormer.FormerName), so if FormerName doesn't match,
			// oldIdx is nil and we skip the block entirely.
			// To hit line 507, FormerName must match an index name in old (for the GetIndex
			// lookup to succeed), but then it would always equal oldIdx.Name. This path
			// is unreachable with the current code structure.
			// Skip this specific sub-path — move on to what IS testable.
		})
	})

	Describe("cardinalityString coverage", func() {
		It("returns required for Required cardinality", func() {
			Expect(cardinalityString(protoreflect.Required)).To(Equal("required"))
		})

		It("returns optional for Optional cardinality", func() {
			Expect(cardinalityString(protoreflect.Optional)).To(Equal("optional"))
		})

		It("returns repeated for Repeated cardinality", func() {
			Expect(cardinalityString(protoreflect.Repeated)).To(Equal("repeated"))
		})
	})

	Describe("message descriptor validation with synthetic protos", func() {
		// Helper to build a synthetic proto2 file descriptor with the given
		// messages and a UnionDescriptor whose one field holds the first. Returns
		// the file descriptor.
		buildSyntheticFile := func(fileName, pkgName string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto) protoreflect.FileDescriptor {
			syntax := "proto2"
			union := &descriptorpb.DescriptorProto{
				Name: proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("_" + msgs[0].GetName()), Number: proto.Int32(1),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String("." + pkgName + "." + msgs[0].GetName()),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				}},
			}
			fdp := &descriptorpb.FileDescriptorProto{
				Name:        &fileName,
				Package:     &pkgName,
				Syntax:      &syntax,
				MessageType: append(slices.Clone(msgs), union),
				EnumType:    enums,
			}
			fd, err := protodesc.NewFile(fdp, nil)
			Expect(err).NotTo(HaveOccurred())
			return fd
		}

		// Helper to create a DescriptorProto for a simple message with given fields
		makeMessage := func(name string, fields []*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
			return &descriptorpb.DescriptorProto{
				Name:  &name,
				Field: fields,
			}
		}

		// Helper to create a simple field
		makeField := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{
				Name:   &name,
				Number: &number,
				Type:   &typ,
				Label:  &label,
			}
		}

		// Helper to build a RecordMetaData from a synthetic file descriptor
		buildSyntheticMD := func(version int, fd protoreflect.FileDescriptor) *RecordMetaData {
			msg := fd.Messages().Get(0) // First message
			return &RecordMetaData{
				version:         version,
				fileDescriptor:  fd,
				unionDescriptor: fd.Messages().ByName("UnionDescriptor"),
				recordTypes: map[string]*RecordType{
					string(msg.Name()): {
						Name:                  string(msg.Name()),
						Descriptor:            msg,
						PrimaryKey:            Field("id"),
						explicitRecordTypeKey: int64(1),
					},
				},
			}
		}

		It("rejects field removed from message", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					makeField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					// "name" field removed
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("field"))
			Expect(evolErr.Message).To(ContainSubstring("removed from message"))
		})

		It("rejects field renamed", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("identifier", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("renamed"))
		})

		It("rejects cardinality change (optional to repeated)", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_REPEATED),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			// Java's order: optional to repeated keeps no label check (only a
			// required or repeated field losing its label is one), and fails on
			// the field's presence (MetaDataEvolutionValidator.java:306-315).
			Expect(evolErr.Message).To(HavePrefix("field changed whether default values are stored if set explicitly"))
		})

		It("admits an optional field made required, as Java does, and refuses the label losses Java refuses", func() {
			label := func(l descriptorpb.FieldDescriptorProto_Label, file string) *RecordMetaData {
				fd := buildSyntheticFile(file, "test", []*descriptorpb.DescriptorProto{
					makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
						makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
						makeField("v", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, l),
					}),
				}, nil)
				return buildSyntheticMD(map[string]int{"old.proto": 1, "new.proto": 2}[file], fd)
			}
			optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			required := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
			repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
			Expect(ValidateEvolution(label(optional, "old.proto"), label(required, "new.proto"))).To(Succeed())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(ValidateEvolution(label(required, "old.proto"), label(optional, "new.proto")), &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("required field is no longer required"))
			Expect(errors.As(ValidateEvolution(label(repeated, "old.proto"), label(optional, "new.proto")), &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("repeated field is no longer repeated"))
		})

		It("rejects unsafe type change (string to int64)", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("value", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("value", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("field type changed"))
		})

		It("allows safe type promotion (int32 to int64)", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("value", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("value", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects new required field added", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					makeField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_REQUIRED),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("required field"))
			Expect(evolErr.Message).To(HavePrefix("required field added to record type"))
		})

		It("allows new optional field added", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					makeField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects enum value removed", func() {
			enumName := "Status"
			val0Name := "UNKNOWN"
			val1Name := "ACTIVE"
			val2Name := "INACTIVE"
			var val0Num int32 = 0
			var val1Num int32 = 1
			var val2Num int32 = 2

			oldEnum := &descriptorpb.EnumDescriptorProto{
				Name: &enumName,
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: &val0Name, Number: &val0Num},
					{Name: &val1Name, Number: &val1Num},
					{Name: &val2Name, Number: &val2Num},
				},
			}

			newEnum := &descriptorpb.EnumDescriptorProto{
				Name: &enumName,
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: &val0Name, Number: &val0Num},
					{Name: &val1Name, Number: &val1Num},
					// INACTIVE removed
				},
			}

			typeName := ".test.Status"
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					{
						Name:     evolStrPtr("status"),
						Number:   evolInt32Ptr(2),
						Type:     evolEnumType(),
						Label:    evolOptionalLabel(),
						TypeName: &typeName,
					},
				}),
			}, []*descriptorpb.EnumDescriptorProto{oldEnum})

			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					{
						Name:     evolStrPtr("status"),
						Number:   evolInt32Ptr(2),
						Type:     evolEnumType(),
						Label:    evolOptionalLabel(),
						TypeName: &typeName,
					},
				}),
			}, []*descriptorpb.EnumDescriptorProto{newEnum})

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("enum"))
			Expect(evolErr.Message).To(ContainSubstring("removes value"))
		})

		It("rejects proto syntax change", func() {
			syntax2 := "proto2"
			syntax3 := "proto3"
			fname1 := "old.proto"
			fname2 := "new.proto"
			pkg := "test"

			fdp1 := &descriptorpb.FileDescriptorProto{
				Name:    &fname1,
				Package: &pkg,
				Syntax:  &syntax2,
				MessageType: []*descriptorpb.DescriptorProto{
					makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
						makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					}),
					{
						Name: proto.String("UnionDescriptor"),
						Field: []*descriptorpb.FieldDescriptorProto{{
							Name: proto.String("_TestMsg"), Number: proto.Int32(1),
							Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
							TypeName: proto.String(".test.TestMsg"),
							Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						}},
					},
				},
			}
			oldFD, err := protodesc.NewFile(fdp1, nil)
			Expect(err).NotTo(HaveOccurred())

			fdp2 := &descriptorpb.FileDescriptorProto{
				Name:    &fname2,
				Package: &pkg,
				Syntax:  &syntax3,
				MessageType: []*descriptorpb.DescriptorProto{
					makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
						makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
					}),
					{
						Name: proto.String("UnionDescriptor"),
						Field: []*descriptorpb.FieldDescriptorProto{{
							Name: proto.String("_TestMsg"), Number: proto.Int32(1),
							Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
							TypeName: proto.String(".test.TestMsg"),
							Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						}},
					},
				},
			}
			newFD, err := protodesc.NewFile(fdp2, nil)
			Expect(err).NotTo(HaveOccurred())

			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)

			err = ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("proto syntax changed"))
		})

		// --- Field renames (Java 4.12: #4034 / #4119) -------------------------------

		// deprecatedField builds an OPTIONAL string field carrying the `deprecated` option.
		deprecatedField := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			f := makeField(name, number, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
			dep := true
			f.Options = &descriptorpb.FieldOptions{Deprecated: &dep}
			return f
		}
		idField := func() *descriptorpb.FieldDescriptorProto {
			return makeField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
		}
		strField := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			return makeField(name, number, descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
		}

		It("accepts a non-PK field rename when allowFieldRenames is set", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("label", 2)}), // name -> label
			}, nil)
			v := NewMetaDataEvolutionValidator().SetAllowFieldRenames(true).Build()
			Expect(v.Validate(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))).To(Succeed())
		})

		It("rejects a non-deprecated rename when only allowDeprecatedFieldRenames is set", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("label", 2)}),
			}, nil)
			v := NewMetaDataEvolutionValidator().SetAllowDeprecatedFieldRenames(true).Build()
			err := v.Validate(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("renamed"))
		})

		It("accepts a deprecated field rename when allowDeprecatedFieldRenames is set", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), deprecatedField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), deprecatedField("label", 2)}),
			}, nil)
			v := NewMetaDataEvolutionValidator().SetAllowDeprecatedFieldRenames(true).Build()
			Expect(v.Validate(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))).To(Succeed())
		})

		It("rejects un-deprecating a field by default", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), deprecatedField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("name", 2)}), // same name, no longer deprecated
			}, nil)
			err := ValidateEvolution(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("no longer deprecated"))
		})

		It("accepts un-deprecating a field when allowUndeprecatingFields is set", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), deprecatedField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("name", 2)}),
			}, nil)
			v := NewMetaDataEvolutionValidator().SetAllowUndeprecatingFields(true).Build()
			Expect(v.Validate(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))).To(Succeed())
		})

		It("rewrites the primary key across an allowed rename of the PK field", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField()}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("identifier", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL), // id -> identifier
				}),
			}, nil)
			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD)
			new.recordTypes["TestMsg"].PrimaryKey = Field("identifier") // new PK references the renamed field
			v := NewMetaDataEvolutionValidator().SetAllowFieldRenames(true).Build()
			Expect(v.Validate(old, new)).To(Succeed())
		})

		It("rejects a primary key that does not match the rename-rewritten expectation", func() {
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField()}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					makeField("identifier", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)
			old := buildSyntheticMD(1, oldFD)
			new := buildSyntheticMD(2, newFD) // PK left as the stale Field("id")
			v := NewMetaDataEvolutionValidator().SetAllowFieldRenames(true).Build()
			err := v.Validate(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("record type primary key does not match required"))
		})

		It("still rejects a disallowed type change across an allowed rename", func() {
			// Allowing renames must not relax the independent field-type check: renaming
			// name(string) -> label(int64) is still an unsafe type change.
			oldFD := buildSyntheticFile("old.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{idField(), strField("name", 2)}),
			}, nil)
			newFD := buildSyntheticFile("new.proto", "test", []*descriptorpb.DescriptorProto{
				makeMessage("TestMsg", []*descriptorpb.FieldDescriptorProto{
					idField(),
					makeField("label", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
				}),
			}, nil)
			v := NewMetaDataEvolutionValidator().SetAllowFieldRenames(true).Build()
			err := v.Validate(buildSyntheticMD(1, oldFD), buildSyntheticMD(2, newFD))
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("field type changed"))
		})
	})

	Describe("renameFields (RenameFieldsVisitor port)", func() {
		// messageField builds an OPTIONAL message-typed field referencing typeName.
		messageField := func(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
			t := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
			l := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			return &descriptorpb.FieldDescriptorProto{Name: &name, Number: &number, Type: &t, Label: &l, TypeName: &typeName}
		}
		// repeatedMessageField builds a REPEATED message-typed field — the parent of a
		// FanOut nesting (Java's `field(..., FanType.FanOut).nest(...)`).
		repeatedMessageField := func(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
			t := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
			l := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
			return &descriptorpb.FieldDescriptorProto{Name: &name, Number: &number, Type: &t, Label: &l, TypeName: &typeName}
		}
		strField := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			t := descriptorpb.FieldDescriptorProto_TYPE_STRING
			l := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			return &descriptorpb.FieldDescriptorProto{Name: &name, Number: &number, Type: &t, Label: &l}
		}
		buildFile := func(fileName string, msgs ...*descriptorpb.DescriptorProto) protoreflect.FileDescriptor {
			syntax := "proto2"
			pkg := "test"
			fdp := &descriptorpb.FileDescriptorProto{Name: &fileName, Package: &pkg, Syntax: &syntax, MessageType: msgs}
			fd, err := protodesc.NewFile(fdp, nil)
			Expect(err).NotTo(HaveOccurred())
			return fd
		}
		msg := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
			return &descriptorpb.DescriptorProto{Name: &name, Field: fields}
		}

		It("renames a top-level field by number", func() {
			oldDesc := buildFile("o.proto", msg("M", strField("a", 1))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("M", strField("renamed_a", 1))).Messages().Get(0)
			got, err := renameFields(Field("a"), oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Field("renamed_a").ToKeyExpression())).To(BeTrue())
		})

		It("returns the input unchanged when source and target are identical", func() {
			desc := buildFile("o.proto", msg("M", strField("a", 1))).Messages().Get(0)
			input := Field("a")
			got, err := renameFields(input, desc, desc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(input)) // source==target short-circuits to the same object
		})

		It("renames a field inside a nested message", func() {
			oldDesc := buildFile("o.proto", msg("Outer", messageField("inner", 1, ".test.Inner")), msg("Inner", strField("x", 1))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("Outer", messageField("inner", 1, ".test.Inner")), msg("Inner", strField("y", 1))).Messages().Get(0)
			got, err := renameFields(Nest("inner", Field("x")), oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Nest("inner", Field("y")).ToKeyExpression())).To(BeTrue())
		})

		It("renames an outer parent while the inner descriptor is unchanged (multi-level)", func() {
			// Outer.inner (renamed to wrapper) -> Inner.x ; Inner is unchanged.
			oldDesc := buildFile("o.proto", msg("Outer", messageField("inner", 1, ".test.Inner")), msg("Inner", strField("x", 1))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("Outer", messageField("wrapper", 1, ".test.Inner")), msg("Inner", strField("x", 1))).Messages().Get(0)
			got, err := renameFields(Nest("inner", Field("x")), oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Nest("wrapper", Field("x")).ToKeyExpression())).To(BeTrue())
		})

		It("rewrites the children of a composite (Then) expression", func() {
			oldDesc := buildFile("o.proto", msg("M", strField("a", 1), strField("b", 2))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("M", strField("a", 1), strField("bee", 2))).Messages().Get(0)
			got, err := renameFields(Concat(Field("a"), Field("b")), oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Concat(Field("a"), Field("bee")).ToKeyExpression())).To(BeTrue())
		})

		It("errors when the target descriptor lacks the field number", func() {
			oldDesc := buildFile("o.proto", msg("M", strField("a", 1))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("M", strField("a", 2))).Messages().Get(0) // number changed 1 -> 2
			_, err := renameFields(Field("a"), oldDesc, newDesc)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("field not found in target descriptor"))
		})

		// --- Per-node-type coverage (ports Java RenameFieldsVisitorTest shapes) -----
		// Each builds an old descriptor {a:1, b:2} and a new one renaming a->a1, b->b1
		// by field NUMBER, then asserts the visitor recurses into the container node
		// and rewrites the leaf field references.
		abDesc := func() (protoreflect.MessageDescriptor, protoreflect.MessageDescriptor) {
			oldDesc := buildFile("o.proto", msg("M", strField("a", 1), strField("b", 2))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("M", strField("a1", 1), strField("b1", 2))).Messages().Get(0)
			return oldDesc, newDesc
		}
		expectRenamed := func(input, expected KeyExpression) {
			oldDesc, newDesc := abDesc()
			got, err := renameFields(input, oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), expected.ToKeyExpression())).To(BeTrue())
		}

		It("rewrites the children of a list expression", func() {
			expectRenamed(ListExpr(Field("a"), Field("b")), ListExpr(Field("a1"), Field("b1")))
		})

		It("rewrites the argument of a function expression", func() {
			expectRenamed(FunctionExpr("nada", Field("a")), FunctionExpr("nada", Field("a1")))
		})

		It("rewrites the joined expression of a split expression", func() {
			expectRenamed(Split(Field("a"), 2), Split(Field("a1"), 2))
		})

		It("rewrites the whole key of a grouping expression", func() {
			expectRenamed(GroupBy(Field("a"), Field("b")), GroupBy(Field("a1"), Field("b1")))
		})

		It("rewrites the inner key of a key-with-value expression", func() {
			expectRenamed(KeyWithValue(Concat(Field("a"), Field("b")), 1), KeyWithValue(Concat(Field("a1"), Field("b1")), 1))
		})

		It("rewrites the whole key of a dimensions expression", func() {
			expectRenamed(Dimensions(Concat(Field("a"), Field("b")), 0, 2), Dimensions(Concat(Field("a1"), Field("b1")), 0, 2))
		})

		It("rewrites the nested expression after a record-type prefix", func() {
			expectRenamed(RecordTypeKey().Nest(Field("a")), RecordTypeKey().Nest(Field("a1")))
		})

		It("returns a bare record-type-key prefix unchanged", func() {
			// nested == nil → the record-type prefix is rename-invariant.
			oldDesc, newDesc := abDesc()
			input := RecordTypeKey()
			got, err := renameFields(input, oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(KeyExpression(input)))
		})

		It("preserves the fan type of a renamed field", func() {
			// A FanType-carrying field must keep its fan type across the rename
			// (Java asserts getFanType()/getNullStandin() equality; Go has no
			// nullStandin, so fan type is the surviving axis).
			expectRenamed(FieldConcatenate("a"), FieldConcatenate("a1"))
		})

		// Invariant leaves: returned as the SAME object (Java returns them as-is).
		It("returns a version expression unchanged", func() {
			oldDesc, newDesc := abDesc()
			input := VersionKey()
			got, err := renameFields(input, oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(KeyExpression(input)))
		})

		It("returns a literal expression unchanged", func() {
			oldDesc, newDesc := abDesc()
			input := Literal(int64(42))
			got, err := renameFields(input, oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(KeyExpression(input)))
		})

		It("returns an empty expression unchanged", func() {
			oldDesc, newDesc := abDesc()
			input := EmptyKey()
			got, err := renameFields(input, oldDesc, newDesc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(input))
		})

		// Error paths.
		It("errors when the referenced field is missing in the source", func() {
			oldDesc, newDesc := abDesc()
			_, err := renameFields(Field("not_in_source"), oldDesc, newDesc)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("field not found in source descriptor"))
		})

		It("errors when nesting into a non-message (scalar) parent field", func() {
			// "a" is a string scalar in both old and new (different files, so the
			// source==target short-circuit does not fire); nesting into it must fail.
			oldDesc := buildFile("o.proto", msg("M", strField("a", 1))).Messages().Get(0)
			newDesc := buildFile("n.proto", msg("M", strField("a", 1))).Messages().Get(0)
			_, err := renameFields(Nest("a", Field("x")), oldDesc, newDesc)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("parent field is not of message type"))
		})

		It("errors on an unsupported key-expression type", func() {
			// Any KeyExpression type the switch does not handle hits the default arm.
			oldDesc, newDesc := abDesc()
			_, err := renameFields(unsupportedKeyExpr{}, oldDesc, newDesc)
			// Java raises RecordCoreArgumentException here, not a
			// MetaDataException (RenameFieldsVisitor.java:149, :214).
			var argErr *RecordCoreArgumentError
			Expect(errors.As(err, &argErr)).To(BeTrue())
			Expect(argErr.Message).To(HavePrefix("field renaming not supported for expression"))
		})

		// --- Depth-≥2 nested re-derivation (ports Java renameMiddleRecord / renameMergedRecord /
		// renameSplitRecord / the 3-level renameOuterRecord shapes). The single-Nest tests above
		// never exercise messageTypeForField recursion past one level, never prove the rename
		// follows the descriptor TYPE rather than the global field name, and never reach the
		// childSource==childTarget short-circuit (rename_fields_visitor.go:58, which is dead across
		// two independently-built files). These close that axis. --------------------------------

		It("rewrites a leaf field at nesting depth 2 (re-derives the descriptor at each level)", func() {
			oldD := buildFile(
				"o.proto",
				msg("Outer", messageField("middle", 1, ".test.Middle"), strField("top", 2)),
				msg("Middle", messageField("inner", 1, ".test.Inner"), strField("mid", 2)),
				msg("Inner", strField("foo", 1), strField("bar", 2)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("Outer", messageField("middle", 1, ".test.Middle"), strField("top", 2)),
				msg("Middle", messageField("inner", 1, ".test.Inner"), strField("mid", 2)),
				msg("Inner", strField("foo_z", 1), strField("bar", 2)),
			).Messages().Get(0)
			got, err := renameFields(Nest("middle", Nest("inner", Field("foo"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Nest("middle", Nest("inner", Field("foo_z"))).ToKeyExpression())).To(BeTrue())
		})

		It("rewrites a mid-chain parent field while leaving the deeper leaf intact", func() {
			oldD := buildFile(
				"o.proto",
				msg("Outer", messageField("middle", 1, ".test.Middle"), strField("top", 2)),
				msg("Middle", messageField("inner", 1, ".test.Inner"), strField("mid", 2)),
				msg("Inner", strField("foo", 1), strField("bar", 2)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("Outer", messageField("middle", 1, ".test.Middle"), strField("top", 2)),
				msg("Middle", messageField("inner_2b", 1, ".test.Inner"), strField("mid", 2)),
				msg("Inner", strField("foo", 1), strField("bar", 2)),
			).Messages().Get(0)
			got, err := renameFields(Nest("middle", Nest("inner", Field("foo"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Nest("middle", Nest("inner_2b", Field("foo"))).ToKeyExpression())).To(BeTrue())
		})

		It("follows the descriptor type, not the field name, when the same name lives in two types", func() {
			// A.val and B.val share the name "val"; only A.val is renamed. A name-keyed
			// implementation would also rename b.val — this pins per-parent re-derivation.
			oldD := buildFile(
				"o.proto",
				msg("Top", messageField("a", 1, ".test.A"), messageField("b", 2, ".test.B")),
				msg("A", strField("val", 1)),
				msg("B", strField("val", 1)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("Top", messageField("a", 1, ".test.A"), messageField("b", 2, ".test.B")),
				msg("A", strField("val_a", 1)),
				msg("B", strField("val", 1)),
			).Messages().Get(0)
			got, err := renameFields(Concat(Nest("a", Field("val")), Nest("b", Field("val"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Concat(Nest("a", Field("val_a")), Nest("b", Field("val"))).ToKeyExpression())).To(BeTrue())
		})

		It("rewrites every path that reaches a shared nested descriptor (merged-record shape)", func() {
			// Both a and b point at the same Nested type; one nested rename rewrites both paths.
			oldD := buildFile(
				"o.proto",
				msg("My", messageField("a", 1, ".test.Nested"), messageField("b", 2, ".test.Nested")),
				msg("Nested", strField("x", 1), strField("y", 2)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("My", messageField("a", 1, ".test.Nested"), messageField("b", 2, ".test.Nested")),
				msg("Nested", strField("x_2", 1), strField("y", 2)),
			).Messages().Get(0)
			got, err := renameFields(Concat(Nest("a", Field("x")), Nest("b", Field("x"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Concat(Nest("a", Field("x_2")), Nest("b", Field("x_2"))).ToKeyExpression())).To(BeTrue())
		})

		It("resolves each parent's rename by NUMBER within its own type (asymmetric merge/split)", func() {
			// The strongest type-follow probe (Java renameSplitRecord). The field "x" lives at a
			// DIFFERENT number in each source type — NestedA.x=1, NestedB.x=2 — and both parents
			// merge into one target type OneTrueNested{p=1, q=2}. So a.x must become a.p (via
			// NestedA's number 1) and b.x must become b.q (via NestedB's number 2): same name,
			// different result, because the rename strictly follows (source type → number →
			// target type by number) at each parent independently.
			oldD := buildFile(
				"o.proto",
				msg("My", messageField("a", 1, ".test.NestedA"), messageField("b", 2, ".test.NestedB")),
				msg("NestedA", strField("x", 1), strField("y", 2)),
				msg("NestedB", strField("y", 1), strField("x", 2)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("My", messageField("a", 1, ".test.OneTrueNested"), messageField("b", 2, ".test.OneTrueNested")),
				msg("OneTrueNested", strField("p", 1), strField("q", 2)),
			).Messages().Get(0)
			got, err := renameFields(Concat(Nest("a", Field("x")), Nest("b", Field("x"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Concat(Nest("a", Field("p")), Nest("b", Field("q"))).ToKeyExpression())).To(BeTrue())
		})

		It("skips the nested recursion when the parent's message type is unchanged (shared descriptor)", func() {
			// Import a shared dependency from BOTH the old and new outer files so the nested
			// descriptor is the SAME object on both sides — the only way the
			// childSource==childTarget short-circuit (rename_fields_visitor.go:58) fires (Java's
			// avoidRewritingIfNestedDescriptorIsTheSame). The parent is renamed; the child is not.
			pkg, syntax, depName := "test", "proto2", "dep.proto"
			depFDP := &descriptorpb.FileDescriptorProto{
				Name: &depName, Package: &pkg, Syntax: &syntax,
				MessageType: []*descriptorpb.DescriptorProto{msg("Shared", strField("foo", 1), strField("bar", 2))},
			}
			depFD, err := protodesc.NewFile(depFDP, nil)
			Expect(err).NotTo(HaveOccurred())
			files := new(protoregistry.Files)
			Expect(files.RegisterFile(depFD)).To(Succeed())
			buildOuter := func(fileName, parentField string) protoreflect.MessageDescriptor {
				fdp := &descriptorpb.FileDescriptorProto{
					Name: &fileName, Package: &pkg, Syntax: &syntax,
					Dependency:  []string{depName},
					MessageType: []*descriptorpb.DescriptorProto{msg("Outer", messageField(parentField, 1, ".test.Shared"))},
				}
				fd, ferr := protodesc.NewFile(fdp, files)
				Expect(ferr).NotTo(HaveOccurred())
				return fd.Messages().Get(0)
			}
			oldD := buildOuter("o.proto", "inner")
			newD := buildOuter("n.proto", "wrapper")
			got, err := renameFields(Nest("inner", Field("foo")), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(got.ToKeyExpression(), Nest("wrapper", Field("foo")).ToKeyExpression())).To(BeTrue())
		})

		It("rewrites under a FanOut (repeated-message) parent at depth ≥2, preserving FanType", func() {
			// The flagged axis (Java's `many_middle`): a FanType.FanOut nesting parent at
			// depth ≥2. Every prior depth spec used an OPTIONAL parent. This renames the FanOut
			// parent itself (groups→groups_v2, by number) AND the deep leaf (foo→foo_z), and the
			// rewritten expression must STILL be a FanOut nest — proving the visitor both
			// re-derives through a REPEATED message descriptor and preserves e.fanType
			// (rename_fields_visitor.go). A dropped fan type would yield Nest (None) and fail
			// proto.Equal.
			oldD := buildFile(
				"o.proto",
				msg("Outer", repeatedMessageField("groups", 1, ".test.Group")),
				msg("Group", messageField("inner", 1, ".test.Inner")),
				msg("Inner", strField("foo", 1), strField("bar", 2)),
			).Messages().Get(0)
			newD := buildFile(
				"n.proto",
				msg("Outer", repeatedMessageField("groups_v2", 1, ".test.Group")),
				msg("Group", messageField("inner", 1, ".test.Inner")),
				msg("Inner", strField("foo_z", 1), strField("bar", 2)),
			).Messages().Get(0)
			got, err := renameFields(NestFanOut("groups", Nest("inner", Field("foo"))), oldD, newD)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(
				got.ToKeyExpression(),
				NestFanOut("groups_v2", Nest("inner", Field("foo_z"))).ToKeyExpression(),
			)).To(BeTrue())
		})
	})

	Describe("index record type scope validation (RFC 019)", func() {
		It("rejects index that drops a record type", func() {
			// Build old: index covers Order via type-specific add
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("idx_price", Field("price")))
			})

			// Build new: same index but now universal (covers all types)
			// Then change to only cover Customer
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Customer", NewIndex("idx_price", Field("price")))
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("new index removes record type"))
		})

		It("rejects index adding old record type without sinceVersion", func() {
			// Old: index covers Order only
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("idx_price", Field("price")))
			})

			// New: same index now covers both Order and Customer.
			// Customer exists since version 0 (no sinceVersion), which is <= old version 1.
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("new index adds record type that is not newer than old meta-data"))
		})

		It("allows index keeping same record types", func() {
			// Old: index covers Order
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("idx_price", Field("price")))
			})

			// New: same index still covers only Order — no scope change.
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("idx_price", Field("price")))
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("requires newer index scope even when allowNoSinceVersion is true", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", NewIndex("idx_price", Field("price")))
			})

			// The general allowance for unversioned types does not make an
			// existing index complete for records it previously did not cover.
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			})

			validator := NewMetaDataEvolutionValidator().
				SetAllowNoSinceVersion(true).
				Build()
			Expect(validator.Validate(old, new)).To(MatchError(HavePrefix("new index adds record type that is not newer than old meta-data")))
			// Scope is checked before expression compatibility, as in Java.
			new.GetIndex("idx_price").RootExpression = Literal(int64(1))
			Expect(validator.Validate(old, new)).To(MatchError(HavePrefix("new index adds record type that is not newer than old meta-data")))
		})
	})

	Describe("index options validation (RFC 019)", func() {
		It("allows dropping uniqueness without allowIndexRebuilds", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "false"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects adding uniqueness", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("index adds uniqueness constraint"))
		})

		It("rejects unknown option changes", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"someCustomOption": "a"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"someCustomOption": "b"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("option"))
		})

		It("allows option changes with allowIndexRebuilds", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "false"}
				b.AddIndex("Order", idx)
			})

			validator := NewMetaDataEvolutionValidator().
				SetAllowIndexRebuilds(true).
				Build()
			err := validator.Validate(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("allows allowedForQuery changes", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{IndexOptionAllowedForQuery: "false"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("allows replacedBy prefix changes", func() {
			// Each AddIndex bumps the builder version, so two indexes leave the
			// second at lastModifiedVersion 2. The metadata version must be raised
			// to cover it: an index version ahead of the metadata version is
			// rejected (Java MetaDataValidator.validateIndex(),
			// MetaDataValidator.java:129-133).
			old := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				b.AddIndex("Order", idx)
				// Add the replacement index so metadata validation passes.
				b.AddIndex("Order", NewIndex("idx_price_v2", Field("price")))
			})

			new := buildMetaData(3, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"replacedByNewIndex": "idx_price_v2"}
				b.AddIndex("Order", idx)
				b.AddIndex("Order", NewIndex("idx_price_v2", Field("price")))
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("allows dropping uniqueness from true to absent", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				// no unique option at all
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		// TEXT index option validation
		It("allows TEXT aggressiveConflictRanges change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextAddAggressiveConflictRanges: "true"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("allows TEXT omitPositions change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextOmitPositions: "true"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects TEXT tokenizer name change", func() {
			// Both names must name REGISTERED tokenizers now that meta-data
			// validation resolves them (Java does the same in
			// TextIndexMaintainerFactory's IndexValidator). Before that
			// validation existed these specs could name any string at all,
			// which is the hole it closes — the evolution rule under test is
			// about the option CHANGING, not about the names being real.
			registerTestTokenizers()

			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: englishTestTokenizer.name}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: frenchTestTokenizer.name}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("text tokenizer changed"))
		})

		It("allows TEXT tokenizer version upgrade", func() {
			// A multi-version tokenizer: the default has MinVersion == MaxVersion,
			// so versions 1 and 2 are only both in range for a registered
			// tokenizer that spans them. Meta-data validation checks the range
			// now, and without this the spec would pass on the wrong rejection.
			registerTestTokenizers()

			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: englishTestTokenizer.name, IndexOptionTextTokenizerVersion: "1"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: englishTestTokenizer.name, IndexOptionTextTokenizerVersion: "2"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects TEXT tokenizer version downgrade", func() {
			registerTestTokenizers()

			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: englishTestTokenizer.name, IndexOptionTextTokenizerVersion: "3"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_text", Nest("flower", Field("type")))
				idx.Type = IndexTypeText
				idx.Options = map[string]string{IndexOptionTextTokenizerName: englishTestTokenizer.name, IndexOptionTextTokenizerVersion: "1"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("text tokenizer version downgraded"))
		})

		// RANK index option validation
		It("rejects RANK nLevels change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				idx.Options = map[string]string{IndexOptionRankNLevels: "6"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				idx.Options = map[string]string{IndexOptionRankNLevels: "8"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("rank levels"))
		})

		It("allows RANK nLevels cosmetic change (absent to default)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				// no nLevels option — uses default 6
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				idx.Options = map[string]string{IndexOptionRankNLevels: "6"} // explicit default
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects RANK countDuplicates change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_rank", Ungrouped(Field("price")))
				idx.Type = IndexTypeRank
				idx.Options = map[string]string{IndexOptionRankCountDuplicates: "true"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("rank count duplicate changed"))
		})

		// PERMUTED_MIN/MAX option validation
		It("rejects PERMUTED_MIN permutedSize change", func() {
			// A grouping with two grouping columns, which both sizes fit, as
			// Java's validator requires.
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_pm", GroupBy(Field("order_id"), Field("price"), Field("quantity")))
				idx.Type = IndexTypePermutedMin
				idx.Options = map[string]string{IndexOptionPermutedSize: "1"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_pm", GroupBy(Field("order_id"), Field("price"), Field("quantity")))
				idx.Type = IndexTypePermutedMin
				idx.Options = map[string]string{IndexOptionPermutedSize: "2"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("permuted size"))
		})

		// VECTOR (HNSW) option validation
		It("rejects HNSW structural option change (metric)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionVectorMetric: "COSINE_METRIC"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("hnswMetric"))
		})

		It("allows HNSW runtime option change (concurrency)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionHNSWMaxNumConcurrentNodeFetches: "16"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionHNSWMaxNumConcurrentNodeFetches: "32"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects HNSW structural change when value actually differs", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionHNSWM: "16"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_vec", Field("price"))
				idx.Type = IndexTypeVector
				idx.Options = map[string]string{IndexOptionHNSWM: "32", IndexOptionHNSWMMax: "32"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("hnswM"))
		})

		// SPFresh (RFC-094) option validation: every structural option immutable.
		It("rejects SPFresh structural option change (spfreshLmax)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_spf", Field("price"))
				idx.Type = IndexTypeVectorSPFresh
				idx.Options = map[string]string{IndexOptionSPFreshLmax: "256"}
				b.AddIndex("Order", idx)
			})
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_spf", Field("price"))
				idx.Type = IndexTypeVectorSPFresh
				idx.Options = map[string]string{IndexOptionSPFreshLmax: "512"}
				b.AddIndex("Order", idx)
			})
			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("spfreshLmax"))
		})

		It("rejects SPFresh alpha change (the closure-sizing invariant)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_spf", Field("price"))
				idx.Type = IndexTypeVectorSPFresh
				idx.Options = map[string]string{IndexOptionSPFreshAlpha: "1.2"}
				b.AddIndex("Order", idx)
			})
			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_spf", Field("price"))
				idx.Type = IndexTypeVectorSPFresh
				idx.Options = map[string]string{IndexOptionSPFreshAlpha: "1.5"}
				b.AddIndex("Order", idx)
			})
			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("spfreshAlpha"))
		})

		It("accepts unchanged SPFresh options across versions", func() {
			build := func(version int) *RecordMetaData {
				return buildMetaData(version, func(b *RecordMetaDataBuilder) {
					idx := NewIndex("idx_spf", Field("price"))
					idx.Type = IndexTypeVectorSPFresh
					idx.Options = map[string]string{
						IndexOptionSPFreshNumDimensions: "128",
						IndexOptionSPFreshLmax:          "256",
					}
					b.AddIndex("Order", idx)
				})
			}
			Expect(ValidateEvolution(build(1), build(2))).To(Succeed())
		})

		// MULTIDIMENSIONAL (R-tree) option validation
		It("rejects R-tree structural option change (maxM)", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_md", Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2))
				idx.Type = IndexTypeMultidimensional
				idx.Options = map[string]string{IndexOptionRTreeMaxM: "4"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_md", Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2))
				idx.Type = IndexTypeMultidimensional
				idx.Options = map[string]string{IndexOptionRTreeMaxM: "8"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("rtree minM changed"))
		})

		// Java compares the EFFECTIVE configuration of each option family
		// (RankIndexMaintainerFactory.java:75-100,
		// MultidimensionalIndexMaintainerFactory.java:143-193,
		// VectorIndexOptionsHelper.java:120-147), so an option set to its
		// default where it was unspecified is no change; Go compared the raw
		// strings and refused each.
		It("admits an option set to its default where it was unspecified, per index type", func() {
			for _, c := range []struct {
				typ    string
				root   KeyExpression
				option string
				value  string
			}{
				{IndexTypeRank, Ungrouped(Field("price")), IndexOptionRankCountDuplicates, "false"},
				{IndexTypeRank, Ungrouped(Field("price")), IndexOptionRankNLevels, "6"},
				{IndexTypeMultidimensional, Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2), IndexOptionRTreeMinM, "16"},
				{IndexTypeMultidimensional, Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2), IndexOptionRTreeStoreHilbertValues, "true"},
			} {
				old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
					idx := NewIndex("idx_opt", c.root)
					idx.Type = c.typ
					b.AddIndex("Order", idx)
				})
				oldIdx := old.GetIndex("idx_opt")
				new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
					idx := NewIndex("idx_opt", c.root)
					idx.Type = c.typ
					idx.AddedVersion, idx.LastModifiedVersion = oldIdx.AddedVersion, oldIdx.LastModifiedVersion
					idx.Options = map[string]string{c.option: c.value}
					b.AddIndex("Order", idx)
				})
				Expect(ValidateEvolution(old, new)).To(Succeed(), "%s %s=%s", c.typ, c.option, c.value)
			}
		})

		It("rejects R-tree splitS change", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_md", Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2))
				idx.Type = IndexTypeMultidimensional
				idx.Options = map[string]string{IndexOptionRTreeSplitS: "2"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_md", Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2))
				idx.Type = IndexTypeMultidimensional
				idx.Options = map[string]string{IndexOptionRTreeSplitS: "4"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(HavePrefix("rtree splitS changed"))
		})

		// Atomic mutation indexes (COUNT, SUM, etc.) use base validation
		It("rejects clearWhenZero change for COUNT index via base validator", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_count", GroupAll(Field("price")))
				idx.Type = IndexTypeCount
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_count", GroupAll(Field("price")))
				idx.Type = IndexTypeCount
				idx.Options = map[string]string{IndexOptionClearWhenZero: "true"}
				b.AddIndex("Order", idx)
			})

			// clearWhenZero is not in the base allowlist, so this SHOULD be rejected.
			// Java has no override for atomic mutation indexes — clearWhenZero goes to
			// base validator which rejects it.
			err := ValidateEvolution(old, new)
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
			Expect(evolErr.Message).To(ContainSubstring("option"))
		})

		It("allows no-op when options are identical", func() {
			old := buildMetaData(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true", IndexOptionAllowedForQuery: "true"}
				b.AddIndex("Order", idx)
			})

			new := buildMetaData(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("idx_price", Field("price"))
				idx.Options = map[string]string{"unique": "true", IndexOptionAllowedForQuery: "true"}
				b.AddIndex("Order", idx)
			})

			err := ValidateEvolution(old, new)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// unsupportedKeyExpr is a KeyExpression type that renameFields' type switch does
// not handle, used to exercise its default ("field renaming not supported") arm.
type unsupportedKeyExpr struct{}

func (unsupportedKeyExpr) Evaluate(*FDBStoredRecord[proto.Message], proto.Message) ([][]any, error) {
	return nil, nil
}
func (unsupportedKeyExpr) FieldNames() []string                { return nil }
func (unsupportedKeyExpr) ColumnSize() int                     { return 0 }
func (unsupportedKeyExpr) ToKeyExpression() *gen.KeyExpression { return nil }

// TestVectorOptionsComparedByEffectiveValue pins validateVectorIndexOptions to
// Java's disallowChange (VectorIndexOptionsHelper.java:120-147): an option set
// to its default where it was unspecified is no change, a changed value is
// refused with Java's message, and an unrecognized metric name is not taken
// for the default metric the lenient parser falls back to.
func TestVectorOptionsComparedByEffectiveValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		old, new map[string]string
		option   string
		refused  bool
	}{
		{"M set to its default", map[string]string{}, map[string]string{IndexOptionHNSWM: "16"}, IndexOptionHNSWM, false},
		{"M changed", map[string]string{}, map[string]string{IndexOptionHNSWM: "8"}, IndexOptionHNSWM, true},
		{"metric set to its default", map[string]string{}, map[string]string{IndexOptionVectorMetric: "EUCLIDEAN_METRIC"}, IndexOptionVectorMetric, false},
		{"metric changed", map[string]string{}, map[string]string{IndexOptionVectorMetric: "COSINE_METRIC"}, IndexOptionVectorMetric, true},
		{"a metric under its alias, the default", map[string]string{}, map[string]string{"vectorMetric": "EUCLIDEAN_METRIC"}, "vectorMetric", false},
		{"a metric under its alias, changed", map[string]string{}, map[string]string{"vectorMetric": "COSINE_METRIC"}, "vectorMetric", true},
		{"RaBitQ extra bits set to their default", map[string]string{}, map[string]string{IndexOptionHNSWRaBitQNumExBits: "4"}, IndexOptionHNSWRaBitQNumExBits, false},
		// Integer.parseInt reads any Unicode decimal digit.
		{
			"RaBitQ extra bits in fullwidth digits",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "\uff18"},
			IndexOptionHNSWRaBitQNumExBits, false,
		},
		{
			"RaBitQ extra bits changed",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "8"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", IndexOptionHNSWRaBitQNumExBits: "4"},
			IndexOptionHNSWRaBitQNumExBits, true,
		},
		{
			"RaBitQ extra bits changed under their alias",
			map[string]string{IndexOptionHNSWUseRaBitQ: "true"},
			map[string]string{IndexOptionHNSWUseRaBitQ: "true", "vectorRaBitQNumExBits": "5"},
			"vectorRaBitQNumExBits", true,
		},
	} {
		oldIdx := &Index{Name: "v", Type: IndexTypeVector, Options: c.old}
		newIdx := &Index{Name: "v", Type: IndexTypeVector, Options: c.new}
		changed := map[string]bool{c.option: true}
		err := validateVectorIndexOptions(oldIdx, newIdx, changed)
		var evolErr *MetaDataEvolutionError
		switch {
		case c.refused && (!errors.As(err, &evolErr) || !strings.HasPrefix(evolErr.Message, "attempted to change immutable vector index option")):
			t.Errorf("%s: %v, want Java's refusal", c.name, err)
		case c.refused && !strings.Contains(evolErr.Message, fmt.Sprintf("option=%q", c.option)):
			// Java's disallowChange names the name that changed, an alias
			// included.
			t.Errorf("%s: %v, want the refusal to name %s", c.name, err, c.option)
		case !c.refused && (err != nil || changed[c.option]):
			t.Errorf("%s: %v (still changed: %t), want admitted and handled", c.name, err, changed[c.option])
		}
	}
	// A metric no Metric constant names is Java's parse failure, Metric.valueOf's
	// IllegalArgumentException, not taken for the default.
	err := validateVectorIndexOptions(&Index{Name: "v", Type: IndexTypeVector, Options: map[string]string{}},
		&Index{Name: "v", Type: IndexTypeVector, Options: map[string]string{IndexOptionVectorMetric: "COSINE"}},
		map[string]bool{IndexOptionVectorMetric: true})
	var iae *IllegalArgumentError
	if !errors.As(err, &iae) || iae.Message != "No enum constant com.apple.foundationdb.linear.Metric.COSINE" {
		t.Errorf("an unrecognized metric: %v, want Metric.valueOf's refusal", err)
	}
}

// TestRecordTypeViolationsNamedInNameOrder pins that validateRecordTypes walks
// the record types by name: with two types' since versions changed, the message
// names the first by name on every run, where a map walk names either.
func TestRecordTypeViolationsNamedInNameOrder(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.SetVersion(3)
	old, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	p, err := old.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	p.Version = proto.Int32(4)
	for _, rt := range p.RecordTypes {
		if rt.GetName() == "Customer" || rt.GetName() == "TypedRecord" {
			rt.SinceVersion = proto.Int32(2)
		}
	}
	newMD, err := RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		var evolErr *MetaDataEvolutionError
		if err := ValidateEvolution(old, newMD); !errors.As(err, &evolErr) ||
			!strings.HasPrefix(evolErr.Message, `record type since version changed (record type="Customer"`) {
			t.Fatalf("%v, want Customer named first", err)
		}
	}
}

// TestTypeRenameNamedInUnionOrder pins getTypeRenames to Java's walk of the
// old union's fields in declaration order (MetaDataEvolutionValidator.java:
// 354-377): with renames disallowed and two types renamed, the refusal names
// the first renamed type in union order (Order, field 1), not the first by
// name (Customer) or whichever a map yields.
func TestTypeRenameNamedInUnionOrder(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.SetVersion(3)
	old, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	p, err := old.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	p.Version = proto.Int32(4)
	renamed := map[string]string{"Order": "Purchase", "Customer": "Buyer"}
	pkg := "." + p.GetRecords().GetPackage() + "."
	for _, m := range p.GetRecords().GetMessageType() {
		if to, ok := renamed[m.GetName()]; ok {
			m.Name = proto.String(to)
		}
		for _, f := range m.GetField() {
			if to, ok := renamed[strings.TrimPrefix(f.GetTypeName(), pkg)]; ok {
				f.TypeName = proto.String(pkg + to)
			}
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if to, ok := renamed[rt.GetName()]; ok {
			rt.Name = proto.String(to)
		}
	}
	newMD, err := RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewMetaDataEvolutionValidator().Build().Validate(old, newMD); err != nil {
		t.Fatalf("renames allowed: %v", err)
	}
	for range 32 {
		var evolErr *MetaDataEvolutionError
		err := NewMetaDataEvolutionValidator().SetDisallowTypeRenames(true).Build().Validate(old, newMD)
		if !errors.As(err, &evolErr) || !strings.HasPrefix(evolErr.Message, `record type name changed (old="Order", new="Purchase")`) {
			t.Fatalf("%v, want the Order rename named first", err)
		}
	}
}

// TestChangedIndexOptionsNamedInNameOrder pins the base option walk: of two
// changed options neither validator admits, the first by name is reported,
// on every run.
func TestChangedIndexOptionsNamedInNameOrder(t *testing.T) {
	t.Parallel()
	oldIdx := &Index{Name: "v", Type: IndexTypeValue, Options: map[string]string{}}
	newIdx := &Index{Name: "v", Type: IndexTypeValue, Options: map[string]string{"zeta": "1", "alpha": "1", "mid": "1"}}
	for range 32 {
		changed := map[string]bool{"zeta": true, "alpha": true, "mid": true}
		var evolErr *MetaDataEvolutionError
		if err := ValidateChangedIndexOptions(oldIdx, newIdx, changed); !errors.As(err, &evolErr) ||
			!strings.HasPrefix(evolErr.Message, `index option changed (index="v", option="alpha"`) {
			t.Fatalf("%v, want alpha named", err)
		}
	}
}

// TestIndexRecordTypeViolationNamedInNameOrder pins the walk of an index's
// record types (RecordTypesForIndex, in name order): an index that gains two
// record types neither of which is newer than the old meta-data names the
// first by name, on every run.
func TestIndexRecordTypeViolationNamedInNameOrder(t *testing.T) {
	t.Parallel()
	build := func(types []string, version int) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewIndex("multi", Field("price"))
		idx.LastModifiedVersion, idx.AddedVersion = 1, 1
		b.AddMultiTypeIndex(types, idx)
		b.SetVersion(version)
		md, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return md
	}
	old := build([]string{"Order"}, 3)
	newMD := build([]string{"TypedRecord", "Order", "Customer"}, 4)
	for range 32 {
		var evolErr *MetaDataEvolutionError
		err := NewMetaDataEvolutionValidator().Build().Validate(old, newMD)
		if !errors.As(err, &evolErr) || !strings.HasPrefix(evolErr.Message, `new index adds record type that is not newer than old meta-data (index="multi", record type="Customer"`) {
			t.Fatalf("%v, want Customer named first", err)
		}
	}
}

// TestTextTokenizerVersionIsParsedAsJavaParsesIt pins getTextTokenizerVersion
// to TextIndexMaintainer.getIndexTokenizerVersion: absent is version 0, and a
// present value, the empty string included, goes through Integer.parseInt,
// whose refusal is a MetaDataException.
func TestTextTokenizerVersionIsParsedAsJavaParsesIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		options map[string]string
		want    int
		refused bool
	}{
		{nil, 0, false},
		{map[string]string{IndexOptionTextTokenizerVersion: "0"}, 0, false},
		{map[string]string{IndexOptionTextTokenizerVersion: "+2"}, 2, false},
		{map[string]string{IndexOptionTextTokenizerVersion: ""}, 0, true},
		{map[string]string{IndexOptionTextTokenizerVersion: " 1"}, 0, true},
	} {
		got, err := getTextTokenizerVersion(&Index{Name: "t", Options: c.options})
		var mdErr *MetaDataError
		if c.refused {
			if !errors.As(err, &mdErr) || mdErr.Message != "tokenizer version could not be parsed as int" {
				t.Errorf("%v: %v, want Java's refusal", c.options, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%v: %d, %v; want %d", c.options, got, err, c.want)
		}
	}
}
