package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// IndexEntry is the entry source the planner's index-entry reader Values read.
var _ values.IndexEntryTuples = (*IndexEntry)(nil)

type indexEntryBinding struct {
	alias values.CorrelationIdentifier
	entry *IndexEntry
}

func (b indexEntryBinding) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	return b.entry, id == b.alias
}

// An IndexEntryObjectValue reads the raw KEY and VALUE tuples of an entry
// scanned from a real index, as Java's reads IndexEntry.getKey()/getValue()
// (IndexEntryObjectValue.eval), converting into the row domain.
var _ = Describe("IndexEntryObjectValue over scanned entries", func() {
	It("reads KEY and VALUE elements of a key-with-value index entry", func() {
		ctx := context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		// KEY (val_float, val_string, id), VALUE (val_bytes, val_double).
		idx := NewIndex("typed_kwv", KeyWithValue(Concat(Field("val_float"), Field("val_string"),
			Field("val_bytes"), Field("val_double")), 2))
		builder.AddIndex("TypedRecord", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		var entries []*IndexEntry
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(&gen.TypedRecord{
				Id: proto.Int64(7), ValFloat: proto.Float32(2.5),
				ValString: proto.String("s"), ValBytes: []byte{1, 2}, ValDouble: proto.Float64(0.25),
			}); err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(8)}); err != nil {
				return nil, err
			}
			entries, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(2))

		alias := values.CurrentCorrelation()
		read := func(entry *IndexEntry, source values.TupleSource, path []int, typ values.Type) any {
			v, err := values.NewIndexEntryObjectValue(alias, source, path, typ)
			Expect(err).NotTo(HaveOccurred())
			got, err := v.Evaluate(indexEntryBinding{alias: alias, entry: entry})
			Expect(err).NotTo(HaveOccurred())
			return got
		}
		// The all-NULL record sorts first.
		empty, full := entries[0], entries[1]
		Expect(full.Key).To(Equal(tuple.Tuple{float32(2.5), "s", int64(7)}))
		Expect(read(full, values.TupleSourceKey, []int{0}, values.NullableFloat)).To(Equal(float64(2.5)), "a FLOAT key reads as the row domain's float64")
		Expect(read(full, values.TupleSourceKey, []int{1}, values.NullableString)).To(Equal("s"))
		Expect(read(full, values.TupleSourceKey, []int{2}, values.NullableLong)).To(Equal(int64(7)))
		Expect(read(full, values.TupleSourceValue, []int{0}, values.NullableBytes)).To(Equal([]byte{1, 2}))
		Expect(read(full, values.TupleSourceValue, []int{1}, values.NullableDouble)).To(Equal(0.25))
		Expect(read(full, values.TupleSourceOther, []int{1}, values.NullableDouble)).To(Equal(0.25), "OTHER reads VALUE")
		Expect(read(empty, values.TupleSourceKey, []int{0}, values.NullableFloat)).To(BeNil())
		Expect(read(empty, values.TupleSourceValue, []int{1}, values.NullableDouble)).To(BeNil())

		v, err := values.NewIndexEntryObjectValue(alias, values.TupleSourceKey, []int{3}, values.NullableLong)
		Expect(err).NotTo(HaveOccurred())
		_, err = v.Evaluate(indexEntryBinding{alias: alias, entry: full})
		Expect(err).To(HaveOccurred(), "a KEY of three elements has no ordinal 3")
		_, err = v.Evaluate(indexEntryBinding{alias: alias, entry: nil})
		Expect(err).To(HaveOccurred(), "an entry bound as nil is no entry")
	})
})
