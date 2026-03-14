package recordlayer

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
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
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
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

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
	}

	// Apply context config
	if r.ContextConfig != nil {
		if r.ContextConfig.TransactionTimeout > 0 {
			if err := tx.Options().SetTimeout(int64(r.ContextConfig.TransactionTimeout / time.Millisecond)); err != nil {
				return nil, err
			}
		}
		if r.ContextConfig.Priority != PriorityDefault {
			if err := recordCtx.SetTransactionPriority(r.ContextConfig.Priority); err != nil {
				return nil, err
			}
		}
		if r.ContextConfig.TransactionID != "" {
			if err := tx.Options().SetDebugTransactionIdentifier(r.ContextConfig.TransactionID); err != nil {
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
func isRetryableError(err error) bool {
	// FDB errors that are retryable include:
	// 1020 - not_committed (conflict)
	// 1021 - commit_unknown_result
	// 1009 - request_for_timestamp_not_yet_set
	// The FDB Go binding wraps these as fdb.Error
	var fdbErr fdb.Error
	if errors.As(err, &fdbErr) {
		code := fdbErr.Code
		return code == 1020 || code == 1021 || code == 1009
	}
	return false
}
