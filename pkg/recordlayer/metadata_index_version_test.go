package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// An index whose lastModifiedVersion is ahead of the metadata version that
// carries it is permanently "modified since" whatever version the store header
// records. GetIndexesToBuildSince() therefore selects it on EVERY subsequent
// metadata version bump, and the rebuild policy — which reports an unknown
// record count as unbounded when the store has no record-count key — marks it
// DISABLED, clearing an index a background build already populated.
//
// Java refuses to construct such metadata at all: MetaDataValidator.validateIndex()
// throws for lastModifiedVersion > metadata version (MetaDataValidator.java:129-133)
// and for addedVersion > metadata version (MetaDataValidator.java:124-128).
var _ = Describe("Index version vs metadata version", func() {
	ctx := context.Background()

	baseBuilder := func() *RecordMetaDataBuilder {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return b
	}

	// seedStoreWithBuiltIndex creates a store holding 20 Orders and an
	// online-built, readable price index at metadata version 2. It returns the
	// keyspace and the version-2 metadata.
	seedStoreWithBuiltIndex := func() (subspace.Subspace, *RecordMetaData, *Index) {
		ks := specSubspace()

		b1 := baseBuilder()
		b1.SetVersion(1)
		md1, err := b1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 20; i++ {
				_, serr = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(i),
					Price:    proto.Int32(int32(i * 10)),
					Quantity: proto.Int32(int32(i)),
				})
				Expect(serr).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		priceIdx := NewIndex("Order$price", Field("price"))
		priceIdx.AddedVersion = 2
		priceIdx.LastModifiedVersion = 2
		b2 := baseBuilder()
		b2.AddIndex("Order", priceIdx)
		b2.SetVersion(2)
		md2, err := b2.Build()
		Expect(err).NotTo(HaveOccurred())

		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(md2).
			SetIndex(priceIdx).
			SetSubspace(ks).
			SetLimit(7).
			Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).Open()
			Expect(serr).NotTo(HaveOccurred())
			Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
			entries, serr := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(serr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(20))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		return ks, md2, priceIdx
	}

	It("rejects metadata whose index lastModifiedVersion exceeds the metadata version", func() {
		ks, _, priceIdx := seedStoreWithBuiltIndex()

		// A routine version bump to 3 that leaves the index claiming it was last
		// modified at version 5. Nothing about this looks alarming to a producer:
		// the index definition is unchanged and the metadata version only moves
		// forward. It is nonetheless metadata that can never settle.
		bad := baseBuilder()
		badIdx := NewIndex("Order$price", Field("price"))
		badIdx.AddedVersion = 2
		badIdx.LastModifiedVersion = 5
		bad.AddIndex("Order", badIdx)
		bad.SetVersion(3)
		md3, err := bad.Build()

		if err == nil {
			// Only reachable without the validator. Show what the invalid metadata
			// does to the already-built index rather than merely noting that a
			// comparison failed to fire.
			_, openErr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md3).SetSubspace(ks).Open()
				Expect(serr).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue(),
					"DATA LOSS: a readable, fully built index was disabled by a version bump "+
						"because its lastModifiedVersion (5) is ahead of the metadata version (3)")
				entries, serr := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(serr).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20),
					"DATA LOSS: index entries were cleared by the version bump")
				return nil, nil
			})
			Expect(openErr).NotTo(HaveOccurred())
			Fail("Build() accepted metadata whose index lastModifiedVersion (5) exceeds the " +
				"metadata version (3); Java rejects this at MetaDataValidator.java:129-133")
		}

		var vErr *IndexVersionTooNewError
		Expect(errors.As(err, &vErr)).To(BeTrue())
		Expect(vErr.IndexName).To(Equal("Order$price"))
		Expect(vErr.Kind).To(Equal(IndexVersionLastModified))
		Expect(vErr.LastModifiedVersion).To(Equal(5))
		Expect(vErr.MetaDataVersion).To(Equal(3))

		// It is a metadata validation failure in Java's exception hierarchy.
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
	})

	It("rejects metadata whose index addedVersion exceeds the metadata version", func() {
		b := baseBuilder()
		idx := NewIndex("Order$price", Field("price"))
		idx.AddedVersion = 9
		idx.LastModifiedVersion = 9
		b.AddIndex("Order", idx)
		b.SetVersion(4)
		_, err := b.Build()

		var vErr *IndexVersionTooNewError
		Expect(errors.As(err, &vErr)).To(BeTrue())
		Expect(vErr.IndexName).To(Equal("Order$price"))
		Expect(vErr.Kind).To(Equal(IndexVersionAdded))
		Expect(vErr.AddedVersion).To(Equal(9))
		Expect(vErr.MetaDataVersion).To(Equal(4))
	})

	It("rejects metadata whose record type since version exceeds the metadata version", func() {
		b := baseBuilder()
		b.GetRecordType("Order").recordType.SinceVersion = 7
		b.SetVersion(3)
		_, err := b.Build()

		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue())
		Expect(mdErr.Message).To(ContainSubstring("since version 7"))
		Expect(mdErr.Message).To(ContainSubstring("meta-data version 3"))
	})

	It("keeps an already-built index readable across a valid version bump", func() {
		ks, _, priceIdx := seedStoreWithBuiltIndex()

		// The same version bump to 3, this time with the index's versions left
		// where they belong. Nothing is re-decided, so nothing is cleared.
		good := baseBuilder()
		goodIdx := NewIndex("Order$price", Field("price"))
		goodIdx.AddedVersion = 2
		goodIdx.LastModifiedVersion = 2
		good.AddIndex("Order", goodIdx)
		good.SetVersion(3)
		md3, err := good.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(md3.Version()).To(Equal(3))

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md3).SetSubspace(ks).Open()
			Expect(serr).NotTo(HaveOccurred())
			Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
			entries, serr := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(serr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(20))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
