package recordlayer

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
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
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
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
	order := func(id int64, price int32) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(price)}
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

	It("saves every record and index entry across windows", func() {
		md, ss := metaData(true), specSubspace()
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
			_, err := s.SaveRecord(&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String(long)})
			Expect(err).NotTo(HaveOccurred())
		})
		inStore(md, ss, func(s *FDBRecordStore) {
			_, err := s.SaveRecordsPipelined([]proto.Message{&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String("short")}}, RecordExistenceCheckErrorIfExists)
			var exists *RecordAlreadyExistsError
			Expect(errors.As(err, &exists)).To(BeTrue(), "error %v", err)
		})
		inStore(md, ss, func(s *FDBRecordStore) {
			saved, err := s.SaveRecordsPipelined([]proto.Message{&gen.TypedRecord{Id: proto.Int64(1), ValString: proto.String("short")}}, RecordExistenceCheckNone)
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

var _ = Describe("startLoadWithSplit", func() {
	ss := subspace.FromBytes([]byte("records"))
	pk := tuple.Tuple{int64(7)}
	load := func(probeSplit bool) []string {
		var events []string
		tx := orderedReadTx{events: &events, names: map[string]string{
			string(ss.Pack(tuple.Tuple{int64(7), unsplitRecord})):    "unsplit",
			string(ss.Pack(tuple.Tuple{int64(7), startSplitRecord})): "first chunk",
		}}
		p := startLoadWithSplit(tx, ss, pk, true, false, probeSplit)
		value, err := p.finish(&sizeInfo{})
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(BeNil())
		return events
	}

	It("sends a missing record's chunk read with its unsplit read when probing", func() {
		Expect(load(true)).To(Equal([]string{"send unsplit", "send first chunk", "wait unsplit", "wait first chunk"}))
	})

	It("keeps loadWithSplit's one read at a time otherwise", func() {
		Expect(load(false)).To(Equal([]string{"send unsplit", "wait unsplit", "send first chunk", "wait first chunk"}))
	})
})
