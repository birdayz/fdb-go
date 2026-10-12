package recordlayer

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// SaveRecordsPipelined starts a window's existing-record reads together and
// then saves in order; every outcome must be the one serial saves produce.
var _ = Describe("SaveRecordsPipelined", func() {
	ctx := context.Background()
	priceIndex := NewIndex("Order$price", Field("price"))
	metaData := func(split bool) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", priceIndex)
		b.SetSplitLongRecords(split)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	inStore := func(md *RecordMetaData, ss subspace.Subspace, fn func(*FDBRecordStore)) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			fn(s)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	order := func(id int64, price int32) *demopb.Order {
		return &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(price)}
	}
	prices := func(s *FDBRecordStore) []int64 {
		entries, err := AsList(ctx, s.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
		Expect(err).NotTo(HaveOccurred())
		out := make([]int64, len(entries))
		for i, e := range entries {
			out[i] = e.IndexValues()[0].(int64)
		}
		return out
	}

	It("saves every record and index entry across windows, split or not", func() {
		for _, split := range []bool{true, false} {
			md, ss := metaData(split), specSubspace().Sub(split)
			inStore(md, ss, func(s *FDBRecordStore) {
				var records []proto.Message
				var want []int64
				for i := range int64(25) {
					records = append(records, order(i, int32(100+i)))
					want = append(want, 100+i)
				}
				saved, err := s.SaveRecordsPipelined(records, RecordExistenceCheckErrorIfExists)
				Expect(err).NotTo(HaveOccurred())
				Expect(saved).To(HaveLen(25))
				for i, rec := range saved {
					Expect(rec.PrimaryKey).To(Equal(tuple.Tuple{int64(i)}))
				}
				Expect(prices(s)).To(Equal(want))
			})
		}
	})

	It("sends a window's reads before waiting on any", func() {
		md, ss := metaData(true), specSubspace()
		inStore(md, ss, func(s *FDBRecordStore) {
			var log []string
			tx := recordingReadTx{ReadTransaction: s.context.Transaction(), log: &log}
			window := []proto.Message{order(1, 10), order(2, 20), order(1, 30)}
			loads := s.startExistingRecordLoads(tx, window)
			// One whole-record range per distinct key; a repeated key reads
			// only at its save, after the preceding save has taken effect.
			Expect(log).To(Equal([]string{"send", "send"}))
			Expect(loads[2].sent).To(BeFalse())
			for i, record := range window {
				_, err := s.saveRecordInternal(record, RecordExistenceCheckNone, false, &loads[i], nil)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(prices(s)).To(Equal([]int64{20, 30}))
		})
	})

	It("keeps SaveRecordBatch from reading a repeated key before its earlier save", func() {
		md, ss := metaData(true), specSubspace()
		inStore(md, ss, func(s *FDBRecordStore) {
			_, err := s.SaveRecordBatch([]proto.Message{order(1, 10), order(2, 15), order(1, 20)})
			Expect(err).NotTo(HaveOccurred())
			Expect(prices(s)).To(Equal([]int64{15, 20}))
		})
	})

	It("reads a key repeated within a window after the earlier save", func() {
		md, ss := metaData(true), specSubspace()
		inStore(md, ss, func(s *FDBRecordStore) {
			// A read started before the first save would see no record: the
			// second save would then keep the first's index entry.
			_, err := s.SaveRecordsPipelined([]proto.Message{order(1, 10), order(2, 15), order(1, 20)}, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
			Expect(prices(s)).To(Equal([]int64{15, 20}))

			_, err = s.SaveRecordsPipelined([]proto.Message{order(3, 30), order(3, 31)}, RecordExistenceCheckErrorIfExists)
			var exists *RecordAlreadyExistsError
			Expect(errors.As(err, &exists)).To(BeTrue(), "error %v", err)
			Expect(exists.PrimaryKey).To(Equal(tuple.Tuple{int64(3)}))
		})
	})

	It("reports the first failing record, as serial saves do", func() {
		md, ss := metaData(true), specSubspace()
		inStore(md, ss, func(s *FDBRecordStore) {
			_, err := s.SaveRecord(order(2, 5))
			Expect(err).NotTo(HaveOccurred())
			// Record 2 exists and the record after it is invalid: the existence
			// error comes first, and the record before it is saved.
			_, err = s.SaveRecordsPipelined([]proto.Message{order(1, 1), order(2, 2), nil}, RecordExistenceCheckErrorIfExists)
			var exists *RecordAlreadyExistsError
			Expect(errors.As(err, &exists)).To(BeTrue(), "error %v", err)
			Expect(exists.PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
			Expect(prices(s)).To(Equal([]int64{1, 5}))
		})
	})

	It("finds and replaces a split record", func() {
		md, ss := metaData(true), specSubspace()
		long := strings.Repeat("x", 3*splitRecordSize)
		inStore(md, ss, func(s *FDBRecordStore) {
			_, err := s.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String(long)})
			Expect(err).NotTo(HaveOccurred())
		})
		inStore(md, ss, func(s *FDBRecordStore) {
			_, err := s.SaveRecordsPipelined([]proto.Message{&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String("short")}}, RecordExistenceCheckErrorIfExists)
			var exists *RecordAlreadyExistsError
			Expect(errors.As(err, &exists)).To(BeTrue(), "error %v", err)
		})
		inStore(md, ss, func(s *FDBRecordStore) {
			saved, err := s.SaveRecordsPipelined([]proto.Message{&demopb.TypedRecord{Id: proto.Int64(1), ValString: proto.String("short")}}, RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
			Expect(saved[0].Split).To(BeFalse())
			// The replaced record's chunks are cleared: only the unsplit key is left.
			r, err := fdb.PrefixRange(s.recordsSubspace.Pack(tuple.Tuple{int64(1)}))
			Expect(err).NotTo(HaveOccurred())
			kvs, err := s.context.Transaction().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(kvs).To(HaveLen(1))
			Expect(kvs[0].Key).To(Equal(s.recordsSubspace.Pack(tuple.Tuple{int64(1), unsplitRecord})))
		})
	})
})

// recordingReadTx logs each read it sends and each wait on one.
type recordingReadTx struct {
	fdb.ReadTransaction
	log *[]string
}

type recordingFuture struct {
	fdb.FutureByteSlice
	log *[]string
}

func (tx recordingReadTx) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	*tx.log = append(*tx.log, "send")
	return recordingFuture{FutureByteSlice: tx.ReadTransaction.Get(key), log: tx.log}
}

func (f recordingFuture) Get() ([]byte, error) {
	*f.log = append(*f.log, "wait")
	return f.FutureByteSlice.Get()
}

func (tx recordingReadTx) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	*tx.log = append(*tx.log, "send")
	return recordingRange{RangeResult: tx.ReadTransaction.GetRange(r, options), log: tx.log}
}

type recordingRange struct {
	fdb.RangeResult
	log *[]string
}

func (r recordingRange) Iterator() fdb.RangeIterator {
	return &recordingRangeIterator{RangeIterator: r.RangeResult.Iterator(), log: r.log}
}

type recordingRangeIterator struct {
	fdb.RangeIterator
	log    *[]string
	waited bool
}

func (it *recordingRangeIterator) Advance() bool {
	if !it.waited {
		*it.log = append(*it.log, "wait")
		it.waited = true
	}
	return it.RangeIterator.Advance()
}

// orderedReadTx records when each Get is issued and when its value is taken.
type orderedReadTx struct {
	fdb.ReadTransaction
	events *[]string
	names  map[string]string
}

type orderedFuture struct {
	fdb.FutureByteSlice
	name   string
	events *[]string
}

func (tx orderedReadTx) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	name := tx.names[string(key.FDBKey())]
	*tx.events = append(*tx.events, "send "+name)
	return orderedFuture{name: name, events: tx.events}
}

func (f orderedFuture) Get() ([]byte, error) {
	*f.events = append(*f.events, "wait "+f.name)
	return nil, nil
}

type orderedRange struct {
	fdb.RangeResult
	events *[]string
}

func (tx orderedReadTx) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	ss := subspace.FromBytes([]byte("records")).Sub(int64(7))
	begin, end := ss.FDBRangeKeys()
	gotBegin, gotEnd := r.(fdb.ExactRange).FDBRangeKeys()
	Expect(gotBegin.FDBKey()).To(Equal(begin.FDBKey()))
	Expect(gotEnd.FDBKey()).To(Equal(end.FDBKey()))
	Expect(options).To(Equal(fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
	*tx.events = append(*tx.events, "send range")
	return orderedRange{events: tx.events}
}

func (r orderedRange) Iterator() fdb.RangeIterator {
	return &orderedRangeIterator{events: r.events}
}

type orderedRangeIterator struct {
	fdb.RangeIterator
	events *[]string
	waited bool
}

func (it *orderedRangeIterator) Advance() bool {
	if !it.waited {
		*it.events = append(*it.events, "wait range")
		it.waited = true
	}
	return false
}

func (it *orderedRangeIterator) Get() (fdb.KeyValue, error) {
	return fdb.KeyValue{}, nil
}

var _ = Describe("startLoadWithSplit", func() {
	ss := subspace.FromBytes([]byte("records"))
	pk := tuple.Tuple{int64(7)}

	It("issues one unlimited record range before waiting, whether splitting is enabled or not", func() {
		for _, split := range []bool{false, true} {
			var events []string
			p := startLoadWithSplit(orderedReadTx{events: &events}, ss, pk, split, false, nil)
			Expect(events).To(Equal([]string{"send range"}))
			value, version, err := p.finish(&sizeInfo{})
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(BeNil())
			Expect(version).To(BeNil())
			Expect(events).To(Equal([]string{"send range", "wait range"}))
		}
	})

	It("keeps legacy bare-key loads as a single point read", func() {
		var events []string
		tx := orderedReadTx{events: &events, names: map[string]string{string(ss.Pack(pk)): "legacy"}}
		p := startLoadWithSplit(tx, ss, pk, false, true, nil)
		Expect(events).To(Equal([]string{"send legacy"}))
		_, _, err := p.finish(&sizeInfo{})
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(Equal([]string{"send legacy", "wait legacy"}))
	})
})
