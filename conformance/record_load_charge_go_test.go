//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	. "github.com/onsi/gomega"
)

func compareQuiescedGoRecordCharge(ctx context.Context, env *TenantEnvironment, fixture []byte) {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewIndex("price", recordlayer.Field("price")).SetSubspaceKey("price"))
	builder.AddIndex("Order", recordlayer.NewVersionIndex("version", recordlayer.VersionKey()).SetSubspaceKey("version"))
	metadata, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	transaction, err := env.RecordDB.CreateTransaction()
	Expect(err).NotTo(HaveOccurred())
	defer transaction.Cancel()
	Expect(transaction.Options().SetTimeout(4000)).To(Succeed())
	gate := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	defer unblock()
	observer := &recordChargeGoTransaction{WritableTransaction: transaction, ctx: ctx, gate: gate}
	store, err := recordlayer.NewStoreBuilder().SetContext(env.RecordDB.NewRecordContext(observer)).
		SetSubspace(subspace.FromBytes(fixture)).SetMetaDataProvider(metadata).SetFormatVersion(14).CreateOrOpen()
	Expect(err).NotTo(HaveOccurred())
	prefix := store.RecordsSubspace().Pack(tuple.Tuple{int64(0)})
	observer.begin = append(append([]byte(nil), prefix...), 0x00)
	observer.end = append(append([]byte(nil), prefix...), 0xff)
	properties := recordlayer.DefaultExecuteProperties()
	properties.ScannedRecordsLimit = 1
	properties.FailOnScanLimitReached = true
	cursor := store.ScanIndexRecords("version", recordlayer.TupleRangeAll, nil, recordlayer.NewScanProperties(properties))
	defer func() { Expect(cursor.Close()).To(Succeed()) }()
	next, err := cursor.OnNext(ctx)
	var limit *recordlayer.ScanLimitReachedError
	Expect(errors.As(err, &limit)).To(BeTrue(), "expected the production scan-limit error: %v", err)
	Expect(next.HasNext()).To(BeFalse())
	Expect(observer.issued).To(Equal(1))
	Expect(observer.nativeReady).To(BeTrue())
	Expect(properties.ScanState.BytesScanned()).To(Equal(int64(35)))
	unblock()
	// This does not consume the discarded cursor mapping or issue another read.
	properties.ScanState.WaitForIssuedRecordLoadCharges()
	Expect(properties.ScanState.BytesScanned()).To(Equal(int64(89)))
	Expect(observer.issued).To(Equal(1))
}

// Prime the real iterator on the issuing thread to prove native readiness before
// the cursor error, then hold delivery to the raw worker. No rows are fabricated.
type recordChargeGoTransaction struct {
	fdb.WritableTransaction
	ctx         context.Context
	begin, end  []byte
	gate        <-chan struct{}
	issued      int
	nativeReady bool
}

func (t *recordChargeGoTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := t.WritableTransaction.GetRange(r, options)
	begin, end := r.FDBRangeKeySelectors()
	if !bytes.Equal(begin.FDBKeySelector().Key.FDBKey(), t.begin) || !bytes.Equal(end.FDBKeySelector().Key.FDBKey(), t.end) {
		return result
	}
	t.issued++
	iterator := result.Iterator()
	t.nativeReady = iterator.Advance()
	Expect(t.nativeReady).To(BeTrue(), "the Java-seeded primary-key-zero record must exist")
	return &recordChargeGoRange{RangeResult: result, iterator: &recordChargeGoIterator{
		RangeIterator: iterator, ctx: t.ctx, gate: t.gate,
	}}
}

type recordChargeGoRange struct {
	fdb.RangeResult
	iterator fdb.RangeIterator
}

func (r *recordChargeGoRange) Iterator() fdb.RangeIterator { return r.iterator }

type recordChargeGoIterator struct {
	fdb.RangeIterator
	ctx     context.Context
	gate    <-chan struct{}
	started bool
	err     error
}

func (i *recordChargeGoIterator) Advance() bool {
	if i.started {
		return i.err == nil && i.RangeIterator.Advance()
	}
	i.started = true
	select {
	case <-i.gate:
		return true
	case <-i.ctx.Done():
		i.err = i.ctx.Err()
		return false
	}
}

func (i *recordChargeGoIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	return i.RangeIterator.Get()
}
