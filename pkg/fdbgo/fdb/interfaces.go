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
