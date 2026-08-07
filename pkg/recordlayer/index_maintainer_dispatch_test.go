package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// The maintainer dispatch must FAIL CLOSED on an index type it does not
// implement, and must resolve the deprecated bare _EVER spellings rather than
// treating them as unknown.
//
// Both halves are the same defect seen from two sides. The dispatch used to end
// in `default: return newStandardIndexMaintainer(...)`, so EVERY unrecognised
// type became a value index: no error at open, no error at save, and
// value-index key bytes written into that index's own subspace. Java refuses the
// same lookup — IndexMaintainerFactoryRegistryImpl.getIndexMaintainerFactory
// throws MetaDataException("Unknown index type for " + index) when the registry
// misses (IndexMaintainerFactoryRegistryImpl.java:78-82) — and separately
// RESOLVES min_ever/max_ever onto their _LONG maintainers
// (AtomicMutationIndexMaintainer.java:100-106), so a bare-spelled index is not
// an unknown type at all.
//
// A fail-closed default without the bare-type resolution would trade a silent
// corruption for a refusal to open Java-authored metadata, which is why the two
// are tested together.
var _ = Describe("index maintainer dispatch", func() {
	ctx := context.Background()

	// buildMetaWithIndex builds metadata carrying one index on Order, whatever
	// its type string, so a type no maintainer implements can reach the dispatch.
	buildMetaWithIndex := func(idx *Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	saveOne := func(md *RecordMetaData) error {
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
		})
		return err
	}

	Describe("an index type no maintainer implements", func() {
		It("refuses the write instead of maintaining it as a value index", func() {
			idx := NewIndex("Order$mystery", Field("price"))
			idx.Type = "not_an_index_type_this_build_implements"

			err := saveOne(buildMetaWithIndex(idx))
			Expect(err).To(HaveOccurred(),
				"an unimplemented index type must stop the write. Succeeding here means the "+
					"record was saved and SOME maintainer wrote entries into this index's "+
					"subspace — which, with the old default arm, was the value-index format")

			var unknown *UnknownIndexTypeError
			Expect(errors.As(err, &unknown)).To(BeTrue(),
				"expected UnknownIndexTypeError, got %T: %v", err, err)
			Expect(unknown.IndexName).To(Equal("Order$mystery"))
			Expect(unknown.IndexType).To(Equal("not_an_index_type_this_build_implements"))

			// Java's throw IS a MetaDataException, and callers catch it as one.
			// Go expresses that relationship by unwrapping, so a caller written
			// against the general metadata failure keeps working.
			var md *MetaDataError
			Expect(errors.As(err, &md)).To(BeTrue(),
				"UnknownIndexTypeError must unwrap to *MetaDataError — Java throws "+
					"MetaDataException here and `catch (MetaDataException)` is what callers use")
		})

		It("names the offending index, since a store may hold many", func() {
			idx := NewIndex("Order$other", Field("price"))
			idx.Type = "still_unknown"

			err := saveOne(buildMetaWithIndex(idx))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Order$other"))
			Expect(err.Error()).To(ContainSubstring("still_unknown"))
		})
	})

	Describe("the deprecated bare _EVER spellings", func() {
		// The unit-level companion to the cross-engine conformance spec: this
		// asserts the maintainer TYPE selected, where the conformance spec
		// asserts the resulting bytes against a live JVM.
		It("selects the same maintainer as the _LONG spelling", func() {
			for _, tc := range []struct {
				bare, long string
			}{
				{bare: IndexTypeMinEver, long: IndexTypeMinEverLong},
				{bare: IndexTypeMaxEver, long: IndexTypeMaxEverLong},
			} {
				bareIdx := NewIndex("Order$price", Field("price"))
				bareIdx.Type = tc.bare
				longIdx := NewIndex("Order$price", Field("price"))
				longIdx.Type = tc.long

				ks := specSubspace()
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(buildMetaWithIndex(NewIndex("Order$dummy", Field("price")))).
						SetSubspace(ks).CreateOrOpen()
					if err != nil {
						return nil, err
					}

					bareM, err := store.createIndexMaintainer(bareIdx)
					Expect(err).NotTo(HaveOccurred(),
						"%q is a type Java resolves (AtomicMutationIndexMaintainer.java:100-106); "+
							"rejecting it would refuse Java-authored metadata", tc.bare)
					longM, err := store.createIndexMaintainer(longIdx)
					Expect(err).NotTo(HaveOccurred())

					Expect(bareM).To(BeAssignableToTypeOf(longM),
						"bare %q must reach the same maintainer as %q", tc.bare, tc.long)

					bareAtomic, ok := bareM.(*atomicMutationIndexMaintainer)
					Expect(ok).To(BeTrue(), "bare %q must reach the ATOMIC maintainer, not %T", tc.bare, bareM)
					longAtomic, ok := longM.(*atomicMutationIndexMaintainer)
					Expect(ok).To(BeTrue())

					// Same mutation, not merely the same maintainer struct: a
					// bare min_ever reaching the MAX mutation would be the same
					// class of silent wrong-answer the default arm produced.
					//
					// Compared by mutation identity rather than deep equality —
					// each mutation closes over its own *Index, and those differ
					// here precisely BECAUSE the type strings differ, which is
					// the thing under test.
					bareMut, ok := bareAtomic.mutation.(*minMaxEverLongMutation)
					Expect(ok).To(BeTrue(),
						"bare %q must resolve to the _LONG mutation, got %T", tc.bare, bareAtomic.mutation)
					longMut, ok := longAtomic.mutation.(*minMaxEverLongMutation)
					Expect(ok).To(BeTrue())
					Expect(bareMut.isMax).To(Equal(longMut.isMax),
						"bare %q must resolve to the same direction as %q — a bare min_ever "+
							"reaching the MAX mutation is a silent wrong answer, not a failure",
						tc.bare, tc.long)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("is treated as an atomic-mutation index everywhere the type is re-derived", func() {
			// Go re-derives behaviour from the type string in several switches
			// that Java answers once, off the maintainer the registry chose. Each
			// of these would silently mis-judge a bare-spelled index if it did
			// not canonicalize.
			for _, bare := range []string{IndexTypeMinEver, IndexTypeMaxEver} {
				idx := NewIndex("Order$price", Field("price"))
				idx.Type = bare

				Expect(idx.IsAtomicMutationIndex()).To(BeTrue(),
					"%q must be recognised as atomic — otherwise it becomes a value-scan "+
						"candidate, which is an atomic index served by a value scan", bare)
				Expect(isIndexIdempotent(idx)).To(BeTrue(),
					"%q is an _EVER index and idempotent like its _LONG twin; a wrong answer "+
						"here changes the read isolation an online build uses", bare)
			}
		})
	})

	Describe("an index proto with no type field", func() {
		It("defaults to VALUE the way Java reads it", func() {
			// Java: `type = proto.hasType() ? proto.getType() : IndexTypes.VALUE`
			// (Index.java:203). The field carries no protobuf default, so metadata
			// written by a Java app that never set it arrives with type absent.
			//
			// This was masked while the dispatch answered everything with the
			// value maintainer: absent type flattened to "" and came back correct
			// by accident. With the dispatch failing closed, the default has to be
			// real or Java-authored metadata stops opening.
			idx, err := indexFromProto(&gen.Index{Name: proto.String("Order$notype")})
			Expect(err).NotTo(HaveOccurred())
			Expect(idx.Type).To(Equal(IndexTypeValue))
		})

		It("does NOT promote an explicitly empty type to VALUE", func() {
			// hasType() is presence, not emptiness. A type explicitly set to ""
			// is a type no maintainer implements and must reach the dispatch and
			// be refused there — Java draws the line in the same place.
			idx, err := indexFromProto(&gen.Index{
				Name: proto.String("Order$emptytype"),
				Type: proto.String(""),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(idx.Type).To(Equal(""))
		})
	})
})
