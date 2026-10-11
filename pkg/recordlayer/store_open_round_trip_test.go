package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// roundTripTx instruments the two reads loadRecordStoreState issues: the
// store-header Get and the snapshot index-state range read. GetRange issues its
// first batch when called, so its return marks the read as on the wire. The
// header future does not complete until then (or a timeout passes), so a
// sequential implementation shows up as rangeBeforeHeader=false instead of
// hanging.
type roundTripTx struct {
	fdb.WritableTransaction
	headerKey     []byte
	isBegin       []byte
	headerErr     error
	rangeIssued   chan struct{}
	rangeCalls    atomic.Int32
	rangeConsumed atomic.Int32
	// Set once the header future resolves: whether the range read was issued first.
	rangeBeforeHeader atomic.Bool
	headerResolved    atomic.Bool
	// Options and isolation of the index-state read, pinned to what the
	// sequential implementation used.
	rangeSnapshot bool
	rangeOptions  fdb.RangeOptions
	rangeEnd      []byte
	headerGets    atomic.Int32
}

func newRoundTripTx(inner fdb.WritableTransaction, ss subspace.Subspace) *roundTripTx {
	b, _ := ss.Sub(IndexStateSpaceKey).FDBRangeKeys()
	return &roundTripTx{
		WritableTransaction: inner,
		headerKey:           ss.Pack(tuple.Tuple{StoreInfoKey}),
		isBegin:             b.FDBKey(),
		rangeIssued:         make(chan struct{}),
	}
}

func (t *roundTripTx) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	f := t.WritableTransaction.Get(key)
	if !bytes.Equal(key.FDBKey(), t.headerKey) {
		return f
	}
	t.headerGets.Add(1)
	return &gatedHeaderFuture{FutureByteSlice: f, tx: t}
}

func (t *roundTripTx) Snapshot() fdb.ReadTransaction {
	return roundTripSnapshot{ReadTransaction: t.WritableTransaction.Snapshot(), tx: t}
}

type roundTripSnapshot struct {
	fdb.ReadTransaction
	tx *roundTripTx
}

func (s roundTripSnapshot) GetRange(r fdb.Range, opts fdb.RangeOptions) fdb.RangeResult {
	rr := s.ReadTransaction.GetRange(r, opts)
	begin, end := r.(fdb.ExactRange).FDBRangeKeys()
	if !bytes.Equal(begin.FDBKey(), s.tx.isBegin) {
		return rr
	}
	s.tx.rangeSnapshot = true
	s.tx.rangeOptions = opts
	s.tx.rangeEnd = end.FDBKey()
	if s.tx.rangeCalls.Add(1) == 1 {
		close(s.tx.rangeIssued)
	}
	return &countingRangeResult{RangeResult: rr, tx: s.tx}
}

type countingRangeResult struct {
	fdb.RangeResult
	tx *roundTripTx
}

func (r *countingRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	r.tx.rangeConsumed.Add(1)
	return r.RangeResult.GetSliceWithError()
}

type gatedHeaderFuture struct {
	fdb.FutureByteSlice
	tx *roundTripTx
}

func (f *gatedHeaderFuture) Get() ([]byte, error) {
	select {
	case <-f.tx.rangeIssued:
		f.tx.rangeBeforeHeader.Store(true)
	case <-time.After(2 * time.Second):
	}
	f.tx.headerResolved.Store(true)
	v, err := f.FutureByteSlice.Get()
	if f.tx.headerErr != nil {
		return nil, f.tx.headerErr
	}
	return v, err
}

func (f *gatedHeaderFuture) MustGet() []byte {
	v, err := f.Get()
	if err != nil {
		panic(err)
	}
	return v
}

var _ = Describe("StoreOpen round trips", func() {
	ctx := context.Background()

	metaBuilder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	metaBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	metaBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := metaBuilder.Build()

	// open runs Open (ErrorIfNotExists) or CreateOrOpen (None) on an
	// instrumented transaction, through the pass-through cache so every call
	// reaches loadRecordStoreState.
	open := func(ss subspace.Subspace, check StoreExistenceCheck, headerErr error) (*roundTripTx, fdb.Transaction, error) {
		inner, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(inner.Cancel)
		rt := newRoundTripTx(inner, ss)
		rt.headerErr = headerErr
		rtx := NewFDBRecordContext(rt, nil)
		b := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).
			SetStoreStateCache(PassThroughStoreStateCache())
		if check == ExistenceCheckErrorIfNotExists {
			_, err = b.Open()
		} else {
			_, err = b.CreateOrOpen()
		}
		return rt, inner, err
	}

	// On an early return the issued index-state read is left to the
	// transaction, whose commit waits on every issued read: it must still
	// commit cleanly.
	expectIssuedReadDrainedByCommit := func(rt *roundTripTx, inner fdb.Transaction) {
		Expect(rt.rangeCalls.Load()).To(Equal(int32(1)), "index-state range read issued once")
		Expect(rt.rangeConsumed.Load()).To(BeZero(), "early return must not consume the read")
		Expect(inner.Commit().Get()).To(Succeed())
	}

	It("issues the index-state read before the header read completes (one wave)", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).CreateOrOpen()
		})
		Expect(err).NotTo(HaveOccurred())

		rt, _, err := open(ss, ExistenceCheckErrorIfNotExists, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.headerGets.Load()).To(BeNumerically(">=", 1))
		Expect(rt.rangeBeforeHeader.Load()).To(BeTrue(),
			"index-state read waited for the header read: two sequential round trips instead of one")
		Expect(rt.rangeCalls.Load()).To(Equal(int32(1)))
		Expect(rt.rangeConsumed.Load()).To(Equal(int32(1)))
		// Same read as the sequential code: snapshot isolation, default options
		// (no limit, default streaming mode), the index-state subspace range.
		_, wantEnd := ss.Sub(IndexStateSpaceKey).FDBRangeKeys()
		Expect(rt.rangeSnapshot).To(BeTrue())
		Expect(rt.rangeOptions).To(Equal(fdb.RangeOptions{}))
		Expect(rt.rangeEnd).To(Equal([]byte(wantEnd.FDBKey())))
	})

	It("returns the header error with the index-state read issued", func() {
		ss := specSubspace()
		injected := fdb.Error{Code: 1009}
		rt, inner, err := open(ss, ExistenceCheckNone, injected)
		Expect(err).To(HaveOccurred())
		var fe fdb.Error
		Expect(errors.As(err, &fe)).To(BeTrue())
		Expect(fe.Code).To(Equal(1009), "header error must win")
		expectIssuedReadDrainedByCommit(rt, inner)
	})

	It("leaves the issued index-state read to commit when the store does not exist", func() {
		ss := specSubspace()
		rt, inner, err := open(ss, ExistenceCheckErrorIfNotExists, nil)
		var dne *RecordStoreDoesNotExistError
		Expect(errors.As(err, &dne)).To(BeTrue(), "got %v", err)
		expectIssuedReadDrainedByCommit(rt, inner)
	})
})
