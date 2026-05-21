package recordlayer

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// RecordContextConfig holds configuration for creating FDBRecordContext instances.
// Matches Java's FDBRecordContextConfig.
type RecordContextConfig struct {
	// TransactionTimeout is the timeout for the FDB transaction.
	// Zero means use FDB's default.
	TransactionTimeout time.Duration

	// Priority sets the transaction priority.
	Priority TransactionPriority

	// TransactionID is an optional identifier for tracing/debugging.
	TransactionID string

	// TransactionSizeWarnBytes triggers a warning when the approximate
	// transaction size exceeds this threshold. Zero disables the check.
	// FDB's hard limit is 10MB; a typical warning threshold is 8MB.
	TransactionSizeWarnBytes int64

	// TransactionSizeErrorBytes causes operations to return
	// TransactionSizeExceededError when the approximate transaction
	// size exceeds this threshold. Zero disables the check.
	// Setting this below FDB's 10MB limit lets callers commit early.
	TransactionSizeErrorBytes int64
}

// FDBDatabaseRunner provides configurable retry logic for FDB transactions.
// Matches Java's FDBDatabaseRunnerImpl.
type FDBDatabaseRunner struct {
	db *FDBDatabase

	// MaxAttempts is the maximum number of retry attempts (default 10).
	MaxAttempts int

	// InitialDelay is the initial delay between retries (default 10ms).
	InitialDelay time.Duration

	// MaxDelay is the maximum delay between retries (default 1s).
	MaxDelay time.Duration

	// ContextConfig is applied to each transaction.
	ContextConfig *RecordContextConfig
}

// NewFDBDatabaseRunner creates a runner with default settings.
func NewFDBDatabaseRunner(db *FDBDatabase) *FDBDatabaseRunner {
	return &FDBDatabaseRunner{
		db:           db,
		MaxAttempts:  10,
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     1 * time.Second,
	}
}

// SetMaxAttempts sets the maximum retry attempts.
func (r *FDBDatabaseRunner) SetMaxAttempts(n int) *FDBDatabaseRunner {
	r.MaxAttempts = n
	return r
}

// SetInitialDelay sets the initial retry delay.
func (r *FDBDatabaseRunner) SetInitialDelay(d time.Duration) *FDBDatabaseRunner {
	r.InitialDelay = d
	return r
}

// SetMaxDelay sets the maximum retry delay.
func (r *FDBDatabaseRunner) SetMaxDelay(d time.Duration) *FDBDatabaseRunner {
	r.MaxDelay = d
	return r
}

// SetContextConfig sets the transaction context configuration.
func (r *FDBDatabaseRunner) SetContextConfig(config *RecordContextConfig) *FDBDatabaseRunner {
	r.ContextConfig = config
	return r
}

// RunWithRetry executes fn with configurable retry logic and exponential backoff.
// Retries on FDB retryable errors (conflict, etc.) up to MaxAttempts times.
// Non-retryable errors are returned immediately.
// Matches Java's FDBDatabaseRunnerImpl.run().
func (r *FDBDatabaseRunner) RunWithRetry(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var lastErr error

	for attempt := 0; attempt < r.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := r.calculateDelay(attempt)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		result, err := r.runOnce(ctx, fn)
		if err == nil {
			return result, nil
		}

		lastErr = err
		if !isRetryableError(err) {
			return nil, err
		}
	}

	return nil, lastErr
}

// runOnce executes fn in a single transaction, applying context config.
func (r *FDBDatabaseRunner) runOnce(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	var createTx func() (fdb.Transaction, error)
	if r.db.tenant != (fdb.Tenant{}) {
		createTx = r.db.tenant.CreateTransaction
	} else {
		createTx = r.db.db.CreateTransaction
	}

	tx, err := createTx()
	if err != nil {
		return nil, err
	}
	tx.Options().SetReadSystemKeys()

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
	}

	// Apply context config
	if r.ContextConfig != nil {
		if r.ContextConfig.TransactionTimeout > 0 {
			if err := tx.Options().SetTimeout(int64(r.ContextConfig.TransactionTimeout / time.Millisecond)); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
		if r.ContextConfig.Priority != PriorityDefault {
			if err := recordCtx.SetTransactionPriority(r.ContextConfig.Priority); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
		if r.ContextConfig.TransactionID != "" {
			if err := tx.Options().SetDebugTransactionIdentifier(r.ContextConfig.TransactionID); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
	}

	result, err := fn(recordCtx)
	if err != nil {
		tx.Cancel()
		return nil, err
	}

	// Run pre-commit checks
	if err := recordCtx.runCommitChecks(); err != nil {
		tx.Cancel()
		return nil, err
	}

	recordCtx.flushVersionMutations()

	if err := tx.Commit().Get(); err != nil {
		tx.Cancel()
		return nil, err
	}

	recordCtx.runPostCommits()
	return result, nil
}

// OpenContext creates a new FDBRecordContext with a fresh transaction, applying
// the runner's context configuration. Matches Java's FDBDatabaseRunner.openContext().
// The caller is responsible for committing or cancelling the transaction.
func (r *FDBDatabaseRunner) OpenContext(ctx context.Context) (*FDBRecordContext, error) {
	var tx fdb.Transaction
	var err error
	if r.db.tenant != (fdb.Tenant{}) {
		tx, err = r.db.tenant.CreateTransaction()
	} else {
		tx, err = r.db.db.CreateTransaction()
	}
	if err != nil {
		return nil, err
	}
	tx.Options().SetReadSystemKeys()

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
	}

	if r.ContextConfig != nil {
		if r.ContextConfig.TransactionTimeout > 0 {
			if err := tx.Options().SetTimeout(int64(r.ContextConfig.TransactionTimeout / time.Millisecond)); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
		if r.ContextConfig.Priority != PriorityDefault {
			if err := recordCtx.SetTransactionPriority(r.ContextConfig.Priority); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
		if r.ContextConfig.TransactionID != "" {
			if err := tx.Options().SetDebugTransactionIdentifier(r.ContextConfig.TransactionID); err != nil {
				tx.Cancel()
				return nil, err
			}
		}
		recordCtx.txSizeWarnBytes = r.ContextConfig.TransactionSizeWarnBytes
		recordCtx.txSizeErrorBytes = r.ContextConfig.TransactionSizeErrorBytes
	}

	return recordCtx, nil
}

// calculateDelay returns the delay for the given attempt using exponential backoff with jitter.
func (r *FDBDatabaseRunner) calculateDelay(attempt int) time.Duration {
	delay := float64(r.InitialDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(r.MaxDelay) {
		delay = float64(r.MaxDelay)
	}
	// Add jitter: random value between 0.5x and 1.5x
	jitter := 0.5 + rand.Float64()
	return time.Duration(delay * jitter)
}

// isRetryableError checks if an FDB error is retryable.
// These codes match FDB's fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE, code),
// which is RETRYABLE = MAYBE_COMMITTED ∪ RETRYABLE_NOT_COMMITTED.
// The Go binding exposes fdb.ErrorPredicateRetryable (50000) but not the C function
// fdb_error_predicate() itself, so we maintain the list manually.
// Source of truth: fdb_c.cpp fdb_error_predicate() + flow/error_definitions.h
func isRetryableError(err error) bool {
	var fdbErr fdb.Error
	if !errors.As(err, &fdbErr) {
		return false
	}
	switch fdbErr.Code {
	// MAYBE_COMMITTED
	case 1021, // commit_unknown_result
		1039: // cluster_version_changed
		return true
	// RETRYABLE_NOT_COMMITTED
	case 1007, // transaction_too_old
		1009, // future_version
		1020, // not_committed (conflict)
		1037, // process_behind
		1038, // database_locked
		1042, // commit_proxy_memory_limit_exceeded
		1051, // batch_transaction_throttled
		1078, // grv_proxy_memory_limit_exceeded
		1213, // tag_throttled
		1223, // proxy_tag_throttled
		1235, // transaction_throttled_hot_shard
		1242: // transaction_rejected_range_locked
		return true
	}
	return false
}
