package recordlayer

import (
	"context"

	"fdb.dev/gen/demopb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Online build read conflicts match Java", func() {
	ctx := context.Background()

	demoMetaData := func(configure func(*RecordMetaDataBuilder)) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if configure != nil {
			configure(builder)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	Describe("buildRange", func() {
		// A build transaction adds a record read conflict only for records its
		// targets index (IndexingBase.handleCursorResult), so a concurrent write
		// to another record type in the scanned range does not conflict it.
		buildRangeWithConcurrentWrite := func(concurrent proto.Message) (attempts int) {
			GinkgoHelper()
			ks := specSubspace()
			priceIdx := NewIndex("Order$price", Field("price"))
			plain := demoMetaData(nil)
			indexed := demoMetaData(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", priceIdx) })
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(plain).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for _, rec := range []proto.Message{
					&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)},
					&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("c")},
					&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)},
				} {
					_, err := store.SaveRecord(rec)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(indexed).
				SetIndex(priceIdx).SetSubspace(ks).Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(oi.allTargetIndexesIdempotent()).To(BeTrue())
			oi.sessionHeartbeat = oi.newHeartbeat(oi.buildIndexingStamp().GetMethod().String(), false, oi.db.Env())
			_, err = oi.markWriteOnly(ctx)
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				r, err := oi.buildRangeInTransaction(ctx, rtx)
				if err != nil {
					return nil, err
				}
				Expect(r.recordsProcessed).To(Equal(int64(3)))
				if attempts == 1 {
					_, err := sharedDB.Run(ctx, func(rtx2 *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().SetContext(rtx2).SetMetaDataProvider(indexed).SetSubspace(ks).Open()
						Expect(err).NotTo(HaveOccurred())
						return store.SaveRecord(concurrent)
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return attempts
		}

		It("ignores a write to a record type no target indexes", func() {
			Expect(buildRangeWithConcurrentWrite(&demopb.Customer{CustomerId: proto.Int64(2), Name: proto.String("changed")})).To(Equal(1))
		})

		It("still conflicts on a write to an indexed record", func() {
			Expect(buildRangeWithConcurrentWrite(&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(31)})).To(Equal(2))
		})
	})
})
