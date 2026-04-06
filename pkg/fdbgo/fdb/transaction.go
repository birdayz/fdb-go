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

	// commitDone is closed after Commit completes (success or failure).
	// GetVersionstamp() blocks on this channel to match Apple binding
	// semantics where the future resolves after commit.
	commitDone chan struct{}
	commitErr  error
}

// Transaction is a handle to a FoundationDB transaction.
// Individual methods (Get, Set, Commit, etc.) are safe for concurrent use
// by multiple goroutines. However, Reset() is NOT concurrent-safe — callers
// must drain all pending futures before calling Reset, as in-flight
// goroutines hold references to the internal transaction handle.
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
	inner, ctx := tr.t.inner, tr.t.ctx
	return newFutureByteSlice(func() ([]byte, error) {
		v, err := inner.Get(ctx, key.FDBKey())
		return v, convertError(err)
	})
}

// GetKey returns the key referenced by the given key selector.
func (tr Transaction) GetKey(sel Selectable) FutureKey {
	inner, ctx := tr.t.inner, tr.t.ctx
	ks := sel.FDBKeySelector()
	return newFutureKey(func() (Key, error) {
		k, err := inner.GetKey(ctx, ks.Key.FDBKey(), ks.OrEqual, int32(ks.Offset))
		return Key(k), convertError(err)
	})
}

// GetRange performs a range read. The range is specified by a Range and
// options. Returns a RangeResult that can be iterated or fetched as a slice.
func (tr Transaction) GetRange(r Range, options RangeOptions) RangeResult {
	return newRangeResult(tr.t, r, options)
}

// GetReadVersion returns the read version used by this transaction.
func (tr Transaction) GetReadVersion() FutureInt64 {
	inner, ctx := tr.t.inner, tr.t.ctx
	return newFutureInt64(func() (int64, error) {
		v, err := inner.GetReadVersion(ctx)
		return v, convertError(err)
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
// versionstamp operations in this transaction.
//
// Can be called before or after Commit(). If called before, the returned
// future blocks until commit completes — matching the Apple binding's
// deferred versionstamp pattern. Read-only transactions (no mutations)
// return error 2015 (used_during_commit).
func (tr Transaction) GetVersionstamp() FutureKey {
	inner := tr.t.inner
	commitDone := tr.t.commitDone
	return newFutureKey(func() (Key, error) {
		// Block until commit completes (or has already completed).
		<-commitDone
		vs, err := inner.GetVersionstamp()
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
	return newReadyFutureInt64(0, errNotSupported)
}

// GetRangeSplitPoints suggests split points for the given key range.
// Not yet implemented in the pure Go client.
func (tr Transaction) GetRangeSplitPoints(_ ExactRange, _ int64) FutureKeyArray {
	return newReadyFutureKeyArray(nil, errNotSupported)
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
// The Apple binding's ClearRange is void because mutations are buffered
// locally with no I/O. Our pure Go client validates begin <= end and
// returns inverted_range (2005) on violation. We suppress all errors to
// match the Apple void API contract. Today the only possible error is
// inverted_range; if the client adds new error paths, this should be
// revisited (see client.Transaction.ClearRange).
func (tr Transaction) ClearRange(er ExactRange) {
	begin, end := er.FDBRangeKeys()
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
// when the commit has been acknowledged by the cluster. Also unblocks
// any pending GetVersionstamp() futures.
func (tr Transaction) Commit() FutureNil {
	inner, ctx := tr.t.inner, tr.t.ctx
	t := tr.t
	return newFutureNil(func() error {
		err := convertError(inner.Commit(ctx))
		t.commitErr = err
		close(t.commitDone)
		return err
	})
}

// Cancel cancels the transaction.
func (tr Transaction) Cancel() {
	tr.t.inner.Cancel()
}

// OnError determines whether an error is retryable. The returned FutureNil
// includes the retry delay — the delay runs when Get() is called, not when
// OnError() is called. This matches Apple binding semantics.
func (tr Transaction) OnError(e Error) FutureNil {
	inner := tr.t.inner
	return newFutureNil(func() error {
		err := inner.OnError(&wire.FDBError{Code: e.Code})
		return convertError(err)
	})
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
// NOT safe to call concurrently with other Transaction methods.
// Callers must drain all pending futures before calling Reset —
// in-flight goroutines from Get/GetRange/Commit access tr.t.inner
// and will race with Reset. This matches Apple binding semantics
// where Reset must not be called while the transaction is in use.
func (tr Transaction) Reset() {
	old := tr.t.inner
	tr.t.inner = tr.t.db.d.inner.CreateTransaction()
	tr.t.commitDone = make(chan struct{})
	tr.t.commitErr = nil
	old.Cancel()
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
	return newReadyFutureNil(errNotSupported)
}

// Tenant operations (stubs).

func (tr Transaction) CreateTenant(_ KeyConvertible) error {
	return errNotSupported
}

func (tr Transaction) DeleteTenant(_ KeyConvertible) error {
	return errNotSupported
}

func (tr Transaction) ListTenants() ([]Key, error) {
	return nil, errNotSupported
}

// LocalityGetAddressesForKey is not yet implemented.
func (tr Transaction) LocalityGetAddressesForKey(_ KeyConvertible) FutureStringSlice {
	return newReadyFutureStringSlice(nil, errNotSupported)
}

// Transact implements Transactor for composability.
// Catches Error panics from MustGet() and returns them as errors,
// matching the Apple binding's panicToError recovery.
func (tr Transaction) Transact(f func(Transaction) (any, error)) (r any, e error) {
	defer panicToError(&e)
	return f(tr)
}

// ReadTransact implements ReadTransactor for composability.
func (tr Transaction) ReadTransact(f func(ReadTransaction) (any, error)) (r any, e error) {
	defer panicToError(&e)
	return f(tr)
}

// panicToError catches error panics and converts them to returned errors.
// Apple's binding only catches fdb.Error (since the C client only produces
// those), but our pure Go client can surface arbitrary errors (network,
// context, etc.) via MustGet(), so we catch the full error interface.
func panicToError(e *error) {
	if r := recover(); r != nil {
		if err, ok := r.(error); ok {
			*e = err
		} else {
			panic(r) // re-panic non-error panics
		}
	}
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

// unconvertError converts fdb.Error back to *wire.FDBError so the
// client retry loop (OnError) can recognize it via errors.As.
// Without this, retryable errors from Get().Get() inside Transact
// escape the retry loop because OnError sees fdb.Error (not *wire.FDBError).
func unconvertError(err error) error {
	if err == nil {
		return nil
	}
	var fdbErr Error
	if errors.As(err, &fdbErr) {
		return &wire.FDBError{Code: fdbErr.Code}
	}
	return err
}
