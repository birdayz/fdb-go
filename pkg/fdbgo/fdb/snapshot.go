package fdb

import "bytes"

type snapshot struct {
	tx *transaction
}

// Snapshot is a handle to a FoundationDB transaction snapshot.
// Snapshot reads do not add read conflict ranges.
type Snapshot struct {
	s *snapshot
}

func (sn Snapshot) Get(key KeyConvertible) FutureByteSlice {
	inner, ctx := sn.s.tx.inner, sn.s.tx.ctx
	k := bytes.Clone(key.FDBKey())
	return newFutureByteSlice(func() ([]byte, error) {
		v, err := inner.Snapshot().Get(ctx, k)
		return v, convertError(err)
	})
}

func (sn Snapshot) GetKey(sel Selectable) FutureKey {
	inner, ctx := sn.s.tx.inner, sn.s.tx.ctx
	ks := sel.FDBKeySelector()
	key := bytes.Clone(ks.Key.FDBKey())
	// OrEqual values match the wire convention. Pass directly.
	return newFutureKey(func() (Key, error) {
		k, err := inner.Snapshot().GetKey(ctx, key, ks.OrEqual, int32(ks.Offset))
		return Key(k), convertError(err)
	})
}

func (sn Snapshot) GetRange(r Range, options RangeOptions) RangeResult {
	return newSnapshotRangeResult(sn.s.tx, r, options)
}

func (sn Snapshot) GetReadVersion() FutureInt64 {
	inner, ctx := sn.s.tx.inner, sn.s.tx.ctx
	return newFutureInt64(func() (int64, error) {
		v, err := inner.Snapshot().GetReadVersion(ctx)
		return v, convertError(err)
	})
}

func (sn Snapshot) GetDatabase() Database {
	return sn.s.tx.db
}

func (sn Snapshot) Snapshot() ReadTransaction {
	return sn
}

func (sn Snapshot) GetEstimatedRangeSizeBytes(r ExactRange) FutureInt64 {
	begin, end := cloneRangeKeys(r)
	return newFutureInt64(func() (int64, error) {
		v, err := sn.s.tx.inner.GetEstimatedRangeSizeBytes(sn.s.tx.ctx, begin, end)
		return v, convertError(err)
	})
}

func (sn Snapshot) GetRangeSplitPoints(r ExactRange, chunkSize int64) FutureKeyArray {
	begin, end := cloneRangeKeys(r)
	return newFutureKeyArray(func() ([]Key, error) {
		points, err := sn.s.tx.inner.GetRangeSplitPoints(sn.s.tx.ctx, begin, end, chunkSize)
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
	return goTransactionOptions{tx: sn.s.tx}
}

func (sn Snapshot) Cancel() {
	sn.s.tx.inner.Cancel()
}

func (sn Snapshot) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return f(sn)
}
