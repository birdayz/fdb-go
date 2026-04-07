package fdb

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
	return newFutureByteSlice(func() ([]byte, error) {
		v, err := inner.Snapshot().Get(ctx, key.FDBKey())
		return v, convertError(err)
	})
}

func (sn Snapshot) GetKey(sel Selectable) FutureKey {
	inner, ctx := sn.s.tx.inner, sn.s.tx.ctx
	ks := sel.FDBKeySelector()
	return newFutureKey(func() (Key, error) {
		k, err := inner.Snapshot().GetKey(ctx, ks.Key.FDBKey(), ks.OrEqual, int32(ks.Offset))
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

func (sn Snapshot) Snapshot() Snapshot {
	return sn
}

func (sn Snapshot) GetEstimatedRangeSizeBytes(r ExactRange) FutureInt64 {
	return newFutureInt64(func() (int64, error) {
		begin, end := r.FDBRangeKeys()
		v, err := sn.s.tx.inner.GetEstimatedRangeSizeBytes(sn.s.tx.ctx, begin.FDBKey(), end.FDBKey())
		return v, convertError(err)
	})
}

func (sn Snapshot) GetRangeSplitPoints(_ ExactRange, _ int64) FutureKeyArray {
	return newReadyFutureKeyArray(nil, errNotSupported)
}

func (sn Snapshot) Options() TransactionOptions {
	return TransactionOptions{tx: sn.s.tx}
}

func (sn Snapshot) Cancel() {
	sn.s.tx.inner.Cancel()
}

func (sn Snapshot) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return f(sn)
}
