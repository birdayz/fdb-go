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
//
// Data operations — Get, GetRange, GetKey, Set, Clear, ClearRange, Atomic,
// Commit, GetApproximateSize, and the conflict-range adders — are safe for
// concurrent use by multiple goroutines (the shared mutation/conflict buffers
// are guarded internally). Read-your-writes ordering across concurrent
// read-modify-write on the SAME key is still racy (lost update), matching the
// C binding; serialize such sequences yourself.
//
// Option setters (the Set* configuration methods, e.g. SetPriority,
// SetWriteConflictsDisabled, SetTag, SetReadVersion) and Reset() are NOT
// concurrent-safe with operations or each other — configure the transaction
// BEFORE issuing concurrent operations, and drain all pending futures before
// calling Reset (in-flight goroutines hold references to the internal handle).
// This mirrors the FDB C API, where fdb_transaction_set_option is not meant to
// race the transaction's data calls.
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
	// Try pipelined path first: send the request synchronously (no goroutine),
	// return a future backed by the reply channel. This enables true pipelining —
	// N Gets send N frames immediately, then N future.Get() calls collect responses.
	val, pending, err := inner.GetPipelined(ctx, key.FDBKey())
	if err != nil {
		// GetPipelined failed before any request was in flight — either
		// ErrNeedFullRYW (the key has pending atomics needing a server read +
		// merge) or a layer-2 locate/send failure (e.g. every located connection
		// dropped between getOrDial and SendFrameDeferred, surfaced as
		// all_alternatives_failed / 1006). Re-drive through the full
		// ryw.get()/getValue path, which does the RYW merge AND the local
		// wrong-shard/all-alternatives retry loop.
		//
		// This MUST fall back for all of them, not just ErrNeedFullRYW: 1006 is
		// retried locally by getValue, but Transact.OnError does NOT retry 1006
		// (the read path is expected to absorb it), so surfacing it here would
		// turn a transient send failure into a failed transaction (RFC-010 #3,
		// regression caught by codex review). A genuinely terminal error (e.g.
		// key_outside_legal_range) re-fails identically in inner.Get — nothing is
		// masked, and the illegal frame was already rejected before send.
		return newFutureByteSlice(func() ([]byte, error) {
			v, gerr := inner.Get(ctx, key.FDBKey())
			return v, convertError(gerr)
		})
	}
	if pending != nil {
		// Server request in flight — future resolves when response arrives.
		return newPendingFutureByteSlice(pending)
	}
	// RYW cache hit or cleared key.
	return newReadyFutureByteSlice(val, nil)
}

// GetKey returns the key referenced by the given key selector.
func (tr Transaction) GetKey(sel Selectable) FutureKey {
	inner, ctx := tr.t.inner, tr.t.ctx
	ks := sel.FDBKeySelector()
	// OrEqual values in our KeySelector match the C++ wire convention
	// (same as Apple Go binding). Pass directly — no inversion needed.
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
// deferred versionstamp pattern.
//
// Only supported on transactions created via CreateTransaction() with
// explicit Commit(). Transactions from Transact()/ReadTransact() return
// error 2015 (used_during_commit) because commit is managed internally.
func (tr Transaction) GetVersionstamp() FutureKey {
	if tr.t.commitDone == nil {
		// Transact/ReadTransact manage commit internally — we can't
		// defer the versionstamp read. Use CreateTransaction() instead.
		return newReadyFutureKey(nil, Error{Code: 2015})
	}
	inner := tr.t.inner
	t := tr.t
	return newFutureKey(func() (Key, error) {
		// Block until commit completes (or has already completed).
		<-t.commitDone
		// If commit failed, return the commit error.
		if t.commitErr != nil {
			return nil, t.commitErr
		}
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

// GetEstimatedRangeSizeBytes returns an estimate of the byte size of the key range.
func (tr Transaction) GetEstimatedRangeSizeBytes(r ExactRange) FutureInt64 {
	return newFutureInt64(func() (int64, error) {
		begin, end := r.FDBRangeKeys()
		v, err := tr.t.inner.GetEstimatedRangeSizeBytes(tr.t.ctx, begin.FDBKey(), end.FDBKey())
		return v, convertError(err)
	})
}

// GetRangeSplitPoints suggests split points for the given key range.
func (tr Transaction) GetRangeSplitPoints(r ExactRange, chunkSize int64) FutureKeyArray {
	return newFutureKeyArray(func() ([]Key, error) {
		begin, end := r.FDBRangeKeys()
		points, err := tr.t.inner.GetRangeSplitPoints(tr.t.ctx, begin.FDBKey(), end.FDBKey(), chunkSize)
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

// Snapshot returns a Snapshot view of this transaction.
func (tr Transaction) Snapshot() ReadTransaction {
	return Snapshot{s: &snapshot{tx: tr.t}}
}

// Set sets a key-value pair in the database.
func (tr Transaction) Set(key KeyConvertible, value []byte) {
	tr.t.inner.Set(key.FDBKey(), value)
}

// SetBytes sets a key-value pair using raw byte slices. Avoids the
// KeyConvertible interface boxing allocation in the hot path.
func (tr Transaction) SetBytes(key, value []byte) {
	tr.t.inner.Set(key, value)
}

// Clear removes a key from the database.
func (tr Transaction) Clear(key KeyConvertible) {
	tr.t.inner.Clear(key.FDBKey())
}

// ClearBytes deletes a key using raw bytes. Avoids KeyConvertible boxing.
func (tr Transaction) ClearBytes(key []byte) {
	tr.t.inner.Clear(key)
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

// AddBytes performs an atomic Add using raw byte slices. Avoids
// KeyConvertible interface boxing in the hot path.
func (tr Transaction) AddBytes(key, param []byte) {
	tr.t.inner.Atomic(client.MutAddValue, key, param)
}

func (tr Transaction) And(key KeyConvertible, param []byte) {
	// C++ ReadYourWritesTransaction::atomicOp upgrades And → AndV2 for API >= 510.
	tr.t.inner.Atomic(client.MutAndV2, key.FDBKey(), param)
}

func (tr Transaction) BitAnd(key KeyConvertible, param []byte) {
	tr.t.inner.Atomic(client.MutAndV2, key.FDBKey(), param)
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
	// C++ ReadYourWritesTransaction::atomicOp upgrades Min → MinV2 for API >= 510.
	tr.t.inner.Atomic(client.MutMinV2, key.FDBKey(), param)
}

func (tr Transaction) MaxBytes(key, param []byte) {
	tr.t.inner.Atomic(client.MutMax, key, param)
}

func (tr Transaction) MinBytes(key, param []byte) {
	tr.t.inner.Atomic(client.MutMinV2, key, param)
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

func (tr Transaction) CompareAndClearBytes(key, param []byte) {
	tr.t.inner.Atomic(client.MutCompareAndClear, key, param)
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
		if t.commitDone != nil {
			select {
			case <-t.commitDone:
			default:
				close(t.commitDone)
			}
		}
		return err
	})
}

// Cancel cancels the transaction. Also unblocks any pending
// GetVersionstamp() futures.
func (tr Transaction) Cancel() {
	tr.t.inner.Cancel()
	if tr.t.commitDone != nil {
		select {
		case <-tr.t.commitDone:
		default:
			close(tr.t.commitDone)
		}
	}
}

// OnError determines whether an error is retryable. The returned FutureNil
// includes the retry delay — the delay runs when Get() is called, not when
// OnError() is called. This matches Apple binding semantics.
func (tr Transaction) OnError(e Error) FutureNil {
	inner, ctx := tr.t.inner, tr.t.ctx
	return newFutureNil(func() error {
		err := inner.OnError(ctx, &wire.FDBError{Code: e.Code})
		return convertError(err)
	})
}

// Options returns a TransactionOptions handle.
func (tr Transaction) Options() TransactionOptions {
	return goTransactionOptions{tx: tr.t}
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
	oldDone := tr.t.commitDone
	tr.t.inner = tr.t.db.d.inner.CreateTransaction()
	tr.t.commitDone = make(chan struct{})
	tr.t.commitErr = nil
	// Re-apply DB-level option defaults to the fresh inner — C++ reset() re-copies
	// the database persistent options (ReadYourWrites.actor.cpp). Without this a
	// reset manual transaction silently loses its inherited timeout/retry/size/
	// system-key defaults (codex). NOTE: user-set per-tx options and tenant scoping
	// are NOT yet re-applied here (the fresh-inner approach drops them) — a separate
	// pre-existing Reset divergence, tracked in TODO-production.
	tr.t.db.applyTxDefaults(tr.t)
	old.Cancel()
	// Unblock any goroutines from GetVersionstamp() calls made before Reset.
	if oldDone != nil {
		select {
		case <-oldDone:
		default:
			close(oldDone)
		}
	}
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

// Watch returns a future that becomes ready when the value associated with the
// given key changes. The watch is a long-poll to the storage server.
func (tr Transaction) Watch(key KeyConvertible) FutureNil {
	inner, ctx := tr.t.inner, tr.t.ctx
	k := key.FDBKey()
	// Capture the watched value AND the read version at the transaction's read
	// version SYNCHRONOUSLY, before returning the future. The watch fires when the
	// storage server sees a value different from this one, so both must be pinned
	// at a version BEFORE any later transaction changes the key. Doing it in the
	// future's goroutine (as before) races subsequent mutations and the
	// transaction's own postCommitReset: the value-read could land after a modify
	// (watch registered against the already-current value, never fires), and the
	// read version could be cleared to 0 by commit before the async poll sends the
	// watch. Capturing both here and threading them through keeps only the
	// long-poll asynchronous.
	value, readVersion, setupErr := inner.WatchSetup(ctx, k)
	return newFutureNil(func() error {
		if setupErr != nil {
			return convertError(setupErr)
		}
		return convertError(inner.WatchPoll(ctx, k, value, readVersion))
	})
}

// CreateTenant creates a tenant within this transaction.
// Convenience method matching Apple binding — delegates to Database.CreateTenant.
func (tr Transaction) CreateTenant(name KeyConvertible) error {
	return tr.t.db.CreateTenant(name)
}

// DeleteTenant deletes a tenant within this transaction.
// Convenience method matching Apple binding — delegates to Database.DeleteTenant.
func (tr Transaction) DeleteTenant(name KeyConvertible) error {
	return tr.t.db.DeleteTenant(name)
}

// ListTenants lists all tenants.
// Convenience method matching Apple binding — delegates to Database.ListTenants.
func (tr Transaction) ListTenants() ([]Key, error) {
	return tr.t.db.ListTenants()
}

// LocalityGetAddressesForKey returns the addresses of storage servers that
// hold the given key. Uses the location cache, querying the cluster on miss.
func (tr Transaction) LocalityGetAddressesForKey(key KeyConvertible) FutureStringSlice {
	inner, ctx := tr.t.inner, tr.t.ctx
	return newFutureStringSlice(func() ([]string, error) {
		addrs, err := inner.GetAddressesForKey(ctx, key.FDBKey())
		if err != nil {
			return nil, convertError(err)
		}
		return addrs, nil
	})
}

func newFutureStringSlice(fn func() ([]string, error)) FutureStringSlice {
	f := &futureStringSlice{}
	f.init()
	go func() {
		defer close(f.done)
		defer recoverFuturePanic(func(e error) { f.err = e }) // RFC-110
		f.val, f.err = fn()
	}()
	return f
}

// Transact implements Transactor for composability.
// Catches Error panics from MustGet() and returns them as errors,
// matching the Apple binding's panicToError recovery.
func (tr Transaction) Transact(f func(WritableTransaction) (any, error)) (r any, e error) {
	defer panicToError(&e)
	return f(tr)
}

// ReadTransact implements ReadTransactor for composability.
func (tr Transaction) ReadTransact(f func(ReadTransaction) (any, error)) (r any, e error) {
	defer panicToError(&e)
	return f(tr)
}

// WrapTransaction wraps an existing client.Transaction as an fdb.Transaction.
// This is useful when you need to pass a client.Transaction to code that
// expects the fdb facade types (e.g., the directory layer).
func WrapTransaction(tx *client.Transaction, db Database) Transaction {
	return Transaction{t: &transaction{
		inner:      tx,
		db:         db,
		ctx:        db.d.ctx,
		commitDone: make(chan struct{}),
	}}
}

// panicToError catches error panics and converts them to returned errors.
// Apple's binding only catches fdb.Error (since the C client only produces
// those), but our pure Go client can surface arbitrary errors (network,
// context, etc.) via MustGet(), so we catch the full error interface.
func panicToError(e *error) {
	if r := recover(); r != nil {
		if err, ok := r.(error); ok {
			// Apply unconvertError so fdb.Error panics from MustGet()
			// are converted back to *wire.FDBError for the retry loop.
			// Without this, panicked fdb.Error escapes the retry loop
			// because OnError doesn't recognize it via errors.As.
			*e = unconvertError(err)
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
