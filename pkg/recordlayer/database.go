package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// FDBDatabase provides access to the underlying FoundationDB database or tenant
// and manages transaction execution with retry logic.
// This is the Record Layer equivalent of Java's FDBDatabase.
//
// The transactor field can be either an fdb.Database or fdb.Tenant, both of which
// implement the fdb.Transactor interface. This allows transparent support for both
// regular database operations and tenant-isolated operations.
type FDBDatabase struct {
	transactor fdb.Transactor
	// Keep original db/tenant for CreateTransaction which isn't on Transactor interface
	db     fdb.Database
	tenant fdb.Tenant
}

// NewFDBDatabase creates a new FDBDatabase wrapping the core FDB database
func NewFDBDatabase(db fdb.Database) *FDBDatabase {
	return &FDBDatabase{
		transactor: db,
		db:         db,
	}
}

// NewFDBDatabaseFromTenant creates a new FDBDatabase wrapping an FDB tenant
// for tenant-isolated operations. All operations will be scoped to the tenant's keyspace.
func NewFDBDatabaseFromTenant(tenant fdb.Tenant) *FDBDatabase {
	return &FDBDatabase{
		transactor: tenant,
		tenant:     tenant,
	}
}

// Run executes a function within a transaction with automatic retry handling.
// Before committing, flushes any queued SET_VERSIONSTAMPED_VALUE mutations.
// Matches Java's FDBRecordContext.commitAsync() behavior.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (interface{}, error)) (interface{}, error) {
	return d.transactor.Transact(func(tx fdb.Transaction) (interface{}, error) {
		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// Flush queued version mutations before FDB's Transact commits.
		recordCtx.flushVersionMutations()

		return result, nil
	})
}

// RunWithVersionstamp is like Run but also returns the committed versionstamp.
// Use this when you need the versionstamp after commit (e.g. for record versioning).
// Returns (result, versionstamp, error). Versionstamp is nil for read-only transactions.
func (d *FDBDatabase) RunWithVersionstamp(ctx context.Context, fn func(rtx *FDBRecordContext) (interface{}, error)) (interface{}, []byte, error) {
	var vsFuture fdb.FutureKey
	var hasVersionMutations bool

	result, err := d.transactor.Transact(func(tx fdb.Transaction) (interface{}, error) {
		// Reset on retry — previous attempt's future is stale
		vsFuture = nil
		hasVersionMutations = false

		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		recordCtx.flushVersionMutations()

		hasVersionMutations = recordCtx.HasVersionMutations()
		if hasVersionMutations {
			vsFuture = tx.GetVersionstamp()
		}

		return result, nil
	})
	if err != nil {
		return nil, nil, err
	}

	if hasVersionMutations && vsFuture != nil {
		vs, err := vsFuture.Get()
		if err != nil {
			return result, nil, fmt.Errorf("failed to get versionstamp: %w", err)
		}
		return result, []byte(vs), nil
	}

	return result, nil, nil
}

// CreateTransaction creates a new transaction without retry logic.
// This is primarily used for testing scenarios where manual transaction control is needed,
// such as testing isolation levels with concurrent transactions.
// For tenant-isolated databases, the transaction will be scoped to the tenant's keyspace.
func (d *FDBDatabase) CreateTransaction() (fdb.Transaction, error) {
	if d.tenant != (fdb.Tenant{}) {
		return d.tenant.CreateTransaction()
	}
	return d.db.CreateTransaction()
}

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
// Matches Java's FDBRecordContext.
type FDBRecordContext struct {
	tx  fdb.Transaction
	ctx context.Context

	// Version management — matches Java's FDBRecordContext
	localVersion      atomic.Int32 // per-transaction local version counter
	localVersionCache sync.Map     // key (string) → local version (int)
	versionMutations  sync.Map     // key (string) → value ([]byte) for SET_VERSIONSTAMPED_VALUE
}

// NewFDBRecordContext creates a new FDBRecordContext wrapping an FDB transaction.
// This is primarily used for testing scenarios where direct transaction control is needed.
func NewFDBRecordContext(tx fdb.Transaction) *FDBRecordContext {
	return &FDBRecordContext{
		tx:  tx,
		ctx: context.Background(),
	}
}

// Transaction returns the underlying FDB transaction
func (rc *FDBRecordContext) Transaction() fdb.Transaction {
	return rc.tx
}

// Context returns the Go context
func (rc *FDBRecordContext) Context() context.Context {
	return rc.ctx
}

// Commit commits the transaction
func (rc *FDBRecordContext) Commit() error {
	return rc.tx.Commit().Get()
}

// Cancel cancels the transaction
func (rc *FDBRecordContext) Cancel() {
	rc.tx.Cancel()
}

// ClaimLocalVersion atomically claims the next local version number.
// Matches Java's FDBRecordContext.claimLocalVersion().
func (rc *FDBRecordContext) ClaimLocalVersion() int {
	return int(rc.localVersion.Add(1) - 1) // returns 0, 1, 2, ...
}

// AddToLocalVersionCache caches a local version for a version key within this transaction.
// Matches Java's FDBRecordContext.addToLocalVersionCache().
func (rc *FDBRecordContext) AddToLocalVersionCache(versionKey []byte, localVersion int) {
	rc.localVersionCache.Store(string(versionKey), localVersion)
}

// GetLocalVersion retrieves a cached local version for the given key.
// Returns (localVersion, true) if found, (0, false) otherwise.
func (rc *FDBRecordContext) GetLocalVersion(versionKey []byte) (int, bool) {
	v, ok := rc.localVersionCache.Load(string(versionKey))
	if !ok {
		return 0, false
	}
	return v.(int), true
}

// RemoveLocalVersion removes a cached local version entry.
func (rc *FDBRecordContext) RemoveLocalVersion(versionKey []byte) {
	rc.localVersionCache.Delete(string(versionKey))
}

// AddVersionMutation queues a SET_VERSIONSTAMPED_VALUE mutation to be applied at commit.
// The value must include the versionstamp placeholder bytes.
// Matches Java's FDBRecordContext.addVersionMutation().
func (rc *FDBRecordContext) AddVersionMutation(versionKey []byte, value []byte) {
	rc.versionMutations.Store(string(versionKey), value)
}

// RemoveVersionMutation removes a queued version mutation.
func (rc *FDBRecordContext) RemoveVersionMutation(versionKey []byte) {
	rc.versionMutations.Delete(string(versionKey))
}

// flushVersionMutations applies all queued SET_VERSIONSTAMPED_VALUE mutations
// to the underlying FDB transaction. Called before commit.
func (rc *FDBRecordContext) flushVersionMutations() {
	rc.versionMutations.Range(func(key, value any) bool {
		keyBytes := []byte(key.(string))
		valueBytes := value.([]byte)
		rc.tx.SetVersionstampedValue(fdb.Key(keyBytes), valueBytes)
		return true
	})
}

// HasVersionMutations returns true if there are pending version mutations.
func (rc *FDBRecordContext) HasVersionMutations() bool {
	has := false
	rc.versionMutations.Range(func(_, _ any) bool {
		has = true
		return false // stop after first
	})
	return has
}

// CommitWithVersionstamp commits the transaction, first flushing all queued
// SET_VERSIONSTAMPED_VALUE mutations. Returns the committed versionstamp
// (10 bytes) or nil for read-only transactions / no versionstamp mutations.
// Use this instead of Commit() when you need the versionstamp after commit.
func (rc *FDBRecordContext) CommitWithVersionstamp() ([]byte, error) {
	rc.flushVersionMutations()

	// Get the versionstamp future BEFORE committing
	vsFuture := rc.tx.GetVersionstamp()

	// Commit the transaction
	if err := rc.tx.Commit().Get(); err != nil {
		return nil, err
	}

	// Retrieve the committed versionstamp
	vs, err := vsFuture.Get()
	if err != nil {
		// Read-only transactions don't have a versionstamp
		return nil, nil
	}

	return []byte(vs), nil
}

// buildVersionstampedValue builds the value for SET_VERSIONSTAMPED_VALUE mutation.
// Format: 12-byte version (with 0xFF placeholder in global portion) + 4-byte offset (little-endian).
// The offset (0) tells FDB where in the value to write the versionstamp.
func buildVersionstampedValue(version *FDBRecordVersion) []byte {
	buf := make([]byte, VersionBytes+4)
	copy(buf, version.ToBytes())
	// Offset = 0: versionstamp goes at the beginning of the value
	binary.LittleEndian.PutUint32(buf[VersionBytes:], 0)
	return buf
}