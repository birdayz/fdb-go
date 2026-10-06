package recordlayer

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// A vector search read-locks the partition it searches, under the identifier a
// write to that partition write-locks (Java's LockIdentifier(partitionSubspace)
// in VectorIndexMaintainer.scan and doWithWriteLock). Both the cursor scan and
// SearchKNN wait for a writer of their partition and not for a writer of
// another. The search used to lock nothing (scan) or the whole index subspace
// (SearchKNN), which a partition writer never excluded.
var _ = Describe("Vector partition locks", func() {
	It("make a search of a partition wait for that partition's writer only", func() {
		ctx := context.Background()
		ks := specSubspace()
		vecIdx := NewVectorIndex("vec_partition_lock", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			for id, q := range map[int64]int32{1: 1, 2: 1, 3: 2} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id) * 10), Quantity: proto.Int32(q)})
				Expect(err).NotTo(HaveOccurred())
			}
			m, err := store.getIndexMaintainer(vecIdx)
			Expect(err).NotTo(HaveOccurred())
			vm := m.(*vectorIndexMaintainer)
			partition1 := string(vm.getSubspaceForPrefix(tuple.Tuple{int64(1)}).Bytes())

			searches := map[string]func(prefix int64) error{
				"scan": func(prefix int64) error {
					cursor := store.ScanVectorIndexWithPrefix(vecIdx, tuple.Tuple{prefix}, []float64{15}, 10, 100, nil, ForwardScan())
					_, err := cursor.OnNext(ctx)
					return err
				},
				"SearchKNN": func(prefix int64) error {
					_, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{prefix}, []float64{15}, 10, 100)
					return err
				},
			}
			for name, search := range searches {
				store.AcquireWriteLock(partition1)
				other := make(chan error, 1)
				go func() { other <- search(2) }()
				Eventually(other, 5*time.Second).Should(Receive(BeNil()), "%s of partition 2 waited for partition 1's writer", name)

				same := make(chan error, 1)
				go func() { same <- search(1) }()
				Consistently(same, 300*time.Millisecond).ShouldNot(Receive(), "%s of partition 1 ran beside its writer", name)
				store.ReleaseWriteLock(partition1)
				Eventually(same, 5*time.Second).Should(Receive(BeNil()), "%s of partition 1", name)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
