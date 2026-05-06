package recordlayer

import (
	"context"
	"math"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("FieldTypeIndexes", func() {
	ctx := context.Background()

	buildTypedRecordMeta := func(indexes ...*Index) *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		for _, idx := range indexes {
			builder.AddIndex("TypedRecord", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	scanIndexEntries := func(store *FDBRecordStore, index *Index) []fdb.KeyValue {
		idxSubspace := store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
		begin, end := idxSubspace.FDBRangeKeys()
		kvs, err := store.context.Transaction().GetRange(
			fdb.KeyRange{Begin: begin, End: end},
			fdb.RangeOptions{},
		).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}

	unpackEntry := func(store *FDBRecordStore, index *Index, kv fdb.KeyValue) tuple.Tuple {
		idxSubspace := store.subspace.Sub(IndexKey, index.SubspaceTupleKey())
		t, err := idxSubspace.Unpack(kv.Key)
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	It("int32 index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_int32", Field("val_int32"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValInt32: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t).To(HaveLen(2)) // (val_int32, pk)
			Expect(t[0]).To(Equal(int64(42)))
			Expect(t[1]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sint32 index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_sint32", Field("val_sint32"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValSint32: proto.Int32(-100)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(int64(-100)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sint64 index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_sint64", Field("val_sint64"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValSint64: proto.Int64(-9999999)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(int64(-9999999)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sfixed32 index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_sfixed32", Field("val_sfixed32"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValSfixed32: proto.Int32(12345)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(int64(12345)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sfixed64 index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_sfixed64", Field("val_sfixed64"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValSfixed64: proto.Int64(-12345)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(int64(-12345)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("float index stores as float32 (matches Java FDB tuple encoding)", func() {
		idx := NewIndex("TypedRecord$val_float", Field("val_float"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValFloat: proto.Float32(3.14)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			// Proto float fields must encode as float32 in FDB tuple (type code 0x20).
			// Java's Tuple.add(Float) uses 0x20. Go must match.
			Expect(t[0]).To(BeAssignableToTypeOf(float32(0)))
			Expect(t[0]).To(BeNumerically("~", 3.14, 0.001))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("double index stores exact float64", func() {
		idx := NewIndex("TypedRecord$val_double", Field("val_double"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValDouble: proto.Float64(2.718281828)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(float64(2.718281828)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("bool index sorts false before true", func() {
		idx := NewIndex("TypedRecord$val_bool", Field("val_bool"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValBool: proto.Bool(true)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(2), ValBool: proto.Bool(false)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// FDB tuple ordering: false < true
			Expect(entries[0].IndexValues()[0]).To(Equal(false))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
			Expect(entries[1].IndexValues()[0]).To(Equal(true))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("string index stores exact string value", func() {
		idx := NewIndex("TypedRecord$val_string", Field("val_string"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String("hello world")})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal("hello world"))
			Expect(t[1]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("bytes index stores exact byte slice", func() {
		idx := NewIndex("TypedRecord$val_bytes", Field("val_bytes"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValBytes: []byte{0x00, 0xFF, 0xAB}})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal([]byte{0x00, 0xFF, 0xAB}))
			Expect(t[1]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("enum index normalizes to int64", func() {
		idx := NewIndex("TypedRecord$val_enum", Field("val_enum"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValEnum: gen.Color_BLUE.Enum()})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t[0]).To(Equal(int64(2))) // BLUE=2
			Expect(t[1]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("null (unset) field produces nil in index entry", func() {
		idx := NewIndex("TypedRecord$val_string", Field("val_string"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Only set id, leave val_string unset
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1)})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			Expect(t).To(HaveLen(2))
			Expect(t[0]).To(BeNil()) // unset optional -> nil
			Expect(t[1]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("composite index with multiple types", func() {
		idx := NewIndex("TypedRecord$composite", Concat(Field("val_int32"), Field("val_float"), Field("val_string")))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{
				Id:        proto.Int64(1),
				ValInt32:  proto.Int32(42),
				ValFloat:  proto.Float32(1.5),
				ValString: proto.String("abc"),
			})
			Expect(err).NotTo(HaveOccurred())

			kvs := scanIndexEntries(store, idx)
			Expect(kvs).To(HaveLen(1))
			t := unpackEntry(store, idx, kvs[0])
			// (int64, float32, string, pk) — float proto field → float32
			Expect(t).To(HaveLen(4))
			Expect(t[0]).To(Equal(int64(42)))
			Expect(t[1]).To(BeAssignableToTypeOf(float32(0)))
			Expect(t[1]).To(BeNumerically("~", 1.5, 0.001))
			Expect(t[2]).To(Equal("abc"))
			Expect(t[3]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("save/delete/scan roundtrip with sorted doubles", func() {
		idx := NewIndex("TypedRecord$val_double", Field("val_double"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValDouble: proto.Float64(-1.5)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(2), ValDouble: proto.Float64(0.0)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(3), ValDouble: proto.Float64(99.9)})
			Expect(err).NotTo(HaveOccurred())

			// Verify FDB tuple ordering: -1.5 < 0.0 < 99.9
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			Expect(entries[0].IndexValues()[0]).To(Equal(float64(-1.5)))
			Expect(entries[1].IndexValues()[0]).To(Equal(float64(0.0)))
			Expect(entries[2].IndexValues()[0]).To(Equal(float64(99.9)))

			// Delete middle record (id=2, val_double=0.0)
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			entries, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].IndexValues()[0]).To(Equal(float64(-1.5)))
			Expect(entries[1].IndexValues()[0]).To(Equal(float64(99.9)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("float special values: +Inf, -Inf, 0.0, -0.0", func() {
		idx := NewIndex("TypedRecord$val_float", Field("val_float"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValFloat: proto.Float32(float32(math.Inf(1)))})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(2), ValFloat: proto.Float32(float32(math.Inf(-1)))})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(3), ValFloat: proto.Float32(0.0)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(4), ValFloat: proto.Float32(float32(math.Copysign(0, -1)))})
			Expect(err).NotTo(HaveOccurred())

			// All entries should be scannable
			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4))

			// FDB tuple ordering: -Inf < -0.0 < 0.0 < +Inf
			// float proto field → float32 in FDB tuple
			firstVal := entries[0].IndexValues()[0].(float32)
			Expect(math.IsInf(float64(firstVal), -1)).To(BeTrue())

			lastVal := entries[len(entries)-1].IndexValues()[0].(float32)
			Expect(math.IsInf(float64(lastVal), 1)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("int32 boundary values: MaxInt32 and MinInt32", func() {
		idx := NewIndex("TypedRecord$val_int32", Field("val_int32"))
		md := buildTypedRecordMeta(idx)

		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValInt32: proto.Int32(math.MaxInt32)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(2), ValInt32: proto.Int32(math.MinInt32)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// MinInt32 < MaxInt32 in FDB tuple ordering
			Expect(entries[0].IndexValues()[0]).To(Equal(int64(math.MinInt32)))
			Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
			Expect(entries[1].IndexValues()[0]).To(Equal(int64(math.MaxInt32)))
			Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
