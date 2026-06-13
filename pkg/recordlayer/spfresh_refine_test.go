package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// RFC-104 assignment refinement. The headline recovery (drifted fast-fill →
// recall recovers to the bulk baseline) is measured in the env-gated
// foreground-fill bench (SIFT_REFINE). These FDB specs pin the correctness
// invariants that gate the design: a converged bulk index refines to ZERO moves
// (the no-op-on-converged property — what pins kc = 4·spfreshClosurePool), and
// the budgeted cursor advances + wraps deterministically.
var _ = Describe("SPFresh refinement (RFC-104)", func() {
	ctx := context.Background()

	buildMeta := func(idx *Index) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	newVecIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	It("a bulk-built (converged) index refines to ZERO moves, unchanged recall", func() {
		ks := specSubspace()
		idx := newVecIndex("spf_refine")
		md := buildMeta(idx)
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Load + bulk build (build-then-read).
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexDisabled("spf_refine")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		const n = 120
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := 0; i < n; i++ {
				if _, serr = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32((i * 13) % 50)),
					Quantity: proto.Int32(int32((i*7)%40 + 1)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_refine", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_refine")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		before := knnIDs(ctx, storeBuilder, idx)

		// Refine the converged bulk index: every vector re-routes to the SAME
		// closure set the wide build placed (kc=4·spfreshClosurePool matches the
		// build pool), so NOTHING moves. A narrower kc would drop replicas here.
		total := 0
		for {
			m, wrapped, rerr := RefineSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_refine", 1000)
			Expect(rerr).NotTo(HaveOccurred())
			total += m
			if wrapped {
				break
			}
		}
		Expect(total).To(Equal(0), "a converged bulk index must refine to zero moves (gates kc = 4·spfreshClosurePool)")

		after := knnIDs(ctx, storeBuilder, idx)
		Expect(after).To(Equal(before), "zero moves ⇒ identical kNN results")
	})
})

// knnIDs runs a fixed kNN query and returns the result order_ids.
func knnIDs(ctx context.Context, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), idx *Index) []int64 {
	var got []int64
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		maintainer, merr := store.getIndexMaintainer(idx)
		if merr != nil {
			return nil, merr
		}
		sbd := maintainer.(interface {
			ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
		})
		cursor := sbd.ScanByDistance(TupleRange{
			Low:  tuple.Tuple{SerializeVector([]float64{15, 15})},
			High: tuple.Tuple{int64(10)},
		}, nil, ScanProperties{})
		got = got[:0]
		for {
			res, cerr := cursor.OnNext(ctx)
			if cerr != nil {
				return nil, cerr
			}
			if !res.HasNext() {
				break
			}
			got = append(got, res.GetValue().Key[0].(int64))
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return got
}
