package vectorindex

import (
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SPFresh transactional search state", func() {
	It("returns live results, rejects another handle's disable and propagates cancellation", func() {
		b := baseBuilder()
		index := recordlayer.NewIndex("spf_state", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")))
		index.Type = recordlayer.IndexTypeVectorSPFresh
		index.Options = map[string]string{recordlayer.IndexOptionSPFreshNumDimensions: "2"}
		b.AddIndex("Order", index)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rtx := sharedDB.NewRecordContext(tx)
		ss := specSubspace()
		first, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Create()
		Expect(err).NotTo(HaveOccurred())
		_, err = first.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(20)})
		Expect(err).NotTo(HaveOccurred())
		second, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		Expect(err).NotTo(HaveOccurred())
		rows, err := SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
		_, err = first.MarkIndexDisabled(index.Name)
		Expect(err).NotTo(HaveOccurred())
		_, err = SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		var unreadable *recordlayer.IndexNotReadableError
		Expect(errors.As(err, &unreadable)).To(BeTrue())
		Expect(unreadable.CurrentState).To(Equal(recordlayer.IndexStateDisabled))
		tx.Cancel()
		_, err = SearchSPFreshIndex(second, index.Name, []float64{10, 20}, 1)
		var canceled fdb.Error
		Expect(errors.As(err, &canceled)).To(BeTrue())
		Expect(canceled.Code).To(Equal(1025))
	})
})
