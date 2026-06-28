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

var _ = Describe("MetadataBugVerify", func() {
	// Bug 1: RemoveIndex must pre-increment version before recording RemovedVersion.
	Describe("RemoveIndex version increment", func() {
		It("RemovedVersion > AddedVersion for FormerIndex", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetVersion(1)

			priceIndex := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", priceIndex)
			addedVersion := priceIndex.AddedVersion

			builder.RemoveIndex("Order$price")

			formers := builder.GetFormerIndexes()
			Expect(formers).To(HaveLen(1))
			Expect(formers[0].RemovedVersion).To(BeNumerically(">", addedVersion))
		})

		It("version counter incremented on remove", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetVersion(5)

			idx := NewIndex("Order$price", Field("price"))
			builder.AddIndex("Order", idx)
			builder.RemoveIndex("Order$price")

			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			formers := md.GetFormerIndexes()
			Expect(formers).To(HaveLen(1))
			Expect(formers[0].RemovedVersion).To(Equal(7))
		})
	})

	// Bug 2: checkPossiblyRebuild must clean up former index data.
	Describe("checkPossiblyRebuild former index cleanup", func() {
		It("clears former index data when metadata version changes", func() {
			ctx := context.Background()
			ss := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder1.AddIndex("Order", priceIndex)
			builder1.SetVersion(1)
			md1, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create store and save a record so index has data.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ss).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: intPtr(1), Price: int32Ptr(42)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify index data exists.
			var hasIndexDataBefore bool
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				indexSubspace := ss.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				begin, end := indexSubspace.FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				hasIndexDataBefore = len(kvs) > 0
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(hasIndexDataBefore).To(BeTrue())

			// Remove index in v2 metadata.
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx2 := NewIndex("Order$price", Field("price"))
			idx2.SetSubspaceKey(priceIndex.SubspaceTupleKey())
			builder2.AddIndex("Order", idx2)
			builder2.RemoveIndex("Order$price")
			builder2.SetVersion(3)
			md2, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md2.GetFormerIndexes()).To(HaveLen(1))

			// Open with new metadata — checkPossiblyRebuild should clean up.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ss).Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Index data must be cleared.
			var hasIndexDataAfter bool
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				indexSubspace := ss.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				begin, end := indexSubspace.FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				hasIndexDataAfter = len(kvs) > 0
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(hasIndexDataAfter).To(BeFalse(),
				"former index data should be cleared during checkPossiblyRebuild")
		})
	})

	// Bug 3: allowIndexRebuilds should skip type/expression checks.
	Describe("allowIndexRebuilds skips type/expression checks", func() {
		It("allows type change with allowIndexRebuilds=true and lastModifiedVersion bumped", func() {
			old := buildMetaDataForBugTest(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.Type = "value"
				b.AddIndex("Order", idx)
			})

			new := buildMetaDataForBugTest(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", GroupAll(Field("price")))
				idx.Type = "count"
				idx.subspaceKey = old.GetIndex("Order$price").SubspaceTupleKey()
				idx.AddedVersion = old.GetIndex("Order$price").AddedVersion
				idx.LastModifiedVersion = 2
				b.AddIndex("Order", idx)
			})

			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
			Expect(v.Validate(old, new)).NotTo(HaveOccurred())
		})

		It("still rejects type change with allowIndexRebuilds=false", func() {
			old := buildMetaDataForBugTest(1, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.Type = "value"
				b.AddIndex("Order", idx)
			})
			oldIdx := old.GetIndex("Order$price")

			new := buildMetaDataForBugTest(2, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", GroupAll(Field("price")))
				idx.Type = "count"
				idx.subspaceKey = oldIdx.SubspaceTupleKey()
				idx.AddedVersion = oldIdx.AddedVersion
				// Keep lastModifiedVersion same as old so the LMV check passes,
				// and the type-change check fires instead.
				idx.LastModifiedVersion = oldIdx.LastModifiedVersion
				b.AddIndex("Order", idx)
			})

			v := NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(false).Build()
			err := v.Validate(old, new)
			Expect(err).To(HaveOccurred())
			var evolErr *MetaDataEvolutionError
			Expect(errors.As(err, &evolErr)).To(BeTrue())
		})
	})

	// Bug 4: validateFormerIndexes addedVersion checks.
	Describe("validateFormerIndexes addedVersion", func() {
		It("rejects former addedVersion > old index addedVersion unconditionally", func() {
			old := buildMetaDataForBugTest(5, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = 2
				idx.LastModifiedVersion = 2
				b.AddIndex("Order", idx)
			})
			oldIdx := old.GetIndex("Order$price")
			Expect(oldIdx).NotTo(BeNil())

			new := buildMetaDataForBugTest(7, func(b *RecordMetaDataBuilder) {
				b.formerIndexes = append(b.formerIndexes, &FormerIndex{
					SubspaceKey:    oldIdx.SubspaceTupleKey(),
					AddedVersion:   4,
					RemovedVersion: 6,
					FormerName:     "Order$price",
				})
			})

			// Unconditional — even with allowOlder=true
			v := NewMetaDataEvolutionValidator().SetAllowOlderFormerIndexAddedVersion(true).Build()
			Expect(v.Validate(old, new)).To(HaveOccurred())
		})

		It("rejects former addedVersion < old when not allowed", func() {
			old := buildMetaDataForBugTest(5, func(b *RecordMetaDataBuilder) {
				idx := NewIndex("Order$price", Field("price"))
				idx.AddedVersion = 3
				idx.LastModifiedVersion = 3
				b.AddIndex("Order", idx)
			})
			oldIdx := old.GetIndex("Order$price")
			Expect(oldIdx).NotTo(BeNil())

			new := buildMetaDataForBugTest(7, func(b *RecordMetaDataBuilder) {
				b.formerIndexes = append(b.formerIndexes, &FormerIndex{
					SubspaceKey:    oldIdx.SubspaceTupleKey(),
					AddedVersion:   1,
					RemovedVersion: 6,
					FormerName:     "Order$price",
				})
			})

			v := NewMetaDataEvolutionValidator().SetAllowOlderFormerIndexAddedVersion(false).Build()
			Expect(v.Validate(old, new)).To(HaveOccurred())
		})
	})

	// Bug 5: createStoreHeader must persist RecordCountKey.
	Describe("createStoreHeader RecordCountKey", func() {
		It("persists RecordCountKey in header on Create", func() {
			ctx := context.Background()
			ss := specSubspace()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetRecordCountKey(EmptyKey())
			builder.SetVersion(1)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			var hasRecordCountKey bool
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				headerKey := ss.Pack(tuple.Tuple{StoreInfoKey})
				data, err := rtx.Transaction().Get(headerKey).Get()
				if err != nil {
					return nil, err
				}
				header := &gen.DataStoreInfo{}
				if err := proto.Unmarshal(data, header); err != nil {
					return nil, err
				}
				hasRecordCountKey = header.GetRecordCountKey() != nil
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(hasRecordCountKey).To(BeTrue())
		})
	})
})

func buildMetaDataForBugTest(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	if configure != nil {
		configure(builder)
	}
	builder.SetVersion(version)
	md, err := builder.Build()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return md
}

func intPtr(v int64) *int64   { return &v }
func int32Ptr(v int32) *int32 { return &v }
