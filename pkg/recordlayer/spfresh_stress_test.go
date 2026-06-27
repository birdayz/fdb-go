package recordlayer

import (
	"context"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RFC-094 §13 phase 094.2: the N-writer conflict-metrics stress. The §5
// design claim under test: BELOW Lmax, concurrent inserts of DISTINCT pks
// never conflict each other — the generation read is read-only (read-read),
// posting/sidecar/membership writes are blind to disjoint keys, counters are
// atomic ADDs, and no probe fires. Lmax is set high so every writer lands in
// the SAME posting — the strongest version of the claim. (PAST Lmax the
// sampled probes conflict on the task key BY DESIGN — the Set-if-absent
// conflict range is what protects live claims; that behavior has its own
// pinned test in the fine-split suite. The first draft of this test crossed
// Lmax accidentally and measured exactly those designed conflicts.)
// Every Run attempt beyond one per insert is a transaction conflict (1020)
// the design forbids; the attempt counter makes the claim a regression
// instead of a hope.
var _ = Describe("SPFresh N-writer conflict stress", func() {
	ctx := context.Background()

	It("concurrent distinct-pk inserts commit with zero conflicts", func() {
		const (
			writers          = 8
			insertsPerWriter = 32
		)

		ks := specSubspace()
		idx := NewIndex("spf_stress", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			// High Lmax (the validator caps it at the one-posting-one-reply
			// budget): 2·(64 seeds + 256 inserts) = 640 entries stay below
			// the split threshold, so no probe ever fires (see above).
			IndexOptionSPFreshLmax: "768",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Seed build: a spread of points so the stress inserts route across
		// several postings and none crosses Lmax (no probes, no triggers).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexDisabled("spf_stress")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 64; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(i),
					Price:    proto.Int32(int32((i % 16) * 50)),
					Quantity: proto.Int32(int32((i / 16) * 50)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_stress", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable("spf_stress")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// The stress: one record per transaction (maximum interleaving), all
		// writers concurrent, attempts counted at closure entry.
		var attempts, commits atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				defer GinkgoRecover()
				for i := 0; i < insertsPerWriter; i++ {
					id := int64(1000 + w*insertsPerWriter + i)
					_, werr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						attempts.Add(1)
						store, serr := storeBuilder(rtx)
						if serr != nil {
							return nil, serr
						}
						_, serr = store.SaveRecord(&gen.Order{
							OrderId:  proto.Int64(id),
							Price:    proto.Int32(int32((id % 16) * 50)),
							Quantity: proto.Int32(int32((id % 13) * 60)),
						})
						return nil, serr
					})
					if werr != nil {
						errs <- werr
						return
					}
					commits.Add(1)
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for werr := range errs {
			Expect(werr).NotTo(HaveOccurred())
		}

		total := int64(writers * insertsPerWriter)
		Expect(commits.Load()).To(Equal(total))
		Expect(attempts.Load()).To(Equal(total),
			"every attempt beyond one per insert is an insert-vs-insert conflict the §5 design forbids")

		// Sanity: the inserts actually landed in the index (not just the
		// record store) — every stressed pk has a membership row.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			storage := newSPFreshStorage(store.indexSubspace(idx), 1)
			for w := 0; w < writers; w++ {
				id := int64(1000 + w*insertsPerWriter) // first pk of each writer
				_, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{id})
				Expect(merr).NotTo(HaveOccurred(), "stressed insert missing from the index")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
