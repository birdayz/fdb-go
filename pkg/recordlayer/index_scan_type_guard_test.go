package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Java's bitmap and text maintainers accept only their own scan type
// (BitmapValueIndexMaintainer.scan, TextIndexMaintainer.scan); RemoteFetchIndexScanTest
// .testScanUnsupportedIndex relies on the bitmap one for SCAN_AND_FETCH.
var _ = Describe("ScanIndexByType scan-type checks", func() {
	It("rejects other scan types on bitmap and text indexes", func() {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		bitmap := NewBitmapValueIndex("bitmap", GroupBy(Field("order_id"), Field("price")))
		bitmap.SetOption(IndexOptionBitmapValueEntrySize, "16")
		builder.AddIndex("Order", bitmap)
		builder.AddIndex("Customer", NewTextIndex("text", Field("name")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetSubspace(specSubspace()).SetMetaDataProvider(md).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("a b")})
			Expect(err).NotTo(HaveOccurred())
			for index, want := range map[string]string{
				"bitmap": "Can only scan bitmap index by group.",
				"text":   "Can only scan text index by text token.",
			} {
				_, err := AsList(context.Background(), store.ScanIndexByType(md.GetIndex(index), IndexScanByValue, TupleRangeAll, nil, ForwardScan()))
				var recordCore *RecordCoreError
				Expect(errors.As(err, &recordCore)).To(BeTrue(), "%s: %v", index, err)
				Expect(recordCore.Message).To(Equal(want))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
