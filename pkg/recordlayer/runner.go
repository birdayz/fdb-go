package recordlayer

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
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

	// Tags are transaction tags used for tag-based throttling, applied to every
	// transaction this config creates. Matches Java's FDBRecordContextConfig
	// tags (FDBRecordContextConfig.java:60), which are applied one at a time via
	// transaction.options().setTag (FDBRecordContext.java:205-207) — the plain
	// TAG option, never AUTO_THROTTLE_TAG.
	//
	// Java models this as a Set<String>, so duplicates collapse and iteration
	// order is arbitrary. ValidateTags dedups; SetTags sorts, because the commit
	// request's TagSet is a length-prefixed concatenation in insertion order and
	// an arbitrary order would make the commit bytes vary run to run for the
	// same config. Order is not a wire contract on either side.
	Tags []string
}

// Java's tag limits (FDBRecordContextConfig.java:661-672). These are STRICTER
// than the FDB client's own 5/255 (TagThrottle.actor.cpp:35-39): the record
// layer caps a tag at 16 characters, so a tag the client would accept can still
// be rejected here. Validation happens at config time, as in Java, so a bad tag
// fails before any transaction is opened rather than mid-commit.
const (
	MaxRecordContextTags   = 5
	MaxRecordContextTagLen = 16
)

// TagValidationError reports a tag set rejected by the record layer's limits.
// Java throws IllegalArgumentException with these exact messages.
type TagValidationError struct {
	Message string
	// Tag is the offending tag for a length violation, empty for a count violation.
	Tag string
}

func (e *TagValidationError) Error() string { return e.Message }

// ValidateTags applies Java's setTags checks in Java's order — the COUNT first,
// then each tag's length (FDBRecordContextConfig.java:662-669). The order is
// observable: an over-long tag in an over-large set reports the count error.
// Note this is the opposite of the FDB client's TagSet::addTag, which checks
// length first because it validates one tag at a time rather than a whole set.
//
// Returns the deduplicated tag set; Java's Set<String> collapses duplicates
// before the size check, so ("a","a","b") is two tags, not three.
func ValidateTags(tags []string) ([]string, error) {
	seen := make(map[string]struct{}, len(tags))
	uniq := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		uniq = append(uniq, t)
	}
	if len(uniq) > MaxRecordContextTags {
		return nil, &TagValidationError{Message: "At most 5 tags allowed"}
	}
	for _, t := range uniq {
		// Java uses String.length(), which counts UTF-16 code units, not bytes.
		// For the ASCII tenant identifiers this is used for the two agree; the
		// rune count is the closer analogue for anything else, and it is the
		// conservative choice since it never exceeds the byte length the client
		// checks against its own 255 limit.
		if len([]rune(t)) > MaxRecordContextTagLen {
			return nil, &TagValidationError{
				Message: "Tag must be 16 characters or shorter",
				Tag:     t,
			}
		}
	}
	return uniq, nil
}

// SetTags validates and stores the tag set, returning an error rather than
// panicking where Java throws. Tags are sorted so the resulting commit bytes are
// deterministic for a given config.
func (c *RecordContextConfig) SetTags(tags []string) error {
	uniq, err := ValidateTags(tags)
	if err != nil {
		return err
	}
	sort.Strings(uniq)
	c.Tags = uniq
	return nil
}

// tagSetter is the narrow slice of the transaction options surface that tag
// application needs. Naming it keeps applyTagsTo testable without standing up a
// whole fdb.TransactionOptions.
type tagSetter interface {
	SetTag(tag string) error
}

// applyTagsTo issues one SetTag per tag, in order, matching
// FDBRecordContext.java:205-207 — the record layer sets each tag individually
// via the plain TAG option, never AUTO_THROTTLE_TAG.
func applyTagsTo(o tagSetter, tags []string) error {
	for _, tag := range tags {
		if err := o.SetTag(tag); err != nil {
			return err
		}
	}
	return nil
}

// applyTags sets the config's tags on a transaction. Java guards the whole
// block on a non-empty set (FDBRecordContext.java:205), so an untagged config
// touches the options surface not at all.
func applyTags(tx interface{ Options() fdb.TransactionOptions }, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	return applyTagsTo(tx.Options(), tags)
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
	// CreateWritableTransaction is backend-agnostic (handles tenant / pure-Go /
	// libfdb_c internally) and returns the WritableTransaction interface the record
	// context holds — so the manual runner works on either backend (RFC-109).
	tx, err := r.db.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	r.db.applyReadSystemKeys(tx.Options())

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
		env: r.db.env,
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
		if err := applyTags(tx, r.ContextConfig.Tags); err != nil {
			tx.Cancel()
			return nil, err
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
	// Backend-agnostic (RFC-109): works on the libfdb_c backend too.
	tx, err := r.db.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	r.db.applyReadSystemKeys(tx.Options())

	recordCtx := &FDBRecordContext{
		tx:  tx,
		ctx: ctx,
		env: r.db.env,
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
		if err := applyTags(tx, r.ContextConfig.Tags); err != nil {
			tx.Cancel()
			return nil, err
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
