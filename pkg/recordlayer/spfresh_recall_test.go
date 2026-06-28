package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("SPFresh recall monitor (RFC-156 ground-truth)", func() {
	ctx := context.Background()

	recallMetadata := func() *RecordMetaDataBuilder {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return b
	}
	recallIndex := func(name string) *Index {
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

	It("reports high recall on a healthy bulk-built index", func() {
		ks := specSubspace()
		idx := recallIndex("spf_recall")
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// 64 DISTINCT grid points (price 0..7 x quantity 0..7) so ground truth
		// is unambiguous; bulk build => the converged-topology ideal.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			if _, serr := store.MarkIndexDisabled("spf_recall"); serr != nil {
				return nil, serr
			}
			id := int64(1)
			for p := int32(0); p < 8; p++ {
				for q := int32(0); q < 8; q++ {
					if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)}); serr != nil {
						return nil, serr
					}
					id++
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_recall", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_recall")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		var report SPFreshRecallReport
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			report, serr = MeasureSPFreshRecall(ctx, store, "spf_recall", 5, 30, 7)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.CorpusSize).To(Equal(64))
		Expect(report.QueriesRun).To(Equal(30))
		Expect(report.K).To(Equal(5))
		// A healthy bulk-built index recalls high; the residual gap from 1.0 is
		// grid-point distance ties (several equidistant neighbors at the k-th
		// rank), not index error — which is exactly what makes this a real
		// recall measurement. The threshold has headroom so a genuine
		// regression (corruption / maintenance behind) drops well below it.
		Expect(report.MeanRecall).To(BeNumerically(">=", 0.90),
			"healthy SPFresh index must have high recall@5 (got %.4f)", report.MeanRecall)
		GinkgoWriter.Printf("recall@5: mean=%.4f min=%.4f perfectFrac=%.4f\n",
			report.MeanRecall, report.MinRecall, report.PerfectFraction)
	})

	It("returns a zero-query report for an empty index", func() {
		ks := specSubspace()
		idx := recallIndex("spf_recall_empty")
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		var report SPFreshRecallReport
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			report, serr = MeasureSPFreshRecall(ctx, store, "spf_recall_empty", 10, 10, 1)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.CorpusSize).To(BeZero())
		Expect(report.QueriesRun).To(BeZero())
	})
})
