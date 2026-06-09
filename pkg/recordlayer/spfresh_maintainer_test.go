package recordlayer

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh index maintainer e2e", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// NewSPFreshIndex-style helper: a 2D vector index over (price, quantity).
	newIndex := func(name string) *Index {
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

	It("build-then-read: records -> BuildSPFreshIndex -> ScanByDistance; writes rejected per 094.1", func() {
		ks := specSubspace()
		idx := newIndex("spf_price_qty")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Phase A: disable the index, load records (the build-then-write
		// contract — a disabled index receives no maintenance).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexDisabled("spf_price_qty")
			Expect(serr).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		points := []struct {
			id       int64
			price    int32
			quantity int32
		}{
			{1, 10, 10},
			{2, 20, 20},
			{3, 100, 100},
			{4, 50, 50},
			{5, 12, 9},
			{6, 95, 105},
			{7, 47, 52},
			{8, 22, 18},
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for _, p := range points {
				_, serr = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(p.id), Price: proto.Int32(p.price), Quantity: proto.Int32(p.quantity),
				})
				Expect(serr).NotTo(HaveOccurred(), "disabled SPFresh index must not block record writes")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase B: bulk-build and mark readable.
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_price_qty", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable("spf_price_qty")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase C: kNN through the maintainer's ScanByDistance (the executor's
		// entry point). Query (15,15), squared distances: id=5 (12,9) d²=45,
		// id=1 (10,10) d²=50, id=2 (20,20) d²=50, id=8 (22,18) d²=58.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd, ok := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			Expect(ok).To(BeTrue(), "SPFresh maintainer must expose ScanByDistance")

			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{15, 15})},
				High: tuple.Tuple{int64(4)},
			}, nil, ScanProperties{})
			var got []int64
			for {
				res, cerr := cursor.OnNext(ctx)
				Expect(cerr).NotTo(HaveOccurred())
				if !res.HasNext() {
					break
				}
				got = append(got, res.GetValue().Key[0].(int64))
			}
			Expect(got).To(HaveLen(4))
			Expect(got[0]).To(Equal(int64(5)), "exact re-rank: (12,9) at d²=45 is nearest to (15,15)")
			sorted := append([]int64(nil), got...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			Expect(sorted).To(Equal([]int64{1, 2, 5, 8}), "the four nearest points")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase D: the 094.1 contract — foreground writes against the readable
		// index are rejected loudly (never silently dropped maintenance).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			return nil, serr
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not supported in phase 094.1"))
	})

	It("rejects an invalid SPFresh config at maintainer construction", func() {
		ks := specSubspace()
		idx := newIndex("spf_bad")
		idx.Options[IndexOptionSPFreshAlpha] = "1.0" // the closure bug as a config error
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if serr != nil {
				return nil, serr
			}
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			return nil, serr
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("alpha"))
	})

	It("ScanByDistance before any build reports a clear error", func() {
		ks := specSubspace()
		idx := newIndex("spf_unbuilt")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{1, 1})},
				High: tuple.Tuple{int64(1)},
			}, nil, ScanProperties{})
			_, cerr := cursor.OnNext(ctx)
			Expect(cerr).To(HaveOccurred())
			Expect(cerr.Error()).To(ContainSubstring("no readable generation"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
