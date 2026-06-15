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
//   - OnError / retry is delegated to libfdb_c, exactly as the RFC requires:
//     cgofdb.Database.Transact runs the retry loop and calls Transaction.OnError
//     (fdb_transaction_on_error) itself. We do not re-implement retry on this path.
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
// fdb.CtxTransactor / fdb.CtxReadTransactor so the record layer's runTransactCtx
// honors a caller context on this backend (not just the pure-Go one): cgofdb owns
// the retry loop, but we (a) bail before each attempt if the ctx is done and
// (b) run a cancel watcher that aborts an in-flight read when the ctx is canceled OR
// its deadline expires. The watcher is the cgo analog of the pure-Go client threading
// ctx into each read RPC; like pure-Go, the auto-commit is detached from the caller
// ctx — the watcher is stopped before it and we impose no FDB transaction timeout, so
// neither a cancel nor a deadline that lands after the callback returned can abort the
// commit (cf. client/transaction.go:1437, the WithoutCancel commit). See
// withCancelWatcher.
type database struct {
	db          cgofdb.Database
	clusterFile string
	closed      atomic.Bool
}

func (d *database) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), f)
}

func (d *database) TransactCtx(ctx context.Context, f func(fdb.WritableTransaction) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, e := d.db.Transact(func(ctr cgofdb.Transaction) (any, error) {
		// Bail before each cgofdb retry attempt if the ctx is already done, so a
		// canceled/expired context cannot keep retrying (this closure re-runs per
		// attempt). The deadline is enforced for reads by the cancel watcher, not by
		// an FDB timeout — see withCancelWatcher.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rr, ee := withCancelWatcher(ctx, ctr.Cancel, func() (any, error) {
			return f(&txn{reader: reader{rt: ctr, opts: ctr.Options()}, tr: ctr})
		})
		if ee == nil {
			// Match the pure-Go Transact (client/database.go:645): a cancellation or
			// deadline that arrived DURING the callback aborts BEFORE cgofdb's
			// auto-commit. Without this the same Run(ctx,…) would commit on the cgo
			// backend where the pure-Go backend aborts. (ctx.Err() is not an
			// fdb.Error, so cgofdb's retryable() returns it terminal — no commit, no
			// retry.) A non-nil callback error takes precedence and is handled below.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
		}
		// Re-wrap fdb.Error back to cgofdb.Error so cgofdb's retryable() loop
		// (errors.As(&cgofdb.Error)) still recognizes a retryable code the record
		// layer propagated up — preserving libfdb_c's OnError retry delegation.
		return rr, toCgoErr(ee)
	})
	// Mirror the pure-Go TransactCtx (client/database.go:660-667): return a result ONLY
	// on success. cgofdb hands back the callback's value even when the run errored (a
	// callback that returned (v, err), or a commit that failed after the callback) — but
	// the transaction did not commit, so surfacing that value would leak an uncommitted
	// result through recordlayer.Run. On any error, return nil.
	if e != nil {
		return nil, mapTransactErr(ctx, e)
	}
	return r, nil
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, e := d.db.ReadTransact(func(crt cgofdb.ReadTransaction) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rr, ee := withCancelWatcher(ctx, crt.Cancel, func() (any, error) {
			return f(reader{rt: crt, opts: crt.Options()})
		})
		return rr, toCgoErr(ee)
	})
	// Mirror the pure-Go ReadTransactCtx (client/database.go:673-688): return a result
	// ONLY on success. cgofdb hands back the callback's value even when the run errored (a
	// callback that returned (v, err), or a failed read), so surfacing it would report a
	// result for a read that did not complete. On any error, return nil. (Reads never
	// commit — same nil-on-error shape as the write path.)
	if e != nil {
		return nil, mapTransactErr(ctx, e)
	}
	return r, nil
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
	// Defer so the watcher is stopped and joined even if fn panics (else the goroutine
	// leaks for a deadline-less ctx), and so an error panic becomes a returned error
	// rather than escaping cgofdb. Mirrors the pure-Go backend's panicToError
	// (fdb/transaction.go:506), which recovers the full error interface — Apple's binding
	// only recovers cgofdb.Error and re-throws everything else, so a non-fdb error panic
	// here would otherwise crash the process instead of Transact returning it.
	defer func() {
		close(stop)
		<-exited // join: the watcher has exited (and decided not to cancel) before we return
		if p := recover(); p != nil {
			if err, ok := p.(error); ok {
				var fe fdb.Error
				if errors.As(err, &fe) {
					// An fdb.Error panic (e.g. an adapter MustGet on a conflict): re-panic
					// as cgofdb.Error so cgofdb's wrapped() panicToError recovers it with a
					// code its retryable() classifier still recognizes for the retry loop.
					panic(cgofdb.Error{Code: fe.Code})
				}
				// A non-FDB error panic (a network/context error, or an app error a MustGet
				// surfaced): RETURN it as the error. cgofdb's panicToError re-throws a
				// non-cgofdb.Error panic, which would escape Transact and crash; the pure-Go
				// backend instead recovers and returns it, so match that.
				e = err
				return
			}
			panic(p) // a non-error panic re-panics (matches both pure-Go and Apple's binding)
		}
	}()
	return fn()
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
	r2, e := r.rt.ReadTransact(func(crt cgofdb.ReadTransaction) (any, error) {
		// Inherit r.opts: crt is the same underlying transaction (or a snapshot of
		// it), so its options are r's — and a snapshot's Options() would overflow.
		rr, ee := f(reader{rt: crt, opts: r.opts})
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
