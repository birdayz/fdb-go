package recordlayer

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

// Tests targeting uncovered lines in metadata.go to push coverage from ~79% to ~85%.
var _ = Describe("RecordMetaData coverage", func() {
	// Helper: build a standard metadata from the demo proto.
	buildDemoMetadata := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	Describe("Builder getter methods", func() {
		// Lines 292-316: GetVersion, IsSplitLongRecords, IsStoreRecordVersions,
		// GetRecordCountKey, GetRecordTypes.
		It("GetVersion returns current builder version", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			initialVersion := b.GetVersion()
			b.SetVersion(42)
			Expect(b.GetVersion()).To(Equal(42))
			Expect(initialVersion).To(BeNumerically(">=", 0))
		})

		It("IsSplitLongRecords returns builder state", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(b.IsSplitLongRecords()).To(BeFalse())
			b.SetSplitLongRecords(true)
			Expect(b.IsSplitLongRecords()).To(BeTrue())
		})

		It("IsStoreRecordVersions returns builder state", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(b.IsStoreRecordVersions()).To(BeFalse())
			b.SetStoreRecordVersions(true)
			Expect(b.IsStoreRecordVersions()).To(BeTrue())
		})

		It("GetRecordCountKey returns builder state", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(b.GetRecordCountKey()).To(BeNil())
			b.SetRecordCountKey(EmptyKey())
			Expect(b.GetRecordCountKey()).NotTo(BeNil())
		})

		It("GetRecordTypes returns builder record types", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			rt := b.GetRecordTypes()
			Expect(rt).To(HaveKey("Order"))
			Expect(rt).To(HaveKey("Customer"))
		})
	})

	Describe("setRecordsWithoutUnion", func() {
		// Lines 219-238: fallback path when proto has no UnionDescriptor.
		It("auto-discovers record types from all top-level messages", func() {
			// Use a proto file that has no UnionDescriptor.
			// record_metadata.proto contains MetaData, Index, RecordType, etc. — no union.
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_metadata_proto)
			rt := b.GetRecordTypes()
			// Should have discovered multiple message types.
			Expect(len(rt)).To(BeNumerically(">", 0))
			// Record type indices should start from 0.
			for _, r := range rt {
				Expect(r.RecordTypeIndex).To(BeNumerically(">=", 0))
				Expect(r.UnionFieldDescriptor).To(BeNil()) // No union field
			}
		})
	})

	Describe("Build validation - union oneof", func() {
		// Lines 482-488: oneof validation in Build().
		// Hard to test without crafting a custom file descriptor with multiple oneofs.
		// Instead we test the happy path — single oneof is fine (demo proto has one).
		It("accepts union with a single oneof", func() {
			md := buildDemoMetadata()
			Expect(md).NotTo(BeNil())
		})
	})

	Describe("BITMAP_VALUE validation", func() {
		// Lines 632-641: missing GroupingKeyExpression / wrong grouped count.
		It("rejects BITMAP_VALUE without GroupingKeyExpression", func() {
			idx := NewIndex("bad_bitmap", Field("price"))
			idx.Type = IndexTypeBitmapValue

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("BITMAP_VALUE"))
			Expect(mdErr.Message).To(ContainSubstring("GroupingKeyExpression"))
		})

		It("rejects BITMAP_VALUE with wrong grouped count", func() {
			// GroupBy(0) = all columns are grouped, none grouping. That's wrong —
			// BITMAP_VALUE needs exactly 1 grouped column.
			idx := NewIndex("bad_bitmap2", GroupAll(Concat(Field("price"), Field("order_id"))))
			idx.Type = IndexTypeBitmapValue

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", idx)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("exactly 1 grouped column"))
		})
	})

	Describe("Metadata accessor methods", func() {
		var md *RecordMetaData

		BeforeEach(func() {
			md = buildDemoMetadata()
		})

		// Lines 1019-1021: GetUnionDescriptor.
		It("GetUnionDescriptor returns the union descriptor", func() {
			ud := md.GetUnionDescriptor()
			Expect(ud).NotTo(BeNil())
			Expect(string(ud.Name())).To(Equal("UnionDescriptor"))
		})

		// Lines 1026-1028: GetUnionFieldForRecordType.
		It("GetUnionFieldForRecordType returns the union field", func() {
			orderType := md.GetRecordType("Order")
			fd := md.GetUnionFieldForRecordType(orderType)
			Expect(fd).NotTo(BeNil())
			Expect(string(fd.Name())).To(HavePrefix("_"))
		})

		// Lines 968-975: GetRecordTypeFromRecordTypeKey.
		It("GetRecordTypeFromRecordTypeKey finds by int key", func() {
			orderType := md.GetRecordType("Order")
			key := orderType.GetRecordTypeKey()
			found := md.GetRecordTypeFromRecordTypeKey(key)
			Expect(found).NotTo(BeNil())
			Expect(found.Name).To(Equal("Order"))
		})

		It("GetRecordTypeFromRecordTypeKey returns nil for unknown key", func() {
			found := md.GetRecordTypeFromRecordTypeKey(999999)
			Expect(found).To(BeNil())
		})

		It("GetRecordTypeFromRecordTypeKey normalizes int types", func() {
			orderType := md.GetRecordType("Order")
			// The key is an int (RecordTypeIndex), test with int64 equivalent.
			found := md.GetRecordTypeFromRecordTypeKey(int64(orderType.RecordTypeIndex))
			Expect(found).NotTo(BeNil())
			Expect(found.Name).To(Equal("Order"))
		})

		// Lines 992-999: GetIndexFromSubspaceKey.
		It("GetIndexFromSubspaceKey finds index", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("price_idx", Field("price"))
			builder.AddIndex("Order", idx)
			md2, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			found := md2.GetIndexFromSubspaceKey("price_idx")
			Expect(found).NotTo(BeNil())
			Expect(found.Name).To(Equal("price_idx"))

			notFound := md2.GetIndexFromSubspaceKey("nonexistent")
			Expect(notFound).To(BeNil())
		})

		// Lines 980-987: GetFormerIndexesSince.
		It("GetFormerIndexesSince filters by version", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("old_idx", Field("price"))
			builder.AddIndex("Order", idx)
			versionBeforeRemove := builder.GetVersion()
			builder.RemoveIndex("old_idx")
			md2, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// All former indexes should be returned for version 0.
			Expect(md2.GetFormerIndexesSince(0)).To(HaveLen(1))

			// Former indexes since a version AFTER removal should be empty.
			Expect(md2.GetFormerIndexesSince(md2.Version())).To(BeEmpty())

			// Former indexes since the version before removal should include it.
			Expect(md2.GetFormerIndexesSince(versionBeforeRemove)).To(HaveLen(1))
		})
	})

	Describe("CommonPrimaryKey", func() {
		// Lines 1033-1044.
		It("returns nil for heterogeneous PKs", func() {
			md := buildDemoMetadata()
			cpk := md.CommonPrimaryKey()
			Expect(cpk).To(BeNil())
		})
	})

	Describe("CommonPrimaryKeyLength", func() {
		// Lines 1050-1062.
		It("returns common length when all types share same PK length", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			// All PKs are single-column fields.
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.CommonPrimaryKeyLength()).To(Equal(1))
		})

		It("returns -1 when types have different PK lengths", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id"), Field("price")))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			Expect(md.CommonPrimaryKeyLength()).To(Equal(-1))
		})
	})

	Describe("GetIndexesForRecordType with multi-type indexes", func() {
		// Lines 926-927: multi-type index path.
		It("returns both single-type and multi-type indexes", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			singleIdx := NewIndex("single_price", Field("price"))
			builder.AddIndex("Order", singleIdx)

			multiIdx := NewIndex("multi_price", Field("price"))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, multiIdx)

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderIndexes := md.GetIndexesForRecordType("Order")
			Expect(orderIndexes).To(HaveLen(2))

			names := make([]string, len(orderIndexes))
			for i, idx := range orderIndexes {
				names[i] = idx.Name
			}
			Expect(names).To(ContainElements("single_price", "multi_price"))
		})

		It("returns nil for unknown record type", func() {
			md := buildDemoMetadata()
			Expect(md.GetIndexesForRecordType("NonExistent")).To(BeNil())
		})
	})

	Describe("RecordType.GetAllIndexes", func() {
		It("combines single-type and multi-type indexes", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			builder.AddIndex("Order", NewIndex("s1", Field("price")))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"}, NewIndex("m1", Field("price")))

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderType := md.GetRecordType("Order")
			all := orderType.GetAllIndexes()
			Expect(all).To(HaveLen(2))
		})

		It("returns only single-type when no multi-type indexes", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("s1", Field("price")))

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderType := md.GetRecordType("Order")
			all := orderType.GetAllIndexes()
			Expect(all).To(HaveLen(1))
			Expect(all[0].Name).To(Equal("s1"))
		})
	})

	Describe("RecordType accessor methods", func() {
		It("HasExplicitRecordTypeKey and GetRecordTypeKey", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			orderType := md.GetRecordType("Order")
			Expect(orderType.HasExplicitRecordTypeKey()).To(BeFalse())
			Expect(orderType.GetRecordTypeKey()).To(Equal(orderType.RecordTypeIndex))
			Expect(orderType.GetIndexes()).To(BeEmpty())
			Expect(orderType.GetMultiTypeIndexes()).To(BeEmpty())
		})
	})

	Describe("normalizeSubspaceKey", func() {
		It("normalizes int types to int64", func() {
			Expect(normalizeSubspaceKey(int(42))).To(Equal(int64(42)))
			Expect(normalizeSubspaceKey(int32(42))).To(Equal(int64(42)))
			Expect(normalizeSubspaceKey(int64(42))).To(Equal(int64(42)))
		})

		It("passes through non-int types", func() {
			Expect(normalizeSubspaceKey("hello")).To(Equal("hello"))
			Expect(normalizeSubspaceKey(3.14)).To(Equal(3.14))
		})
	})

	Describe("countVersionColumns deep branches", func() {
		It("counts through FunctionKeyExpression", func() {
			// Wrap a VersionKeyExpression inside a FunctionKeyExpression's arguments.
			inner := VersionKey()
			fn := FunctionExpr("test_func", inner)
			Expect(countVersionColumns(fn)).To(Equal(1))
		})

		It("counts through RecordTypeKeyExpression with nested", func() {
			rtk := &RecordTypeKeyExpression{nested: VersionKey()}
			Expect(countVersionColumns(rtk)).To(Equal(1))
		})

		It("returns 0 for RecordTypeKeyExpression without nested", func() {
			rtk := RecordTypeKey()
			Expect(countVersionColumns(rtk)).To(Equal(0))
		})

		It("counts through KeyWithValueExpression", func() {
			inner := Concat(Field("price"), VersionKey())
			kwv := KeyWithValue(inner, 1)
			Expect(countVersionColumns(kwv)).To(Equal(1))
		})

		It("counts through NestingKeyExpression", func() {
			n := Nest("flower", VersionKey())
			Expect(countVersionColumns(n)).To(Equal(1))
		})

		It("returns 0 for nil", func() {
			Expect(countVersionColumns(nil)).To(Equal(0))
		})

		It("returns 0 for FieldKeyExpression", func() {
			Expect(countVersionColumns(Field("price"))).To(Equal(0))
		})
	})

	Describe("countVersionColumnsInGroupParts", func() {
		It("handles non-composite expression", func() {
			// Non-composite with groupingCount > 0 → all versions are grouping.
			v := VersionKey()
			grouping, grouped := countVersionColumnsInGroupParts(v, 1)
			Expect(grouping).To(Equal(1))
			Expect(grouped).To(Equal(0))

			// Non-composite with groupingCount = 0 → all versions are grouped.
			grouping2, grouped2 := countVersionColumnsInGroupParts(v, 0)
			Expect(grouping2).To(Equal(0))
			Expect(grouped2).To(Equal(1))
		})

		It("splits composite children at boundary", func() {
			// Composite: [Field("a"), VersionKey()] with groupingCount=1
			// Field("a") = 1 column (grouping), VersionKey() = 1 column (grouped)
			comp := Concat(Field("price"), VersionKey())
			grouping, grouped := countVersionColumnsInGroupParts(comp, 1)
			Expect(grouping).To(Equal(0))
			Expect(grouped).To(Equal(1))
		})
	})

	Describe("SetRecordCountKey version bump", func() {
		It("bumps version when value changes", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			v1 := b.GetVersion()
			b.SetRecordCountKey(EmptyKey())
			v2 := b.GetVersion()
			Expect(v2).To(BeNumerically(">", v1))

			// Setting to same value should NOT bump version.
			b.SetRecordCountKey(EmptyKey())
			v3 := b.GetVersion()
			Expect(v3).To(Equal(v2))
		})
	})

	Describe("SetStoreRecordVersions version bump", func() {
		It("bumps version when value changes", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			v1 := b.GetVersion()
			b.SetStoreRecordVersions(true)
			v2 := b.GetVersion()
			Expect(v2).To(BeNumerically(">", v1))

			// Setting to same value should NOT bump.
			b.SetStoreRecordVersions(true)
			v3 := b.GetVersion()
			Expect(v3).To(Equal(v2))
		})
	})

	Describe("SetSplitLongRecords version bump", func() {
		It("bumps version when value changes", func() {
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			v1 := b.GetVersion()
			b.SetSplitLongRecords(true)
			v2 := b.GetVersion()
			Expect(v2).To(BeNumerically(">", v1))

			// Setting to same value should NOT bump.
			b.SetSplitLongRecords(true)
			v3 := b.GetVersion()
			Expect(v3).To(Equal(v2))
		})
	})

	Describe("Build error: no record types", func() {
		It("returns error when builder has no record types", func() {
			b := NewRecordMetaDataBuilder()
			_, err := b.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("no record types"))
		})
	})

	Describe("Build error: primary key creates duplicates", func() {
		It("rejects primary key with fan-out", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(FanOut("tags"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("duplicates"))
		})
	})

	Describe("Build error: primary key zero columns", func() {
		It("rejects EmptyKeyExpression as primary key", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(EmptyKey())
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("produces no columns"))
		})
	})

	Describe("Build error: duplicate index name", func() {
		It("rejects adding same index name twice", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("dup", Field("price")))
			builder.AddIndex("Order", NewIndex("dup", Field("order_id")))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already defined"))
		})
	})

	Describe("AddIndex with unknown record type", func() {
		It("records build error", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("NonExistent", NewIndex("bad", Field("price")))

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unknown record type"))
		})
	})

	Describe("AddMultiTypeIndex edge cases", func() {
		It("delegates to AddUniversalIndex for empty list", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("univ", Field("price"))
			builder.AddMultiTypeIndex(nil, idx)

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetUniversalIndexes()).To(HaveLen(1))
		})

		It("delegates to AddIndex for single-element list", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("single", Field("price"))
			builder.AddMultiTypeIndex([]string{"Order"}, idx)

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetIndexesForRecordType("Order")).To(HaveLen(1))
		})

		It("records error for unknown record type in multi-type list", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("bad_multi", Field("price"))
			builder.AddMultiTypeIndex([]string{"Order", "NonExistent"}, idx)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unknown record type"))
		})
	})

	Describe("GetRecordType panic on unknown type", func() {
		It("panics with MetaDataError", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			Expect(func() {
				builder.GetRecordType("NonExistent")
			}).To(PanicWith(&MetaDataError{Message: `unknown record type "NonExistent"`}))
		})
	})

	Describe("EnableCounterBasedSubspaceKeys", func() {
		It("assigns integer subspace keys", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.EnableCounterBasedSubspaceKeys()
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx1 := NewIndex("idx1", Field("price"))
			idx2 := NewIndex("idx2", Field("order_id"))
			builder.AddIndex("Order", idx1)
			builder.AddIndex("Order", idx2)

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			i1 := md.GetIndex("idx1")
			i2 := md.GetIndex("idx2")
			Expect(i1.SubspaceTupleKey()).To(BeAssignableToTypeOf(int64(0)))
			Expect(i2.SubspaceTupleKey()).To(BeAssignableToTypeOf(int64(0)))
			// Keys should be sequential.
			Expect(i1.SubspaceTupleKey().(int64)).To(BeNumerically("<", i2.SubspaceTupleKey().(int64)))
		})
	})

	Describe("Index replacement chain validation", func() {
		It("rejects replacement index that does not exist", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewIndex("old_idx", Field("price"))
			idx.Options[IndexOptionReplacedByPrefix] = "nonexistent_replacement"
			builder.AddIndex("Order", idx)

			_, err := builder.Build()
			Expect(err).To(HaveOccurred())
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("not in the metadata"))
		})
	})
})
