// Portions derived from FoundationDB Record Layer (FDBRecordContext.java,
// FDBDatabase.java, FDBDatabaseRunner.java, FDBDatabaseFactory.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

// TransactionPriority controls the priority of FDB transactions.
// Matches Java's FDBTransactionPriority.
type TransactionPriority int

const (
	// PriorityDefault is the default transaction priority.
	PriorityDefault TransactionPriority = iota
	// PriorityBatch is a lower priority for background/batch operations.
	PriorityBatch
	// PrioritySystemImmediate is the highest priority, bypasses throttling.
	// Use with extreme care — only for system-level operations.
	PrioritySystemImmediate
)

// CommitCheckFunc is a pre-commit check that runs before the transaction commits.
// If it returns an error, the commit is aborted.
// Matches Java's CommitCheckAsync interface.
type CommitCheckFunc func() error

// PostCommitFunc is a callback that runs after a successful commit.
// Matches Java's PostCommit interface.
type PostCommitFunc func()

// commitCheckRegistration keeps a canceled check distinguishable from a new
// registration with the same name, including during a pre-commit snapshot.
type commitCheckRegistration struct {
	check  CommitCheckFunc
	active bool // protected by FDBRecordContext.commitMu
}

// nextTransactionID generates unique IDs for FDBRecordContext instances.
var nextTransactionID atomic.Int64

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
// Goroutine-safe: all mutable fields are protected by atomics, mutexes, or the
// lockRegistry. Multiple goroutines may safely operate on the same context
// within a single FDB transaction (matching Java's CompletableFuture model).
// Matches Java's FDBRecordContext.
type FDBRecordContext struct {
	tx            fdb.WritableTransaction
	ctx           context.Context
	transactionID int64 // unique ID for logging/tracing
	createTime    time.Time

	// routeOwned marks a context a transaction route (Run and its variants,
	// the runner's RunWithRetry) handed its body: the route commits it, and
	// the body's own commit is refused before anything commits.
	routeOwned bool
	// deactivated is set by the first commit, whatever its outcome (Java's
	// closeTransaction(false) on both arms of commitAsync); a later commit is
	// refused as Java's ensureActive refuses it.
	deactivated atomic.Bool

	// A successful direct commit detaches physical coordination from the
	// caller's cancellation owner, without changing immutable Context().
	directCommitOwned  atomic.Bool
	recordPhysical     recordPhysicalState
	recordPhysicalHook func(stage string, key []byte)

	// database is the FDBDatabase that opened this context (Java's
	// FDBRecordContext.getDatabase); nil for a context over an external
	// transaction built by NewFDBRecordContext.
	database *FDBDatabase

	// configure is reapplied to child transactions (Java's newRunner).
	configure TransactionConfigurer

	// env is the simulation environment inherited from the FDBDatabase (Clock + Random).
	// Nil means production; read it through Env() which is nil-safe. Persisted-byte
	// sites (store header LastUpdateTime, lock-state timestamp, heartbeats, nonces) route
	// their time/randomness through here so a simulation run is byte-reproducible.
	env *Env

	// Client-side transaction size thresholds. Zero = disabled.
	txSizeWarnBytes  int64
	txSizeErrorBytes int64
	txSizeWarned     atomic.Bool // only warn once per transaction

	// Version management — matches Java's FDBRecordContext.
	// Java uses AtomicInteger + ConcurrentSkipListMap.
	// Go uses atomic.Int32 + mutex-protected maps.
	localVersion      atomic.Int32               // per-transaction local version counter
	versionMu         sync.Mutex                 // protects localVersionCache + versionMutations
	localVersionCache map[string]int             // key (string) → local version (int)
	versionMutations  map[string]versionMutation // key (string) → mutation (type + value)

	// Commit hooks — matches Java's CommitCheckAsync / PostCommit.
	// Java uses synchronized blocks on all access.
	commitMu          sync.Mutex
	commitChecks      []*commitCheckRegistration
	namedCommitChecks map[string]*commitCheckRegistration
	postCommits       []PostCommitFunc

	// Diagnostic: tracked read conflict ranges for debugging
	conflictMu     sync.Mutex
	conflictRanges []fdb.KeyRange

	// Store state cache invalidation tracking — matches Java's FDBRecordContext.
	// Java leaves these unprotected (benign single-word writes).
	// Go uses atomics for race-detector cleanliness.
	dirtyStoreState           atomic.Bool // set when any store header or index state is modified
	dirtyMetaDataVersionStamp atomic.Bool // set when SetMetaDataVersionStamp() is called

	// Per-subspace read-write locks for tree-structured indexes (HNSW, R-tree).
	// Matches Java's LockRegistry on FDBRecordContext.
	locks lockRegistry

	// Instrumentation timer — matches Java's FDBRecordContext.timer field.
	// Set once during construction, nil means no instrumentation.
	// Atomic for race-detector cleanliness (concurrent reads from goroutines).
	timer atomic.Pointer[StoreTimer]

	// Session storage — matches Java's FDBRecordContext session
	// (getSession/putSessionIfAbsent): transaction-scoped values shared by
	// index maintainers across store instances opened in the same context.
	// SPFresh keeps its tx-local routing cache here so a same-transaction
	// write-then-search pairs up even when the statements open separate
	// stores.
	// Keys are strings or typed ContextSessionKey names (contextSessionKeyID),
	// which never equal a string key, as Java's ContextSessionKey never equals a
	// String.
	sessionMu sync.Mutex
	session   map[any]any

	pendingWriteQueueOptions atomic.Pointer[PendingWriteQueueOptions]

	indexStateMu    sync.Mutex
	indexStateViews map[string]*transactionIndexStateView
}

// Env returns the simulation environment for this context (Clock + Random),
// inherited from the FDBDatabase. Never nil in the sense that matters: the returned *Env
// may be nil, and the *Env accessors (Now/Since/Read) treat nil as production. Use this
// at every persisted-byte site instead of time.Now / crypto.rand so a simulation run is
// byte-reproducible.
func (rc *FDBRecordContext) Env() *Env {
	if rc == nil {
		return nil
	}
	return rc.env
}

// Session returns the transaction-scoped value stored under key, or nil.
// Matches Java's FDBRecordContext.getSession.
func (rc *FDBRecordContext) Session(key string) any {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	return rc.session[key]
}

// PutSession stores a transaction-scoped value under key. Matches Java's
// FDBRecordContext session storage; values die with the context.
func (rc *FDBRecordContext) PutSession(key string, value any) {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	if rc.session == nil {
		rc.session = make(map[any]any)
	}
	rc.session[key] = value
}

// NewFDBRecordContext wraps an externally-created FDB transaction — the path SQL's explicit
// BeginTx→COMMIT takes, plus direct-transaction-control tests.
//
// env is REQUIRED rather than defaulted, and that is the point. This is the sixth construction
// site of an FDBRecordContext; the other five (Run/RunAsync/ReadOnly in this file and the two
// in runner.go) all thread d.env, and this one silently did not. The consequence was that every
// explicit transaction ran with env==nil — production seams, wall clock, crypto/rand — even
// under a fully seeded simulation environment. That is precisely the path an explicit-COMMIT
// semantics RFC needs to replay, so it was the one path the seam did not cover. A default here
// would reintroduce the same silence; passing nil is allowed but has to be written down.
//
// Callers holding an *FDBDatabase should NOT call this — call db.NewRecordContext(tx),
// which threads every piece of per-database state at once. Enumerating the fields at each
// call site is what let env go unthreaded here in the first place, and a second such field
// (the StoreTimer) would have repeated it exactly.
func NewFDBRecordContext(tx fdb.WritableTransaction, env *Env) *FDBRecordContext {
	return &FDBRecordContext{
		createTime: env.Now(),
		tx:         tx,
		ctx:        context.Background(),
		env:        env,
	}
}

// NewRecordContext wraps an externally-created FDB transaction in a context that inherits
// everything this database gives the contexts it opens itself: the DST environment and the
// StoreTimer. It is the form every caller holding an *FDBDatabase should use; one with a
// context (SQL's explicit BeginTx→COMMIT path) uses NewRecordContextIn.
//
// The point is that per-database state is threaded in ONE place. NewFDBRecordContext takes
// the fields one by one, which is why env was silently dropped on the explicit-transaction
// path until someone noticed; adding the timer as a second such parameter would have set up
// the identical failure for instrumentation, where the symptom is even quieter (a metric
// that reads zero looks exactly like a workload that did nothing).
func (d *FDBDatabase) NewRecordContext(tx fdb.WritableTransaction) *FDBRecordContext {
	return d.NewRecordContextIn(context.Background(), tx)
}

// NewRecordContextIn is NewRecordContext with ctx as the context's Context(),
// which bounds the reads and child transactions that take it from there.
func (d *FDBDatabase) NewRecordContextIn(ctx context.Context, tx fdb.WritableTransaction) *FDBRecordContext {
	rc := NewFDBRecordContext(tx, d.Env())
	rc.ctx = ctx
	rc.SetTimer(d.Timer())
	rc.database = d
	return rc
}

// TransactionConfigurer is the opener's part of Java's FDBRecordContextConfig.
type TransactionConfigurer func(fdb.TransactionOptions) error

// Configure applies f and keeps it for child transactions, which Java's
// newRunner opens with the parent context's config.
func (rc *FDBRecordContext) Configure(f TransactionConfigurer) error {
	if f == nil {
		return nil
	}
	if err := f(rc.tx.Options()); err != nil {
		return err
	}
	rc.configure = f
	return nil
}

// runChild is parentContext.newRunner().run: its own commit, this context's config.
func (rc *FDBRecordContext) runChild(ctx context.Context, fn func(*FDBRecordContext) (any, error)) (any, error) {
	return rc.database.Run(ctx, func(child *FDBRecordContext) (any, error) {
		if err := child.Configure(rc.configure); err != nil {
			return nil, err
		}
		return fn(child)
	})
}

// GetDatabase is the database that opened this context, Java's
// FDBRecordContext.getDatabase; nil for a context NewFDBRecordContext built
// over an external transaction.
func (rc *FDBRecordContext) GetDatabase() *FDBDatabase {
	return rc.database
}

// Transaction returns the underlying FDB transaction
func (rc *FDBRecordContext) Transaction() fdb.WritableTransaction {
	return rc.tx
}

// ReadTransaction returns the transaction a READ should go through for the
// given isolation: the snapshot view (no read conflict ranges) when snapshot
// is true, the plain transaction (conflict-tracking) otherwise.
//
// This is the ONE place the snapshot/serializable choice is made, so that
// "serializable" is a property of the mechanism rather than of each leaf
// remembering to ask. Java has exactly this method —
// FDBRecordContext.readTransaction(boolean) (FDBRecordContext.java:660-666) —
// and every Java leaf resolves through it with the same expression:
// context.readTransaction(scanProperties.getExecuteProperties()
// .getIsolationLevel().isSnapshot()) (KeyValueCursorBase.java:358,
// VectorIndexMaintainer.java:201-202, TextIndexMaintainer.java:542,
// RankIndexMaintainer.java:292).
//
// A leaf that calls Transaction().Snapshot() directly instead is not making a
// local choice — it is silently overriding the caller's isolation, which reads
// as isolated and commits as non-serializable.
func (rc *FDBRecordContext) ReadTransaction(snapshot bool) fdb.ReadTransaction {
	return readTransactionFor(rc.tx, snapshot)
}

// readTransactionFor is the single implementation behind every
// snapshot-vs-serializable choice in the package. It is a pure function of the
// transaction and the flag — which is exactly what Java's
// FDBRecordContext.readTransaction is (`snapshot ? ensureActive().snapshot() :
// ensureActive()`), so siting it here rather than only on the context lets an
// index maintainer resolve isolation from the transaction it already holds
// without needing a context reference it may not have.
func readTransactionFor(tx fdb.WritableTransaction, snapshot bool) fdb.ReadTransaction {
	if snapshot {
		return tx.Snapshot()
	}
	return tx
}

// TransactionID returns a unique ID for this record context.
// Useful for logging and tracing. Matches Java's FDBRecordContext transaction ID.
func (rc *FDBRecordContext) TransactionID() int64 {
	return rc.transactionID
}

// Context returns the Go context
func (rc *FDBRecordContext) Context() context.Context {
	return rc.ctx
}

// GetApproximateTransactionSize returns the approximate size in bytes of the
// transaction's mutations so far. Useful for monitoring proximity to FDB's
// 10MB transaction size limit.
// Matches Java's FDBRecordContext.getApproximateTransactionSize().
func (rc *FDBRecordContext) GetApproximateTransactionSize() (int64, error) {
	return rc.tx.GetApproximateSize().Get()
}

// CheckTransactionSize returns TransactionSizeExceededError past the error threshold; past the
// warning threshold it logs and counts CountTransactionSizeWarning once per transaction.
func (rc *FDBRecordContext) CheckTransactionSize() error {
	if rc.txSizeWarnBytes == 0 && rc.txSizeErrorBytes == 0 {
		return nil
	}
	size, err := rc.GetApproximateTransactionSize()
	if err != nil {
		return err
	}
	if rc.txSizeErrorBytes > 0 && size >= rc.txSizeErrorBytes {
		return &TransactionSizeExceededError{CurrentBytes: size, LimitBytes: rc.txSizeErrorBytes}
	}
	if rc.txSizeWarnBytes > 0 && size >= rc.txSizeWarnBytes && rc.txSizeWarned.CompareAndSwap(false, true) {
		// A warning, not an error: the write that crossed the threshold succeeded.
		rc.Timer().Increment(CountTransactionSizeWarning)
		ctx := rc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		slog.Default().LogAttrs(ctx, slog.LevelWarn, "transaction size exceeds warning threshold",
			slog.Int64("current_bytes", size), slog.Int64("limit_bytes", rc.txSizeWarnBytes))
	}
	return nil
}

// TransactionSizeExceededError is returned when the approximate transaction
// size exceeds the configured error threshold. Callers should commit the
// current transaction and start a new one.
type TransactionSizeExceededError struct {
	CurrentBytes int64
	LimitBytes   int64
}

func (e *TransactionSizeExceededError) Error() string {
	return fmt.Sprintf("transaction size %d bytes exceeds limit %d bytes", e.CurrentBytes, e.LimitBytes)
}

// Commit runs pre-commit checks, flushes pending version mutations, commits the
// transaction, and runs post-commit callbacks. All explicit commit APIs share
// this lifecycle, matching Java's FDBRecordContext.commitAsync().
func (rc *FDBRecordContext) Commit() error {
	_, err := rc.CommitWithVersionstamp()
	return err
}

// CommitWithHooks is an alias for Commit; checks and hooks always run.
func (rc *FDBRecordContext) CommitWithHooks() error {
	return rc.Commit()
}

// CommitWithVersionstamp commits the transaction, first running pre-commit checks,
// then flushing all queued versionstamp mutations. Returns the committed
// versionstamp (10 bytes) or nil for read-only transactions / no versionstamp mutations.
// Runs post-commit hooks after successful commit.
// Matches Java's FDBRecordContext.commitAsync() which always runs checks and hooks.
func (rc *FDBRecordContext) CommitWithVersionstamp() ([]byte, error) {
	// Java's ensureActive, the first statement of commitAsync: a committed
	// context, and one its route owns, commits nothing.
	if rc.routeOwned || !rc.deactivated.CompareAndSwap(false, true) {
		return nil, errTransactionNotActive()
	}
	rc.directCommitOwned.Store(true)
	// Include pre-commit checks in the commit span, but not post-commit hooks.
	commitStart := time.Now()
	if err := rc.drainCommitChecksAndSealWrites(); err != nil {
		rc.tx.Cancel()
		rc.Timer().RecordSince(EventCommit, commitStart)
		return nil, err
	}
	rc.flushVersionMutations()

	// Only request versionstamp future if we actually queued versionstamp mutations.
	// Matches the pattern in RunWithVersionstamp.
	hasVersionMutations := rc.HasVersionMutations()
	var vsFuture fdb.FutureKey
	if hasVersionMutations {
		vsFuture = rc.tx.GetVersionstamp()
	}

	if err := rc.tx.Commit().Get(); err != nil {
		rc.tx.Cancel()
		rc.Timer().RecordSince(EventCommit, commitStart)
		return nil, err
	}

	// Java resolves the committed versionstamp before running post-commit hooks.
	var versionstamp []byte
	if hasVersionMutations && vsFuture != nil {
		vs, err := vsFuture.Get()
		if err != nil {
			rc.Timer().RecordSince(EventCommit, commitStart)
			return nil, fmt.Errorf("get versionstamp after commit: %w", err)
		}
		versionstamp = []byte(vs)
	}
	rc.Timer().RecordSince(EventCommit, commitStart)
	rc.runPostCommits()
	return versionstamp, nil
}

// Cancel cancels the transaction
func (rc *FDBRecordContext) Cancel() {
	rc.tx.Cancel()
}

// AddCommitCheck registers a pre-commit check function.
// All checks run before the transaction commits. If any returns an error,
// the commit is aborted with that error.
// Goroutine-safe via commitMu (matches Java's synchronized blocks).
// Matches Java's FDBRecordContext.addCommitCheck(CommitCheckAsync).
func (rc *FDBRecordContext) AddCommitCheck(check CommitCheckFunc) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	rc.commitChecks = append(rc.commitChecks, &commitCheckRegistration{check: check, active: true})
}

// GetOrCreateCommitCheck returns the named check, invoking ifNotExists only when
// absent. The supplier runs under the registration lock and must not call back
// into context hook registration. The supplied check runs outside that lock.
// Matches Java's FDBRecordContext.getOrCreateCommitCheck().
func (rc *FDBRecordContext) GetOrCreateCommitCheck(name string, ifNotExists func(string) CommitCheckFunc) CommitCheckFunc {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	if entry := rc.namedCommitChecks[name]; entry != nil {
		return entry.check
	}
	entry := &commitCheckRegistration{check: ifNotExists(name), active: true}
	if rc.namedCommitChecks == nil {
		rc.namedCommitChecks = make(map[string]*commitCheckRegistration)
	}
	rc.namedCommitChecks[name] = entry
	rc.commitChecks = append(rc.commitChecks, entry)
	return entry.check
}

// getCommitCheck returns a previously registered named check, or nil.
// Matches Java's FDBRecordContext.getCommitCheck().
func (rc *FDBRecordContext) getCommitCheck(name string) CommitCheckFunc {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	if entry := rc.namedCommitChecks[name]; entry != nil {
		return entry.check
	}
	return nil
}

// removeCommitCheck cancels a named check that has not started in the live
// commit drain. It does not stop an already executing check.
// Store deletion uses this before clearing data to prevent deferred retirement
// from recreating deleted index-state keys.
func (rc *FDBRecordContext) removeCommitCheck(name string) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	if entry := rc.namedCommitChecks[name]; entry != nil {
		entry.active = false
		delete(rc.namedCommitChecks, name)
	}
}

// removeCommitChecksWithPrefix cancels pending store-scoped callback families.
func (rc *FDBRecordContext) removeCommitChecksWithPrefix(prefix string) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	for name, entry := range rc.namedCommitChecks {
		if strings.HasPrefix(name, prefix) {
			entry.active = false
			delete(rc.namedCommitChecks, name)
		}
	}
}

// AddPostCommit registers a post-commit callback.
// All callbacks run after the transaction successfully commits.
// Goroutine-safe via commitMu (matches Java's synchronized blocks).
// Matches Java's FDBRecordContext.addPostCommit(PostCommit).
func (rc *FDBRecordContext) AddPostCommit(hook PostCommitFunc) {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	rc.postCommits = append(rc.postCommits, hook)
}

// runPendingCommitChecks drains registrations exactly once for one commit,
// including checks registered by checks or joined record writers.
func (rc *FDBRecordContext) runPendingCommitChecks(checked *int) error {
	for {
		rc.commitMu.Lock()
		if *checked == len(rc.commitChecks) {
			rc.commitMu.Unlock()
			return nil
		}
		entry := rc.commitChecks[*checked]
		*checked++
		active := entry.active
		rc.commitMu.Unlock()
		if active {
			if err := entry.check(); err != nil {
				return err
			}
		}
	}
}

// runPostCommits runs all registered post-commit callbacks.
// Called after commit when all goroutines should be done; holds commitMu
// for race-detector cleanliness.
func (rc *FDBRecordContext) runPostCommits() {
	rc.commitMu.Lock()
	hooks := make([]PostCommitFunc, len(rc.postCommits))
	copy(hooks, rc.postCommits)
	rc.commitMu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// GetReadVersion returns the transaction's read version.
// Matches Java's FDBRecordContext.getReadVersion().
func (rc *FDBRecordContext) GetReadVersion() (int64, error) {
	startTime := time.Now()
	v, err := rc.tx.GetReadVersion().Get()
	rc.Timer().RecordSince(EventGetReadVersion, startTime)
	return v, err
}

// SetReadVersion sets the transaction's read version explicitly.
// Matches Java's FDBRecordContext.setReadVersion().
func (rc *FDBRecordContext) SetReadVersion(version int64) {
	rc.tx.SetReadVersion(version)
}

// SetTransactionPriority sets the priority for this transaction.
// Matches Java's FDBRecordContext applying FDBTransactionPriority.
func (rc *FDBRecordContext) SetTransactionPriority(priority TransactionPriority) error {
	switch priority {
	case PriorityBatch:
		return rc.tx.Options().SetPriorityBatch()
	case PrioritySystemImmediate:
		return rc.tx.Options().SetPrioritySystemImmediate()
	default:
		return nil // Default priority, no option needed
	}
}

// GetConflictingKeys attempts to identify conflicting keys after a commit failure.
// Reads the transaction's read conflict ranges. This is a best-effort diagnostic tool.
// Goroutine-safe via conflictMu.
// Matches Java's FDBRecordContext.reportConflictingKeys() (diagnostic, not exact).
func (rc *FDBRecordContext) GetConflictingKeys() []fdb.KeyRange {
	rc.conflictMu.Lock()
	defer rc.conflictMu.Unlock()
	result := make([]fdb.KeyRange, len(rc.conflictRanges))
	copy(result, rc.conflictRanges)
	return result
}

// AddReadConflictRange adds a read conflict range and tracks it for diagnostics.
// Goroutine-safe via conflictMu (FDB transaction is already goroutine-safe).
func (rc *FDBRecordContext) AddReadConflictRange(r fdb.ExactRange) error {
	if err := rc.tx.AddReadConflictRange(r); err != nil {
		return err
	}
	begin, end := r.FDBRangeKeys()
	rc.conflictMu.Lock()
	rc.conflictRanges = append(rc.conflictRanges, fdb.KeyRange{
		Begin: begin.FDBKey(),
		End:   end.FDBKey(),
	})
	rc.conflictMu.Unlock()
	return nil
}

// Timer returns the instrumentation timer for this context, or nil if not set.
// Goroutine-safe via atomic.Pointer.
// Matches Java's FDBRecordContext.getTimer().
// Nil-safe on the receiver, for the same reason FDBRecordContext.Env is: every
// StoreTimer method tolerates a nil timer, so "no instrumentation" is a supported
// state all the way down — and an accessor at the head of that chain must not be
// the one call in it that panics. Call sites are of the form
// store.context.Timer().RecordSince(...), where the nil check would otherwise have
// to be repeated at each one.
func (rc *FDBRecordContext) Timer() *StoreTimer {
	if rc == nil {
		return nil
	}
	return rc.timer.Load()
}

// SetTimer sets the instrumentation timer for this context.
// Goroutine-safe via atomic.Pointer.
// Matches Java's FDBRecordContext.setTimer().
func (rc *FDBRecordContext) SetTimer(timer *StoreTimer) {
	rc.timer.Store(timer)
}
