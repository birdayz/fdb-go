//go:build cgo && libfdbc

// Package libfdbc is the config-selectable libfdb_c (Apple CGo) backend for the
// record layer — RFC-109's escape hatch. It implements the fdb client interfaces
// (fdb.Transactor / fdb.WritableTransaction / fdb.ReadTransaction / …) by
// forwarding to the decade-hardened Apple Go binding (cgofdb), so an operator who
// does not yet trust the from-scratch pure-Go client can run the exact same record
// layer on libfdb_c by config — with no code change to the layer above the seam.
//
// Why build on cgofdb rather than raw cgo (a refinement of the RFC, which assumed
// raw libfdb_c calls):
//
//   - Future resolution is already M-friendly. The RFC worried that
//     cgofdb.Future.Get pins an OS thread per in-flight read. Reading the binding
//     (bindings/go/src/fdb/futures.go) shows it does NOT: BlockUntilReady registers
//     an fdb_future_set_callback that signals a sync.Mutex, then parks on that mutex
//     — a Go-runtime park that frees the M (the network thread fires the callback).
//     So the callback→channel design the RFC mandated is what cgofdb already does;
//     forwarding to it inherits correct, non-thread-pinning resolution for free.
//   - OnError / retry backoff is delegated to libfdb_c: tr.OnError
//     (fdb_transaction_on_error) makes the retry decision and computes+sleeps the
//     backoff. We drive the retry LOOP ourselves (runLoop) rather than calling
//     cgofdb.Database.Transact, for one reason only — to bound the OnError backoff WAIT
//     by the caller ctx (cgofdb's own retryable() loop waits with no ctx, so a
//     cancel/deadline during the inter-attempt backoff is not honored, violating the
//     CtxTransactor contract recordlayer.Run relies on). The backoff algorithm stays
//     libfdb_c's; only the wait is made cancellable. See runLoop.
//   - FDB error codes map 1:1: cgofdb.Error.Code and fdb.Error.Code are both the
//     raw fdb_error_t int. We translate the wrapper type at the boundary, never the
//     code, and synthesize nothing.
//
// The one place this differs from the RFC's "options by raw integer": cgofdb's raw
// setOpt is unexported, so options are forwarded through cgofdb's generated typed
// setters (same fdb.options codes, generated for this 7.3.77 binding). The handful
// of options cgofdb lacks a setter for (SkipGrvCache) or that have no libfdb_c
// analog (write-conflicts-disabled, EnsureMutationCapacity) are no-ops here; this
// is documented per-method and is a known v1 limitation, not a silent divergence.
package libfdbc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// apiVersion is the libfdb_c API version this binding is built against (the
// binding's header is FDB_API_VERSION 730, matching the 7.3.77 server pin).
const apiVersion = 730

// cgofdb.OpenDatabase caches one Database (one C fdb_database) per cluster file and
// hands the SAME handle to every caller. So we refcount per cluster file: each Open
// shares the cached handle, and only the LAST Close destroys it (cgofdb.Database.
// Close → fdb_database_destroy) — otherwise closing one backend would invalidate
// another sharing the same file, and closing both would double-destroy the handle.
var (
	openMu   sync.Mutex
	openRefs = map[string]*sharedDB{}
)

type sharedDB struct {
	db   cgofdb.Database
	refs int
}

// Open initializes libfdb_c (once-per-process: fdb_select_api_version +
// fdb_setup_network/fdb_run_network happen lazily on first OpenDatabase inside
// cgofdb) and opens the cluster. The returned BackendDatabase drives the record
// layer's Run/RunRead path. There is no runtime teardown of the network thread —
// backend selection is a process-launch-time decision (the libfdb_c network thread
// is one-shot); only the per-cluster Database handle is refcounted and released.
func Open(clusterFile string) (fdb.BackendDatabase, error) {
	// Set BOTH API-version registries to 730. cgofdb.APIVersion touches the C network
	// (fdb_select_api_version); fdb.APIVersion is the pure-Go facade's independent
	// in-process bookkeeping that the record layer reads via fdb.GetAPIVersion — notably
	// tuple.PackWithVersionstamp (record versions, MAX_EVER, SPFresh). Without setting the
	// facade version, versionstamped writes on this backend would fail with
	// api_version_unset (2200) even though the cluster opened fine. Both calls are
	// idempotent (no-op if already 730, error on a mismatched prior version).
	if err := cgofdb.APIVersion(apiVersion); err != nil {
		return nil, convErr(err)
	}
	if err := fdb.APIVersion(apiVersion); err != nil {
		return nil, err
	}
	db, err := openSharedDatabase(clusterFile)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// openSharedDatabase serializes cgofdb's cache and tracks every owner of its handles.
func openSharedDatabase(clusterFile string) (*database, error) {
	openMu.Lock()
	defer openMu.Unlock()
	s, ok := openRefs[clusterFile]
	if !ok {
		cdb, err := cgofdb.OpenDatabase(clusterFile)
		if err != nil {
			return nil, convErr(err)
		}
		s = &sharedDB{db: cdb}
		openRefs[clusterFile] = s
	}
	s.refs++
	return &database{db: s.db, clusterFile: clusterFile}, nil
}

// SetKnob sets a libfdb_c client knob, "name=value" (the knob network option,
// 40, which Java's FDBDatabaseFactoryImpl sets through NetworkOptions.setKnob).
// It selects API version 730 first, as Open does, since network options need a
// selected version.
func SetKnob(knob string) error {
	if err := cgofdb.APIVersion(apiVersion); err != nil {
		return convErr(err)
	}
	return convErr(cgofdb.Options().SetKnob(knob))
}

// database adapts cgofdb.Database to fdb.BackendDatabase. It also implements
// fdb.CtxTransactor / fdb.CtxReadTransactor so the record layer's runTransactCtx honors a
// caller context on this backend (not just the pure-Go one). We drive the retry loop
// ourselves (runLoop) and bound every ctx-sensitive wait: (a) bail before each attempt if
// the ctx is done; (b) a cancel watcher aborts an in-flight read when the ctx is canceled
// or its deadline expires (the cgo analog of the pure-Go client threading ctx into each
// read RPC); (c) the inter-attempt OnError backoff wait is ctx-bounded too (runLoop /
// ctxBoundedWait). Like pure-Go, the auto-commit is detached from the caller ctx — the
// watcher is stopped before it and no FDB timeout bounds it, so neither a cancel nor a
// deadline that lands after the callback returned can abort the commit (cf.
// client/transaction.go:1437, the WithoutCancel commit). See runLoop + withCancelWatcher.
type database struct {
	db          cgofdb.Database
	clusterFile string
	closed      atomic.Bool
}

func (d *database) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), f)
}

func (d *database) TransactCtx(ctx context.Context, f func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.runLoop(ctx, true, func(tr cgofdb.Transaction) (any, error) {
		return f(&txn{reader: reader{rt: tr, opts: tr.Options()}, tr: tr})
	})
}

// CreateWritableTransaction satisfies fdb.BackendDatabase: a standalone,
// non-retry transaction as the WritableTransaction interface, for the record
// layer's database/sql explicit-transaction path (BeginTx). It is the cgo analog
// of the pure-Go Database.CreateWritableTransaction — same fdb_database_create_-
// transaction the retry loop uses (runLoop), just handed to the caller instead of
// a closure. The caller owns commit/cancel; the underlying cgofdb.Transaction is
// GC-finalized by Apple's binding (fdb_transaction_destroy), so no explicit
// teardown is required, exactly as for the pure-Go handle.
func (d *database) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tr, err := d.db.CreateTransaction()
	if err != nil {
		return nil, convErr(err) // CreateTransaction failure is non-retryable (cgofdb)
	}
	return &txn{reader: reader{rt: tr, opts: tr.Options()}, tr: tr}, nil
}

// LocalityGetBoundaryKeys satisfies fdb.BackendDatabase via Apple's binding
// (cgofdb.Database.LocalityGetBoundaryKeys → the C locality API over
// \xff/keyServers). Both clients read the SAME system range against the SAME
// cluster, so the boundary keys are byte-identical — the online MUTUAL indexer
// partitions the keyspace the same way on either backend. Each cgofdb.Key is a
// newtype cast to fdb.Key (both []byte) into a fresh result slice.
func (d *database) LocalityGetBoundaryKeys(r fdb.ExactRange, limit int, readVersion int64) ([]fdb.Key, error) {
	// Match the pure-Go backend's limit validation exactly (it routes through
	// RangeOptions): only -1 (ROW_LIMIT_UNLIMITED), 0, and positive are valid; a
	// limit < -1 is range_limits_invalid. Without this the two backends would
	// diverge — and Apple's binding uses a `limit != 0` form that drives
	// make([]Key, size) negative and PANICS on any negative limit, so we must
	// handle both cases before delegating. The record layer passes 0; this keeps
	// the backend-agnostic API consistent for any caller.
	if limit < -1 {
		return nil, fdb.Error{Code: 2012} // range_limits_invalid (matches pure-Go)
	}
	if limit < 0 { // limit == -1: unlimited; pass 0 so the binding doesn't panic.
		limit = 0
	}
	cgoKeys, err := d.db.LocalityGetBoundaryKeys(cgoExactRange(r), limit, readVersion)
	if err != nil {
		return nil, convErr(err)
	}
	keys := make([]fdb.Key, len(cgoKeys))
	for i, k := range cgoKeys {
		keys[i] = fdb.Key(k)
	}
	return keys, nil
}

// runLoop drives the cgofdb retry loop OURSELVES instead of delegating to
// cgofdb.Database.Transact/ReadTransact. We still delegate the per-error retry decision
// and backoff COMPUTATION to libfdb_c (tr.OnError computes and sleeps the backoff inside
// the returned future); we only bound the WAIT on that future by the caller ctx. That is
// the one thing cgofdb.Transact can't do — its retryable() loop runs OnError(...).Get()
// with no ctx, so a cancel/deadline during the inter-attempt backoff is not noticed until
// the next attempt. The pure-Go client bounds the backoff by ctx (transaction.go
// backoffSleep), and recordlayer.Run's CtxTransactor contract requires it.
//
// commit selects the write vs read shape: when true a successful callback is followed by a
// detached auto-commit (cgofdb.Transact's tr.Commit after the callback); when false there
// is no commit (cgofdb.ReadTransact). The auto-commit is NOT under the cancel watcher and
// is bounded by no FDB timeout, so a late cancel/deadline cannot abort it — the commit
// runs to completion (commit_unknown_result idempotency; pure-Go WithoutCancel commit,
// client/transaction.go:1437).
func (d *database) runLoop(ctx context.Context, commit bool, run func(cgofdb.Transaction) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tr, err := d.db.CreateTransaction()
	if err != nil {
		return nil, convErr(err) // CreateTransaction failure is non-retryable (cgofdb)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The callback runs under the cancel watcher (reads bounded by ctx; an error panic
		// from an adapter MustGet is recovered to ee). The commit is NOT under the watcher.
		rr, ee := withCancelWatcher(ctx, tr.Cancel, func() (any, error) {
			return run(tr)
		})
		if ee == nil {
			if !commit {
				return rr, nil
			}
			// Abort before the commit if ctx died DURING the callback (pure-Go parity,
			// client/database.go:645) — otherwise commit detached and report success.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if ee = convErr(tr.Commit().Get()); ee == nil {
				return rr, nil
			}
			// else: a commit error (not_committed / commit_unknown_result / …) falls
			// through to the retry classification below.
		}
		// ee != nil. Only an FDB error goes through OnError; anything else — an app error,
		// a ctx error, a non-fdb error panic — is terminal. mapTransactErr keeps a callback's
		// own application error even on a dead ctx (callback-error precedence), and surfaces
		// ctx.Err() only for a ctx-CAUSED failure. We do NOT short-circuit on ctx.Err() here:
		// a RETRYABLE FDB error racing a cancel must return ctx.Err() (not the FDB code),
		// which OnError's ctx-bounded backoff below does — exactly as the pure-Go loop does
		// (it always calls OnError; OnError bounds only the retryable backoff by ctx).
		var fe fdb.Error
		if !errors.As(ee, &fe) {
			return nil, mapTransactErr(ctx, ee)
		}
		// ctx-bounded backoff: nil → OnError reset tr, take another attempt.
		berr := d.ctxOnError(ctx, tr, fe.Code)
		if berr == nil {
			continue
		}
		// Terminal (OnError re-raised, or ctx aborted the wait). Mirror cgofdb's retryable
		// override rule (database.go:177-183): fdb_transaction_on_error almost always
		// re-raises the SAME code, in which case KEEP the original error ee — preserving the
		// record layer's %w-wrapped context (what cgoErrShim protected on the old delegated
		// path). Substitute OnError's error only when it is not an fdb.Error (a ctx abort) or
		// changed the code.
		var be fdb.Error
		if !errors.As(berr, &be) || be.Code != fe.Code {
			return nil, mapTransactErr(ctx, berr)
		}
		return nil, mapTransactErr(ctx, ee)
	}
}

// ctxOnError runs tr.OnError(code) — libfdb_c makes the retry decision and (for a retryable
// code within the retry limit) computes + sleeps the backoff inside the returned future. A
// nil return means the error was retryable and tr is reset for another attempt; a non-nil
// fdb error means OnError re-raised it (terminal); a ctx error means the backoff was aborted
// by the caller ctx.
//
// The WAIT is ctx-bounded ONLY when OnError actually retries the code — that is the path
// with a backoff to interrupt, and it mirrors the pure-Go OnError, which consults ctx only
// on the retryable backoffSleep and returns a terminal error verbatim without touching ctx.
// A terminal code's OnError re-raises immediately (no backoff), so bounding it by ctx would
// only let a dead ctx race-replace the real terminal error with ctx.Err() — a divergence.
//
// The gate is fdb.IsOnErrorRetryable, NOT fdb.IsRetryable: the relevant question is whether
// fdb_transaction_on_error backs off this code, which is a LARGER set than the generic
// RETRYABLE error-predicate — it also includes blob_granule_request_failed (1079). Gating on
// IsRetryable would leave 1079's real backoff un-ctx-bounded (a CtxTransactor-contract miss).
func (d *database) ctxOnError(ctx context.Context, tr cgofdb.Transaction, code int) error {
	fut := tr.OnError(cgofdb.Error{Code: code})
	if !fdb.IsOnErrorRetryable(code) {
		return convErr(fut.Get())
	}
	return ctxBoundedWait(ctx, fut.Get, tr.Cancel)
}

// ctxBoundedWait runs get() (a blocking cgofdb future wait) in a goroutine and returns its
// result — unless ctx is done first, in which case it calls cancel() to unblock get()
// (fdb_transaction_cancel errors the in-flight future) and returns ctx.Err(). It always
// joins the goroutine, so the C future is fully resolved before return (no leak).
func ctxBoundedWait(ctx context.Context, get func() error, cancel func()) error {
	done := make(chan error, 1)
	go func() { done <- get() }()
	select {
	case err := <-done:
		return convErr(err)
	case <-ctx.Done():
		cancel()
		<-done // join: get() has returned (the future errored with transaction_cancelled)
		return ctx.Err()
	}
}

// mapTransactErr maps the error a cgofdb Transact/ReadTransact returned to the fdb
// world. On a ctx-CAUSED failure (the ctx error itself, or the transaction_cancelled
// the cancel watcher induces when ctx is canceled or its deadline expires), it surfaces
// ctx.Err() so the caller sees context.Canceled/DeadlineExceeded like the pure-Go
// backend (which threads ctx per-RPC). An application error the callback returned is
// NOT masked by ctx.Err() — pure-Go gives the callback error precedence
// (client/database.go:631-637). A successful run (e==nil) is never overridden, even if
// ctx expired right after the commit.
func mapTransactErr(ctx context.Context, e error) error {
	if e == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && ctxCausedFailure(e) {
		return ctxErr
	}
	return convErr(e)
}

func (d *database) ReadTransact(f func(fdb.ReadTransaction) (any, error)) (any, error) {
	return d.ReadTransactCtx(context.Background(), f)
}

func (d *database) ReadTransactCtx(ctx context.Context, f func(fdb.ReadTransaction) (any, error)) (any, error) {
	return d.runLoop(ctx, false, func(tr cgofdb.Transaction) (any, error) {
		return f(reader{rt: tr, opts: tr.Options()})
	})
}

// Close releases this backend's reference to the shared per-cluster Database
// handle; the underlying C handle is destroyed only when the last reference for the
// cluster file closes. Idempotent: a second Close on the same backend is a no-op
// (so it never double-decrements the shared refcount).
func (d *database) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	openMu.Lock()
	defer openMu.Unlock()
	s, ok := openRefs[d.clusterFile]
	if !ok {
		return nil
	}
	s.refs--
	if s.refs <= 0 {
		s.db.Close()
		delete(openRefs, d.clusterFile)
	}
	return nil
}

// withCancelWatcher runs fn with a goroutine that cancels the cgo transaction (via
// cancelTx) when ctx is canceled OR its deadline expires before fn returns. cgofdb
// owns the reads (they block the calling goroutine on a C future), so cancelTx
// (fdb_transaction_cancel — transaction_cancelled, 1025) is the only hook to abort a
// read stuck past the ctx. This is the cgo analog of the pure-Go client threading ctx
// into each read RPC. There is deliberately NO deadline→SetTimeout conversion: pure-Go
// imposes no FDB transaction timeout from the ctx (it threads ctx per-RPC), and an FDB
// timeout would also abort cgofdb's auto-commit — which must run detached from the
// caller ctx (client/transaction.go:1437, the WithoutCancel commit).
//
// The watcher is stopped AND joined before fn returns, and once stopped it never
// cancels: when ctx fires, the goroutine re-checks `stop` before calling cancelTx, so
// a ctx.Done() that races the stop (a deadline expiring just as the callback finishes)
// takes the no-cancel branch. After fn returns, the goroutine has provably exited (the
// join) and will not touch the transaction — so a later cancel/deadline cannot abort
// the in-flight auto-commit, mirroring the pure-Go detached commit.
//
// An error panic from fn (e.g. an adapter MustGet on a conflict) is recovered into the
// returned error, the way the pure-Go backend's panicToError does (fdb/transaction.go:506);
// runLoop then classifies it (a retryable fdb.Error is retried via OnError, anything else
// is terminal). A non-error panic re-panics.
func withCancelWatcher(ctx context.Context, cancelTx func(), fn func() (any, error)) (r any, e error) {
	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			// Prefer stop: if fn has already returned (stop closed), the commit is
			// detached — do NOT cancel it. Only abort a still-running callback's read.
			select {
			case <-stop:
			default:
				cancelTx()
			}
		case <-stop:
		}
	}()
	// Two defers (run LIFO): the watcher is stopped + joined FIRST, then an error panic is
	// recovered into e. recoverErrorPanic must be deferred DIRECTLY — recover() only works
	// in a function invoked by defer, not one it in turn calls — hence the split rather than
	// one closure that calls it. The join still runs even on the panic path (registered
	// last → runs first), so the goroutine never leaks.
	defer recoverErrorPanic(&e)
	defer func() {
		close(stop)
		<-exited // join: the watcher has exited (and decided not to cancel) before we return
	}()
	return fn()
}

// recoverErrorPanic recovers a panic of an error value into *e (the way the pure-Go
// backend's panicToError does, fdb/transaction.go:506 — recovering the full error
// interface, not just fdb.Error). A non-error panic re-panics. MUST be called from a
// deferred function so the recover() is in scope.
func recoverErrorPanic(e *error) {
	if p := recover(); p != nil {
		if err, ok := p.(error); ok {
			*e = err
			return
		}
		panic(p)
	}
}

// ctxCausedFailure reports whether e is a failure attributable to the context: the
// context error itself, or the FDB transaction_cancelled (1025) that the cancel watcher
// induces when ctx is canceled or its deadline expires during a read. An application
// error the callback returned is not, so it is never masked by ctx.Err(). We impose no
// deadline→SetTimeout, so a deadline does not surface as transaction_timed_out (1031)
// from this backend — the watcher cancels (1025) instead; a 1031 here would be the
// app's own transaction-timeout option, which is not ctx-caused and must surface as-is.
func ctxCausedFailure(e error) bool {
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return true
	}
	var fe fdb.Error
	if errors.As(e, &fe) {
		return fe.Code == 1025
	}
	var ce cgofdb.Error
	if errors.As(e, &ce) {
		return ce.Code == 1025
	}
	return false
}

// Compile-time interface conformance.
var (
	_ fdb.BackendDatabase     = (*database)(nil)
	_ fdb.CtxTransactor       = (*database)(nil)
	_ fdb.CtxReadTransactor   = (*database)(nil)
	_ fdb.ReadTransaction     = reader{}
	_ fdb.WritableTransaction = (*txn)(nil)
	_ fdb.RangeResult         = rangeResult{}
	_ fdb.RangeIterator       = (*rangeIterator)(nil)
	_ fdb.TransactionOptions  = txOptions{}
	_ fdb.FutureByteSlice     = futureByteSlice{}
	_ fdb.FutureKey           = futureKey{}
	_ fdb.FutureInt64         = futureInt64{}
	_ fdb.FutureNil           = futureNil{}
	_ fdb.FutureKeyArray      = futureKeyArray{}
)
