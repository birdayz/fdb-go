package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

func testPolicy(maxAttempts int) attemptPolicy {
	return attemptPolicy{owner: "test", maxAttempts: maxAttempts, initialDelay: DefaultInitialDelay, maxDelay: DefaultMaxDelay}
}

// TestAttemptLoop_BoundAndPredicate pins RunRetriable.handle: a retriable error
// is retried until maxAttempts attempts have run (currAttempt + 1 <
// maxAttempts), and the last error is returned unchanged; an error that is not
// retriable ends the call after one attempt.
func TestAttemptLoop_BoundAndPredicate(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(1)
	conflict := fmt.Errorf("body: %w", fdb.Error{Code: 1020})
	runs := 0
	_, err := attemptLoop(context.Background(), env, nil, testPolicy(10), RouteAttempt, func(AttemptCall) (any, error) {
		runs++
		return nil, conflict
	})
	if runs != 10 || err != conflict {
		t.Fatalf("retriable: %d attempts, err %v; want 10 attempts and the last error unchanged", runs, err)
	}

	runs = 0
	terminal := fdb.Error{Code: 2000}
	_, err = attemptLoop(context.Background(), env, nil, testPolicy(10), RouteAttempt, func(AttemptCall) (any, error) {
		runs++
		return nil, terminal
	})
	if runs != 1 || err != terminal {
		t.Fatalf("terminal: %d attempts, err %v; want 1 attempt", runs, err)
	}
}

// TestAttemptLoop_CallNumbering pins AttemptCall: Execution counts every
// execution, Attempt only counted ones, and CallID is fixed within a call.
func TestAttemptLoop_CallNumbering(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(2)
	window := &SPFreshSplitWindowError{Sealed: []SPFreshSealedPosting{{PostingID: 1}}}
	errs := []error{window, fdb.Error{Code: 1020}, window, nil}
	var calls []AttemptCall
	_, err := attemptLoop(context.Background(), env, nil, testPolicy(10), RouteAttempt, func(call AttemptCall) (any, error) {
		calls = append(calls, call)
		return nil, errs[len(calls)-1]
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAttempt := []int{0, 0, 1, 1}
	for i, c := range calls {
		if c.Execution != i || c.Attempt != wantAttempt[i] || c.CallID != calls[0].CallID {
			t.Fatalf("execution %d: %+v; want Execution %d, Attempt %d, one CallID", i, c, i, wantAttempt[i])
		}
	}
}

// TestAttemptLoop_SplitWindowIsUncounted pins that the SPFresh split-window
// signal does not use an attempt: a call bounded at 2 attempts outlasts 30
// split windows (each with a different seal, so the stall bound never trips).
func TestAttemptLoop_SplitWindowIsUncounted(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(3)
	runs := 0
	_, err := attemptLoop(context.Background(), env, nil, testPolicy(2), RouteAttempt, func(AttemptCall) (any, error) {
		runs++
		if runs <= 30 {
			return nil, &SPFreshSplitWindowError{Sealed: []SPFreshSealedPosting{{PostingID: 7, Epoch: int64(runs)}}}
		}
		return nil, nil
	})
	if err != nil || runs != 31 {
		t.Fatalf("got %d executions, err %v; want 31 and success", runs, err)
	}
}

// TestAttemptLoop_StalledSealUnderSimulation pins the simulation bound: the
// same sealed postings on 100 consecutive retries fail the call with
// SPFreshStalledSealError, which is not retried.
func TestAttemptLoop_StalledSealUnderSimulation(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(4)
	sealed := []SPFreshSealedPosting{{CellID: 1, PostingID: 2, Epoch: 3}}
	runs := 0
	_, err := attemptLoop(context.Background(), env, nil, testPolicy(10), RouteAttempt, func(AttemptCall) (any, error) {
		runs++
		return nil, &SPFreshSplitWindowError{Sealed: sealed}
	})
	var stalled *SPFreshStalledSealError
	if !errors.As(err, &stalled) || runs != spfreshStalledSealBound || stalled.Retries != spfreshStalledSealBound {
		t.Fatalf("got %d executions, err %v; want SPFreshStalledSealError after %d", runs, err, spfreshStalledSealBound)
	}
	if isRetriableAnyCause(err) {
		t.Fatal("SPFreshStalledSealError must not be retriable")
	}
}

// TestAttemptLoop_ContextEndsMidCall pins the context rule: an ended context
// stops the loop between attempts, and the error wraps both the context's
// error and the last attempt's.
func TestAttemptLoop_ContextEndsMidCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	conflict := fdb.Error{Code: 1020}
	runs := 0
	_, err := attemptLoop(ctx, nil, nil, attemptPolicy{maxAttempts: 10, initialDelay: time.Millisecond, maxDelay: time.Millisecond}, RouteAttempt, func(AttemptCall) (any, error) {
		runs++
		cancel()
		return nil, conflict
	})
	if runs != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, conflict) {
		t.Fatalf("got %d executions, err %v; want 1 and an error wrapping Canceled and 1020", runs, err)
	}
}

// strippingTransactor reduces any FDB error in a call's result to a bare
// fdb.Error{Code}, the shape a backend that drops the body's chain returns.
type strippingTransactor struct {
	inner *simfdb.SimDB
	// rerun, when set, makes each call execute fn a second time when the
	// first execution fails, as a transactor with its own loop may.
	rerun bool
}

func strip(err error) error {
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fdb.Error{Code: fe.Code}
	}
	return err
}

func (s *strippingTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return s.TransactCtx(context.Background(), fn)
}

func (s *strippingTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	r, err := s.inner.Transact(fn)
	if err != nil && s.rerun {
		r, err = s.inner.Transact(fn)
	}
	return r, strip(err)
}

func (s *strippingTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return s.ReadTransactCtx(context.Background(), fn)
}

func (s *strippingTransactor) ReadTransactCtx(ctx context.Context, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	r, err := s.inner.ReadTransact(fn)
	return r, strip(err)
}

func strippingDB(t *testing.T, seed uint64, rerun bool) *FDBDatabase {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	backend := simfdb.New(env)
	return NewFDBDatabaseWithTransactor(&strippingTransactor{inner: backend, rerun: rerun}, fdb.Database{}).SetEnv(env)
}

// TestRun_BodyErrorChainSurvivesAStrippingTransactor pins the body-error
// recorder independently of any backend: through a transactor that strips the
// chain, Run and RunRead still see the body's split-window error (retried
// uncounted) and hand the caller the body's chain at the limit.
func TestRun_BodyErrorChainSurvivesAStrippingTransactor(t *testing.T) {
	t.Parallel()
	db := strippingDB(t, 5, false)
	_ = db.SetMaxAttempts(2)

	runs := 0
	_, err := db.Run(context.Background(), func(*FDBRecordContext) (any, error) {
		runs++
		if runs <= 5 {
			return nil, &SPFreshSplitWindowError{Sealed: []SPFreshSealedPosting{{Epoch: int64(runs)}}}
		}
		return nil, nil
	})
	if err != nil || runs != 6 {
		t.Fatalf("Run: %d executions, err %v; want the split windows retried uncounted, then success", runs, err)
	}

	type wrapped struct{ error }
	body := fmt.Errorf("read body: %w", fdb.Error{Code: 1020})
	_, err = db.RunRead(context.Background(), func(fdb.ReadTransaction) (any, error) { return nil, body })
	if err != body {
		t.Fatalf("RunRead returned %v (%T); want the body's own error at the limit", err, err)
	}
	_, err = db.Run(context.Background(), func(*FDBRecordContext) (any, error) { return nil, wrapped{body} })
	var w wrapped
	if !errors.As(err, &w) {
		t.Fatalf("Run returned %v (%T); want the body's own error at the limit", err, err)
	}
}

// TestRun_RecordedErrorIsTheLastExecutions pins the per-execution reset: when
// a transactor re-runs the body within one call, an earlier execution's
// split-window error must not be paired with the later execution's failure of
// the same code. Execution 1 returns the split window, execution 2's body
// succeeds and its commit fails with 1020 (a conflicting write between the
// two). The 1020 is bare and COUNTED.
func TestRun_RecordedErrorIsTheLastExecutions(t *testing.T) {
	t.Parallel()
	db := strippingDB(t, 6, true)
	_ = db.SetMaxAttempts(1)
	key := fdb.Key("recorded-error-last-execution")

	execs := 0
	_, err := db.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
		execs++
		if execs == 1 {
			return nil, &SPFreshSplitWindowError{}
		}
		tx := rtx.Transaction()
		if _, err := tx.Get(key).Get(); err != nil {
			return nil, err
		}
		tx.Set(key, []byte("mine"))
		// A conflicting commit lands between this read and the commit.
		_, cerr := db.transactor.(*strippingTransactor).inner.Transact(func(o fdb.WritableTransaction) (any, error) {
			o.Set(key, []byte("theirs"))
			return nil, nil
		})
		return nil, cerr
	})
	var window *SPFreshSplitWindowError
	if errors.As(err, &window) {
		t.Fatalf("Run returned execution 1's split window (%v); the last execution failed with a bare 1020", err)
	}
	if err != (fdb.Error{Code: 1020}) {
		t.Fatalf("Run returned %v (%T), want a bare 1020 counted against the one attempt", err, err)
	}
}
