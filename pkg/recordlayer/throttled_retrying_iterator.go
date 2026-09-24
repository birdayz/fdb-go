package recordlayer

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RunnerClosedError corresponds to FDBDatabaseRunner.RunnerClosed.
type RunnerClosedError struct{}

func (*RunnerClosedError) Error() string { return "runner is closed" }

// iterationQuota belongs to one transaction, including a failed attempt.
type iterationQuota struct {
	scanned, deleted int
	hasMore          bool
}

// throttledRetryingIterator ports Java's ThrottledRetryingIterator. It is the
// sole retry owner: OpenContext must not be replaced with the retrying DB.Run.
// Callbacks execute serially; Close may run concurrently with iterateAll.
type throttledRetryingIterator[T any] struct {
	onSuccess                                               func(*iterationQuota)
	runner                                                  *FDBDatabaseRunner
	cursor                                                  func(context.Context, *FDBRecordContext, []byte, int) (RecordCursor[T], error)
	handle                                                  func(context.Context, *FDBRecordContext, T, *iterationQuota) error
	timeQuota                                               time.Duration
	maxDeletes, scannedPerSecond, deletedPerSecond, retries int
	commit                                                  bool
	mu                                                      sync.Mutex
	closed                                                  bool
	active                                                  context.CancelCauseFunc
}

func newThrottledRetryingIterator[T any](runner *FDBDatabaseRunner,
	cursor func(context.Context, *FDBRecordContext, []byte, int) (RecordCursor[T], error),
	handle func(context.Context, *FDBRecordContext, T, *iterationQuota) error,
) *throttledRetryingIterator[T] {
	return &throttledRetryingIterator[T]{runner: runner, cursor: cursor, handle: handle, timeQuota: 4 * time.Second, retries: 100, commit: true}
}

func (it *throttledRetryingIterator[T]) Close() {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.closed = true
	if it.active != nil {
		it.active(&RunnerClosedError{})
	}
}

func (it *throttledRetryingIterator[T]) iterateAll(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	it.mu.Lock()
	if it.closed {
		it.mu.Unlock()
		cancel(&RunnerClosedError{})
		return context.Cause(ctx)
	}
	if it.active != nil {
		it.mu.Unlock()
		cancel(nil)
		return &RecordCoreError{Message: "throttled iterator is already running"}
	}
	it.active = cancel
	it.mu.Unlock()
	defer func() {
		it.mu.Lock()
		it.active = nil
		it.mu.Unlock()
		cancel(nil)
	}()
	var continuation []byte
	limit, failures, successes := 0, 0, 0
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		quota := iterationQuota{hasMore: true}
		next, started, err := it.iterateOneRange(ctx, continuation, limit, &quota)
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if err != nil {
			failures++
			var closed *RunnerClosedError
			if failures > it.retries || errors.As(err, &closed) {
				return err
			}
			successes = 0
			limit = max(1, quota.scanned*9/10)
			continue
		}
		// Never publish a failed transaction's tentative continuation.
		continuation = next
		if it.onSuccess != nil {
			it.onSuccess(&quota)
		}
		if !quota.hasMore {
			return nil
		}
		successes++
		if successes%40 == 0 && limit < quota.scanned+3 {
			if limit != 0 {
				limit = max(limit*5/4, limit+4)
			}
		}
		failures = 0
		elapsed := max(int64(0), it.runner.db.Env().Now().Sub(started).Milliseconds())
		delay := max(throttledIteratorDelay(elapsed, it.scannedPerSecond, quota.scanned), throttledIteratorDelay(elapsed, it.deletedPerSecond, quota.deleted))
		if delay > 0 {
			timer := time.NewTimer(time.Duration(delay) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return context.Cause(ctx)
			case <-timer.C:
			}
		}
	}
}

func throttledIteratorDelay(elapsed int64, rate, count int) int64 {
	if rate <= 0 {
		return 0
	}
	return max(int64(0), int64(count)*1000/int64(rate)-elapsed)
}

func (it *throttledRetryingIterator[T]) iterateOneRange(ctx context.Context, continuation []byte, limit int, quota *iterationQuota) ([]byte, time.Time, error) {
	rc, err := it.runner.OpenContext(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	stop := context.AfterFunc(ctx, rc.Cancel)
	defer func() { stop(); rc.Cancel() }()
	cursor, err := it.cursor(ctx, rc, continuation, limit)
	if err != nil {
		return nil, time.Time{}, err
	}
	started := it.runner.db.Env().Now()
	next, err := it.consume(ctx, rc, cursor, quota, started)
	closeErr := cursor.Close()
	if err != nil {
		return nil, started, err
	}
	if closeErr != nil {
		return nil, started, closeErr
	}
	if err := context.Cause(ctx); err != nil {
		return nil, started, err
	}
	// The final empty transaction commits too: session validation and heartbeat
	// updates performed by the cursor factory must become durable.
	if it.commit {
		if err := rc.Commit(); err != nil {
			return nil, started, err
		}
	}
	return next, started, nil
}

func (it *throttledRetryingIterator[T]) consume(ctx context.Context, rc *FDBRecordContext, cursor RecordCursor[T], quota *iterationQuota, started time.Time) ([]byte, error) {
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		continuation, err := result.GetContinuation().ToBytes()
		if err != nil {
			return nil, err
		}
		if !result.HasNext() {
			if result.GetNoNextReason().IsSourceExhausted() {
				quota.hasMore = false
			}
			return continuation, nil
		}
		quota.scanned++
		if err := it.handle(ctx, rc, result.GetValue(), quota); err != nil {
			return nil, err
		}
		if !quota.hasMore || (it.timeQuota > 0 && it.runner.db.Env().Now().Sub(started).Milliseconds() > it.timeQuota.Milliseconds()) || (it.maxDeletes > 0 && quota.deleted >= it.maxDeletes) {
			return continuation, nil
		}
	}
}
