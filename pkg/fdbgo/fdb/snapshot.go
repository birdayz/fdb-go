package fdb

import (
	"fdb.dev/pkg/fdbgo/client"
)

// Snapshot is a handle to a FoundationDB transaction snapshot.
// Snapshot reads do not add read conflict ranges.
type Snapshot struct {
	tx *transaction
}

func (sn Snapshot) Get(key KeyConvertible) FutureByteSlice {
	inner, ctx := sn.tx.inner, sn.tx.ctx
	// Issued in the call, like fdb_transaction_get (ThreadSafeTransaction.cpp:426-434):
	// a later write is not visible to it.
	return startedFutureByteSlice(inner.Snapshot().StartGet(ctx, key.FDBKey()))
}

func (sn Snapshot) GetKey(sel Selectable) FutureKey {
	inner, ctx := sn.tx.inner, sn.tx.ctx
	ks := sel.FDBKeySelector()
	key := ks.Key.FDBKey() // StartGetKey takes its own copy
	// OrEqual values match the wire convention. Pass directly. Issued in the call,
	// like fdb_transaction_get_key: a later write is not visible to it.
	p := inner.Snapshot().StartGetKey(ctx, client.KeySelector{Key: key, OrEqual: ks.OrEqual, Offset: int32(ks.Offset)})
	return newFutureKey(func() (Key, error) {
		k, err := p.Get()
		return Key(k), convertError(err)
	})
}

func (sn Snapshot) GetRange(r Range, options RangeOptions) RangeResult {
	return newSnapshotRangeResult(sn.tx, r, options)
}

func (sn Snapshot) GetReadVersion() FutureInt64 {
	inner, ctx := sn.tx.inner, sn.tx.ctx
	return newFutureInt64(func() (int64, error) {
		v, err := inner.Snapshot().GetReadVersion(ctx)
		return v, convertError(err)
	})
}

func (sn Snapshot) GetDatabase() Database {
	return sn.tx.db
}

func (sn Snapshot) Snapshot() ReadTransaction {
	return sn
}

func (sn Snapshot) GetEstimatedRangeSizeBytes(r ExactRange) FutureInt64 {
	begin, end := cloneRangeKeys(r)
	return newFutureInt64(func() (int64, error) {
		v, err := sn.tx.inner.GetEstimatedRangeSizeBytes(sn.tx.ctx, begin, end)
		return v, convertError(err)
	})
}

func (sn Snapshot) GetRangeSplitPoints(r ExactRange, chunkSize int64) FutureKeyArray {
	begin, end := cloneRangeKeys(r)
	return newFutureKeyArray(func() ([]Key, error) {
		points, err := sn.tx.inner.GetRangeSplitPoints(sn.tx.ctx, begin, end, chunkSize)
		if err != nil {
			return nil, convertError(err)
		}
		keys := make([]Key, len(points))
		for i, p := range points {
			keys[i] = Key(p)
		}
		return keys, nil
	})
}

func (sn Snapshot) Options() TransactionOptions {
	return goTransactionOptions{tx: sn.tx}
}

func (sn Snapshot) Cancel() {
	sn.tx.inner.Cancel()
}

func (sn Snapshot) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return f(sn)
}
