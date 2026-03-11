package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
// Before committing, flushes any queued versionstamp mutations.
// Matches Java's FDBRecordContext.commitAsync() behavior.
func (d *FDBDatabase) Run(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastCtx *FDBRecordContext
	result, err := d.transactor.Transact(func(tx fdb.Transaction) (any, error) {
		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// Run pre-commit checks before flushing
		if err := recordCtx.runCommitChecks(); err != nil {
			return nil, err
		}

		// Flush queued version mutations before FDB's Transact commits.
		recordCtx.flushVersionMutations()

		return result, nil
	})
	if err != nil {
		return result, err
	}

	// Run post-commit callbacks after successful commit
	if lastCtx != nil {
		lastCtx.runPostCommits()
	}
	return result, nil
}

// RunWithVersionstamp is like Run but also returns the committed versionstamp.
// Use this when you need the versionstamp after commit (e.g. for record versioning).
// Returns (result, versionstamp, error). Versionstamp is nil for read-only transactions.
func (d *FDBDatabase) RunWithVersionstamp(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, []byte, error) {
	var vsFuture fdb.FutureKey
	var hasVersionMutations bool
	var lastCtx *FDBRecordContext

	result, err := d.transactor.Transact(func(tx fdb.Transaction) (any, error) {
		// Reset on retry — previous attempt's future is stale
		vsFuture = nil
		hasVersionMutations = false

		recordCtx := &FDBRecordContext{
			tx:  tx,
			ctx: ctx,
		}
		lastCtx = recordCtx

		result, err := fn(recordCtx)
		if err != nil {
			return nil, err
		}

		// Run pre-commit checks
		if err := recordCtx.runCommitChecks(); err != nil {
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

	// Run post-commit callbacks after successful commit
	if lastCtx != nil {
		lastCtx.runPostCommits()
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

// VersionMutationType represents the type of versionstamp mutation.
// Matches Java's MutationType used in FDBRecordContext.addVersionMutation().
type VersionMutationType int

const (
	// MutationTypeSetVersionstampedValue queues a SET_VERSIONSTAMPED_VALUE mutation.
	MutationTypeSetVersionstampedValue VersionMutationType = iota
	// MutationTypeSetVersionstampedKey queues a SET_VERSIONSTAMPED_KEY mutation.
	MutationTypeSetVersionstampedKey
)

// versionMutation holds a queued versionstamp mutation with its type and value.
type versionMutation struct {
	mutationType VersionMutationType
	value        []byte
}

// FDBRecordContext represents a transactional context for record operations.
// It wraps an FDB transaction and provides additional record layer functionality.
// Matches Java's FDBRecordContext.
type FDBRecordContext struct {
	tx  fdb.Transaction
	ctx context.Context

	// Version management — matches Java's FDBRecordContext
	localVersion      int32                      // per-transaction local version counter
	localVersionCache map[string]int             // key (string) → local version (int)
	versionMutations  map[string]versionMutation // key (string) → mutation (type + value)

	// Commit hooks — matches Java's CommitCheckAsync / PostCommit
	commitChecks []CommitCheckFunc
	postCommits  []PostCommitFunc

	// Diagnostic: tracked read conflict ranges for debugging
	conflictRanges []fdb.KeyRange
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
	v := rc.localVersion
	rc.localVersion++
	return int(v) // returns 0, 1, 2, ...
}

// AddToLocalVersionCache caches a local version for a version key within this transaction.
// Matches Java's FDBRecordContext.addToLocalVersionCache().
func (rc *FDBRecordContext) AddToLocalVersionCache(versionKey []byte, localVersion int) {
	if rc.localVersionCache == nil {
		rc.localVersionCache = make(map[string]int)
	}
	rc.localVersionCache[string(versionKey)] = localVersion
}

// GetLocalVersion retrieves a cached local version for the given key.
// Returns (localVersion, true) if found, (0, false) otherwise.
func (rc *FDBRecordContext) GetLocalVersion(versionKey []byte) (int, bool) {
	v, ok := rc.localVersionCache[string(versionKey)]
	return v, ok
}

// RemoveLocalVersion removes a cached local version entry.
func (rc *FDBRecordContext) RemoveLocalVersion(versionKey []byte) {
	delete(rc.localVersionCache, string(versionKey))
}

// AddVersionMutation queues a versionstamp mutation to be applied at commit.
// mutationType selects SET_VERSIONSTAMPED_KEY or SET_VERSIONSTAMPED_VALUE.
// The key or value (depending on type) must include the versionstamp placeholder bytes.
// Matches Java's FDBRecordContext.addVersionMutation(MutationType, key, value).
func (rc *FDBRecordContext) AddVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte) {
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	rc.versionMutations[string(versionKey)] = versionMutation{
		mutationType: mutationType,
		value:        value,
	}
}

// UpdateVersionMutation queues or updates a versionstamp mutation with a merge function.
// If a mutation for the same key already exists, the merge function decides which value to keep.
// Matches Java's FDBRecordContext.updateVersionMutation(MutationType, key, value, BiFunction).
func (rc *FDBRecordContext) UpdateVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte, merge func(oldValue, newValue []byte) []byte) {
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	key := string(versionKey)
	if existing, ok := rc.versionMutations[key]; ok && merge != nil {
		merged := merge(existing.value, value)
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        merged,
		}
	} else {
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        value,
		}
	}
}

// RemoveVersionMutation removes a queued version mutation.
func (rc *FDBRecordContext) RemoveVersionMutation(versionKey []byte) {
	delete(rc.versionMutations, string(versionKey))
}

// RemoveVersionMutationsInRange removes all queued version mutations whose key
// falls in [begin, end). Matches Java's FDBRecordContext.removeVersionMutationRange().
func (rc *FDBRecordContext) RemoveVersionMutationsInRange(begin, end fdb.Key) {
	for k := range rc.versionMutations {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.versionMutations, k)
		}
	}
}

// RemoveLocalVersionsInRange removes all cached local versions whose key
// falls in [begin, end). Matches Java's FDBRecordContext.removeLocalVersionRange().
func (rc *FDBRecordContext) RemoveLocalVersionsInRange(begin, end fdb.Key) {
	for k := range rc.localVersionCache {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.localVersionCache, k)
		}
	}
}

// flushVersionMutations applies all queued versionstamp mutations
// to the underlying FDB transaction. Called before commit.
// Dispatches to SetVersionstampedKey or SetVersionstampedValue based on mutation type.
func (rc *FDBRecordContext) flushVersionMutations() {
	for key, mut := range rc.versionMutations {
		switch mut.mutationType {
		case MutationTypeSetVersionstampedKey:
			rc.tx.SetVersionstampedKey(fdb.Key(key), mut.value)
		case MutationTypeSetVersionstampedValue:
			rc.tx.SetVersionstampedValue(fdb.Key(key), mut.value)
		}
	}
}

// AddCommitCheck registers a pre-commit check function.
// All checks run before the transaction commits. If any returns an error,
// the commit is aborted with that error.
// Matches Java's FDBRecordContext.addCommitCheck(CommitCheckAsync).
func (rc *FDBRecordContext) AddCommitCheck(check CommitCheckFunc) {
	rc.commitChecks = append(rc.commitChecks, check)
}

// AddPostCommit registers a post-commit callback.
// All callbacks run after the transaction successfully commits.
// Matches Java's FDBRecordContext.addPostCommit(PostCommit).
func (rc *FDBRecordContext) AddPostCommit(hook PostCommitFunc) {
	rc.postCommits = append(rc.postCommits, hook)
}

// runCommitChecks runs all registered pre-commit checks.
// Returns the first error encountered, or nil if all pass.
func (rc *FDBRecordContext) runCommitChecks() error {
	for _, check := range rc.commitChecks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// runPostCommits runs all registered post-commit callbacks.
func (rc *FDBRecordContext) runPostCommits() {
	for _, hook := range rc.postCommits {
		hook()
	}
}

// GetReadVersion returns the transaction's read version.
// Matches Java's FDBRecordContext.getReadVersion().
func (rc *FDBRecordContext) GetReadVersion() (int64, error) {
	return rc.tx.GetReadVersion().Get()
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
// Note: FDB does not natively expose which specific keys conflicted. This method
// provides the read conflict ranges that were registered, which may help debugging.
// Matches Java's FDBRecordContext.reportConflictingKeys() (diagnostic, not exact).
func (rc *FDBRecordContext) GetConflictingKeys() []fdb.KeyRange {
	return rc.conflictRanges
}

// AddReadConflictRange adds a read conflict range and tracks it for diagnostics.
func (rc *FDBRecordContext) AddReadConflictRange(r fdb.ExactRange) error {
	if err := rc.tx.AddReadConflictRange(r); err != nil {
		return err
	}
	begin, end := r.FDBRangeKeys()
	rc.conflictRanges = append(rc.conflictRanges, fdb.KeyRange{
		Begin: begin.(fdb.Key),
		End:   end.(fdb.Key),
	})
	return nil
}

// HasVersionMutations returns true if there are pending version mutations.
func (rc *FDBRecordContext) HasVersionMutations() bool {
	return len(rc.versionMutations) > 0
}

// CommitWithVersionstamp commits the transaction, first running pre-commit checks,
// then flushing all queued versionstamp mutations. Returns the committed
// versionstamp (10 bytes) or nil for read-only transactions / no versionstamp mutations.
// Runs post-commit hooks after successful commit.
// Matches Java's FDBRecordContext.commitAsync() which always runs checks and hooks.
func (rc *FDBRecordContext) CommitWithVersionstamp() ([]byte, error) {
	// Run pre-commit checks before committing
	if err := rc.runCommitChecks(); err != nil {
		return nil, err
	}

	rc.flushVersionMutations()

	// Get the versionstamp future BEFORE committing
	vsFuture := rc.tx.GetVersionstamp()

	// Commit the transaction
	if err := rc.tx.Commit().Get(); err != nil {
		return nil, err
	}

	// Run post-commit callbacks after successful commit
	rc.runPostCommits()

	// Retrieve the committed versionstamp
	vs, err := vsFuture.Get()
	if err != nil {
		// Read-only transactions don't have a versionstamp
		return nil, nil
	}

	return []byte(vs), nil
}

// buildVersionstampedValue builds the value for SET_VERSIONSTAMPED_VALUE mutation.
// Matches Java's SplitHelper.packVersion(): wraps an incomplete versionstamp in
// a Tuple and uses PackWithVersionstamp to produce bytes with the offset appended.
// After FDB commit, the stored value is a packed Tuple containing the completed versionstamp.
func buildVersionstampedValue(version *FDBRecordVersion) ([]byte, error) {
	vs := tuple.Versionstamp{
		UserVersion: uint16(version.GetLocalVersion()),
	}
	// TransactionVersion is all 0xFF for incomplete versionstamps (placeholder)
	copy(vs.TransactionVersion[:], incompleteGlobalVersionMarker[:])
	return tuple.Tuple{vs}.PackWithVersionstamp(nil)
}