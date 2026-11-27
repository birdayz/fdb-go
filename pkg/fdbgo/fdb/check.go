package fdb

import "io"

// Compile-time interface checks.
var (
	_ io.Closer           = Database{}
	_ Transactor          = Database{}
	_ Transactor          = Transaction{}
	_ BackendDatabase     = Database{}
	_ ReadTransactor      = Database{}
	_ ReadTransactor      = Transaction{}
	_ ReadTransactor      = Snapshot{}
	_ ReadTransaction     = Transaction{}
	_ ReadTransaction     = Snapshot{}
	_ WritableTransaction = Transaction{}
	// The pure-Go backend reports the GRV instant; the capability is optional at
	// the call site but not optional here, so dropping the forwarder is a build
	// break rather than a silent fallback to a coarser budget anchor.
	_ ReadVersionInstantReporter = Transaction{}
	_ TransactionOptions         = goTransactionOptions{}
	_ KeyConvertible             = Key{}
	_ ExactRange                 = KeyRange{}
	_ Range                      = KeyRange{}
	_ Range                      = SelectorRange{}
	_ Selectable                 = KeySelector{}
	_ Future[[]byte]             = FutureByteSlice(nil)
	_ Future[Key]                = FutureKey(nil)
	_ Future[int64]              = FutureInt64(nil)
	_ Future[[]Key]              = FutureKeyArray(nil)
	_ Future[[]string]           = FutureStringSlice(nil)
)
