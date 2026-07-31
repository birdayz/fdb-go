// Package fdbgo is the root of the pure-Go FoundationDB client. It contains no
// code; the importable packages are the subdirectories — [fdb.dev/pkg/fdbgo/fdb]
// (the Apple-Go-binding-compatible surface most callers want), fdbgo/client (the
// lower-level client), fdbgo/transport, fdbgo/wire and fdbgo/tuple.
//
// This doc exists to put one operational fact where a migrator will find it.
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
// open against an unreachable cluster waits indefinitely. So [fdb.OpenDatabase]
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
//   - A context deadline — [fdb.Database.TransactCtx] or
//     [fdb.Database.ReadTransactCtx].
//
// The context form is Go's EXTRA bound (RFC-090), not a substitute for a
// libfdb_c mechanism that does not exist. A migrator porting from libfdb_c who
// set a transaction timeout there should keep setting it here; it works. A
// migrator who set nothing there was already unbounded there, and is equally
// unbounded here.
//
// The no-context [fdb.Database.Transact] runs on context.Background() to stay
// drop-in compatible with the Apple Go binding, so a context deadline is not
// available to it — bound it with a timeout or retry limit, or switch to
// TransactCtx.
package fdbgo
