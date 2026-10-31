package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

type batchPrefetchProbeTransaction struct {
	fdb.WritableTransaction
	ranges  []fdb.KeyRange
	options []fdb.RangeOptions
	drained int
	readErr error
}

func (tx *batchPrefetchProbeTransaction) Snapshot() fdb.ReadTransaction {
	return &batchPrefetchProbeSnapshot{ReadTransaction: tx.WritableTransaction.Snapshot(), owner: tx}
}

type batchPrefetchProbeSnapshot struct {
	fdb.ReadTransaction
	owner *batchPrefetchProbeTransaction
}

func (tx *batchPrefetchProbeSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	begin, end := r.(fdb.ExactRange).FDBRangeKeys()
	tx.owner.ranges = append(tx.owner.ranges, fdb.KeyRange{Begin: append(fdb.Key(nil), begin.FDBKey()...), End: append(fdb.Key(nil), end.FDBKey()...)})
	tx.owner.options = append(tx.owner.options, options)
	return &batchPrefetchProbeRange{RangeResult: tx.ReadTransaction.GetRange(r, options), owner: tx.owner}
}

type batchPrefetchProbeRange struct {
	fdb.RangeResult
	owner *batchPrefetchProbeTransaction
}

func (r *batchPrefetchProbeRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	r.owner.drained++
	if r.owner.readErr != nil {
		return nil, r.owner.readErr
	}
	return r.RangeResult.GetSliceWithError()
}

var _ = Describe("Record Layer batch prefetch", func() {
	ctx := context.Background()
	metadata := func(split bool) *RecordMetaData {
		b := baseBuilder().SetSplitLongRecords(split)
		b.AddIndex("TypedRecord", NewIndex("price", Field("price")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	record := func(id int64, price int32, size int) *demopb.TypedRecord {
		r := &demopb.TypedRecord{Id: proto.Int64(id), Price: proto.Int32(price), ValBytes: bytes.Repeat([]byte{'x'}, size)}
		r.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 999, protowire.BytesType), "future field"))
		return r
	}
	transaction := func() (*FDBRecordContext, *batchPrefetchProbeTransaction) {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tx.Cancel)
		probe := &batchPrefetchProbeTransaction{WritableTransaction: tx}
		return sharedDB.NewRecordContext(probe), probe
	}
	builder := func(rc *FDBRecordContext, md *RecordMetaData, ss subspace.Subspace) *StoreBuilder {
		return NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ss)
	}
	contents := func(store *FDBRecordStore) []fdb.KeyValue {
		rows, err := store.context.Transaction().GetRange(store.subspace, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		var out []fdb.KeyValue
		for _, row := range rows {
			key := bytes.TrimPrefix(row.Key, store.subspace.Bytes())
			if !bytes.Equal(key, tuple.Tuple{StoreInfoKey}.Pack()) {
				out = append(out, fdb.KeyValue{Key: key, Value: row.Value})
			}
		}
		return out
	}

	It("issues canonical snapshot ranges before open, deduplicates keys, and drains once", func() {
		md := metadata(true)
		rc, probe := transaction()
		b := builder(rc, md, specSubspace())
		first, second := record(1, 1, 3), record(2, 2, 4)
		before := proto.Clone(first)
		prefetch := b.PrefetchRecordBatch([]proto.Message{first, second, record(1, 3, 5)})
		Expect(probe.ranges).To(HaveLen(2), "reads must be issued before Wait or CreateOrOpen")
		Expect(probe.drained).To(BeZero())
		Expect(proto.Equal(first, before)).To(BeTrue())
		for i, id := range []int64{1, 2} {
			prefix := b.subspace.Sub(RecordKey).Pack(tuple.Tuple{id})
			begin, end := recordRangeBounds(prefix)
			Expect(probe.ranges[i]).To(Equal(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}))
			Expect(probe.options[i]).To(Equal(fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
		}
		store, err := b.CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		Expect(prefetch.Wait()).To(Succeed())
		Expect(probe.drained).To(Equal(2))
		Expect(prefetch.Wait()).To(Succeed())
		Expect(probe.drained).To(Equal(2))
		_, err = store.SaveRecordBatch([]proto.Message{first, second})
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.Commit()).To(Succeed())
	})

	for _, layout := range []struct {
		name   string
		split  bool
		format int32
		size   int
	}{
		{"modern unsplit", true, 0, 3},
		{"modern split", true, 0, splitRecordSize + 30},
		{"legacy bare key", false, 4, 3},
	} {
		It("preserves replacement, duplicate-key, unrelated-record and unknown-field semantics for "+layout.name, func() {
			md := metadata(layout.split)
			spaces := []subspace.Subspace{specSubspace().Sub("control"), specSubspace().Sub("prefetch")}
			open := func(rc *FDBRecordContext, ss subspace.Subspace) *FDBRecordStore {
				b := builder(rc, md, ss)
				if layout.format != 0 {
					b.SetFormatVersion(layout.format)
				}
				s, err := b.CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return s
			}
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				for _, ss := range spaces {
					s := open(rc, ss)
					_, err := s.SaveRecordBatch([]proto.Message{record(1, 1, layout.size), record(99, 99, 7)})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			input := []proto.Message{record(1, 2, 5), record(2, 4, layout.size), record(1, 3, 8)}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				prefetch := builder(rc, md, spaces[1]).PrefetchRecordBatch(input)
				control, warmed := open(rc, spaces[0]), open(rc, spaces[1])
				Expect(prefetch.Wait()).To(Succeed())
				_, err := control.SaveRecordBatch(input)
				Expect(err).NotTo(HaveOccurred())
				_, err = warmed.SaveRecordBatch(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(contents(warmed)).To(Equal(contents(control)))
				for _, s := range []*FDBRecordStore{control, warmed} {
					other, err := s.LoadRecord(tuple.Tuple{int64(99)})
					Expect(err).NotTo(HaveOccurred())
					Expect(proto.Equal(other.Record, record(99, 99, 7))).To(BeTrue())
					last, err := s.LoadRecord(tuple.Tuple{int64(1)})
					Expect(err).NotTo(HaveOccurred())
					Expect(proto.Equal(last.Record, input[2])).To(BeTrue())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("adds no read conflicts itself but leaves normal save conflicts authoritative", func() {
		md, ss := metadata(true), specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := builder(rc, md, ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = s.SaveRecord(record(1, 1, 3))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		change := func(price int32) {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				s, err := builder(rc, md, ss).Open()
				if err != nil {
					return nil, err
				}
				_, err = s.SaveRecord(record(1, price, 4))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		rc, _ := transaction()
		Expect(builder(rc, md, ss).PrefetchRecordBatch([]proto.Message{record(1, 2, 3)}).Wait()).To(Succeed())
		change(3)
		rc.Transaction().Set(ss.Sub("independent").FDBKey(), []byte("snapshot only"))
		Expect(rc.Commit()).To(Succeed(), "snapshot warming must not protect the old image")

		rc, _ = transaction()
		b := builder(rc, md, ss)
		Expect(b.PrefetchRecordBatch([]proto.Message{record(1, 4, 3)}).Wait()).To(Succeed())
		s, err := b.Open()
		Expect(err).NotTo(HaveOccurred())
		_, err = s.SaveRecordBatch([]proto.Message{record(1, 4, 3)})
		Expect(err).NotTo(HaveOccurred())
		change(5)
		var conflict fdb.Error
		Expect(errors.As(rc.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
	})

	It("does not deserialize corrupt old images or replace the normal save's validation", func() {
		md, ss := metadata(true), specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := builder(rc, md, ss).CreateOrOpen()
			if err == nil {
				rc.Transaction().Set(ss.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(0)}), []byte{0xff})
			}
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		rc, _ := transaction()
		b := builder(rc, md, ss)
		Expect(b.PrefetchRecordBatch([]proto.Message{record(1, 2, 3)}).Wait()).To(Succeed())
		s, err := b.Open()
		Expect(err).NotTo(HaveOccurred())
		_, err = s.SaveRecordBatch([]proto.Message{record(1, 2, 3)})
		var corrupt *RecordDeserializationError
		Expect(errors.As(err, &corrupt)).To(BeTrue())
	})

	It("defers preparation and read errors and returns a stable wait result", func() {
		md := metadata(true)
		rc, probe := transaction()
		b := builder(rc, md, specSubspace())
		for _, input := range [][]proto.Message{
			{record(1, 1, 2), nil},
			{&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("\xff")}},
			{dynamicpb.NewMessage(demopb.File_record_layer_demo_proto.Messages().ByName("UnionDescriptor"))},
		} {
			pending := b.PrefetchRecordBatch(input)
			Expect(probe.ranges).To(BeEmpty(), "preparation failure must not issue partial reads")
			err := pending.Wait()
			Expect(err).To(HaveOccurred())
			Expect(pending.Wait()).To(BeIdenticalTo(err))
		}
		Expect(NewStoreBuilder().PrefetchRecordBatch([]proto.Message{record(1, 1, 2)}).Wait()).NotTo(Succeed())
		Expect(NewStoreBuilder().PrefetchRecordBatch(nil).Wait()).To(Succeed())
		pending := b.PrefetchRecordBatch([]proto.Message{record(1, 1, 2)})
		failure := errors.New("injected range failure")
		probe.readErr = failure
		Expect(pending.Wait()).To(BeIdenticalTo(failure))
		Expect(pending.Wait()).To(BeIdenticalTo(failure))
	})

	It("does not claim local versions and computes type-prefixed Java primary keys", func() {
		mb := baseBuilder()
		mb.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("val_enum"), Field("id")))
		mb.SetStoreRecordVersions(true)
		md, err := mb.Build()
		Expect(err).NotTo(HaveOccurred())
		rc, probe := transaction()
		before := rc.localVersion.Load()
		b := builder(rc, md, specSubspace())
		input := record(1, 1, 2)
		input.ValEnum = demopb.Color(9).Enum()
		copy := proto.Clone(input)
		pending := b.PrefetchRecordBatch([]proto.Message{input})
		Expect(rc.localVersion.Load()).To(Equal(before))
		Expect(proto.Equal(input, copy)).To(BeTrue())
		Expect(pending.Wait()).To(Succeed())
		s, err := b.CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		saved, err := s.SaveRecordBatch([]proto.Message{input})
		Expect(err).NotTo(HaveOccurred())
		Expect(saved).To(HaveLen(1))
		begin, end := recordRangeBounds(s.recordsSubspace.Pack(saved[0].PrimaryKey))
		Expect(probe.ranges[0]).To(Equal(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}))
		Expect(proto.Equal(input, copy)).To(BeTrue())
	})

	It("honors canceled contexts, canceled transactions and ended contexts", func() {
		md := metadata(true)
		rc, probe := transaction()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		rc.ctx = canceled
		pending := builder(rc, md, specSubspace()).PrefetchRecordBatch([]proto.Message{record(1, 1, 2)})
		Expect(pending.Wait()).To(MatchError(context.Canceled))
		Expect(probe.ranges).To(BeEmpty())

		rc, _ = transaction()
		canceled, cancel = context.WithCancel(ctx)
		rc.ctx = canceled
		pending = builder(rc, md, specSubspace()).PrefetchRecordBatch([]proto.Message{record(1, 1, 2)})
		cancel()
		Expect(pending.Wait()).To(MatchError(context.Canceled))

		rc, _ = transaction()
		pending = builder(rc, md, specSubspace()).PrefetchRecordBatch([]proto.Message{record(1, 1, 2)})
		rc.Transaction().Cancel()
		var canceledFDB fdb.Error
		Expect(errors.As(pending.Wait(), &canceledFDB)).To(BeTrue())
		Expect(canceledFDB.Code).To(Equal(1025))

		rc, probe = transaction()
		Expect(rc.Commit()).To(Succeed())
		pending = builder(rc, md, specSubspace()).PrefetchRecordBatch([]proto.Message{record(1, 1, 2)})
		var inactive *RecordContextNotActiveError
		Expect(errors.As(pending.Wait(), &inactive)).To(BeTrue())
		Expect(probe.ranges).To(BeEmpty())
	})
})
