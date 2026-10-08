package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// A task removes itself from the queue before it runs (Primitives
// executeSingleDeferredTask). If its body then fails, a caller that catches the
// error and commits would commit the removal without the work: the record, with
// no index entry, and a cluster whose state flag no task will revisit. The
// failing task poisons the transaction instead.
var _ = Describe("GuardiANN task poisoning", func() {
	It("refuses to commit after a task fails with its removal buffered", func() {
		ctx := context.Background()
		ks := specSubspace()
		vecIdx := NewVectorIndex("vec_poison", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		vecIdx.Options[IndexOptionVectorEngine] = "GUARDIANN"
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		open := func(rtx *FDBRecordContext) *FDBRecordStore {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			return store
		}
		order := func(id int64) *gen.Order {
			return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(10 * id)), Quantity: proto.Int32(1)}
		}
		// The first save creates the partition's access info and first cluster.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := open(rtx).SaveRecord(order(1))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		// A collapse task, first in queue order, whose cluster metadata cannot be read.
		taskID, cluster := tuple.UUID{}, tuple.UUID{9}
		var taskKey fdb.Key
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := open(rtx)
			g := &guardiann{ss: store.indexSubspace(vecIdx).Sub(int64(1))}
			tx := rtx.Transaction()
			tx.Set(g.clusterMetadataKey(cluster), tuple.Tuple{int64(0), int64(0), tuple.Tuple{int64(0), 0.0, 0.0, 0.0}, int64(clusterStateCollapse)}.Pack())
			task := &guardiannTask{kind: taskCollapse, id: taskID, targets: []tuple.UUID{cluster}, centroid: gVector{data: []float64{10}, typ: vectorcodec.TypeDouble}}
			taskKey = fdb.Key(g.sub(gSubTasks).Pack(tuple.Tuple{taskID}))
			tx.Set(taskKey, task.valueTuple(gVector.encode).Pack())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The second save runs that task inline (the relational layer's mode); the
		// caller catches its error and commits.
		var saveErr error
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := open(rtx)
			store.GetIndexDeferredMaintenanceControl().SetAutoMergeDuringCommit(true)
			_, saveErr = store.SaveRecord(order(2))
			return nil, nil
		})
		Expect(saveErr).To(MatchError(ContainSubstring("has 4 elements, want 5")))
		var rce *RecordCoreError
		Expect(errors.As(err, &rce)).To(BeTrue(), "commit error %v", err)
		Expect(err).To(MatchError(saveErr.Error()))

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			stored, err := open(rtx).LoadRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored).To(BeNil(), "the record of the refused save was not committed")
			v, err := rtx.Transaction().Get(taskKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(v).NotTo(BeNil(), "the failed task's removal was not committed")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
