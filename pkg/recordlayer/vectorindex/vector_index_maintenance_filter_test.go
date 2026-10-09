package vectorindex

import (
	"context"
	"errors"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// vectorRecordingFilter is recordlayer's recordingFilter: it answers values for
// every record.
type vectorRecordingFilter struct {
	values recordlayer.IndexValues
}

func (f *vectorRecordingFilter) MaintainIndex(*recordlayer.Index, proto.Message) recordlayer.IndexValues {
	return f.values
}

func (f *vectorRecordingFilter) MaintainIndexValue(*recordlayer.Index, proto.Message, *recordlayer.IndexEntry) bool {
	return true
}

var _ = Describe("IndexMaintenanceFilter (sliding window)", func() {
	ctx := context.Background()

	build := func(add func(*recordlayer.RecordMetaDataBuilder)) *recordlayer.RecordMetaData {
		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		add(b)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// run opens the spec's store with filter in a transaction.
	run := func(md *recordlayer.RecordMetaData, filter recordlayer.IndexMaintenanceFilter, body func(*recordlayer.FDBRecordStore, *recordlayer.FDBRecordContext) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).
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
		md := build(func(b *recordlayer.RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
		o := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), CoordX: proto.Int64(1), CoordY: proto.Int64(1)}
		err := run(md, &vectorRecordingFilter{values: recordlayer.IndexValuesSome}, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
			_, err := store.SaveRecord(o)
			return err
		})
		var rc *recordlayer.RecordCoreError
		Expect(errors.As(err, &rc)).To(BeTrue(), "%v", err)
		Expect(rc.Message).To(Equal("filtering type SOME is not supported"))

		Expect(run(md, &vectorRecordingFilter{values: recordlayer.IndexValuesNone}, func(store *recordlayer.FDBRecordStore, rtx *recordlayer.FDBRecordContext) error {
			if _, err := store.SaveRecord(o); err != nil {
				return err
			}
			keys, _ := readSlidingWindowEntries(rtx.Transaction(), slidingWindowSubspaceFor(store.Subspace(), idx), nil)
			Expect(keys).To(BeEmpty())
			return nil
		})).To(Succeed())
	})
})
