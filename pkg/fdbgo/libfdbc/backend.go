//go:build cgo

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
// setters (same fdb.options codes, generated for this 7.3.75 binding). The handful
// of options cgofdb lacks a setter for (SkipGrvCache) or that have no libfdb_c
// analog (write-conflicts-disabled, EnsureMutationCapacity) are no-ops here; this
// is documented per-method and is a known v1 limitation, not a silent divergence.
package libfdbc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// apiVersion is the libfdb_c API version this binding is built against (the
// binding's header is FDB_API_VERSION 730, matching the 7.3.75 server pin).
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
	// Idempotent for the same version (cgofdb.APIVersion returns nil if already
	// set to 730, and is internally mutex-guarded). The pure-Go fdb.APIVersion is
	// independent in-process bookkeeping; only this call touches the C network.
	if err := cgofdb.APIVersion(apiVersion); err != nil {
		return nil, convErr(err)
	}
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
func (d *database) Close() {
	if d.closed.Swap(true) {
		return
	}
	openMu.Lock()
	defer openMu.Unlock()
	s, ok := openRefs[d.clusterFile]
	if !ok {
		return
	}
	s.refs--
	if s.refs <= 0 {
		s.db.Close()
		delete(openRefs, d.clusterFile)
	}
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

// reader adapts a cgofdb.ReadTransaction (a Transaction or Snapshot) to
// fdb.ReadTransaction. txn embeds it for the read half of WritableTransaction.
type reader struct {
	rt cgofdb.ReadTransaction
	// opts is the underlying transaction's options handle. It is carried explicitly
	// rather than fetched via rt.Options() because cgofdb.Snapshot.Options() is
	// self-recursive (snapshot.go: `return s.Options()`) and stack-overflows — so a
	// snapshot reader must reuse its parent transaction's options (a snapshot sets
	// options on the same underlying transaction anyway).
	opts cgofdb.TransactionOptions
}

func (r reader) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return futureByteSlice{r.rt.Get(cgoKey(key))}
}

func (r reader) GetKey(sel fdb.Selectable) fdb.FutureKey {
	return futureKey{r.rt.GetKey(cgoSelector(sel))}
}

func (r reader) GetRange(rng fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return rangeResult{r.rt.GetRange(cgoRange(rng), cgoRangeOptions(options))}
}

func (r reader) GetReadVersion() fdb.FutureInt64 {
	return futureInt64{r.rt.GetReadVersion()}
}

func (r reader) Snapshot() fdb.ReadTransaction {
	// cgofdb.Snapshot satisfies cgofdb.ReadTransaction, so it slots straight in. Carry
	// the parent's options handle — cgofdb.Snapshot.Options() stack-overflows.
	return reader{rt: r.rt.Snapshot(), opts: r.opts}
}

func (r reader) GetEstimatedRangeSizeBytes(rng fdb.ExactRange) fdb.FutureInt64 {
	return futureInt64{r.rt.GetEstimatedRangeSizeBytes(cgoExactRange(rng))}
}

func (r reader) GetRangeSplitPoints(rng fdb.ExactRange, chunkSize int64) fdb.FutureKeyArray {
	return futureKeyArray{r.rt.GetRangeSplitPoints(cgoExactRange(rng), chunkSize)}
}

func (r reader) Options() fdb.TransactionOptions {
	return txOptions{r.opts}
}

func (r reader) ReadTransact(f func(fdb.ReadTransaction) (any, error)) (any, error) {
	r2, e := r.rt.ReadTransact(func(crt cgofdb.ReadTransaction) (rr any, ee error) {
		// Recover an adapter MustGet panic (fdb.Error) here: cgofdb's nested ReadTransact
		// recovers ONLY cgofdb.Error (transaction.go panicToError), so an fdb.Error panic
		// would otherwise escape this nested call instead of being returned — unlike the
		// pure-Go Transaction.ReadTransact (fdb/transaction.go:487), which recovers the
		// full error interface. (cgofdb's nested ReadTransact does not retry, so there is
		// no backoff to ctx-bound here.)
		defer recoverErrorPanic(&ee)
		// Inherit r.opts: crt is the same underlying transaction (or a snapshot of it), so
		// its options are r's — and a snapshot's Options() would overflow.
		rr, ee = f(reader{rt: crt, opts: r.opts})
		return rr, toCgoErr(ee)
	})
	return r2, convErr(e)
}

// txn adapts cgofdb.Transaction to fdb.WritableTransaction. The embedded reader
// supplies the ReadTransaction half; the methods below are the write half.
type txn struct {
	reader
	tr cgofdb.Transaction
}

// Mutations.
func (t *txn) Set(key fdb.KeyConvertible, value []byte) { t.tr.Set(cgoKey(key), value) }
func (t *txn) Clear(key fdb.KeyConvertible)             { t.tr.Clear(cgoKey(key)) }
func (t *txn) ClearRange(er fdb.ExactRange)             { t.tr.ClearRange(cgoExactRange(er)) }

// Atomic mutations (forwarded 1:1 — cgofdb exposes the identical surface).
func (t *txn) Add(key fdb.KeyConvertible, param []byte)     { t.tr.Add(cgoKey(key), param) }
func (t *txn) And(key fdb.KeyConvertible, param []byte)     { t.tr.And(cgoKey(key), param) }
func (t *txn) BitAnd(key fdb.KeyConvertible, param []byte)  { t.tr.BitAnd(cgoKey(key), param) }
func (t *txn) Or(key fdb.KeyConvertible, param []byte)      { t.tr.Or(cgoKey(key), param) }
func (t *txn) BitOr(key fdb.KeyConvertible, param []byte)   { t.tr.BitOr(cgoKey(key), param) }
func (t *txn) Xor(key fdb.KeyConvertible, param []byte)     { t.tr.Xor(cgoKey(key), param) }
func (t *txn) BitXor(key fdb.KeyConvertible, param []byte)  { t.tr.BitXor(cgoKey(key), param) }
func (t *txn) Max(key fdb.KeyConvertible, param []byte)     { t.tr.Max(cgoKey(key), param) }
func (t *txn) Min(key fdb.KeyConvertible, param []byte)     { t.tr.Min(cgoKey(key), param) }
func (t *txn) ByteMax(key fdb.KeyConvertible, param []byte) { t.tr.ByteMax(cgoKey(key), param) }
func (t *txn) ByteMin(key fdb.KeyConvertible, param []byte) { t.tr.ByteMin(cgoKey(key), param) }
func (t *txn) AppendIfFits(key fdb.KeyConvertible, param []byte) {
	t.tr.AppendIfFits(cgoKey(key), param)
}

func (t *txn) CompareAndClear(key fdb.KeyConvertible, param []byte) {
	t.tr.CompareAndClear(cgoKey(key), param)
}

func (t *txn) SetVersionstampedKey(key fdb.KeyConvertible, param []byte) {
	t.tr.SetVersionstampedKey(cgoKey(key), param)
}

func (t *txn) SetVersionstampedValue(key fdb.KeyConvertible, param []byte) {
	t.tr.SetVersionstampedValue(cgoKey(key), param)
}

// []byte fast-path overloads — cgofdb has no []byte variants, so forward to the
// KeyConvertible form (cgofdb.Key is a []byte newtype, a zero-copy conversion).
func (t *txn) SetBytes(key, value []byte) { t.tr.Set(cgofdb.Key(key), value) }
func (t *txn) ClearBytes(key []byte)      { t.tr.Clear(cgofdb.Key(key)) }
func (t *txn) AddBytes(key, param []byte) { t.tr.Add(cgofdb.Key(key), param) }
func (t *txn) MaxBytes(key, param []byte) { t.tr.Max(cgofdb.Key(key), param) }
func (t *txn) MinBytes(key, param []byte) { t.tr.Min(cgofdb.Key(key), param) }
func (t *txn) CompareAndClearBytes(key, param []byte) {
	t.tr.CompareAndClear(cgofdb.Key(key), param)
}

// Conflict ranges.
func (t *txn) AddReadConflictRange(er fdb.ExactRange) error {
	return convErr(t.tr.AddReadConflictRange(cgoExactRange(er)))
}

func (t *txn) AddReadConflictKey(key fdb.KeyConvertible) error {
	return convErr(t.tr.AddReadConflictKey(cgoKey(key)))
}

func (t *txn) AddWriteConflictRange(er fdb.ExactRange) error {
	return convErr(t.tr.AddWriteConflictRange(cgoExactRange(er)))
}

func (t *txn) AddWriteConflictKey(key fdb.KeyConvertible) error {
	return convErr(t.tr.AddWriteConflictKey(cgoKey(key)))
}

// Lifecycle.
func (t *txn) Commit() fdb.FutureNil { return futureNil{t.tr.Commit()} }
func (t *txn) Cancel()               { t.tr.Cancel() }
func (t *txn) Reset()                { t.tr.Reset() }
func (t *txn) OnError(e fdb.Error) fdb.FutureNil {
	return futureNil{t.tr.OnError(cgofdb.Error{Code: e.Code})}
}
func (t *txn) SetReadVersion(version int64) { t.tr.SetReadVersion(version) }

// Post-commit.
func (t *txn) GetCommittedVersion() (int64, error) {
	v, e := t.tr.GetCommittedVersion()
	return v, convErr(e)
}
func (t *txn) GetVersionstamp() fdb.FutureKey      { return futureKey{t.tr.GetVersionstamp()} }
func (t *txn) GetApproximateSize() fdb.FutureInt64 { return futureInt64{t.tr.GetApproximateSize()} }

// ---- range result / iterator adapters ----

type rangeResult struct {
	rr cgofdb.RangeResult
}

func (r rangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	kvs, err := r.rr.GetSliceWithError()
	return fromCgoKeyValues(kvs), convErr(err)
}

func (r rangeResult) GetSliceOrPanic() []fdb.KeyValue {
	// Panic the converted fdb error, not cgofdb's raw one (see the future MustGet note).
	kvs, err := r.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return kvs
}

func (r rangeResult) Iterator() fdb.RangeIterator {
	return &rangeIterator{it: r.rr.Iterator()}
}

// rangeIterator translates cgofdb's iterator model to the fdb.RangeIterator
// contract, which the record-layer cursors rely on and which differs from
// cgofdb's:
//
//   - fdb model: Advance() moves to the next element and returns whether one
//     exists; Get() returns the CURRENT element idempotently and is SAFE after
//     Advance()==false, returning (zero, nil) on clean exhaustion or (zero, err)
//     on a stored error (the cursors call Get() post-loop to tell exhaustion from a
//     transient FDB error).
//   - cgofdb model: Get() returns the current element AND advances (not
//     idempotent), and Advance() returns *true* on a stored error (so Get() is
//     never called after Advance()==false; doing so panics with index-out-of-range
//     on clean exhaustion).
//
// So we drive cgofdb's Advance()+Get() pair once per fdb Advance(), buffering the
// one current element; fdb Get() just returns that buffer. The stored error is
// sticky and surfaced by Get(), never by indexing a spent batch.
type rangeIterator struct {
	it    *cgofdb.RangeIterator
	cur   fdb.KeyValue
	valid bool  // cur holds a live current element (set by Advance, cleared on exhaustion)
	err   error // sticky error, surfaced by Get() after Advance()==false
}

func (i *rangeIterator) Advance() bool {
	i.valid = false
	if i.err != nil {
		return false
	}
	if !i.it.Advance() {
		// cgofdb returns false ONLY on clean exhaustion (it returns true on a stored
		// error). Leave i.err nil so Get() reports (zero, nil).
		return false
	}
	// cgofdb Advance()==true ⟹ a current element OR a stored error; Get() yields it
	// (and advances cgofdb's index — which is why we call it exactly once here).
	kv, err := i.it.Get()
	if err != nil {
		i.err = convErr(err)
		return false
	}
	i.cur = fromCgoKeyValue(kv)
	i.valid = true
	return true
}

func (i *rangeIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	if !i.valid {
		return fdb.KeyValue{}, nil // before first Advance, or after exhaustion
	}
	return i.cur, nil
}

func (i *rangeIterator) MustGet() fdb.KeyValue {
	kv, err := i.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

// SetTraceLog is a no-op: cgofdb's iterator exposes no per-batch trace hook (that
// is a pure-Go-client debugging aid). The data is identical with or without it.
func (i *rangeIterator) SetTraceLog(func(iteration, requested, returned int, more bool, err error)) {
}

// ---- future adapters (cgofdb future -> fdb future) ----

// The MustGet adapters call their OWN Get() (which runs convErr) and panic the
// converted error — they must NOT delegate to cgofdb's MustGet, which panics a raw
// cgofdb.Error. A caller using the backend-agnostic fdb.Future interface and
// recovering/matching fdb.Error must see the same panic type on both backends; the
// pure-Go future panics the fdb-world error (future.go:66). Inside a Transact callback
// this fdb.Error panic is bridged back to cgofdb.Error by withCancelWatcher for
// cgofdb's retry classification.
type futureByteSlice struct{ f cgofdb.FutureByteSlice }

func (f futureByteSlice) Get() ([]byte, error) { v, e := f.f.Get(); return v, convErr(e) }
func (f futureByteSlice) MustGet() []byte {
	v, e := f.Get()
	if e != nil {
		panic(e)
	}
	return v
}
func (f futureByteSlice) BlockUntilReady() { f.f.BlockUntilReady() }
func (f futureByteSlice) IsReady() bool    { return f.f.IsReady() }
func (f futureByteSlice) Cancel()          { f.f.Cancel() }

type futureKey struct{ f cgofdb.FutureKey }

func (f futureKey) Get() (fdb.Key, error) { k, e := f.f.Get(); return fdb.Key(k), convErr(e) }
func (f futureKey) MustGet() fdb.Key {
	k, e := f.Get()
	if e != nil {
		panic(e)
	}
	return k
}
func (f futureKey) BlockUntilReady() { f.f.BlockUntilReady() }
func (f futureKey) IsReady() bool    { return f.f.IsReady() }
func (f futureKey) Cancel()          { f.f.Cancel() }

type futureInt64 struct{ f cgofdb.FutureInt64 }

func (f futureInt64) Get() (int64, error) { v, e := f.f.Get(); return v, convErr(e) }
func (f futureInt64) MustGet() int64 {
	v, e := f.Get()
	if e != nil {
		panic(e)
	}
	return v
}
func (f futureInt64) BlockUntilReady() { f.f.BlockUntilReady() }
func (f futureInt64) IsReady() bool    { return f.f.IsReady() }
func (f futureInt64) Cancel()          { f.f.Cancel() }

type futureNil struct{ f cgofdb.FutureNil }

func (f futureNil) Get() error { return convErr(f.f.Get()) }
func (f futureNil) MustGet() {
	if e := f.Get(); e != nil {
		panic(e)
	}
}
func (f futureNil) BlockUntilReady() { f.f.BlockUntilReady() }
func (f futureNil) IsReady() bool    { return f.f.IsReady() }
func (f futureNil) Cancel()          { f.f.Cancel() }

// futureKeyArray wraps cgofdb.FutureKeyArray. Note: unlike cgofdb's other future
// interfaces, FutureKeyArray does NOT embed cgofdb.Future, so the Future base
// methods are reached by asserting the concrete value (every cgofdb future embeds
// *future, which provides them) to cgofdb.Future.
type futureKeyArray struct{ f cgofdb.FutureKeyArray }

func (f futureKeyArray) Get() ([]fdb.Key, error) {
	ks, e := f.f.Get()
	return fromCgoKeys(ks), convErr(e)
}

func (f futureKeyArray) MustGet() []fdb.Key {
	ks, e := f.Get()
	if e != nil {
		panic(e)
	}
	return ks
}
func (f futureKeyArray) BlockUntilReady() { f.f.(cgofdb.Future).BlockUntilReady() }
func (f futureKeyArray) IsReady() bool    { return f.f.(cgofdb.Future).IsReady() }
func (f futureKeyArray) Cancel()          { f.f.(cgofdb.Future).Cancel() }

// ---- type conversions ----

func cgoKey(k fdb.KeyConvertible) cgofdb.KeyConvertible { return cgofdb.Key(k.FDBKey()) }

func cgoSelector(s fdb.Selectable) cgofdb.Selectable { return cgoKeySelector(s) }

func cgoKeySelector(s fdb.Selectable) cgofdb.KeySelector {
	ks := s.FDBKeySelector()
	return cgofdb.KeySelector{Key: cgofdb.Key(ks.Key.FDBKey()), OrEqual: ks.OrEqual, Offset: ks.Offset}
}

func cgoRange(r fdb.Range) cgofdb.Range {
	b, e := r.FDBRangeKeySelectors()
	return cgofdb.SelectorRange{Begin: cgoKeySelector(b), End: cgoKeySelector(e)}
}

func cgoExactRange(r fdb.ExactRange) cgofdb.KeyRange {
	b, e := r.FDBRangeKeys()
	return cgofdb.KeyRange{Begin: cgofdb.Key(b.FDBKey()), End: cgofdb.Key(e.FDBKey())}
}

func cgoRangeOptions(o fdb.RangeOptions) cgofdb.RangeOptions {
	return cgofdb.RangeOptions{
		Limit:   o.Limit,
		Mode:    cgofdb.StreamingMode(o.Mode), // identical enum values (-1..5)
		Reverse: o.Reverse,
	}
}

func fromCgoKeyValue(kv cgofdb.KeyValue) fdb.KeyValue {
	return fdb.KeyValue{Key: fdb.Key(kv.Key), Value: kv.Value}
}

func fromCgoKeyValues(kvs []cgofdb.KeyValue) []fdb.KeyValue {
	if kvs == nil {
		return nil
	}
	out := make([]fdb.KeyValue, len(kvs))
	for i, kv := range kvs {
		out[i] = fromCgoKeyValue(kv)
	}
	return out
}

func fromCgoKeys(ks []cgofdb.Key) []fdb.Key {
	if ks == nil {
		return nil
	}
	out := make([]fdb.Key, len(ks))
	for i, k := range ks {
		out[i] = fdb.Key(k)
	}
	return out
}

// cgoErrShim carries an fdb.Error's raw code so cgofdb's retryable() loop — and
// this package's database boundary — can recover it via errors.As(&cgofdb.Error),
// WHILE preserving the original (possibly %w-wrapped) record-layer error so its
// context survives the cgofdb round-trip on a terminal failure. Without it,
// toCgoErr would flatten a wrapped fdb.Error to a bare cgofdb.Error{Code} and the
// operator would lose the record layer's "save record: …" context that the pure-Go
// backend keeps (Torvalds review).
type cgoErrShim struct {
	code int
	orig error
}

func (e *cgoErrShim) Error() string { return e.orig.Error() }
func (e *cgoErrShim) Unwrap() error { return e.orig }

// As lets errors.As(&cgofdb.Error) succeed (cgofdb's retryable() depends on it)
// without putting a second fdb.Error in the chain that would shadow orig.
func (e *cgoErrShim) As(target any) bool {
	if p, ok := target.(*cgofdb.Error); ok {
		*p = cgofdb.Error{Code: e.code}
		return true
	}
	return false
}

// convErr maps an error surfacing FROM cgofdb back to the fdb world: our own shim
// round-tripped intact → the original fdb.Error (context preserved); a plain
// cgofdb.Error → fdb.Error with the same raw fdb_error_t code (so errors.As /
// retry classification is identical on this backend); anything else passes
// through. Nothing is synthesized.
func convErr(err error) error {
	if err == nil {
		return nil
	}
	var shim *cgoErrShim
	if errors.As(err, &shim) {
		return shim.orig
	}
	var ce cgofdb.Error
	if errors.As(err, &ce) {
		return fdb.Error{Code: ce.Code}
	}
	return err
}

// toCgoErr is the inverse, applied on the way OUT of a Transact/ReadTransact
// callback: an fdb.Error the record layer propagated back is wrapped in a shim so
// cgofdb's retryable() loop (errors.As(&cgofdb.Error)) still recognizes the
// retryable code and delegates to libfdb_c's OnError — and, if the failure is
// terminal, the original wrapped error (with its context) is what convErr returns.
// Non-FDB errors (record-layer semantic errors) pass through unchanged so cgofdb
// treats them as terminal — exactly as it would its own.
func toCgoErr(err error) error {
	if err == nil {
		return nil
	}
	var fe fdb.Error
	if errors.As(err, &fe) {
		return &cgoErrShim{code: fe.Code, orig: err}
	}
	return err
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
