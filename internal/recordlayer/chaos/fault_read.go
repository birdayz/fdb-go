package chaos

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/fdb"
)

// readErrorTransaction is the transaction FaultReadError hands to the caller's
// function: reads of keys under prefix fail with injectedReadError, everything else
// — including all writes — passes straight through to the real transaction.
//
// The embedded interface carries the rest of the surface, so the wrapper stays a
// statement about reads and cannot silently drift as fdb.WritableTransaction grows.
type readErrorTransaction struct {
	fdb.WritableTransaction
	prefix []byte
}

// failing reports whether a read of key is in the injected fault's scope. A nil
// prefix scopes the fault to every read.
func (t *readErrorTransaction) failing(key []byte) bool {
	return t.prefix == nil || bytes.HasPrefix(key, t.prefix)
}

func (t *readErrorTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	if t.failing(key.FDBKey()) {
		return failedByteSlice{}
	}
	return t.WritableTransaction.Get(key)
}

func (t *readErrorTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	begin, _ := r.FDBRangeKeySelectors()
	if t.failing(begin.FDBKeySelector().Key.FDBKey()) {
		return failedRangeResult{}
	}
	return t.WritableTransaction.GetRange(r, options)
}

func (t *readErrorTransaction) Snapshot() fdb.ReadTransaction {
	return &readErrorReadTransaction{
		ReadTransaction: t.WritableTransaction.Snapshot(),
		prefix:          t.prefix,
	}
}

// readErrorReadTransaction is the same wrapper over a read-only view (the snapshot
// a record store uses for its non-conflicting reads).
type readErrorReadTransaction struct {
	fdb.ReadTransaction
	prefix []byte
}

func (t *readErrorReadTransaction) failing(key []byte) bool {
	return t.prefix == nil || bytes.HasPrefix(key, t.prefix)
}

func (t *readErrorReadTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	if t.failing(key.FDBKey()) {
		return failedByteSlice{}
	}
	return t.ReadTransaction.Get(key)
}

func (t *readErrorReadTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	begin, _ := r.FDBRangeKeySelectors()
	if t.failing(begin.FDBKeySelector().Key.FDBKey()) {
		return failedRangeResult{}
	}
	return t.ReadTransaction.GetRange(r, options)
}

func (t *readErrorReadTransaction) Snapshot() fdb.ReadTransaction { return t }

// failedByteSlice is an already-ready FutureByteSlice carrying the injected error.
type failedByteSlice struct{}

func (failedByteSlice) Get() ([]byte, error) { return nil, injectedReadError }
func (failedByteSlice) MustGet() []byte      { panic(injectedReadError) }
func (failedByteSlice) BlockUntilReady()     {}
func (failedByteSlice) IsReady() bool        { return true }
func (failedByteSlice) Cancel()              {}

// failedRangeResult is the range equivalent: every way of consuming it reports the
// injected error rather than an empty range, which is the whole point — an empty
// range and a failed read are what callers must not confuse.
type failedRangeResult struct{}

func (failedRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	return nil, injectedReadError
}

func (failedRangeResult) GetSliceOrPanic() []fdb.KeyValue { panic(injectedReadError) }

func (failedRangeResult) Iterator() fdb.RangeIterator { return failedRangeIterator{} }

type failedRangeIterator struct{}

func (failedRangeIterator) Advance() bool              { return true }
func (failedRangeIterator) Get() (fdb.KeyValue, error) { return fdb.KeyValue{}, injectedReadError }
func (failedRangeIterator) MustGet() fdb.KeyValue      { panic(injectedReadError) }
func (failedRangeIterator) SetTraceLog(func(iteration, requested, returned int, more bool, err error)) {
}
