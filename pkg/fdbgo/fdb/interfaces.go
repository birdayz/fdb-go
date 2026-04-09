package fdb

// Transactor can execute a function that requires a Transaction.
// Both Database and Transaction implement Transactor.
type Transactor interface {
	Transact(func(Transaction) (any, error)) (any, error)
	ReadTransactor
}

// ReadTransactor can execute a function that requires a ReadTransaction.
// Database, Transaction, and Snapshot implement ReadTransactor.
type ReadTransactor interface {
	ReadTransact(func(ReadTransaction) (any, error)) (any, error)
}

// ReadTransaction can asynchronously read from a FoundationDB database.
// Transaction and Snapshot both satisfy ReadTransaction.
type ReadTransaction interface {
	Get(key KeyConvertible) FutureByteSlice
	GetKey(sel Selectable) FutureKey
	GetRange(r Range, options RangeOptions) RangeResult
	GetReadVersion() FutureInt64
	GetDatabase() Database
	Snapshot() Snapshot
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
