package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Java saveTypedRecord loads the current record before each write, then removes
// that record's old index entries. The batch API promises the same sequential semantics.
var _ = Describe("SaveRecordBatch repeated primary keys", func() {
	ctx := context.Background()
	record := func(id int64, price int32, size int, unknown protowire.Number) *gen.TypedRecord {
		r := &gen.TypedRecord{Id: proto.Int64(id), Price: proto.Int32(price), ValBytes: bytes.Repeat([]byte{'x'}, size)}
		r.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, unknown, protowire.BytesType), "forward-compatible"))
		return r
	}
	build := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto).SetSplitLongRecords(true)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.SetRecordCountKey(EmptyKey())
		b.AddIndex("TypedRecord", NewIndex("price", Field("price")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	contents := func(store *FDBRecordStore) []fdb.KeyValue {
		kvs, err := store.context.Transaction().GetRange(store.subspace, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		var out []fdb.KeyValue
		for _, kv := range kvs {
			key := bytes.TrimPrefix(kv.Key, store.subspace.Bytes())
			if bytes.Equal(key, tuple.Tuple{StoreInfoKey}.Pack()) {
				continue // Store header creation timestamps are not record data.
			}
			out = append(out, fdb.KeyValue{Key: key, Value: kv.Value})
		}
		return out
	}
	It("replaces both committed and earlier batch VERSION index entries", func() {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.SetStoreRecordVersions(true)
		index := NewVersionIndex("versions", VersionKey())
		b.AddIndex("TypedRecord", index)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		run := func(body func(*FDBRecordStore) error) error {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, body(store)
			})
			return err
		}
		Expect(run(func(store *FDBRecordStore) error {
			_, err := store.SaveRecord(record(1, 5, 1, 998))
			return err
		})).To(Succeed())
		Expect(run(func(store *FDBRecordStore) error {
			_, err := store.SaveRecordBatch([]proto.Message{record(1, 10, 3, 999), record(1, 20, 7, 1000)})
			return err
		})).To(Succeed())
		Expect(run(func(store *FDBRecordStore) error {
			entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1), "neither the committed nor the intermediate version may remain indexed")
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Version).NotTo(BeNil())
			versionstamp, err := loaded.Version.ToVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].Key[0]).To(Equal(versionstamp))
			return nil
		})).To(Succeed())
	})

	for _, c := range []struct {
		label             string
		existing          bool
		seed, first, last int
	}{
		{"insert then update", false, 0, 3, 7},
		{"two updates of an existing record", true, 5, 3, 7},
		{"unsplit to split", false, 0, 3, splitRecordSize + 500},
		{"split to unsplit", false, 0, splitRecordSize + 500, 7},
		{"existing split through unsplit to split", true, splitRecordSize + 900, 3, splitRecordSize + 500},
	} {
		It(c.label+" matches sequential saves, including indexes, counts and unknown fields", func() {
			md := build()
			referenceSpace, batchSpace := specSubspace().Sub("ref"), specSubspace().Sub("bat")
			open := func(rc *FDBRecordContext, space subspace.Subspace) *FDBRecordStore {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(space).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return store
			}
			if c.existing {
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					for _, space := range []subspace.Subspace{referenceSpace, batchSpace} {
						_, err := open(rc, space).SaveRecord(record(1, 5, c.seed, 998))
						Expect(err).NotTo(HaveOccurred())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
			input := []proto.Message{
				record(1, 10, c.first, 999),
				record(2, 30, 1, 1001), // The repeated key is deliberately non-adjacent.
				record(1, 20, c.last, 1000),
			}
			check := func(reference, batch *FDBRecordStore) {
				for _, store := range []*FDBRecordStore{reference, batch} {
					count, err := store.GetRecordCount()
					Expect(err).NotTo(HaveOccurred())
					Expect(count).To(Equal(int64(2)), "repeated key must count once in %x", store.subspace.Bytes())
					entries, err := AsList(ctx, store.ScanIndex(md.GetIndex("price"), TupleRangeAll, nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					var keys []tuple.Tuple
					for _, entry := range entries {
						keys = append(keys, entry.Key)
					}
					Expect(keys).To(Equal([]tuple.Tuple{{int64(20), int64(1)}, {int64(30), int64(2)}}), "old price entry must be removed")
					loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
					Expect(err).NotTo(HaveOccurred())
					Expect(loaded).NotTo(BeNil())
					Expect(proto.Equal(loaded.Record, input[2])).To(BeTrue(), "last write must win, including only its unknown fields")
					Expect(loaded.Split).To(Equal(c.last > splitRecordSize))
				}
				want, got := contents(reference), contents(batch)
				Expect(got).To(HaveLen(len(want)), "no orphaned record chunks or index entries")
				for i := range want {
					Expect([]byte(got[i].Key)).To(Equal([]byte(want[i].Key)))
					Expect(bytes.Equal(got[i].Value, want[i].Value)).To(BeTrue(), "stored bytes differ at %x (lengths %d/%d)", want[i].Key, len(got[i].Value), len(want[i].Value))
				}
			}
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				reference, batch := open(rc, referenceSpace), open(rc, batchSpace)
				var want []*FDBStoredRecord[proto.Message]
				for _, rec := range input {
					saved, err := reference.SaveRecord(proto.Clone(rec))
					Expect(err).NotTo(HaveOccurred())
					want = append(want, saved)
				}
				got, err := batch.SaveRecordBatch(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(HaveLen(len(want)))
				for i := range want {
					Expect(proto.Equal(got[i].Record, want[i].Record)).To(BeTrue(), fmt.Sprintf("result %d", i))
					Expect(got[i].PrimaryKey).To(Equal(want[i].PrimaryKey))
					Expect([]any{got[i].Split, got[i].KeyCount, got[i].KeySize, got[i].ValueSize}).To(Equal([]any{want[i].Split, want[i].KeyCount, want[i].KeySize, want[i].ValueSize}), "result %d sizes", i)
				}
				check(reference, batch)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				check(open(rc, referenceSpace), open(rc, batchSpace))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
