package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RFC-156 Phase B (spfresh-reviewer + Torvalds NAK fix): the distance-ordered
// stream's horizon is the RE-RANK BUDGET (c) and must NOT be threaded as the
// probe width. The executor passes the ordered High-tuple (k=horizon,
// efSearch=0 ⇒ index-tuned kc, w=0, c=horizon) so the searcher re-ranks
// ~horizon candidates with kc unchanged. The earlier "bump efSearch to the
// horizon" shape overrode kc AND triggered c = 4·k, re-ranking ~4×horizon.
//
// This A/B pins the decoupling on a corpus large enough that the re-rank budget
// (c), not the candidate supply, is the binding constraint: the ordered shape
// re-ranks at most the horizon, while the bumped-efSearch shape inflates beyond
// it.
var _ = Describe("SPFresh ordered-stream re-rank budget (RFC-156 Phase B)", func() {
	ctx := context.Background()

	It("re-ranks ~horizon for the ordered shape, NOT 4× (decoupled from probe width)", func() {
		ks := specSubspace()
		idx := NewIndex("spf_rerank_decouple", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		sb := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Disable the index, seed ~1000 spread-out 2-d points (batched to stay
		// within the tx limits), bulk-build, mark readable — the cold corpus
		// path newSweeperTenant uses, sized so there are enough cells that a
		// kc=200 probe gathers >> horizon candidates.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := sb(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexDisabled("spf_rerank_decouple")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		const n = 1000
		for batch := 0; batch < n; batch += 100 {
			start := batch
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := sb(rtx)
				if serr != nil {
					return nil, serr
				}
				for i := start; i < start+100 && i < n; i++ {
					if _, serr := store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(int64(i + 1)),
						Price:    proto.Int32(int32(i)),
						Quantity: proto.Int32(int32((i * 7) % 1000)),
					}); serr != nil {
						return nil, serr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(BuildSPFreshIndex(ctx, sharedDB, sb, "spf_rerank_decouple", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := sb(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_rerank_decouple")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		measure := func(high tuple.Tuple) int64 {
			timer := NewStoreTimer()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetTimer(timer)
				store, serr := sb(rtx)
				if serr != nil {
					return nil, serr
				}
				idxRef := store.GetMetaData().GetIndex("spf_rerank_decouple")
				m, merr := store.GetIndexMaintainer(idxRef)
				if merr != nil {
					return nil, merr
				}
				type sbd interface {
					ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
				}
				cur := m.(sbd).ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector([]float64{500, 500})},
					High: high,
				}, nil, ScanProperties{})
				for {
					res, cerr := cur.OnNext(ctx)
					if cerr != nil {
						return nil, cerr
					}
					if !res.HasNext() {
						break
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return timer.GetCount(CountSPFreshRerankReads)
		}

		const horizon = 200
		// Ordered-stream shape the executor emits: (k=horizon, efSearch=0 ⇒
		// tuned kc, w=0, c=horizon).
		rerankOrdered := measure(tuple.Tuple{int64(horizon), int64(0), int64(0), int64(horizon)})
		// The NAK'd "bump efSearch to the horizon" shape: (k=horizon,
		// efSearch=horizon) ⇒ kc override + c = 4·k.
		rerankBumped := measure(tuple.Tuple{int64(horizon), int64(horizon)})

		// Measured here: ordered(c=horizon)=200, bumped(efSearch=horizon)=641 —
		// the decoupling holds (ordered re-ranks exactly the horizon; the bumped
		// shape inflates ~3-4× as candidates allow).
		Expect(rerankOrdered).To(BeNumerically(">", int64(0)),
			"ordered scan must actually re-rank candidates")
		Expect(rerankOrdered).To(BeNumerically("<=", int64(horizon)),
			"ordered-stream re-rank budget is the horizon (c=horizon) — no 4× inflation")
		Expect(rerankBumped).To(BeNumerically(">", rerankOrdered),
			"the bumped-efSearch shape inflates the re-rank budget beyond the horizon (the NAK'd behavior)")
	})
})
