package recordlayer

import (
	"context"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// An HNSW scan searches layer 0 with its efSearch option as given
// (HnswVectorIndexEngine.efSearch, Search.beamSearchLayer): below k it returns
// fewer entries, 0 keeps the entry point's beam of one, and one Java's
// PriorityQueue(efSearch + 1) cannot even allocate visits the whole layer.
// Absent, it derives from k.
var _ = Describe("HNSW efSearch scan option", func() {
	It("searches with the option as given", func() {
		ctx := context.Background()
		index := NewVectorIndex("hnsw_ef", Concat(Field("price"), Field("quantity")), 2)
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		ks := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).Create()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 8; i++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i)), Quantity: proto.Int32(int32(i))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		count := func(efSearch *int) int {
			n, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				cursor := store.ScanVectorIndexWithOptions(index, nil, []float64{0, 0}, 4, VectorIndexScanOptions{EfSearch: efSearch}, nil, ForwardScan())
				n := 0
				for {
					r, err := cursor.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !r.HasNext() {
						return n, nil
					}
					n++
				}
			})
			Expect(err).NotTo(HaveOccurred())
			return n.(int)
		}
		ef := func(n int) *int { return &n }
		Expect(count(nil)).To(Equal(4))
		Expect(count(ef(2))).To(Equal(2))
		Expect(count(ef(1))).To(Equal(1))
		Expect(count(ef(0))).To(Equal(1))
		Expect(count(ef(4))).To(Equal(4))
		Expect(count(ef(math.MaxInt32))).To(Equal(4))
	})
})
