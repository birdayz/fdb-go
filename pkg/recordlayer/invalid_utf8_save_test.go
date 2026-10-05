package recordlayer

import (
	"bytes"
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Java's strings are UTF-16 and protobuf-java writes them as valid UTF-8, so a
// Go record holding a string that is not valid UTF-8 is one whose bytes and
// index keys Java reads back as different text. Every save path refuses it
// with nothing written; a record stored with such bytes before stays readable
// and can be repaired or deleted.
var _ = Describe("A string that is not valid UTF-8", func() {
	ctx := context.Background()
	const invalid = "ok\xffno"
	metaData := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	open := func(rc *FDBRecordContext, md *RecordMetaData, ss subspace.Subspace) *FDBRecordStore {
		s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	refused := func(err error, field string) {
		var invalidUTF8 *InvalidUTF8StringError
		ExpectWithOffset(1, errors.As(err, &invalidUTF8)).To(BeTrue(), "error %v", err)
		ExpectWithOffset(1, invalidUTF8.Field).To(Equal(field))
	}
	records := func(md *RecordMetaData, ss subspace.Subspace) int {
		n := 0
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			r, err := fdb.PrefixRange(ss.Bytes())
			if err != nil {
				return nil, err
			}
			kvs, err := rc.Transaction().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			header := tuple.Tuple{int64(0)}.Pack()
			for _, kv := range kvs {
				if !bytes.Equal(bytes.TrimPrefix(kv.Key, ss.Bytes()), header) {
					n++
				}
			}
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	It("is refused by SaveRecord, DryRunSaveRecord and SaveRecordBatch with nothing written", func() {
		md := metaData()
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, ss)
			_, err := s.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String(invalid)})
			refused(err, "val_string")
			_, err = s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Tags: []string{"a", invalid}})
			refused(err, "tags")
			_, err = s.DryRunSaveRecord(&gen.TypedRecord{Id: proto.Int64(2), ValString: proto.String(invalid)}, RecordExistenceCheckNone)
			refused(err, "val_string")
			// The bad record is last: the valid ones before it are not written either.
			_, err = s.SaveRecordBatch([]proto.Message{
				&gen.TypedRecord{Id: proto.Int64(3), ValString: proto.String("fine")},
				&gen.Order{OrderId: proto.Int64(4)},
				&gen.TypedRecord{Id: proto.Int64(5), ValString: proto.String(invalid)},
			})
			refused(err, "val_string")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(records(md, ss)).To(Equal(0))
	})

	It("stays readable where stored before, and is repaired by setting the field or deleting the record", func() {
		md := metaData()
		ss := specSubspace()
		// Store a record whose bytes the save would refuse, as an older Go writer did.
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, ss)
			_, err := s.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String("placeholder"), Price: proto.Int32(1)})
			if err != nil {
				return nil, err
			}
			rt := md.GetRecordType("TypedRecord")
			inner, err := proto.Marshal(&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String(invalid), Price: proto.Int32(1)})
			if err != nil {
				return nil, err
			}
			union := protowire.AppendBytes(protowire.AppendTag(nil, rt.unionFieldNumber, protowire.BytesType), inner)
			rc.Transaction().Set(fdb.Key(s.recordsSubspace.Pack(appendToTuple(tuple.Tuple{int64(1)}, unsplitRecord))), union)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, ss)
			loaded, err := s.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			held := loaded.Record.(*gen.TypedRecord)
			Expect(held.GetValString()).To(Equal(invalid))

			held.Price = proto.Int32(2)
			_, err = s.SaveRecord(held)
			refused(err, "val_string")

			held.ValString = proto.String("repaired")
			_, err = s.SaveRecord(held)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			deleted, err := open(rc, md, ss).DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
