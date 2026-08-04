package fdb

import (
	"context"
	"time"
)

// Transactor can execute a function that requires a Transaction.
// Both Database and Transaction implement Transactor.
type Transactor interface {
	Transact(func(WritableTransaction) (any, error)) (any, error)
	ReadTransactor
}

// ReadTransactor can execute a function that requires a ReadTransaction.
// Database, Transaction, and Snapshot implement ReadTransactor.
type ReadTransactor interface {
	ReadTransact(func(ReadTransaction) (any, error)) (any, error)
}

// CtxTransactor is an OPTIONAL capability: a Transactor whose retry loop, backoff,
// and reads are bounded by a caller context. Database and Tenant implement it;
// recordlayer.Run type-asserts for it and falls back to Transact otherwise (so the
// Transactor interface stays unwidened and Transaction — which has no retry loop —
// needs no meaningless TransactCtx). Per RFC-090 the dispatched commit and its
// commit_unknown_result barrier deliberately run on a DETACHED context, so the
// caller's ctx never cancels an in-flight commit (which is already bounded by the
// per-RPC timeout).
type CtxTransactor interface {
	TransactCtx(ctx context.Context, f func(WritableTransaction) (any, error)) (any, error)
}

// CtxReadTransactor is the read-side analog of CtxTransactor (bounds the read-retry
// loop + backoff by the caller context).
type CtxReadTransactor interface {
	ReadTransactCtx(ctx context.Context, f func(ReadTransaction) (any, error)) (any, error)
}

// ReadVersionInstantReporter is an OPTIONAL capability: a transaction that can say
// WHEN its current read version was obtained, i.e. when FDB's 5-second MVCC window
// opened for it. ok is false when no read version is held or when the version was
// supplied by the caller (SetReadVersion), whose window opened at an instant the
// client never observed.
//
// Optional, and type-asserted at the call site, for the same reason CtxTransactor is
// (RFC-109): a backend that cannot report it — the cgo escape hatch has no such
// accessor — must not be forced to fake one. A layer budgeting against the MVCC
// window falls back to its own anchor when the assertion fails, so the capability
// SHARPENS the budget where available and never gates correctness on it.
type ReadVersionInstantReporter interface {
	ReadVersionInstant() (time.Time, bool)
}

// ReadTransaction can asynchronously read from a FoundationDB database.
// Transaction and Snapshot both satisfy ReadTransaction.
//
// GetDatabase() is intentionally OFF this interface (RFC-109): it returns the
// concrete pure-Go fdb.Database (a ~40-method handle a cgo backend cannot
// build), and nothing in the record layer calls it through the interface — same
// rationale as Watch/Locality/tenant methods staying concrete-only. It remains a
// concrete method on Transaction/Snapshot for direct-typed callers.
type ReadTransaction interface {
	Get(key KeyConvertible) FutureByteSlice
	GetKey(sel Selectable) FutureKey
	GetRange(r Range, options RangeOptions) RangeResult
	GetReadVersion() FutureInt64
	Snapshot() ReadTransaction
	GetEstimatedRangeSizeBytes(r ExactRange) FutureInt64
	GetRangeSplitPoints(r ExactRange, chunkSize int64) FutureKeyArray
	Options() TransactionOptions

	ReadTransactor
}

// WritableTransaction extends ReadTransaction with write operations.
// Only Transaction satisfies this (not Snapshot).
type WritableTransaction interface {
	ReadTransaction

	// Mutations
	Set(key KeyConvertible, value []byte)
	Clear(key KeyConvertible)
	ClearRange(er ExactRange)

	// Atomic mutations
	Add(key KeyConvertible, param []byte)
	And(key KeyConvertible, param []byte)
	BitAnd(key KeyConvertible, param []byte)
	Or(key KeyConvertible, param []byte)
	BitOr(key KeyConvertible, param []byte)
	Xor(key KeyConvertible, param []byte)
	BitXor(key KeyConvertible, param []byte)
	Max(key KeyConvertible, param []byte)
	Min(key KeyConvertible, param []byte)
	ByteMax(key KeyConvertible, param []byte)
	ByteMin(key KeyConvertible, param []byte)
	AppendIfFits(key KeyConvertible, param []byte)
	CompareAndClear(key KeyConvertible, param []byte)
	SetVersionstampedKey(key KeyConvertible, param []byte)
	SetVersionstampedValue(key KeyConvertible, param []byte)

	// []byte fast-path overloads — avoid KeyConvertible boxing on the hot index-
	// maintenance path (RFC-109: callers invoke these through the interface, so they
	// must be on it; both backends implement them).
	SetBytes(key, value []byte)
	ClearBytes(key []byte)
	AddBytes(key, param []byte)
	MaxBytes(key, param []byte)
	MinBytes(key, param []byte)
	CompareAndClearBytes(key, param []byte)

	// Conflict ranges
	AddReadConflictRange(er ExactRange) error
	AddReadConflictKey(key KeyConvertible) error
	AddWriteConflictRange(er ExactRange) error
	AddWriteConflictKey(key KeyConvertible) error

	// Transaction lifecycle
	Commit() FutureNil
	Cancel()
	Reset()
	OnError(e Error) FutureNil
	SetReadVersion(version int64)

	// Post-commit
	GetCommittedVersion() (int64, error)
	GetVersionstamp() FutureKey
	GetApproximateSize() FutureInt64
}
