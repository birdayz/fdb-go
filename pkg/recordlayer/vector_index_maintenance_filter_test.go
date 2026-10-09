package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// vectorRecordingFilter is recordlayer's recordingFilter: it answers values for
// every record.
type vectorRecordingFilter struct {
	values IndexValues
}

func (f *vectorRecordingFilter) MaintainIndex(*Index, proto.Message) IndexValues { return f.values }

func (f *vectorRecordingFilter) MaintainIndexValue(*Index, proto.Message, *IndexEntry) bool {
	return true
}

var _ = Describe("IndexMaintenanceFilter (sliding window)", func() {
	ctx := context.Background()

	build := func(add func(*RecordMetaDataBuilder)) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		add(b)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// run opens the spec's store with filter in a transaction.
	run := func(md *RecordMetaData, filter IndexMaintenanceFilter, body func(*FDBRecordStore, *FDBRecordContext) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).
				SetIndexMaintenanceFilter(filter).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, body(store, rtx)
		})
		return err
	}

	It("refuses SOME in the sliding window, and maintains no window entry for NONE", func() {
		idx := newWindowedVectorIndex("sw_filter", 2, gen.RowNumberWindowPredicate_ASC)
		md := build(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
		o := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), CoordX: proto.Int64(1), CoordY: proto.Int64(1)}
		err := run(md, &vectorRecordingFilter{values: IndexValuesSome}, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			_, err := store.SaveRecord(o)
			return err
		})
		var rc *RecordCoreError
		Expect(errors.As(err, &rc)).To(BeTrue(), "%v", err)
		Expect(rc.Message).To(Equal("filtering type SOME is not supported"))

		Expect(run(md, &vectorRecordingFilter{values: IndexValuesNone}, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			if _, err := store.SaveRecord(o); err != nil {
				return err
			}
			keys, _ := readSlidingWindowEntries(rtx.Transaction(), slidingWindowSubspaceFor(store.Subspace(), idx), nil)
			Expect(keys).To(BeEmpty())
			return nil
		})).To(Succeed())
	})
})
