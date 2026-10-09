package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
)

// Java's FDBDatabaseFactory defaults, which every FDBDatabaseRunner inherits
// (FDBDatabaseFactory.java: DEFAULT_MAX_ATTEMPTS, maxDelayMillis,
// initialDelayMillis).
const (
	DefaultMaxAttempts  = 10
	DefaultInitialDelay = 10 * time.Millisecond
	DefaultMaxDelay     = 1000 * time.Millisecond
)

// AttemptRoute says how an attempt reaches the backend.
type AttemptRoute int

const (
	// RouteAttempt is one transactor call per attempt, with the backend's own
	// retry limit set to 0, so the loop counts every attempt.
	RouteAttempt AttemptRoute = iota
	// RouteClientLoop is the transactor's own unbounded retry loop
	// (runClientLoop), kept for SPFresh's background lifecycles.
	RouteClientLoop
	// RouteOwnTransaction is an owner that opens its transaction itself
	// (FDBDatabaseRunner.RunWithRetry); its attempts reach no transactor.
	RouteOwnTransaction
)

// AttemptCall identifies one execution of an attempt-loop call to a
// transactor. A transactor that implements AttemptTransactor receives it.
type AttemptCall struct {
	Route AttemptRoute
	// Owner is a stable label for the caller, such as "run" or "runner".
	Owner string
	// Execution counts every execution of this call, uncounted retries included.
	Execution int
	// Attempt is the number of counted attempts made before this execution,
	// the number MaxAttempts bounds; an uncounted retry repeats it.
	Attempt int
	// CallID is unique per attempt-loop call.
	CallID uint64
}

// AttemptTransactor is a transactor that wants each attempt's identity. The
// database hands it every attempt of Run and its variants; a transactor that
// does not implement it is called through its TransactCtx (or Transact) per
// attempt, and sees every attempt but not the call's identity.
type AttemptTransactor interface {
	TransactAttempt(ctx context.Context, call AttemptCall, fn func(fdb.WritableTransaction) (any, error)) (any, error)
	ReadTransactAttempt(ctx context.Context, call AttemptCall, fn func(fdb.ReadTransaction) (any, error)) (any, error)
}

// attemptPolicy bounds one owner's attempts.
type attemptPolicy struct {
	owner        string
	maxAttempts  int
	initialDelay time.Duration
	maxDelay     time.Duration
	// timer records each retry delay as EventRetryDelay (nil: none).
	timer *StoreTimer
}

var nextAttemptCallID atomic.Uint64

// AttemptObserver, when installed on a database, is told the outcome of every
// attempt-loop execution, on the loop's goroutine, as the execution ends.
type AttemptObserver func(call AttemptCall, err error)

// spfreshStalledSealBound is SimFDB's former retry backstop: under a simulated
// environment, this many consecutive uncounted retries that each met the same
// sealed postings fail the call (SPFreshStalledSealError).
const SPFreshStalledSealBound = 100

// attemptLoop runs attempt until it succeeds, fails with an error that is not
// retriable by the runner's rule (isRetriableAnyCause), or has used
// policy.maxAttempts counted attempts. It is FDBDatabaseRunnerImpl.RunRetriable
// (:180-207): the same predicate, the same bound (currAttempt + 1 <
// maxAttempts) and the same ExponentialDelay between attempts.
//
// An SPFreshSplitWindowError does not count as an attempt: the write met a
// posting a split owns, and re-runs until the split publishes, bounded by ctx.
// It still takes the delay. Under a simulated environment the delay is drawn
// and not waited (the simulation does not model the retry delay's time), so a
// stalled seal would be a hot loop; there, spfreshStalledSealBound consecutive
// retries meeting the same sealed postings fail with SPFreshStalledSealError.
//
// A context that ends before the first attempt returns ctx.Err(); one that
// ends later returns an error wrapping both ctx.Err() and the last attempt's
// error. After the final attempt the last error is returned unchanged.
func attemptLoop(ctx context.Context, env *dst.Env, observe AttemptObserver, policy attemptPolicy, route AttemptRoute, attempt func(call AttemptCall) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := AttemptCall{Route: route, Owner: policy.owner, CallID: nextAttemptCallID.Add(1)}
	delay := newExponentialDelay(policy.initialDelay, policy.maxDelay, env)
	var stalled []SPFreshSealedPosting
	stallCount := 0
	for {
		result, err := attempt(call)
		if observe != nil {
			observe(call, err)
		}
		if err == nil {
			return result, nil
		}
		var window *SPFreshSplitWindowError
		if errors.As(err, &window) {
			if env != nil {
				if slices.Equal(window.Sealed, stalled) {
					stallCount++
				} else {
					stalled, stallCount = window.Sealed, 1
				}
				if stallCount >= SPFreshStalledSealBound {
					return nil, &SPFreshStalledSealError{Sealed: window.Sealed, Retries: stallCount}
				}
			}
		} else {
			stalled, stallCount = nil, 0
			if call.Attempt+1 >= policy.maxAttempts || !isRetriableAnyCause(err) {
				return nil, err
			}
			call.Attempt++
		}
		call.Execution++
		if wait := delay.delay(); env != nil {
			// Simulated: the delay is drawn, not waited; the timer records
			// the drawn delay, the time a real run would have spent.
			policy.timer.Record(EventRetryDelay, int64(wait))
		} else {
			start := time.Now()
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
			policy.timer.RecordSince(EventRetryDelay, start)
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("%w (last attempt: %w)", cerr, err)
		}
	}
}

// bodyErrorRecorder keeps the error the LAST execution's body returned, so the
// loop can hand the caller the body's chain when the transactor reports the
// same FDB code without it (a transactor that strips the chain). It is reset
// when an execution starts, set when the body fails and left nil when it
// succeeds, so an earlier execution's error never pairs with a later commit
// failure of the same code.
type bodyErrorRecorder struct{ err error }

func (r *bodyErrorRecorder) start() { r.err = nil }

func (r *bodyErrorRecorder) record(err error) error {
	r.err = err
	return err
}

// resolve returns the body's error when the transactor's error carries the
// same FDB code, and the transactor's error otherwise.
func (r *bodyErrorRecorder) resolve(err error) error {
	if err == nil || r.err == nil {
		return err
	}
	var got, body fdb.Error
	if errors.As(err, &got) && errors.As(r.err, &body) && got.Code == body.Code {
		return r.err
	}
	return err
}

// policy returns the database's attempt policy for owner.
func (d *FDBDatabase) policy(owner string) attemptPolicy {
	return attemptPolicy{
		owner:        owner,
		maxAttempts:  d.MaxAttempts(),
		initialDelay: d.InitialDelay(),
		maxDelay:     d.MaxDelay(),
		timer:        d.Timer(),
	}
}

// run runs fn as Run does, with the indexer's attempt bound (SetMaxAttempts).
func (oi *OnlineIndexer) run(ctx context.Context, fn func(rtx *FDBRecordContext) (any, error)) (any, error) {
	policy := oi.db.policy("online.indexer")
	if oi.maxAttempts > 0 {
		policy.maxAttempts = oi.maxAttempts
	}
	result, _, err := oi.db.runContexts(ctx, policy, RouteAttempt, nil, false, fn)
	return result, err
}

// SetMaxAttempts sets how many attempts Run and its variants make, Java's
// FDBDatabaseFactory.setMaxAttempts. Values below 1 are rejected as Java's are.
func (d *FDBDatabase) SetMaxAttempts(n int) error {
	if n <= 0 {
		return &RecordCoreError{Message: "Cannot set maximum number of attempts to less than or equal to zero"}
	}
	d.maxAttempts.Store(int64(n))
	return nil
}

// MaxAttempts returns the attempt bound of Run and its variants (default 10).
func (d *FDBDatabase) MaxAttempts() int {
	if n := d.maxAttempts.Load(); n > 0 {
		return int(n)
	}
	return DefaultMaxAttempts
}

// SetRetryDelays sets the initial and maximum delay between attempts, Java's
// FDBDatabaseFactory.setInitialDelayMillis / setMaxDelayMillis.
func (d *FDBDatabase) SetRetryDelays(initial, maxDelay time.Duration) error {
	if initial < 0 {
		return &RecordCoreError{Message: "Cannot set initial delay milleseconds to less than zero"}
	}
	if maxDelay < 0 {
		return &RecordCoreError{Message: "Cannot set maximum delay milliseconds to less than zero"}
	}
	if initial > maxDelay {
		return &RecordCoreError{Message: "Cannot set initial delay to greater than maximum delay"}
	}
	d.initialDelay.Store(int64(initial))
	d.maxDelay.Store(int64(maxDelay))
	d.delaysSet.Store(true)
	return nil
}

// InitialDelay returns the first retry delay bound (default 10 ms).
func (d *FDBDatabase) InitialDelay() time.Duration {
	if d.delaysSet.Load() {
		return time.Duration(d.initialDelay.Load())
	}
	return DefaultInitialDelay
}

// MaxDelay returns the retry delay cap (default 1 s).
func (d *FDBDatabase) MaxDelay() time.Duration {
	if d.delaysSet.Load() {
		return time.Duration(d.maxDelay.Load())
	}
	return DefaultMaxDelay
}

// SetAttemptObserver installs an observer of every attempt-loop execution.
func (d *FDBDatabase) SetAttemptObserver(o AttemptObserver) {
	if o == nil {
		d.attemptObserver.Store(nil)
		return
	}
	d.attemptObserver.Store(&o)
}

func (d *FDBDatabase) observer() AttemptObserver {
	if p := d.attemptObserver.Load(); p != nil {
		return *p
	}
	return nil
}

// transactAttempt runs one attempt's writable body through the transactor.
func (d *FDBDatabase) transactAttempt(ctx context.Context, call AttemptCall, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if at, ok := d.transactor.(AttemptTransactor); ok {
		return at.TransactAttempt(ctx, call, fn)
	}
	return runTransactCtx(d.transactor, ctx, fn)
}

// readTransactAttempt runs one attempt's read body through the transactor.
func (d *FDBDatabase) readTransactAttempt(ctx context.Context, call AttemptCall, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	if at, ok := d.transactor.(AttemptTransactor); ok {
		return at.ReadTransactAttempt(ctx, call, fn)
	}
	return runReadTransactCtx(d.transactor, ctx, fn)
}

// SPFreshSealedPosting names a posting an SPFresh write found SEALED.
type SPFreshSealedPosting struct {
	CellID, PostingID, Epoch int64
}

// SPFreshSplitWindowError is the retryable conflict an SPFresh foreground
// write raises when every posting its routing reaches is SEALED by a split in
// progress. It wraps not_committed (1020), so every owner and the SQL mapping
// read it as a retryable conflict, but the attempt loop retries it without
// counting an attempt: the write re-runs until the split publishes (RFC-094
// §6), bounded by the caller's context.
type SPFreshSplitWindowError struct {
	Sealed []SPFreshSealedPosting
}

func (e *SPFreshSplitWindowError) Error() string {
	return fmt.Sprintf("SPFresh write met only sealed postings %v: %v", e.Sealed, e.Unwrap())
}

func (e *SPFreshSplitWindowError) Unwrap() error { return fdb.Error{Code: 1020} }

// SPFreshStalledSealError fails a call whose SPFresh write met the same sealed
// postings on spfreshStalledSealBound consecutive retries under a simulated
// environment, where no second actor can take the stalled lease over. It is
// not retriable, so a simulation that stalls a seal fails instead of hanging.
type SPFreshStalledSealError struct {
	Sealed  []SPFreshSealedPosting
	Retries int
}

func (e *SPFreshStalledSealError) Error() string {
	return fmt.Sprintf("SPFresh write stalled: postings %v stayed sealed across %d retries", e.Sealed, e.Retries)
}
