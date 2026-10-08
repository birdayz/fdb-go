package recordlayer

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// A generated message with a map, decoded through the store: google.protobuf
// .Struct (map<string, Value> fields) as a record type, so LoadRecord and the
// record cursor decode into the generated type, whose map entries are then read
// back from the stored bytes by the generated message's pointer, in wire order.
var _ = Describe("Map entries of a generated record type in wire order", func() {
	ctx := context.Background()

	var structMetaDataWith func(func(*RecordMetaDataBuilder)) *RecordMetaData
	structMetaData := func() *RecordMetaData { return structMetaDataWith(nil) }
	structMetaDataWith = func(edit func(*RecordMetaDataBuilder)) *RecordMetaData {
		optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name:       proto.String("struct_records.proto"),
			Package:    proto.String("structrec"),
			Syntax:     proto.String("proto2"),
			Dependency: []string{"google/protobuf/struct.proto"},
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("RecordTypeUnion"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("_Struct"), Number: proto.Int32(1), Label: optional,
					Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".google.protobuf.Struct"),
				}},
			}},
		}, protoregistry.GlobalFiles)
		Expect(err).NotTo(HaveOccurred())
		builder := NewRecordMetaDataBuilder().SetRecords(fd)
		builder.GetRecordType("Struct").SetPrimaryKey(RecordTypeKey())
		if edit != nil {
			edit(builder)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// structBytes is a Struct whose fields map holds keys in the order given,
	// each a string Value, but key "n", whose Value is a struct_value (Value
	// field 5) holding nested's bytes.
	var structBytes func(nested []byte, keys ...string) []byte
	structBytes = func(nested []byte, keys ...string) []byte {
		var raw []byte
		for _, k := range keys {
			value, err := proto.Marshal(structpb.NewStringValue("v"))
			Expect(err).NotTo(HaveOccurred())
			if k == "n" {
				value = protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), nested)
			}
			entry := protowire.AppendTag(nil, 1, protowire.BytesType)
			entry = protowire.AppendString(entry, k)
			entry = protowire.AppendTag(entry, 2, protowire.BytesType)
			entry = protowire.AppendBytes(entry, value)
			raw = protowire.AppendTag(raw, 1, protowire.BytesType)
			raw = protowire.AppendBytes(raw, entry)
		}
		return raw
	}

	// A saved record is indexed in the order its bytes were written, not in
	// key order: stored fields [z], then saved as {z, a}, both string values
	// "5", is written [z, a] (z, changed, in its stored position, the new key a
	// after it), and the covering index's one key ("5", pk), written by both
	// entries, holds the last one's, a; indexed in key order, [a, z], it would
	// hold z. Through SaveRecord and SaveRecordBatch.
	It("indexes a saved Struct in the order its fields were written", func() {
		md := structMetaDataWith(func(b *RecordMetaDataBuilder) {
			b.AddIndex("Struct", NewIndex("byValue", KeyWithValue(
				NestFanOut("fields", Concat(Nest("value", Field("string_value")), Field("key"))), 1)))
		})
		idx := md.GetIndex("byValue")
		for _, batch := range []bool{false, true} {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&structpb.Struct{Fields: map[string]*structpb.Value{"z": structpb.NewStringValue("1")}})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				rec := &structpb.Struct{Fields: map[string]*structpb.Value{"z": structpb.NewStringValue("5"), "a": structpb.NewStringValue("5")}}
				if batch {
					_, err = store.SaveRecordBatch([]proto.Message{rec})
				} else {
					_, err = store.SaveRecord(rec)
				}
				Expect(err).NotTo(HaveOccurred())
				begin, end := store.IndexSubspace(idx).FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(kvs).To(HaveLen(1), "batch=%t", batch)
				key, err := store.IndexSubspace(idx).Unpack(kvs[0].Key)
				Expect(err).NotTo(HaveOccurred())
				Expect(key[0]).To(Equal("5"))
				value, err := tuple.Unpack(kvs[0].Value)
				Expect(err).NotTo(HaveOccurred())
				Expect(value).To(Equal(tuple.Tuple{"a"}), "batch=%t: the last entry written", batch)
				loaded, err := store.LoadRecord(key[1:])
				Expect(err).NotTo(HaveOccurred())
				Expect(NestFanOut("fields", Field("key")).Evaluate(loaded, loaded.Record)).To(Equal([][]any{{"z"}, {"a"}}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	// A dry-run save serializes over the stored record, as the save does, so
	// its preview is the save's size: the stored map writes key x twice, which
	// the save keeps and a marshal of the loaded message, one entry per key,
	// does not.
	It("previews a dry-run save of a map type at the size the save writes", func() {
		md := structMetaData()
		rt := md.GetRecordType("Struct")
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			saved, err := store.SaveRecord(&structpb.Struct{Fields: map[string]*structpb.Value{"a": structpb.NewStringValue("v")}})
			Expect(err).NotTo(HaveOccurred())
			key := fdb.Key(store.recordsSubspace.Pack(append(saved.PrimaryKey, int64(0))))
			union := protowire.AppendTag(nil, 1, protowire.BytesType)
			rtx.Transaction().Set(key, protowire.AppendBytes(union, structBytes(nil, "x", "y", "x")))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			loaded, err := store.LoadRecord(tuple.Tuple{rt.GetRecordTypeKey()})
			Expect(err).NotTo(HaveOccurred())
			marshaled, err := proto.Marshal(loaded.Record)
			Expect(err).NotTo(HaveOccurred())
			dry, err := store.DryRunSaveRecord(loaded.Record, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
			saved, err := store.SaveRecord(loaded.Record)
			Expect(err).NotTo(HaveOccurred())
			Expect(dry.ValueSize).To(Equal(saved.ValueSize))
			Expect(dry.ValueSize).To(Equal(len(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), structBytes(nil, "x", "y", "x")))))
			Expect(dry.ValueSize).To(BeNumerically(">", len(marshaled)+2), "x written twice")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads a Struct's fields in the order its stored bytes hold them", func() {
		md := structMetaData()
		rt := md.GetRecordType("Struct")
		ks := specSubspace()
		expr := NestFanOut("fields", Field("key"))

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			saved, err := store.SaveRecord(&structpb.Struct{Fields: map[string]*structpb.Value{"a": structpb.NewStringValue("v")}})
			Expect(err).NotTo(HaveOccurred())
			// Replace the stored record with bytes whose keys are out of key
			// order, as another writer would store them.
			key := fdb.Key(store.recordsSubspace.Pack(append(saved.PrimaryKey, int64(0))))
			stored, err := rtx.Transaction().Get(key).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(stored).NotTo(BeNil(), "the record is unsplit, at suffix 0")
			union := protowire.AppendTag(nil, 1, protowire.BytesType)
			rtx.Transaction().Set(key, protowire.AppendBytes(union, structBytes(structBytes(nil, "z", "y"), "c", "a", "n", "b")))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			want := [][]any{{"c"}, {"a"}, {"n"}, {"b"}}

			loaded, err := store.LoadRecord(tuple.Tuple{rt.GetRecordTypeKey()})
			Expect(err).NotTo(HaveOccurred())
			Expect(fmt.Sprintf("%T", loaded.Record)).To(Equal("*structpb.Struct"), "the generated type, not a dynamic message")
			Expect(expr.Evaluate(loaded, loaded.Record)).To(Equal(want))

			scanned, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(scanned).To(HaveLen(1))
			Expect(fmt.Sprintf("%T", scanned[0].Record)).To(Equal("*structpb.Struct"))
			Expect(expr.Evaluate(scanned[0], scanned[0].Record)).To(Equal(want))

			// A nested map, in a map value's message: fields["n"].struct_value
			// .fields, whose stored order z, y is not key order either.
			nested := NestFanOut("fields", Nest("value", Nest("struct_value", NestFanOut("fields", Field("key")))))
			Expect(nested.Evaluate(loaded, loaded.Record)).To(Equal([][]any{{"z"}, {"y"}}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
