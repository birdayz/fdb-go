package fdb

import (
	"context"
	"errors"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// transaction wraps client.Transaction with the Apple-compatible API.
type transaction struct {
	inner *client.Transaction
	db    Database
	ctx   context.Context
}

// Transaction is a handle to a FoundationDB transaction.
// Transaction is safe for concurrent use by multiple goroutines.
//
// In the Apple binding, Transaction is a concrete struct with value
// receiver methods. We match this by making Transaction a struct that
// wraps a pointer to the internal state.
type Transaction struct {
	t *transaction
}

// Get returns the value associated with the specified key, or nil if the
// key does not exist. The read is performed asynchronously.
func (tr Transaction) Get(key KeyConvertible) FutureByteSlice {
	return newFutureByteSlice(func() ([]byte, error) {
		return tr.t.inner.Get(tr.t.ctx, key.FDBKey())
	})
}

// GetKey returns the key referenced by the given key selector.
func (tr Transaction) GetKey(sel Selectable) FutureKey {
	ks := sel.FDBKeySelector()
	return newFutureKey(func() (Key, error) {
		k, err := tr.t.inner.GetKey(tr.t.ctx, ks.Key.FDBKey(), ks.OrEqual, int32(ks.Offset))
		return Key(k), err
	})
}

// GetRange performs a range read. The range is specified by a Range and
// options. Returns a RangeResult that can be iterated or fetched as a slice.
func (tr Transaction) GetRange(r Range, options RangeOptions) RangeResult {
	return newRangeResult(tr.t, r, options)
}

// GetReadVersion returns the read version used by this transaction.
func (tr Transaction) GetReadVersion() FutureInt64 {
	return newFutureInt64(func() (int64, error) {
		return tr.t.inner.GetReadVersion(tr.t.ctx)
	})
}

// GetDatabase returns the Database this transaction is operating on.
func (tr Transaction) GetDatabase() Database {
	return tr.t.db
}

// GetCommittedVersion returns the version at which this transaction
// committed. Must be called after a successful Commit.
func (tr Transaction) GetCommittedVersion() (int64, error) {
	v, err := tr.t.inner.GetCommittedVersion()
	if err != nil {
		return 0, convertError(err)
	}
	return v, nil
}

// GetVersionstamp returns the versionstamp which was used by any
// versionstamp operations in this transaction. The future is ready
// after a successful Commit.
func (tr Transaction) GetVersionstamp() FutureKey {
	return newFutureKey(func() (Key, error) {
		vs, err := tr.t.inner.GetVersionstamp()
		if err != nil {
			return nil, convertError(err)
		}
		return Key(vs), nil
	})
}

// GetApproximateSize returns the approximate transaction size so far.
func (tr Transaction) GetApproximateSize() FutureInt64 {
	return newReadyFutureInt64(tr.t.inner.GetApproximateSize(), nil)
}

// GetEstimatedRangeSizeBytes returns an estimate of the byte size of
// the key range. Not yet implemented in the pure Go client.
func (tr Transaction) GetEstimatedRangeSizeBytes(_ ExactRange) FutureInt64 {
	return newReadyFutureInt64(0, Error{Code: 2000}) // operation_failed
}

// GetRangeSplitPoints suggests split points for the given key range.
// Not yet implemented in the pure Go client.
func (tr Transaction) GetRangeSplitPoints(_ ExactRange, _ int64) FutureKeyArray {
	return newReadyFutureKeyArray(nil, Error{Code: 2000}) // operation_failed
}

// Snapshot returns a Snapshot view of this transaction.
func (tr Transaction) Snapshot() Snapshot {
	return Snapshot{s: &snapshot{tx: tr.t}}
}

// Set sets a key-value pair in the database.
func (tr Transaction) Set(key KeyConvertible, value []byte) {
	tr.t.inner.Set(key.FDBKey(), value)
}

// Clear removes a key from the database.
func (tr Transaction) Clear(key KeyConvertible) {
	tr.t.inner.Clear(key.FDBKey())
}

// ClearRange removes all keys k such that begin <= k < end.
func (tr Transaction) ClearRange(er ExactRange) {
	begin, end := er.FDBRangeKeys()
	// ClearRange in client returns error for inverted range; Apple API ignores it.
	_ = tr.t.inner.ClearRange(begin.FDBKey(), end.FDBKey())
}

// SetVersionstampedKey sets a key with an embedded incomplete versionstamp.
func (tr Transaction) SetVersionstampedKey(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutSetVersionstampedKey, key.FDBKey(), param)
}

// SetVersionstampedValue sets a value with an embedded incomplete versionstamp.
func (tr Transaction) SetVersionstampedValue(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutSetVersionstampedValue, key.FDBKey(), param)
}

// Atomic mutation methods — named to match Apple's API.

func (tr Transaction) Add(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutAddValue, key.FDBKey(), param)
}

func (tr Transaction) And(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutAnd, key.FDBKey(), param)
}

func (tr Transaction) BitAnd(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutAnd, key.FDBKey(), param)
}

func (tr Transaction) Or(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutOr, key.FDBKey(), param)
}

func (tr Transaction) BitOr(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutOr, key.FDBKey(), param)
}

func (tr Transaction) Xor(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutXor, key.FDBKey(), param)
}

func (tr Transaction) BitXor(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutXor, key.FDBKey(), param)
}

func (tr Transaction) Max(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutMax, key.FDBKey(), param)
}

func (tr Transaction) Min(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutMin, key.FDBKey(), param)
}

func (tr Transaction) ByteMax(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutByteMax, key.FDBKey(), param)
}

func (tr Transaction) ByteMin(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutByteMin, key.FDBKey(), param)
}

func (tr Transaction) AppendIfFits(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutAppendIfFits, key.FDBKey(), param)
}

func (tr Transaction) CompareAndClear(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutCompareAndClear, key.FDBKey(), param)
}

// Commit commits the transaction. The returned FutureNil becomes ready
// when the commit has been acknowledged by the cluster.
func (tr Transaction) Commit() FutureNil {
	return newFutureNil(func() error {
		return convertError(tr.t.inner.Commit(tr.t.ctx))
	})
}

// Cancel cancels the transaction.
func (tr Transaction) Cancel() {
	tr.t.inner.Cancel()
}

// OnError determines whether an error is retryable.
func (tr Transaction) OnError(e Error) FutureNil {
	err := tr.t.inner.OnError(&wire.FDBError{Code: e.Code})
	if err != nil {
		return newReadyFutureNil(convertError(err))
	}
	return newReadyFutureNil(nil)
}

// Options returns a TransactionOptions handle.
func (tr Transaction) Options() TransactionOptions {
	return TransactionOptions{tx: tr.t}
}

// SetReadVersion sets the read version for this transaction.
func (tr Transaction) SetReadVersion(version int64) {
	tr.t.inner.SetReadVersion(version)
}

// Reset resets the transaction to its initial state.
func (tr Transaction) Reset() {
	// Create a fresh inner transaction on the same database.
	tr.t.inner = tr.t.db.d.inner.CreateTransaction()
}

// AddReadConflictRange adds a read conflict range.
func (tr Transaction) AddReadConflictRange(er ExactRange) error {
	begin, end := er.FDBRangeKeys()
	return convertError(tr.t.inner.AddReadConflictRange(begin.FDBKey(), end.FDBKey()))
}

// AddReadConflictKey adds a read conflict on a single key.
func (tr Transaction) AddReadConflictKey(key KeyConvertible) error {
	tr.t.inner.AddReadConflictKey(key.FDBKey())
	return nil
}

// AddWriteConflictRange adds a write conflict range.
func (tr Transaction) AddWriteConflictRange(er ExactRange) error {
	begin, end := er.FDBRangeKeys()
	return convertError(tr.t.inner.AddWriteConflictRange(begin.FDBKey(), end.FDBKey()))
}

// AddWriteConflictKey adds a write conflict on a single key.
func (tr Transaction) AddWriteConflictKey(key KeyConvertible) error {
	tr.t.inner.AddWriteConflictKey(key.FDBKey())
	return nil
}

// Watch is not yet implemented.
func (tr Transaction) Watch(_ KeyConvertible) FutureNil {
	return newReadyFutureNil(Error{Code: 2000})
}

// Tenant operations (stubs).

func (tr Transaction) CreateTenant(_ KeyConvertible) error {
	return Error{Code: 2000}
}

func (tr Transaction) DeleteTenant(_ KeyConvertible) error {
	return Error{Code: 2000}
}

func (tr Transaction) ListTenants() ([]Key, error) {
	return nil, Error{Code: 2000}
}

// LocalityGetAddressesForKey is not yet implemented.
func (tr Transaction) LocalityGetAddressesForKey(_ KeyConvertible) FutureStringSlice {
	f := &futureStringSlice{}
	f.init()
	f.err = Error{Code: 2000}
	close(f.done)
	return f
}

// Transact implements Transactor for composability.
func (tr Transaction) Transact(f func(Transaction) (any, error)) (any, error) {
	return f(tr)
}

// ReadTransact implements ReadTransactor for composability.
func (tr Transaction) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	return f(tr)
}

// convertError converts a client error to an fdb.Error if applicable.
func convertError(err error) error {
	if err == nil {
		return nil
	}
	var fdbErr *wire.FDBError
	if errors.As(err, &fdbErr) {
		return Error{Code: fdbErr.Code}
	}
	return err
}
