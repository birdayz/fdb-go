package recordlayer

import (
	"bytes"
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen/demopb"
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
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
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
			_, err := s.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String(invalid)})
			refused(err, "val_string")
			_, err = s.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Tags: []string{"a", invalid}})
			refused(err, "tags")
			_, err = s.DryRunSaveRecord(&demopb.TypedRecord{Id: proto.Int64(2), ValString: proto.String(invalid)}, RecordExistenceCheckNone)
			refused(err, "val_string")
			// The bad record is last: the valid ones before it are not written either.
			_, err = s.SaveRecordBatch([]proto.Message{
				&demopb.TypedRecord{Id: proto.Int64(3), ValString: proto.String("fine")},
				&demopb.Order{OrderId: proto.Int64(4)},
				&demopb.TypedRecord{Id: proto.Int64(5), ValString: proto.String(invalid)},
			})
			refused(err, "val_string")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(records(md, ss)).To(Equal(0))
	})

	// The batch checks every record before its first write and names the
	// string's path below the record: at the top level, in a message of a
	// repeated field, in a map's message value, and at every other arm
	// TestCheckUTF8Strings_EveryArm drives. SaveRecord is the control.
	It("is refused by SaveRecordBatch at its path in nested, repeated and map-held messages with nothing written", func() {
		fdp := protodesc.ToFileDescriptorProto(utf8CheckFile(GinkgoTB()))
		fdp.MessageType = append(fdp.MessageType, &descriptorpb.DescriptorProto{
			Name: proto.String("RecordTypeUnion"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("_Rec"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".utf8check.Rec"),
			}},
		})
		file, err := protodesc.NewFile(fdp, nil)
		Expect(err).NotTo(HaveOccurred())
		b := NewRecordMetaDataBuilder().SetRecords(file)
		b.GetRecordType("Rec").SetPrimaryKey(Field("id"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())

		rec := md.GetRecordType("Rec").Descriptor
		field := rec.Fields().ByName
		inner := field("inner").Message()
		innerT, innerDeep := inner.Fields().ByName("t"), inner.Fields().ByName("deep")
		str := protoreflect.ValueOfString
		innerWith := func(text string) protoreflect.Value {
			m := dynamicpb.NewMessage(inner)
			m.Set(innerT, str(text))
			return protoreflect.ValueOfMessage(m)
		}
		// Each arm puts text in one place a string can sit, named by path.
		arms := []struct {
			path string
			fill func(m protoreflect.Message, text string)
		}{
			{"s", func(m protoreflect.Message, text string) { m.Set(field("s"), str(text)) }},
			{"rs", func(m protoreflect.Message, text string) {
				l := m.Mutable(field("rs")).List()
				l.Append(str("fine"))
				l.Append(str(text))
			}},
			{"mk", func(m protoreflect.Message, text string) {
				m.Mutable(field("mk")).Map().Set(str(text).MapKey(), protoreflect.ValueOfInt64(1))
			}},
			{"mv", func(m protoreflect.Message, text string) {
				m.Mutable(field("mv")).Map().Set(protoreflect.ValueOfInt64(1).MapKey(), str(text))
			}},
			{"mm.t", func(m protoreflect.Message, text string) {
				mm := m.Mutable(field("mm")).Map()
				mm.Set(protoreflect.ValueOfInt64(1).MapKey(), innerWith("fine"))
				mm.Set(protoreflect.ValueOfInt64(2).MapKey(), innerWith(text))
			}},
			{"inner.t", func(m protoreflect.Message, text string) {
				m.Mutable(field("inner")).Message().Set(innerT, str(text))
			}},
			{"inner.deep.t", func(m protoreflect.Message, text string) {
				m.Mutable(field("inner")).Message().Mutable(innerDeep).Message().Set(innerT, str(text))
			}},
			{"ri.t", func(m protoreflect.Message, text string) {
				l := m.Mutable(field("ri")).List()
				l.Append(innerWith("fine"))
				l.Append(innerWith(text))
			}},
		}
		// record holds valid text at every arm but bad, which holds invalid.
		record := func(id int64, bad string) proto.Message {
			m := dynamicpb.NewMessage(rec)
			m.Set(field("id"), protoreflect.ValueOfInt64(id))
			for _, arm := range arms {
				text := "naïve 日本"
				if arm.path == bad {
					text = invalid
				}
				arm.fill(m, text)
			}
			return m
		}

		ss := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, ss)
			for _, arm := range arms {
				// Last in the batch: the valid records before it are not written either.
				_, err := s.SaveRecordBatch([]proto.Message{record(1, ""), record(2, ""), record(3, arm.path)})
				refused(err, arm.path)
				_, err = s.SaveRecordBatch([]proto.Message{record(3, arm.path), record(4, "")})
				refused(err, arm.path)
				_, err = s.SaveRecord(record(3, arm.path))
				refused(err, arm.path)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(records(md, ss)).To(Equal(0))

		// The control: the same records with valid text at every arm are written.
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := open(rc, md, ss).SaveRecordBatch([]proto.Message{record(1, ""), record(2, "")})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(records(md, ss)).To(BeNumerically(">=", 2))
	})

	It("stays readable where stored before, and is repaired by setting the field or deleting the record", func() {
		md := metaData()
		ss := specSubspace()
		// Store a record whose bytes the save would refuse, as an older Go writer did.
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, ss)
			_, err := s.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String("placeholder"), Price: proto.Int32(1)})
			if err != nil {
				return nil, err
			}
			rt := md.GetRecordType("TypedRecord")
			inner, err := proto.Marshal(&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String(invalid), Price: proto.Int32(1)})
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
			held := loaded.Record.(*demopb.TypedRecord)
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
