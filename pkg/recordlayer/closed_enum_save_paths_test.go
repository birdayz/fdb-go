package recordlayer

import (
	"bytes"
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// A record Go holds with a closed enum's undeclared number is saved as Java
// reads it (RFC-257 ws-j-design.md 4g: the save moves the number to an unknown
// field, as every later load reads it). SaveRecord's reading is pinned against
// the target (conformance, "a record Go holds with an undeclared number is
// saved, updated and deleted as Java reads it"); the batch save and the dry run
// apply the same move, and this pins that they write and return what SaveRecord
// does.
var _ = Describe("A closed enum's undeclared number on the other save paths", func() {
	ctx := context.Background()
	// TypedRecord's val_enum (field 13) is the proto2, so closed, Color; 9 is
	// not one of its values.
	held := func() *gen.TypedRecord {
		return &gen.TypedRecord{Id: proto.Int64(1), ValEnum: gen.Color(9).Enum()}
	}
	undeclared := protowire.AppendVarint(protowire.AppendTag(nil, 13, protowire.VarintType), 9)
	metaData := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("TypedRecord", NewIndex("typed_by_enum", Field("val_enum")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// written is every key and value under ss, relative to ss, but the store
	// header (tuple (0)), whose last-update time differs between two stores.
	written := func(ss subspace.Subspace) [][2][]byte {
		var out [][2][]byte
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			r, err := fdb.PrefixRange(ss.Bytes())
			if err != nil {
				return nil, err
			}
			kvs, err := rc.Transaction().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			header := tuple.Tuple{int64(0)}.Pack()
			for _, kv := range kvs {
				if rel := bytes.TrimPrefix(kv.Key, ss.Bytes()); !bytes.Equal(rel, header) {
					out = append(out, [2][]byte{rel, kv.Value})
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	It("SaveRecordBatch writes the bytes and index entries SaveRecord writes", func() {
		md := metaData()
		single, batch := specSubspace().Sub("single"), specSubspace().Sub("batch")
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(single).Create()
			if err != nil {
				return nil, err
			}
			if _, err := s.SaveRecord(held()); err != nil {
				return nil, err
			}
			b, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(batch).Create()
			if err != nil {
				return nil, err
			}
			_, err = b.SaveRecordBatch([]proto.Message{held()})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		want := written(single)
		Expect(want).To(HaveLen(2), "the record and its index entry")
		Expect(written(batch)).To(Equal(want))
	})

	It("DryRunSaveRecord returns the record as Java reads it and leaves the caller's", func() {
		md := metaData()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).Create()
			if err != nil {
				return nil, err
			}
			in := held()
			got, err := s.DryRunSaveRecord(in, RecordExistenceCheckNone)
			if err != nil {
				return nil, err
			}
			r := got.Record.ProtoReflect()
			valEnum := r.Descriptor().Fields().ByName("val_enum")
			Expect(r.Has(valEnum)).To(BeFalse(), "the previewed record's val_enum")
			Expect([]byte(r.GetUnknown())).To(Equal(undeclared), "the previewed record's unknown fields")
			Expect(in.GetValEnum()).To(Equal(gen.Color(9)), "the caller's record is not changed")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
