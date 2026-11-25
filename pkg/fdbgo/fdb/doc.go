// Package fdb provides a pure-Go client for FoundationDB.
//
// Its API is modeled on Apple's FoundationDB Go binding
// (github.com/apple/foundationdb/bindings/go/src/fdb) but requires no
// C library (libfdb_c). It uses a native Go wire protocol implementation.
//
// Basic usage:
//
//	fdb.MustAPIVersion(730)
//	db := fdb.MustOpenDefault()
//	defer db.Close()
//
//	ret, err := db.Transact(func(tr fdb.WritableTransaction) (interface{}, error) {
//	    tr.Set(fdb.Key("hello"), []byte("world"))
//	    return tr.Get(fdb.Key("foo")).MustGet(), nil
//	})
//
// Known differences from the Apple binding:
//   - Transact and Transactor take func(WritableTransaction), not
//     func(Transaction): an Apple-style closure needs its parameter type changed.
//   - Error messages: Error.Error() returns "<description> (<code>)", or
//     "FoundationDB error: <code>" for a code without a description, where the
//     Apple binding returns "FoundationDB error code <code> (<description>)".
//     Use Error.Code for programmatic matching.
//   - Future.Cancel() completes the future with operation_cancelled (1101); the
//     operation behind it continues, except a watch, which is cancelled.
//   - As in the Apple binding, methods like Get/GetRange take no context.
//     TransactCtx and ReadTransactCtx bound the retry loop, backoff and reads
//     with an explicit context. Transact, ReadTransact and CreateTransaction reads inherit context.Background()
//     or the WrapDatabaseContext context. SetTimeout and Cancel() also bound transactions.
//
// # Retries and timeouts are UNBOUNDED by default — in this client and in libfdb_c
//
// A transaction created with no options has no timeout and no retry limit. A
// Transact against a down or unreachable cluster therefore retries until the
// cluster returns or the caller stops it. Nothing internal stops it.
//
// This is NOT a divergence from libfdb_c — it is libfdb_c's behaviour, matched
// deliberately. The C++ client's per-transaction defaults are set in
// ReadYourWrites.actor.cpp:2078-2082:
//
//	void ReadYourWritesTransactionOptions::reset(Transaction const& tr) {
//		memset(this, 0, sizeof(*this));
//		timeoutInSeconds = 0.0;
//		maxRetries = -1;
//
// and resetTimeout (ReadYourWrites.actor.cpp:1576-1578) arms the `timebomb` actor
// only when timeoutInSeconds is non-zero, so by default no timer exists at all.
// fdb.options documents the option in the same terms: "If set to 0, will disable
// all timeouts." A default-configured libfdb_c transaction hangs against a dead
// cluster exactly as long as this one does.
//
// Do not read "no internal timeout" as a Go weakness to be worked around. Both
// clients hand the bound to the caller; that is the FoundationDB contract.
//
// # Where this client is STRICTER than libfdb_c
//
// Bootstrap — the initial coordinator connection — is internally bounded here at
// defaultBootstrapTimeout (60s, fdb/database.go:139-153) whenever the caller
// supplies no deadline of its own. libfdb_c has no equivalent bound: a C-client
// open against an unreachable cluster waits indefinitely. So [OpenDatabase]
// fails fast where libfdb_c would not. Only the LIVE database (post-open
// reconnection, and the transaction retry loop) is unbounded.
//
// # What the caller must do
//
// Pick at least one bound for every transaction. Any of these terminates it:
//
//   - A transaction timeout — Transaction.SetTimeout, or
//     DatabaseOptions.SetTransactionTimeout for a default across all
//     transactions. This is the direct analog of libfdb_c's timeout option and
//     of C++ `timebomb`: it cancels in-flight RPC waits, not merely the gaps
//     between them (client/readpath.go:89-100, RFC-112), and surfaces
//     transaction_timed_out (1031).
//   - A retry limit — Transaction.SetRetryLimit or
//     DatabaseOptions.SetTransactionRetryLimit. Caps retries; the most recent
//     error escapes once the cap is hit.
//   - A context deadline — [Database.TransactCtx] or
//     [Database.ReadTransactCtx].
//
// The context form is Go's EXTRA bound (RFC-090), not a substitute for a
// libfdb_c mechanism that does not exist. A migrator porting from libfdb_c who
// set a transaction timeout there should keep setting it here; it works. A
// migrator who set nothing there was already unbounded there, and is equally
// unbounded here.
//
// Ordinary databases use context.Background(): bound retries with a timeout, retry limit or TransactCtx/ReadTransactCtx.
// [WrapDatabaseContext] instead supplies the inherited context for retries, CreateTransaction reads and snapshots.
package fdb
