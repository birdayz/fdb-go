package simfdb

import (
	"sync"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// SimDB is a deterministic, in-memory MVCC FoundationDB backend — the third
// fdb.BackendDatabase alongside the pure-Go client and libfdb_c (RFC-179 Tier 1). It mints
// versions from a monotonic logical counter, resolves serializable-snapshot-isolation
// conflicts in-process, and stores data in a sorted keyspace, so the whole record +
// relational stack runs single-goroutine, no cluster, no Docker, reproducibly.
//
// Commits are serialized: the whole commit (conflict resolution + version assignment + apply)
// holds db.mu, so there is one committing transaction at a time and each gets one monotonic
// commit version. This sidesteps FDB's batch/MiniConflictSet intra-batch path (RFC-179 Tier 1
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
}

// committedWrites records one committed transaction's write-conflict ranges at its version.
type committedWrites struct {
	version int64
	ranges  []keyRange
}

// New returns an empty SimDB. env supplies the Buggify fault points for commit-time injection
// (nil = production, no faults). The sim clock/randomness in env are for the record-layer
// persisted-byte sites (Tier 0), not the store itself, which uses only logical versions.
func New(env *dst.Env) *SimDB {
	return &SimDB{store: &mvccStore{}, env: env}
}

// Compile-time assertion that SimDB satisfies the backend contract.
var _ fdb.BackendDatabase = (*SimDB)(nil)

// currentReadVersion returns the version a new transaction reads at: the highest committed
// version. Caller holds db.mu.
func (db *SimDB) currentReadVersion() int64 { return db.lastVersion }

// Transact runs fn inside a standalone writable transaction with no retry loop. The record
// layer's own runner (recordlayer.Run / FDBDatabaseRunner) drives retries on OnError, exactly
// as with the real backends, so the backend Transact itself does not retry.
func (db *SimDB) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	tx := db.newTxn(false)
	result, err := fn(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit().Get(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadTransact runs fn inside a read-only transaction (no commit needed).
func (db *SimDB) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	tx := db.newTxn(false)
	return fn(tx)
}

// CreateWritableTransaction returns a standalone, non-retry writable transaction whose
// lifecycle the caller owns (Commit / Cancel) — the interface path used by SQL BeginTx,
// explicit transactions, and the FDBDatabaseRunner (RFC-179 Q1: load-bearing for SimFDB).
func (db *SimDB) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	return db.newTxn(false), nil
}

// LocalityGetBoundaryKeys returns shard boundaries within r. SimFDB is a single logical shard,
// so v1 returns no interior boundaries (the whole range is one shard). This satisfies the
// interface method that the constructor's capability check requires (RFC-179 Q1); real shard
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
