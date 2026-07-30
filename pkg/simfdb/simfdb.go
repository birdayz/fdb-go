package simfdb

import (
	"errors"
	"sync"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// maxRetries bounds Transact's retry loop so a Buggify point that keeps injecting a retryable
// fault cannot spin forever. Real FDB bounds retries by a wall-clock timeout; the sim has no
// wall clock, so a generous attempt count serves the same backstop.
const maxRetries = 100

// SimDB is a deterministic, in-memory MVCC FoundationDB backend — the third
// fdb.BackendDatabase alongside the pure-Go client and libfdb_c (RFC-199 Tier 1). It mints
// versions from a monotonic logical counter, resolves serializable-snapshot-isolation
// conflicts in-process, and stores data in a sorted keyspace, so the whole record +
// relational stack runs single-goroutine, no cluster, no Docker, reproducibly.
//
// Commits are serialized: the whole commit (conflict resolution + version assignment + apply)
// holds db.mu, so there is one committing transaction at a time and each gets one monotonic
// commit version. This sidesteps FDB's batch/MiniConflictSet intra-batch path (RFC-199 Tier 1
// item 1) — any serial order is a valid batch order, so the verdict is identical.
type SimDB struct {
	mu          sync.Mutex
	store       *mvccStore
	lastVersion int64 // highest committed version; read version = this, commit = ++this
	env         *dst.Env
	closed      bool

	// recentWrites is the log of committed transactions' write-conflict ranges, keyed by
	// commit version — the FDB resolver's model. SSI resolution checks a committing
	// transaction's read conflict ranges against these (not the store's write history), which
	// correctly excludes no-write-conflict writes (the versionstamp path) and includes ranges
	// added via AddWriteConflictRange without a data write. Never GC'd in v1 (full history is
	// strictly safer — see mvccStore).
	recentWrites []committedWrites

	// pendingInject, when non-zero, forces the next commit to return that FDB error code — a
	// deterministic, targeted fault (the analog of chaos.InjectOnce but with SimFDB's TRUE
	// rollback). A retryable code (1020/1007) fires BEFORE the mutations apply; commit_unknown
	// (1021) resolves to one of its two branches — applied or discarded — so the caller sees an
	// ambiguous result over a store that either did or did not take the write, the exact
	// non-idempotency surface. Cleared after firing once.
	pendingInject int
	injectQueue   []int // faults scheduled for successive commits (InjectSequence)

	// lastUnknownApplied records which branch the most recent commit_unknown_result took. See
	// LastCommitUnknownApplied().
	lastUnknownApplied bool
}

// LastCommitUnknownApplied reports, for the most recent commit that returned
// commit_unknown_result(1021), whether that commit's mutations were applied.
//
// This is SIMULATOR GROUND TRUTH. No FDB client can learn it — the impossibility is the entire
// reason the error exists — and it is exposed here for one purpose: a model-based oracle has to
// predict the store's contents, and after a 1021 the contents depend on a branch the seed chose.
// A harness that could not ask would have to either guess (and start certifying wrong verdicts
// again) or drop the fault dimension.
//
// It is an ORACLE input, never a control input: nothing in the system under test may consult it
// to decide what to do, or the run stops modelling a real client.
func (db *SimDB) LastCommitUnknownApplied() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.lastUnknownApplied
}

// injectCommitUnknownApplied / injectCommitUnknownDiscarded are the two BRANCH-EXPLICIT forms
// of commit_unknown_result. They are not FDB error codes — they are sentinels understood only
// by the injection queue, which turns each into a 1021 with the named outcome. Plain 1021 lets
// the per-seed coin choose; these pin a branch so a test can assert one specific reality.
//
// The values sit far outside FDB's error-code space so a mis-plumbed sentinel surfaces as an
// obviously bogus code rather than a plausible one.
const (
	injectCommitUnknownApplied   = -1021
	injectCommitUnknownDiscarded = -1022
)

// CommitUnknownApplied and CommitUnknownDiscarded name the two real outcomes behind
// commit_unknown_result(1021): the mutations were durable but the answer was lost, or the
// commit never reached the proxy. Pass either to InjectOnce/InjectSequence to pin the branch;
// a plain 1021 leaves the choice to the run's seed.
//
// Both are real FDB. A harness that only ever exercises one of them is not testing 1021
// handling, it is testing half of it.
const (
	CommitUnknownApplied   = injectCommitUnknownApplied
	CommitUnknownDiscarded = injectCommitUnknownDiscarded
)

// InjectOnce schedules fault code to be returned by the NEXT commit. Deterministic (no seed):
// 1020 (not_committed) / 1007 (transaction_too_old) fire before apply; 1021 (commit_unknown)
// resolves its applied/discarded branch by the run's seeded coin, or by the branch named with
// CommitUnknownApplied / CommitUnknownDiscarded. Use to reproduce a precise fault at a chosen
// point when hunting real record-layer retry/idempotency bugs.
func (db *SimDB) InjectOnce(code int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pendingInject = code
}

// InjectSequence schedules faults for successive commits (codes[0] on the next commit, codes[1]
// on the one after, etc.). A zero in the sequence means "no fault on that commit". Lets a hunt
// drive a whole workload through a fixed fault schedule.
func (db *SimDB) InjectSequence(codes ...int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.injectQueue = append(db.injectQueue, codes...)
}

// takeInject returns the fault to apply to this commit (pendingInject, else the head of
// injectQueue), consuming it. Caller holds db.mu.
func (db *SimDB) takeInject() int {
	if db.pendingInject != 0 {
		c := db.pendingInject
		db.pendingInject = 0
		return c
	}
	if len(db.injectQueue) > 0 {
		c := db.injectQueue[0]
		db.injectQueue = db.injectQueue[1:]
		return c
	}
	return 0
}

// committedWrites records one committed transaction's write-conflict ranges at its version.
type committedWrites struct {
	version int64
	ranges  []keyRange
}

// simDefaultAPIVersion is the API version a SimDB selects when the process hasn't already
// chosen one. It matches the record layer's wire version (7.3 → 730); the versionstamp/tuple
// layer keys the placeholder-offset encoding off fdb.GetAPIVersion() exactly as against a real
// cluster (apiVersion >= 520 strips the trailing offset — see conflict.go:stampVersionstamp).
const simDefaultAPIVersion = 730

// New returns an empty SimDB. env supplies the Buggify fault points for commit-time injection
// (nil = production, no faults). The sim clock/randomness in env are for the record-layer
// persisted-byte sites (Tier 0), not the store itself, which uses only logical versions.
//
// A SimDB stands in for an *opened* FDB database, and fdb.OpenDatabase refuses to construct one
// without a selected API version (api_version_unset, 2200) — the versionstamp path reads that
// global. Since New cannot return an error, it enforces the same precondition by selecting the
// default version when the process hasn't already chosen one (idempotent; an already-selected
// version is respected). Without this, the first versionstamp/VERSION-index write over SimFDB
// fails 2200 — a fidelity gap surfaced by the DST hunt (pkg/simfdb/hunt).
func New(env *dst.Env) *SimDB {
	if !fdb.IsAPIVersionSelected() {
		fdb.MustAPIVersion(simDefaultAPIVersion)
	}
	return &SimDB{store: &mvccStore{}, env: env}
}

// Compile-time assertion that SimDB satisfies the backend contract.
var _ fdb.BackendDatabase = (*SimDB)(nil)

// currentReadVersion returns the version a new transaction reads at: the highest committed
// version. Caller holds db.mu.
func (db *SimDB) currentReadVersion() int64 { return db.lastVersion }

// Transact runs fn inside a writable transaction with the retry loop the real backends'
// Transact also provides (recordlayer.Run delegates to it): run fn, commit, and on a retryable
// error (conflict 1020 / too_old 1007 / commit_unknown 1021) reset and retry, up to maxRetries.
// A non-retryable error or exhausted retries propagates. This is what makes SimFDB a drop-in
// backend — the record layer relies on Transact retrying, exactly as with the pure-Go client.
func (db *SimDB) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	tx := db.newTxn()
	for attempt := 0; ; attempt++ {
		result, err := fn(tx)
		if err == nil {
			err = tx.Commit().Get()
		}
		if err == nil {
			return result, nil
		}
		var fe fdb.Error
		if attempt < maxRetries && errors.As(err, &fe) && fdb.IsOnErrorRetryable(fe.Code) {
			// Route the retry through OnError, not a bare Reset: OnError is where the
			// MAYBE_COMMITTED write→read conflict promotion lives, and a retry loop that
			// skips it re-applies a possibly-already-durable transaction instead of
			// conflicting on it. The real backends' Transact loops call OnError too.
			if oerr := tx.OnError(fe).Get(); oerr != nil {
				return nil, oerr
			}
			continue
		}
		return nil, err
	}
}

// ReadTransact runs fn inside a read-only transaction. Reads retry on a retryable error for
// symmetry with Transact, though the sim's synchronous reads rarely produce one.
func (db *SimDB) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	tx := db.newTxn()
	for attempt := 0; ; attempt++ {
		result, err := fn(tx)
		if err == nil {
			return result, nil
		}
		var fe fdb.Error
		if attempt < maxRetries && errors.As(err, &fe) && fdb.IsOnErrorRetryable(fe.Code) {
			tx.Reset()
			continue
		}
		return nil, err
	}
}

// CreateWritableTransaction returns a standalone, non-retry writable transaction whose
// lifecycle the caller owns (Commit / Cancel) — the interface path used by SQL BeginTx,
// explicit transactions, and the FDBDatabaseRunner (RFC-199 Q1: load-bearing for SimFDB).
func (db *SimDB) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	return db.newTxn(), nil
}

// LocalityGetBoundaryKeys returns shard boundaries within r. SimFDB is a single logical shard,
// so v1 returns no interior boundaries (the whole range is one shard). This satisfies the
// interface method that the constructor's capability check requires (RFC-199 Q1); real shard
// boundaries are deferrable (online MUTUAL indexing then builds a single fragment).
func (db *SimDB) LocalityGetBoundaryKeys(_ fdb.ExactRange, _ int, _ int64) ([]fdb.Key, error) {
	return nil, nil
}

// Close releases the store. The sim holds no OS resources, so this only marks the db closed.
func (db *SimDB) Close() {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.closed = true
}
